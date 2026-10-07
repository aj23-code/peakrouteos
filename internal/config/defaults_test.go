package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultDHCPForA24(t *testing.T) {
	d := DefaultDHCP("192.168.1.1", "255.255.255.0")
	want := DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"}
	if d == nil || *d != want {
		t.Errorf("DefaultDHCP = %+v, want %+v", d, want)
	}

	// follows the router's own /24
	d = DefaultDHCP("10.5.5.1", "255.255.255.0")
	if d == nil || d.RangeStart != "10.5.5.100" || d.RangeEnd != "10.5.5.200" {
		t.Errorf("DefaultDHCP for 10.5.5.1 = %+v", d)
	}
}

func TestDefaultDHCPIsOnlyOfferedWhereTheRangeFits(t *testing.T) {
	for _, c := range []struct{ ip, mask string }{
		{"192.168.1.1", "255.255.255.128"}, // /25: .100-.200 would not fit
		{"10.0.0.1", "255.255.0.0"},
		{"192.168.1.1", ""},
		{"", "255.255.255.0"},
		{"not-an-ip", "255.255.255.0"},
		{"192.168.1.1/24", "255.255.255.0"},
	} {
		if d := DefaultDHCP(c.ip, c.mask); d != nil {
			t.Errorf("DefaultDHCP(%q, %q) = %+v, want nil", c.ip, c.mask, d)
		}
	}
}

func TestTheDefaultsAreAValidConfiguration(t *testing.T) {
	cfg := NetworkConfig{
		WanInterface: "eth0", LanInterfaces: []string{"eth1"},
		LanIP: DefaultLanIP, LanSubnet: DefaultLanNetmask,
		DHCP: DefaultDHCP(DefaultLanIP, DefaultLanNetmask),
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the router defaults do not validate: %v", err)
	}
}

func TestDHCPRangeFitsLan(t *testing.T) {
	ok := func(start, end string) error {
		return DHCPRangeFitsLan("192.168.1.1", "255.255.255.0", start, end)
	}
	if err := ok("192.168.1.100", "192.168.1.200"); err != nil {
		t.Errorf("a normal range was rejected: %v", err)
	}
	for _, c := range []struct{ name, start, end string }{
		{"outside the subnet", "10.0.0.5", "10.0.0.9"},
		{"reversed", "192.168.1.200", "192.168.1.100"},
		{"covers the router", "192.168.1.1", "192.168.1.50"},
		{"junk", "x", "y"},
		{"broadcast end", "192.168.1.100", "192.168.1.255"},
	} {
		if err := ok(c.start, c.end); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
	if err := DHCPRangeFitsLan("bad", "255.255.255.0", "192.168.1.100", "192.168.1.200"); err == nil {
		t.Error("an invalid LAN address must not be reported as fitting")
	}
}

func TestAdoptNetworkWritesOnlyWhenSomethingChanged(t *testing.T) {
	mgr, dir := newTestManager(t)
	path := filepath.Join(dir, "network.json")

	changed := mgr.AdoptNetwork(func(c *NetworkConfig) bool { c.LanIP = "192.168.1.1"; return true })
	if changed != nil {
		t.Fatal(changed)
	}
	if got := mgr.NetworkConfig().LanIP; got != "192.168.1.1" {
		t.Errorf("active LanIP = %q", got)
	}
	onDisk, err := LoadNetworkConfig(path)
	if err != nil || onDisk.LanIP != "192.168.1.1" {
		t.Fatalf("network.json = %+v, %v", onDisk, err)
	}

	before, _ := os.Stat(path)
	if err := mgr.AdoptNetwork(func(c *NetworkConfig) bool { return false }); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("network.json was rewritten although nothing changed")
	}
}

func TestAdoptionIsNotAPendingChange(t *testing.T) {
	mgr, dir := newTestManager(t)

	if err := mgr.AdoptNetwork(func(c *NetworkConfig) bool {
		c.DHCP = DefaultDHCP("192.168.1.1", "255.255.255.0")
		c.LanIP = "192.168.1.1"
		c.LanSubnet = "255.255.255.0"
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if got := mgr.StagedChanges(); len(got) != 0 {
		t.Errorf("adopting settings must not look like an uncommitted change: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "network.json.staged")); !os.IsNotExist(err) {
		t.Error("adoption must not create a stage file")
	}
	select {
	case <-mgr.OnNetworkChange():
		t.Error("adoption must not emit a network-change notification (nothing is applied)")
	default:
	}
}

func TestAdoptionKeepsAPendingCandidateConsistent(t *testing.T) {
	mgr, _ := newTestManager(t)
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { c.WanInterface = "eth9"; return nil }); err != nil {
		t.Fatal(err)
	}

	if err := mgr.AdoptNetwork(func(c *NetworkConfig) bool {
		if c.LanIP != "" {
			return false
		}
		c.LanIP = "192.168.1.1"
		return true
	}); err != nil {
		t.Fatal(err)
	}

	// the user's one real edit is still the only pending change
	got := mgr.StagedChanges()
	if len(got) != 1 || got[0].Field != "wan_interface" {
		t.Errorf("pending changes = %+v, want only the WAN interface edit", got)
	}
	if staged, _ := mgr.StagedNetworkConfig(); staged.LanIP != "192.168.1.1" {
		t.Errorf("the candidate missed the adopted value: %q", staged.LanIP)
	}
}

func TestFailedAdoptionChangesNothing(t *testing.T) {
	mgr, dir := newTestManager(t)
	readOnlyDir(t, dir)

	if err := mgr.AdoptNetwork(func(c *NetworkConfig) bool { c.LanIP = "192.168.1.1"; return true }); err == nil {
		t.Fatal("expected a save failure in a read-only directory")
	}
	if got := mgr.NetworkConfig().LanIP; got != "" {
		t.Errorf("memory moved ahead of disk: %q", got)
	}
}

// Calling the Manager's own accessors from inside the edit function used to deadlock the daemon
// at startup (the lock was held while the function ran). It must simply work.
func TestAdoptNetworkCallbackMayUseTheManager(t *testing.T) {
	mgr, _ := newTestManager(t)

	done := make(chan error, 1)
	go func() {
		done <- mgr.AdoptNetwork(func(c *NetworkConfig) bool {
			_ = mgr.SysConfig()
			_ = mgr.NetworkConfig()
			_ = mgr.Users()
			_ = mgr.IsConfigured()
			c.LanIP = "192.168.1.1"
			return true
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AdoptNetwork deadlocked when its callback used the Manager")
	}
	if got := mgr.NetworkConfig().LanIP; got != "192.168.1.1" {
		t.Errorf("LanIP = %q", got)
	}
}

func TestAdoptNetworkRefusesToOverwriteAConcurrentChange(t *testing.T) {
	mgr, dir := newTestManager(t)

	err := mgr.AdoptNetwork(func(c *NetworkConfig) bool {
		// someone else commits a change while this edit is being worked out
		if err := mgr.StageNetwork(func(s *NetworkConfig) error { s.WanInterface = "eth7"; return nil }); err != nil {
			t.Error(err)
		}
		if err := mgr.CommitNetwork(); err != nil {
			t.Error(err)
		}
		c.LanIP = "192.168.1.1"
		return true
	})
	if !errors.Is(err, ErrConfigChanged) {
		t.Fatalf("error = %v, want ErrConfigChanged", err)
	}

	got := mgr.NetworkConfig()
	if got.WanInterface != "eth7" || got.LanIP != "" {
		t.Errorf("the concurrent change was clobbered or the stale edit applied: %+v", got)
	}
	onDisk, loadErr := LoadNetworkConfig(filepath.Join(dir, "network.json"))
	if loadErr != nil || onDisk.WanInterface != "eth7" || onDisk.LanIP != "" {
		t.Errorf("network.json = %+v, %v", onDisk, loadErr)
	}
}

package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNoChangesWhenConfigsMatch(t *testing.T) {
	if got := DiffNetwork(validNet(), validNet()); len(got) != 0 {
		t.Errorf("identical configs produced changes: %+v", got)
	}
}

func TestDiffReportsEachChangedSettingInOrder(t *testing.T) {
	active := validNet()
	candidate := validNet()
	candidate.LanIP = "10.0.0.1"
	candidate.LanSubnet = "255.255.0.0"
	candidate.DHCP = &DHCPConfig{Enabled: true, RangeStart: "10.0.1.10", RangeEnd: "10.0.1.99", LeaseTime: "30m"}
	candidate.LanInterfaces = []string{"eth1", "eth2"}

	got := DiffNetwork(active, candidate)
	want := []Change{
		{Field: "lan_interfaces", Label: "LAN ports", Old: "eth1", New: "eth1, eth2"},
		{Field: "lan_ip", Label: "LAN IP address", Old: "192.168.1.1", New: "10.0.0.1"},
		{Field: "lan_subnet", Label: "LAN netmask", Old: "255.255.255.0", New: "255.255.0.0"},
		{Field: "dhcp_range_start", Label: "DHCP range start", Old: "192.168.1.100", New: "10.0.1.10"},
		{Field: "dhcp_range_end", Label: "DHCP range end", Old: "192.168.1.200", New: "10.0.1.99"},
		{Field: "dhcp_lease_time", Label: "DHCP lease time", Old: "12h", New: "30m"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changes =\n%+v\nwant\n%+v", got, want)
	}
}

func TestDiffWanChanges(t *testing.T) {
	active := validNet()
	candidate := staticWan(validNet())

	got := DiffNetwork(active, candidate)
	fields := map[string]Change{}
	for _, c := range got {
		fields[c.Field] = c
	}

	if c := fields["wan_mode"]; c.Old != "dhcp" || c.New != "static" {
		t.Errorf("wan_mode = %+v", c)
	}
	if c := fields["wan_ip"]; c.Old != "(none)" || c.New != "203.0.113.10" {
		t.Errorf("wan_ip = %+v", c)
	}
	if c := fields["wan_dns"]; c.Old != "(none)" || c.New != "1.1.1.1, 9.9.9.9" {
		t.Errorf("wan_dns = %+v", c)
	}
	if _, present := fields["lan_ip"]; present {
		t.Error("lan_ip did not change but was reported")
	}
}

// Detecting hardware is not a user edit: it must never show up as an uncommitted change
func TestDiscoveredHardwareIsNotAPendingChange(t *testing.T) {
	active := validNet()
	candidate := validNet()
	candidate.Interfaces = []SystemInterface{{Name: "eth9", MAC: "aa:bb:cc:dd:ee:ff", State: "up"}}
	candidate.ManagedSwitches = []ManagedSwitchConfig{{SwitchID: "sw1"}}

	if got := DiffNetwork(active, candidate); len(got) != 0 {
		t.Errorf("hardware inventory produced changes: %+v", got)
	}
}

func TestEmptyWanModeMeansDHCP(t *testing.T) {
	a, b := validNet(), validNet()
	a.WanMode, b.WanMode = "", "DHCP"
	if got := DiffNetwork(a, b); len(got) != 0 {
		t.Errorf("an unset WAN mode and \"DHCP\" are the same setting, got %+v", got)
	}
}

func TestSurroundingWhitespaceIsNotAChange(t *testing.T) {
	a, b := validNet(), validNet()
	b.LanIP = "192.168.1.1 "
	if got := DiffNetwork(a, b); len(got) != 0 {
		t.Errorf("trailing whitespace reported as a change: %+v", got)
	}
}

func TestDHCPChangesFromNotSetToConfigured(t *testing.T) {
	active := validNet()
	active.DHCP = nil // an older install, DHCP configured outside the JSON
	candidate := validNet()

	got := DiffNetwork(active, candidate)
	if len(got) == 0 || got[0].Field != "dhcp_enabled" || got[0].Old != "(not set)" || got[0].New != "enabled" {
		t.Errorf("changes = %+v, want dhcp_enabled (not set) -> enabled first", got)
	}

	// and disabling it is a distinct, visible change
	candidate.DHCP = &DHCPConfig{Enabled: false}
	got = DiffNetwork(validNet(), candidate)
	if got[0].Field != "dhcp_enabled" || got[0].Old != "enabled" || got[0].New != "disabled" {
		t.Errorf("disable = %+v", got)
	}
}

func TestStagedChangesComparesCandidateWithActive(t *testing.T) {
	mgr, _ := newTestManager(t)

	if got := mgr.StagedChanges(); got != nil {
		t.Errorf("no candidate yet, got %+v", got)
	}

	cfg := validNet()
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { *c = cfg; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
	if got := mgr.StagedChanges(); got != nil {
		t.Errorf("nothing staged after a commit, got %+v", got)
	}

	if err := mgr.StageNetwork(func(c *NetworkConfig) error { c.LanIP = "192.168.50.1"; return nil }); err != nil {
		t.Fatal(err)
	}
	got := mgr.StagedChanges()
	if len(got) != 1 || got[0].Field != "lan_ip" || got[0].Old != "192.168.1.1" || got[0].New != "192.168.50.1" {
		t.Errorf("StagedChanges() = %+v", got)
	}

}

// Stage files come and go with real changes: reverting an edit removes the candidate
func TestRevertingAnEditRemovesTheStageFile(t *testing.T) {
	mgr, dir := newTestManager(t)
	stagedPath := filepath.Join(dir, "network.json.staged")

	if err := mgr.StageNetwork(func(c *NetworkConfig) error { *c = validNet(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}

	if err := mgr.StageNetwork(func(c *NetworkConfig) error { c.LanIP = "192.168.50.1"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("a real change should leave a stage file: %v", err)
	}
	if _, has := mgr.StagedNetworkConfig(); !has {
		t.Fatal("StagedNetworkConfig reports no pending change")
	}

	// Put the value back: nothing is pending any more
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { c.LanIP = "192.168.1.1"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Error("the stage file should be gone once the edit is reverted")
	}
	if _, has := mgr.StagedNetworkConfig(); has {
		t.Error("StagedNetworkConfig still reports a pending change")
	}
	if got := mgr.StagedChanges(); len(got) != 0 {
		t.Errorf("StagedChanges() = %+v, want none", got)
	}

	// and a later edit starts from the active config again
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { c.LanIP = "10.9.8.1"; return nil }); err != nil {
		t.Fatal(err)
	}
	if got := mgr.StagedChanges(); len(got) != 1 || got[0].New != "10.9.8.1" {
		t.Errorf("StagedChanges() = %+v", got)
	}
}

func TestStageFileThatMatchesTheActiveConfigIsCleanedUpAtStartup(t *testing.T) {
	mgr, dir := newTestManager(t)
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { *c = validNet(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}

	// A leftover candidate identical to the active config (e.g. from an interrupted run)
	stagedPath := filepath.Join(dir, "network.json.staged")
	active := mgr.NetworkConfig()
	if err := SaveNetworkConfig(stagedPath, &active); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := reloaded.StagedNetworkConfig(); has {
		t.Error("a no-op stage file was treated as a pending change")
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Error("the no-op stage file should have been removed")
	}
}

func TestRealPendingChangeSurvivesARestart(t *testing.T) {
	mgr, dir := newTestManager(t)
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { *c = validNet(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { c.LanIP = "192.168.77.1"; return nil }); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.StagedChanges(); len(got) != 1 || got[0].New != "192.168.77.1" {
		t.Errorf("after restart StagedChanges() = %+v", got)
	}
}

func TestDetectedHardwareAloneNeverCreatesAStageFile(t *testing.T) {
	mgr, dir := newTestManager(t)
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { *c = validNet(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}

	if err := mgr.StageNetwork(func(c *NetworkConfig) error {
		c.Interfaces = []SystemInterface{{Name: "eth9", State: "up"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "network.json.staged")); !os.IsNotExist(err) {
		t.Error("a hardware-inventory refresh must not look like a pending change")
	}
}

func TestDHCPSectionSurvivesJSONRoundTripAndCloning(t *testing.T) {
	mgr, dir := newTestManager(t)
	if err := mgr.StageNetwork(func(c *NetworkConfig) error { *c = validNet(); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}

	// Mutating a returned copy must not reach the live DHCP settings (the pointer is cloned)
	cfg := mgr.NetworkConfig()
	cfg.DHCP.RangeStart = "192.168.1.5"
	if got := mgr.NetworkConfig().DHCP.RangeStart; got != "192.168.1.100" {
		t.Errorf("a copy shares the DHCP struct with the manager: %q", got)
	}

	reloaded, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	d := reloaded.NetworkConfig().DHCP
	if d == nil || !d.Enabled || d.RangeStart != "192.168.1.100" || d.LeaseTime != "12h" {
		t.Errorf("DHCP after reload = %+v", d)
	}
}

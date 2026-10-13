package translator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func TestParseDnsmasqDHCP(t *testing.T) {
	good, ok := ParseDnsmasqDHCP([]byte("interface=eth1\ndhcp-range=192.168.1.100,192.168.1.200,255.255.255.0,12h\ndhcp-option=option:router,192.168.1.1\n"))
	want := config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"}
	if !ok || good != want {
		t.Errorf("ParseDnsmasqDHCP = %+v, %v; want %+v", good, ok, want)
	}

	// tolerant of surrounding whitespace
	if d, ok := ParseDnsmasqDHCP([]byte("  dhcp-range=10.0.0.5,10.0.0.9,255.255.255.0,30m  \n")); !ok || d.LeaseTime != "30m" {
		t.Errorf("whitespace: %+v, %v", d, ok)
	}

	notAdoptable := map[string]string{
		"empty file":       "",
		"no range":         "interface=eth1\n",
		"two ranges":       "dhcp-range=10.0.0.5,10.0.0.9,255.255.255.0,1h\ndhcp-range=10.0.1.5,10.0.1.9,255.255.255.0,1h\n",
		"tagged range":     "dhcp-range=set:red,10.0.0.5,10.0.0.9,255.255.255.0,1h\n",
		"no lease":         "dhcp-range=10.0.0.5,10.0.0.9,255.255.255.0\n",
		"ipv6":             "dhcp-range=::1,::ff,constructor:eth0\n",
		"commented out":    "# dhcp-range=10.0.0.5,10.0.0.9,255.255.255.0,1h\n",
		"carriage returns": "dhcp-range=a,b\r\n",
	}
	for name, content := range notAdoptable {
		if d, ok := ParseDnsmasqDHCP([]byte(content)); ok {
			t.Errorf("%s: unexpectedly adopted %+v", name, d)
		}
	}
}

func blank() config.NetworkConfig {
	return config.NetworkConfig{WanInterface: "eth0", LanInterfaces: []string{"eth1"}}
}

func rootWith(t *testing.T, dnsmasq string) RootFS {
	t.Helper()
	root := t.TempDir()
	if dnsmasq != "" {
		full := filepath.Join(root, PathDnsmasqDHCP)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(dnsmasq), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return RootFS(root)
}

func TestBlankLanBecomesTheRouterDefault(t *testing.T) {
	cfg := blank()
	if !AdoptExisting(&cfg, rootWith(t, ""), "ROUTER") {
		t.Fatal("expected changes")
	}
	if cfg.LanIP != "192.168.1.1" || cfg.LanSubnet != "255.255.255.0" {
		t.Errorf("LAN = %s %s, want 192.168.1.1 255.255.255.0", cfg.LanIP, cfg.LanSubnet)
	}
	want := config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"}
	if cfg.DHCP == nil || *cfg.DHCP != want {
		t.Errorf("DHCP = %+v, want the default %+v", cfg.DHCP, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the adopted defaults do not validate: %v", err)
	}
}

func TestAnExistingDnsmasqRangeBeatsTheDefault(t *testing.T) {
	cfg := blank()
	cfg.LanIP, cfg.LanSubnet = "192.168.1.1", "255.255.255.0"
	root := rootWith(t, "interface=eth1\ndhcp-range=192.168.1.20,192.168.1.60,255.255.255.0,2h\n")

	if !AdoptExisting(&cfg, root, "router") {
		t.Fatal("expected the range to be imported")
	}
	want := config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.20", RangeEnd: "192.168.1.60", LeaseTime: "2h"}
	if cfg.DHCP == nil || *cfg.DHCP != want {
		t.Errorf("DHCP = %+v, want the existing range %+v", cfg.DHCP, want)
	}
}

func TestAnUnderstoodButMismatchedFileIsNotAdopted(t *testing.T) {
	cfg := blank()
	cfg.LanIP, cfg.LanSubnet = "192.168.1.1", "255.255.255.0"
	// a range for some other subnet: do not pretend it belongs to this LAN
	root := rootWith(t, "dhcp-range=10.9.9.20,10.9.9.60,255.255.255.0,2h\n")

	AdoptExisting(&cfg, root, "ROUTER")
	if cfg.DHCP != nil {
		t.Errorf("a range outside the LAN was adopted: %+v", cfg.DHCP)
	}
}

func TestAnUnparseableDnsmasqFileLeavesDhcpUnmanaged(t *testing.T) {
	cfg := blank()
	cfg.LanIP, cfg.LanSubnet = "192.168.1.1", "255.255.255.0"
	root := rootWith(t, "dhcp-range=set:a,192.168.1.10,192.168.1.20,1h\ndhcp-range=192.168.1.30,192.168.1.40,1h\n")

	if AdoptExisting(&cfg, root, "ROUTER") {
		t.Error("nothing should change: someone's hand-written config must not be replaced by a guess")
	}
	if cfg.DHCP != nil {
		t.Errorf("DHCP = %+v, want it left unmanaged", cfg.DHCP)
	}
}

func TestAnInvalidLeaseInTheFileIsNotAdopted(t *testing.T) {
	cfg := blank()
	cfg.LanIP, cfg.LanSubnet = "192.168.1.1", "255.255.255.0"
	AdoptExisting(&cfg, rootWith(t, "dhcp-range=192.168.1.20,192.168.1.60,255.255.255.0,5\n"), "ROUTER")
	if cfg.DHCP != nil {
		t.Errorf("a lease with no unit was adopted: %+v", cfg.DHCP)
	}
}

func TestDefaultDhcpFollowsACustomLanAddress(t *testing.T) {
	cfg := blank()
	cfg.LanIP, cfg.LanSubnet = "10.5.5.1", "255.255.255.0"
	AdoptExisting(&cfg, rootWith(t, ""), "ROUTER")
	if cfg.DHCP == nil || cfg.DHCP.RangeStart != "10.5.5.100" || cfg.DHCP.RangeEnd != "10.5.5.200" {
		t.Errorf("DHCP = %+v", cfg.DHCP)
	}
	if cfg.LanIP != "10.5.5.1" {
		t.Errorf("an existing LAN address was overwritten: %s", cfg.LanIP)
	}
}

func TestNoDefaultDhcpWhereTheRangeWouldNotFit(t *testing.T) {
	cfg := blank()
	cfg.LanIP, cfg.LanSubnet = "10.0.0.1", "255.255.0.0"
	AdoptExisting(&cfg, rootWith(t, ""), "ROUTER")
	if cfg.DHCP != nil {
		t.Errorf("DHCP = %+v: a /24-style default range must not be forced onto a /16", cfg.DHCP)
	}
}

func TestAdoptionIsIdempotentAndNeverOverwrites(t *testing.T) {
	cfg := blank()
	cfg.LanIP, cfg.LanSubnet = "172.16.0.1", "255.255.255.0"
	cfg.DHCP = &config.DHCPConfig{Enabled: false}
	before := cfg
	beforeDHCP := *cfg.DHCP

	if AdoptExisting(&cfg, rootWith(t, "dhcp-range=172.16.0.20,172.16.0.60,255.255.255.0,1h\n"), "ROUTER") {
		t.Error("a config that already has everything must not be changed")
	}
	if cfg.LanIP != before.LanIP || *cfg.DHCP != beforeDHCP {
		t.Errorf("existing values were overwritten: %+v", cfg)
	}

	// and a second pass over a freshly adopted config does nothing
	fresh := blank()
	root := rootWith(t, "")
	AdoptExisting(&fresh, root, "ROUTER")
	if AdoptExisting(&fresh, root, "ROUTER") {
		t.Error("the second adoption changed something")
	}
}

func TestNothingIsAdoptedOutsideRouterMode(t *testing.T) {
	for _, mode := range []string{"AP", "ap", "bridge"} {
		cfg := blank()
		if AdoptExisting(&cfg, rootWith(t, ""), mode) || cfg.LanIP != "" || cfg.DHCP != nil {
			t.Errorf("mode %q: config was changed: %+v", mode, cfg)
		}
	}
	for _, mode := range []string{"", "ROUTER", "router"} {
		cfg := blank()
		if !AdoptExisting(&cfg, rootWith(t, ""), mode) {
			t.Errorf("mode %q should be treated as router", mode)
		}
	}
}

type failingReader struct{}

func (failingReader) ReadFile(string) ([]byte, error)  { return nil, os.ErrPermission }
func (failingReader) ListDir(string) ([]string, error) { return nil, os.ErrPermission }

func TestAnUnreadableDnsmasqFileLeavesDhcpUnmanaged(t *testing.T) {
	cfg := blank()
	cfg.LanIP, cfg.LanSubnet = "192.168.1.1", "255.255.255.0"
	AdoptExisting(&cfg, failingReader{}, "ROUTER")
	if cfg.DHCP != nil {
		t.Errorf("DHCP = %+v: a file we could not read must not be replaced by defaults", cfg.DHCP)
	}
}

// A box whose JSON has no dhcp section (set up by hand, or before the section existed) keeps the range its dnsmasq file has: after
// adoption the translator sees no difference between the JSON and the files.
func TestABoxWithoutADhcpSectionAdoptsItsRealRangeAndStillNeedsNoChanges(t *testing.T) {
	for name, w := range wizardScenarios {
		root := t.TempDir()
		install(t, root, mustRender(t, w.config()))

		// the JSON without its dhcp section
		cfg := w.config()
		cfg.DHCP = nil

		AdoptExisting(&cfg, RootFS(root), "ROUTER")
		if cfg.DHCP == nil || cfg.DHCP.RangeStart != w.dhcpStart || cfg.DHCP.RangeEnd != w.dhcpEnd {
			t.Fatalf("%s: adopted DHCP = %+v, want %s-%s", name, cfg.DHCP, w.dhcpStart, w.dhcpEnd)
		}

		d, err := Render(cfg, "ROUTER")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		plan, err := PlanChanges(RootFS(root), d)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Empty() || len(d.Notes) != 0 {
			t.Errorf("%s: after adoption the first commit would still change things: %+v notes=%v", name, plan, d.Notes)
		}
	}
}

func TestAdoptedDefaultsRenderAsAWorkingRouter(t *testing.T) {
	cfg := blank()
	AdoptExisting(&cfg, rootWith(t, ""), "ROUTER")

	d, err := Render(cfg, "ROUTER")
	if err != nil {
		t.Fatalf("the defaults cannot be rendered: %v", err)
	}
	dhcp := artifactContent(t, d, PathDnsmasqDHCP)
	if !strings.Contains(dhcp, "dhcp-range=192.168.1.100,192.168.1.200,255.255.255.0,12h") ||
		!strings.Contains(dhcp, "dhcp-option=option:router,192.168.1.1") {
		t.Errorf("default dnsmasq config:\n%s", dhcp)
	}
}

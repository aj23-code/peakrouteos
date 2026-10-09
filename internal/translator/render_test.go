package translator

import (
	"errors"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func routerCfg() config.NetworkConfig {
	return wizardScenarios["single LAN port, defaults"].config()
}

func hasArtifact(d Desired, path string) (Artifact, bool) {
	for _, a := range d.Artifacts {
		if a.Path == path {
			return a, true
		}
	}
	return Artifact{}, false
}

func TestRenderProducesOneFilePerServiceInApplyOrder(t *testing.T) {
	d, err := Render(routerCfg(), "ROUTER")
	if err != nil {
		t.Fatal(err)
	}

	var services []string
	for _, a := range d.Artifacts {
		if len(services) == 0 || services[len(services)-1] != a.Service {
			services = append(services, a.Service)
		}
	}
	if got := strings.Join(services, ","); got != "sysctl,networking,routes,nftables,dnsmasq" {
		t.Errorf("artifacts are grouped as %s, want sysctl,networking,routes,nftables,dnsmasq", got)
	}

	nft, _ := hasArtifact(d, PathNftables)
	if nft.Mode != 0755 {
		t.Errorf("nftables.conf mode = %o, want 0755 (Debian ships it executable)", nft.Mode)
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	a, _ := Render(routerCfg(), "ROUTER")
	b, _ := Render(routerCfg(), "ROUTER")
	for i := range a.Artifacts {
		if string(a.Artifacts[i].Content) != string(b.Artifacts[i].Content) {
			t.Errorf("%s renders differently on the second run", a.Artifacts[i].Path)
		}
	}
}

func TestOperationModeHandling(t *testing.T) {
	for _, mode := range []string{"", "ROUTER", "router", " Router "} {
		if _, err := Render(routerCfg(), mode); err != nil {
			t.Errorf("mode %q rejected: %v", mode, err)
		}
	}
	for _, mode := range []string{"AP", "ap", "bridge"} {
		d, err := Render(routerCfg(), mode)
		if !errors.Is(err, ErrUnsupportedMode) {
			t.Errorf("mode %q: error = %v, want ErrUnsupportedMode", mode, err)
		}
		if len(d.Artifacts) != 0 {
			t.Errorf("mode %q produced files", mode)
		}
	}
}

func TestInvalidConfigurationProducesNoFiles(t *testing.T) {
	cfg := routerCfg()
	cfg.LanIP = "not-an-ip"

	d, err := Render(cfg, "ROUTER")
	if err == nil {
		t.Fatal("an invalid configuration was rendered")
	}
	var ve config.ValidationError
	if !errors.As(err, &ve) || !ve.Has("lan_ip") {
		t.Errorf("error = %v, want a validation error on lan_ip", err)
	}
	if len(d.Artifacts) != 0 {
		t.Error("files were produced for an invalid configuration")
	}
}

func TestInjectionAttemptsNeverReachTheTemplates(t *testing.T) {
	evil := []func(*config.NetworkConfig){
		func(c *config.NetworkConfig) { c.WanInterface = "eth0\nauto evil" },
		func(c *config.NetworkConfig) { c.LanInterfaces = []string{"eth1\"; flush ruleset; \""} },
		func(c *config.NetworkConfig) { c.LanIP = "192.168.1.1\n    gateway 6.6.6.6" },
		func(c *config.NetworkConfig) { c.DHCP.LeaseTime = "12h\ndhcp-option=3,6.6.6.6" },
		func(c *config.NetworkConfig) { c.DHCP.RangeStart = "192.168.1.100,192.168.1.101" },
	}
	for i, mutate := range evil {
		cfg := routerCfg()
		mutate(&cfg)
		if d, err := Render(cfg, "ROUTER"); err == nil || len(d.Artifacts) != 0 {
			t.Errorf("injection case %d was rendered: %v", i, err)
		}
	}
}

// Validation is the first line of defence; Render must still refuse unsafe values if it ever fails
func TestRenderRefusesUnsafeValuesEvenIfValidationIsBypassed(t *testing.T) {
	orig := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = orig }()

	evil := []func(*config.NetworkConfig){
		func(c *config.NetworkConfig) { c.WanInterface = "eth0\nauto evil" },
		func(c *config.NetworkConfig) { c.LanInterfaces = []string{`eth1"; flush ruleset; "`} },
		func(c *config.NetworkConfig) { c.LanIP = "192.168.1.1\n    gateway 6.6.6.6" },
		func(c *config.NetworkConfig) { c.LanSubnet = "255.255.255.0 # x" },
		func(c *config.NetworkConfig) { c.DHCP.LeaseTime = "12h\ndhcp-option=3,6.6.6.6" },
		func(c *config.NetworkConfig) { c.DHCP.RangeEnd = "192.168.1.200,tag:evil" },
		func(c *config.NetworkConfig) {
			c.WanMode, c.WanIP, c.WanSubnet = "static", "203.0.113.10", "255.255.255.0"
			c.WanDNS = []string{"1.1.1.1\nserver=6.6.6.6"}
		},
		func(c *config.NetworkConfig) {
			c.WanMode, c.WanIP, c.WanSubnet = "static", "203.0.113.10", "255.255.255.0"
			c.WanGateway = "203.0.113.1\n    post-up rm -rf /"
		},
	}
	for i, mutate := range evil {
		cfg := routerCfg()
		mutate(&cfg)
		if d, err := Render(cfg, "ROUTER"); err == nil || len(d.Artifacts) != 0 {
			t.Errorf("case %d: unsafe value reached the templates (err=%v)", i, err)
		}
	}

	// and the happy path still renders with validation off
	if _, err := Render(routerCfg(), "ROUTER"); err != nil {
		t.Errorf("a safe configuration was refused: %v", err)
	}
}

func TestEnsureSafeIsIndependentOfValidation(t *testing.T) {
	for _, ok := range []string{"eth0", "192.168.1.1", "12h", "br0", "enp0s31f6", "eth0.10", "a_b-c"} {
		if err := ensureSafe(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a\nb", `a"b`, "a;b", "a$b", "a`b", "a{b", "a#b", "a/b", "a\\b", "é"} {
		if err := ensureSafe(bad); err == nil {
			t.Errorf("%q must not be considered safe", bad)
		}
	}
}

func TestBridgeIsUsedOnlyForMultipleLanPorts(t *testing.T) {
	single := routerCfg()
	if got := LanTarget(single); got != "eth1" {
		t.Errorf("single port target = %q, want eth1", got)
	}

	multi := routerCfg()
	multi.LanInterfaces = []string{"eth1", "eth2"}
	if got := LanTarget(multi); got != "br0" {
		t.Errorf("two ports target = %q, want br0", got)
	}

	d, err := Render(multi, "ROUTER")
	if err != nil {
		t.Fatal(err)
	}
	ifaces := artifactContent(t, d, PathInterfaces)
	if !strings.Contains(ifaces, "bridge_ports eth1 eth2") {
		t.Errorf("bridge ports missing from interfaces:\n%s", ifaces)
	}
	for _, path := range []string{PathNftables, PathDnsmasqDHCP} {
		if !strings.Contains(artifactContent(t, d, path), "br0") {
			t.Errorf("%s does not use the bridge", path)
		}
	}
}

func TestStaticWanIsRendered(t *testing.T) {
	cfg := routerCfg()
	cfg.WanMode = "static"
	cfg.WanIP, cfg.WanSubnet, cfg.WanGateway = "203.0.113.10", "255.255.255.0", "203.0.113.1"
	cfg.WanDNS = []string{"1.1.1.1", "9.9.9.9"}

	d, err := Render(cfg, "ROUTER")
	if err != nil {
		t.Fatal(err)
	}

	ifaces := artifactContent(t, d, PathInterfaces)
	for _, want := range []string{
		"iface eth0 inet static\n", "    address 203.0.113.10\n", "    netmask 255.255.255.0\n",
		"    gateway 203.0.113.1\n", "    dns-nameservers 1.1.1.1 9.9.9.9\n",
	} {
		if !strings.Contains(ifaces, want) {
			t.Errorf("interfaces is missing %q:\n%s", want, ifaces)
		}
	}
	if strings.Contains(ifaces, "iface eth0 inet dhcp") {
		t.Error("a static WAN must not also be configured with DHCP")
	}

	upstream := artifactContent(t, d, PathDnsmasqDNS)
	if upstream != "no-resolv\nserver=1.1.1.1\nserver=9.9.9.9\n" {
		t.Errorf("upstream DNS file = %q", upstream)
	}
}

func TestDhcpWanRemovesTheUpstreamDnsFile(t *testing.T) {
	d, err := Render(routerCfg(), "ROUTER")
	if err != nil {
		t.Fatal(err)
	}
	a, ok := hasArtifact(d, PathDnsmasqDNS)
	if !ok || !a.Remove {
		t.Errorf("with a DHCP WAN the upstream DNS file should be marked for removal: %+v", a)
	}
}

func TestStaticWanWithoutDnsOmitsGatewayAndDnsLines(t *testing.T) {
	cfg := routerCfg()
	cfg.WanMode = "static"
	cfg.WanIP, cfg.WanSubnet = "203.0.113.10", "255.255.255.0"

	d, err := Render(cfg, "ROUTER")
	if err != nil {
		t.Fatal(err)
	}
	ifaces := artifactContent(t, d, PathInterfaces)
	if strings.Contains(ifaces, "gateway") || strings.Contains(ifaces, "dns-nameservers") {
		t.Errorf("optional lines should be left out:\n%s", ifaces)
	}
	if a, _ := hasArtifact(d, PathDnsmasqDNS); !a.Remove {
		t.Error("no WAN DNS configured, so there is no upstream file")
	}
}

func TestDhcpStates(t *testing.T) {
	// enabled: a file with the range, lease and the router's own address
	d, _ := Render(routerCfg(), "ROUTER")
	dhcp := artifactContent(t, d, PathDnsmasqDHCP)
	for _, want := range []string{
		"interface=eth1\n", "dhcp-range=192.168.1.100,192.168.1.200,255.255.255.0,12h\n",
		"dhcp-option=option:router,192.168.1.1\n", "dhcp-option=option:dns-server,192.168.1.1\n",
	} {
		if !strings.Contains(dhcp, want) {
			t.Errorf("dnsmasq config is missing %q:\n%s", want, dhcp)
		}
	}
	if len(d.Notes) != 0 {
		t.Errorf("unexpected notes: %v", d.Notes)
	}

	// disabled: the file is removed
	off := routerCfg()
	off.DHCP = &config.DHCPConfig{Enabled: false}
	d, err := Render(off, "ROUTER")
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := hasArtifact(d, PathDnsmasqDHCP); !ok || !a.Remove || a.Service != SvcDnsmasq {
		t.Errorf("disabling DHCP should remove the file: %+v", a)
	}

	// not managed yet: no artifact at all, plus a note
	unmanaged := routerCfg()
	unmanaged.DHCP = nil
	d, err = Render(unmanaged, "ROUTER")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := hasArtifact(d, PathDnsmasqDHCP); ok {
		t.Error("DHCP not managed by the JSON: the existing dnsmasq file must be left alone")
	}
	if len(d.Notes) != 1 || !strings.Contains(d.Notes[0], "not managed") {
		t.Errorf("notes = %v, want an explanation", d.Notes)
	}
}

func TestNftablesRulesUseTheConfiguredInterfaces(t *testing.T) {
	d, _ := Render(routerCfg(), "ROUTER")
	nft := artifactContent(t, d, PathNftables)

	for _, want := range []string{
		"policy drop;", `iifname "eth1" accept`, `iifname "eth1" oifname "eth0" accept`, `oifname "eth0" masquerade`,
	} {
		if !strings.Contains(nft, want) {
			t.Errorf("nftables rules are missing %q", want)
		}
	}
	if !strings.HasPrefix(nft, "#!/usr/sbin/nft -f\nflush ruleset\n") {
		t.Error("nftables.conf should start with the shebang and flush ruleset")
	}
}

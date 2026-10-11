package translator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func wanCfg(f func(*config.NetworkConfig)) config.NetworkConfig {
	c := routerCfg()
	c.WanInterface = "eth0"
	c.WanMode = "dhcp"
	c.WanIP, c.WanSubnet, c.WanGateway, c.WanDNS = "", "", "", nil
	f(&c)
	return c
}

func pppoe(c *config.NetworkConfig) {
	c.WanMode = "pppoe"
	c.WanPPPoE = &config.PPPoEConfig{Username: "user@isp.example", Password: "s3cret-pass"}
}

func file(t *testing.T, cfg config.NetworkConfig, path string) (Artifact, bool) {
	t.Helper()
	d := mustRender(t, cfg)
	return hasArtifact(d, path)
}

func text(t *testing.T, cfg config.NetworkConfig, path string) string {
	t.Helper()
	a, ok := file(t, cfg, path)
	if !ok || a.Remove {
		t.Fatalf("%s is not rendered", path)
	}
	return string(a.Content)
}

// wanStanzas is what the interfaces file says before the LAN's part.
func wanStanzas(t *testing.T, cfg config.NetworkConfig) string {
	t.Helper()
	s := text(t, cfg, PathInterfaces)
	s = strings.TrimPrefix(s, "auto lo\niface lo inet loopback\n\n")
	if i := strings.Index(s, "auto "+cfg.LanInterfaces[0]); i >= 0 {
		s = s[:i]
	}
	return s
}

func TestPlainDHCPAndStaticStayExactlyAsTheyWere(t *testing.T) {
	if got := wanStanzas(t, wanCfg(func(c *config.NetworkConfig) {})); got != "auto eth0\niface eth0 inet dhcp\n\n" {
		t.Errorf("dhcp: %q", got)
	}
	static := wanCfg(func(c *config.NetworkConfig) {
		c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway, c.WanDNS = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1", []string{"9.9.9.9", "1.1.1.1"}
	})
	want := "auto eth0\niface eth0 inet static\n    address 203.0.113.10\n    netmask 255.255.255.0\n    gateway 203.0.113.1\n    dns-nameservers 9.9.9.9 1.1.1.1\n\n"
	if got := wanStanzas(t, static); got != want {
		t.Errorf("static: %q", got)
	}
}

func TestWanStanzas(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.NetworkConfig
		want string
	}{
		{"dhcp with a host name", wanCfg(func(c *config.NetworkConfig) { c.WanHostname = "my-router" }),
			"auto eth0\niface eth0 inet dhcp\n    hostname my-router\n\n"},
		{"dhcp with a cloned MAC", wanCfg(func(c *config.NetworkConfig) { c.WanMAC = "02:11:22:33:44:55" }),
			"auto eth0\niface eth0 inet dhcp\n    hwaddress ether 02:11:22:33:44:55\n\n"},
		{"dhcp with a lower MTU", wanCfg(func(c *config.NetworkConfig) { c.WanMTU = 1400 }),
			"auto eth0\niface eth0 inet dhcp\n    post-up ip link set dev $IFACE mtu 1400\n\n"},
		{"dhcp with a jumbo MTU", wanCfg(func(c *config.NetworkConfig) { c.WanMTU = 9000 }),
			"auto eth0\niface eth0 inet dhcp\n    post-up ip link set dev $IFACE mtu 9000\n\n"},
		{"static with a lower MTU", wanCfg(func(c *config.NetworkConfig) {
			c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway, c.WanMTU = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1", 1400
		}), "auto eth0\niface eth0 inet static\n    address 203.0.113.10\n    netmask 255.255.255.0\n    gateway 203.0.113.1\n    mtu 1400\n\n"},
		{"a jumbo MTU over a VLAN raises the port first", wanCfg(func(c *config.NetworkConfig) { c.WanVlan = 10; c.WanMTU = 9000 }),
			"auto eth0.10\niface eth0.10 inet dhcp\n    vlan-raw-device eth0\n    pre-up ip link set dev eth0 mtu 9000\n    post-up ip link set dev $IFACE mtu 9000\n\n"},
		{"a smaller MTU over a VLAN leaves the port alone", wanCfg(func(c *config.NetworkConfig) { c.WanVlan = 10; c.WanMTU = 1400 }),
			"auto eth0.10\niface eth0.10 inet dhcp\n    vlan-raw-device eth0\n    post-up ip link set dev $IFACE mtu 1400\n\n"},
		{"dhcp with the usual MTU written out", wanCfg(func(c *config.NetworkConfig) { c.WanMTU = 1500 }),
			"auto eth0\niface eth0 inet dhcp\n\n"},
		{"dhcp with everything", wanCfg(func(c *config.NetworkConfig) { c.WanHostname = "gw"; c.WanMAC = "02:11:22:33:44:55"; c.WanMTU = 1400 }),
			"auto eth0\niface eth0 inet dhcp\n    hostname gw\n    hwaddress ether 02:11:22:33:44:55\n    post-up ip link set dev $IFACE mtu 1400\n\n"},
		{"dhcp over a VLAN", wanCfg(func(c *config.NetworkConfig) { c.WanVlan = 835 }),
			"auto eth0.835\niface eth0.835 inet dhcp\n    vlan-raw-device eth0\n\n"},
		{"dhcp over a VLAN with a cloned MAC and host name", wanCfg(func(c *config.NetworkConfig) {
			c.WanVlan = 10
			c.WanMAC = "02:11:22:33:44:55"
			c.WanHostname = "gw"
		}), "auto eth0.10\niface eth0.10 inet dhcp\n    vlan-raw-device eth0\n    hostname gw\n    hwaddress ether 02:11:22:33:44:55\n\n"},
		{"static over a VLAN", wanCfg(func(c *config.NetworkConfig) {
			c.WanVlan = 20
			c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1"
		}), "auto eth0.20\niface eth0.20 inet static\n    vlan-raw-device eth0\n    address 203.0.113.10\n    netmask 255.255.255.0\n    gateway 203.0.113.1\n\n"},
		{"pppoe", wanCfg(pppoe),
			"auto eth0\niface eth0 inet manual\n\n" +
				"auto peakroute-wan\niface peakroute-wan inet ppp\n    provider peakroute-wan\n\n"},
		{"pppoe over a VLAN with a cloned MAC", wanCfg(func(c *config.NetworkConfig) {
			pppoe(c)
			c.WanVlan = 835
			c.WanMAC = "02:11:22:33:44:55"
			c.WanMTU = 1400 // belongs to the session, not to the link
		}), "auto eth0.835\niface eth0.835 inet manual\n    vlan-raw-device eth0\n    hwaddress ether 02:11:22:33:44:55\n\n" +
			"auto peakroute-wan\niface peakroute-wan inet ppp\n    provider peakroute-wan\n\n"},
	}
	for _, c := range cases {
		if got := wanStanzas(t, c.cfg); got != c.want {
			t.Errorf("%s:\n%q\nwant\n%q", c.name, got, c.want)
		}
	}
}

func TestPPPoESessionFile(t *testing.T) {
	cfg := wanCfg(pppoe)
	a, ok := file(t, cfg, PathPPPPeer)
	if !ok || a.Remove || a.Mode != 0600 || a.Service != SvcNetworking {
		t.Fatalf("%+v", a)
	}
	want := `# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.
plugin rp-pppoe.so
nic-eth0
user "user@isp.example"
password "s3cret-pass"
hide-password
noauth
unit 0
mtu 1492
mru 1492
noipdefault
defaultroute
usepeerdns
persist
maxfail 0
holdoff 10
lcp-echo-interval 20
lcp-echo-failure 3
noaccomp
nodeflate
nobsdcomp
novj
novjccomp
nopcomp
`
	if string(a.Content) != want {
		t.Errorf("\n%s\nwant\n%s", a.Content, want)
	}

	// over a VLAN, with a service name and an MTU of its own
	cfg.WanVlan = 835
	cfg.WanMTU = 1400
	cfg.WanPPPoE.Service = "Internet"
	s := text(t, cfg, PathPPPPeer)
	for _, line := range []string{"nic-eth0.835\n", "rp_pppoe_service \"Internet\"\n", "mtu 1400\n", "mru 1400\n"} {
		if !strings.Contains(s, line) {
			t.Errorf("missing %q in\n%s", line, s)
		}
	}
	if strings.Contains(s, "mtu 1492") {
		t.Error("the usual MTU is written although another is set")
	}
}

func TestThePPPoEFileOnlyExistsForPPPoE(t *testing.T) {
	for _, mode := range []func(*config.NetworkConfig){func(c *config.NetworkConfig) {}, func(c *config.NetworkConfig) {
		c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1"
	}} {
		a, ok := file(t, wanCfg(mode), PathPPPPeer)
		if !ok || !a.Remove || a.Service != SvcNetworking {
			t.Errorf("not removed: %+v", a)
		}
	}
	// and the artifacts stay grouped by service, which the applier relies on
	d := mustRender(t, wanCfg(pppoe))
	var order []string
	for _, a := range d.Artifacts {
		if len(order) == 0 || order[len(order)-1] != a.Service {
			order = append(order, a.Service)
		}
	}
	if got := strings.Join(order, ","); got != "sysctl,networking,routes,nftables,dnsmasq" {
		t.Errorf("%s", got)
	}
}

func TestTheFirewallFollowsTheDeviceThatCarriesTheConnection(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.NetworkConfig
		dev  string
	}{
		{"plain", wanCfg(func(c *config.NetworkConfig) {}), "eth0"},
		{"vlan", wanCfg(func(c *config.NetworkConfig) { c.WanVlan = 835 }), "eth0.835"},
		{"pppoe", wanCfg(pppoe), "ppp0"},
		{"pppoe over a vlan", wanCfg(func(c *config.NetworkConfig) { pppoe(c); c.WanVlan = 835 }), "ppp0"},
	}
	for _, c := range cases {
		nft := text(t, c.cfg, PathNftables)
		for _, want := range []string{`oifname "` + c.dev + `" accept`, `oifname "` + c.dev + `" masquerade`} {
			if !strings.Contains(nft, want) {
				t.Errorf("%s: no %q in\n%s", c.name, want, nft)
			}
		}
		for _, other := range []string{"eth0", "eth0.835", "ppp0"} {
			if other != c.dev && strings.Contains(nft, `oifname "`+other+`"`) {
				t.Errorf("%s: the rules still name %s", c.name, other)
			}
		}
	}

	// another network's internet access leaves through the same device
	multi := wanCfg(func(c *config.NetworkConfig) {
		pppoe(c)
		c.Networks = []config.Network{{ID: "net2", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0", Internet: true}}
		c.PortNetworks = map[string]config.PortNetworks{c.LanInterfaces[0]: {Tagged: []string{"net2"}}}
	})
	if nft := text(t, multi, PathNftables); !strings.Contains(nft, `iifname "br20" oifname "ppp0" accept`) {
		t.Errorf("the guest network's internet rule:\n%s", nft)
	}
}

func TestMSSIsClampedOnlyWhenTheConnectionHasASmallerMTU(t *testing.T) {
	if nft := text(t, wanCfg(func(c *config.NetworkConfig) {}), PathNftables); strings.Contains(nft, "maxseg") {
		t.Errorf("a normal connection is clamped:\n%s", nft)
	}
	if nft := text(t, wanCfg(func(c *config.NetworkConfig) { c.WanMTU = 1500 }), PathNftables); strings.Contains(nft, "maxseg") {
		t.Errorf("1500 written out is clamped:\n%s", nft)
	}
	if nft := text(t, wanCfg(func(c *config.NetworkConfig) { c.WanMTU = 9000 }), PathNftables); strings.Contains(nft, "maxseg") {
		t.Errorf("a jumbo MTU is clamped:\n%s", nft)
	}

	nft := text(t, wanCfg(pppoe), PathNftables)
	for _, want := range []string{
		`oifname "ppp0" tcp flags syn tcp option maxseg size > 1452 tcp option maxseg size set 1452`,
		`iifname "ppp0" tcp flags syn tcp option maxseg size > 1452 tcp option maxseg size set 1452`,
	} {
		if !strings.Contains(nft, want) {
			t.Errorf("missing %q in\n%s", want, nft)
		}
	}
	// the clamp comes before the rule that lets traffic out, and after the connection-tracking rules
	i, j, k := strings.Index(nft, "ct state invalid drop\n        oifname \"ppp0\" tcp"), strings.Index(nft, "maxseg"), strings.Index(nft, `oifname "ppp0" accept`)
	if i < 0 || j > k {
		t.Errorf("the clamp is in the wrong place:\n%s", nft)
	}

	low := text(t, wanCfg(func(c *config.NetworkConfig) { c.WanMTU = 1400 }), PathNftables)
	if !strings.Contains(low, `oifname "eth0" tcp flags syn tcp option maxseg size > 1360 tcp option maxseg size set 1360`) {
		t.Errorf("a lower MTU set by hand:\n%s", low)
	}
}

func TestUpstreamDNSOverridesWorkWithEveryConnectionType(t *testing.T) {
	// not set: the provider's servers are used, nothing is written
	for name, cfg := range map[string]config.NetworkConfig{"dhcp": wanCfg(func(c *config.NetworkConfig) {}), "pppoe": wanCfg(pppoe)} {
		if a, ok := file(t, cfg, PathDnsmasqDNS); !ok || !a.Remove {
			t.Errorf("%s: %+v", name, a)
		}
	}
	for name, cfg := range map[string]config.NetworkConfig{
		"dhcp":  wanCfg(func(c *config.NetworkConfig) { c.WanDNS = []string{"9.9.9.9", "149.112.112.112"} }),
		"pppoe": wanCfg(func(c *config.NetworkConfig) { pppoe(c); c.WanDNS = []string{"9.9.9.9", "149.112.112.112"} }),
	} {
		if got := text(t, cfg, PathDnsmasqDNS); got != "no-resolv\nserver=9.9.9.9\nserver=149.112.112.112\n" {
			t.Errorf("%s: %q", name, got)
		}
		// they are not written into the connection's own stanza: only a static address has dns-nameservers
		if strings.Contains(text(t, cfg, PathInterfaces), "dns-nameservers") {
			t.Errorf("%s: dns-nameservers in the interfaces file", name)
		}
	}
}

func TestWanValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	saved := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = saved }()

	evil := map[string]func(*config.NetworkConfig){
		"host name":         func(c *config.NetworkConfig) { c.WanHostname = "a\n    post-up evil" },
		"host name space":   func(c *config.NetworkConfig) { c.WanHostname = "a b" },
		"mac":               func(c *config.NetworkConfig) { c.WanMAC = "02:11:22:33:44:55\nup evil" },
		"mac upper case":    func(c *config.NetworkConfig) { c.WanMAC = "02:11:22:33:44:AA" },
		"mac group":         func(c *config.NetworkConfig) { c.WanMAC = "01:11:22:33:44:55" },
		"vlan negative":     func(c *config.NetworkConfig) { c.WanVlan = -5 },
		"vlan too big":      func(c *config.NetworkConfig) { c.WanVlan = 5000 },
		"mtu negative":      func(c *config.NetworkConfig) { c.WanMTU = -1 },
		"mtu too big":       func(c *config.NetworkConfig) { c.WanMTU = 70000 },
		"pppoe user quote":  func(c *config.NetworkConfig) { pppoe(c); c.WanPPPoE.Username = `a" plugin evil.so "` },
		"pppoe newline":     func(c *config.NetworkConfig) { pppoe(c); c.WanPPPoE.Password = "x\nplugin evil.so" },
		"pppoe backslash":   func(c *config.NetworkConfig) { pppoe(c); c.WanPPPoE.Password = `x\` },
		"pppoe service":     func(c *config.NetworkConfig) { pppoe(c); c.WanPPPoE.Service = "a\"b" },
		"vlan on bad port":  func(c *config.NetworkConfig) { c.WanVlan = 5; c.WanInterface = "eth0 evil" },
		"dns with newline":  func(c *config.NetworkConfig) { c.WanDNS = []string{"9.9.9.9\nup evil"}; c.WanMode = "static" },
		"pppoe non-ascii":   func(c *config.NetworkConfig) { pppoe(c); c.WanPPPoE.Username = "ü" },
		"pppoe del control": func(c *config.NetworkConfig) { pppoe(c); c.WanPPPoE.Username = "a\x7f" },
	}
	for name, mutate := range evil {
		cfg := wanCfg(mutate)
		if _, err := Render(cfg, "ROUTER"); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestThePPPoEPasswordIsOnlyEverInThePeerFile(t *testing.T) {
	d := mustRender(t, wanCfg(pppoe))
	for _, a := range d.Artifacts {
		if a.Path != PathPPPPeer && strings.Contains(string(a.Content), "s3cret-pass") {
			t.Errorf("the password is in %s", a.Path)
		}
	}
	for _, n := range d.Notes {
		if strings.Contains(n, "s3cret") {
			t.Errorf("the password is in a note: %s", n)
		}
	}
}

func TestSwitchingToPPPoERestartsNetworkingAndWifiAndRemovingItCleansUp(t *testing.T) {
	root := RootFS(t.TempDir())
	plain := wanCfg(func(c *config.NetworkConfig) {})
	install(t, string(root), mustRender(t, plain))

	plan, err := PlanChanges(root, mustRender(t, wanCfg(pppoe)))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]ChangeKind{}
	for _, f := range plan.Files {
		kinds[f.Path] = f.Kind
	}
	if kinds[PathPPPPeer] != Create || kinds[PathInterfaces] != Update || kinds[PathNftables] != Update {
		t.Errorf("%+v", kinds)
	}
	if got := actionSummary(plan); !strings.Contains(got, "restart networking") || !strings.Contains(got, "nftables") {
		t.Errorf("%s", got)
	}

	// back to DHCP: the login (and its password) is removed from the system
	install(t, string(root), mustRender(t, wanCfg(pppoe)))
	plan, err = PlanChanges(root, mustRender(t, plain))
	if err != nil {
		t.Fatal(err)
	}
	var removed bool
	for _, f := range plan.Files {
		if f.Path == PathPPPPeer && f.Kind == Remove {
			removed = true
		}
	}
	if !removed {
		t.Errorf("the PPPoE file is not removed: %+v", plan.Files)
	}

	// no change, no plan (the applier removes the file; the test does it by hand)
	if err := os.Remove(filepath.Join(string(root), PathPPPPeer)); err != nil {
		t.Fatal(err)
	}
	install(t, string(root), mustRender(t, plain))
	if plan, _ := PlanChanges(root, mustRender(t, plain)); !plan.Empty() {
		t.Errorf("%+v", plan)
	}
}

// runChecked runs a command inside a throw-away user and network namespace, where root-only tools
// work without real privileges.
func runInNamespace(t *testing.T, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	out, err := exec.Command("unshare", append([]string{"-rn"}, args...)...).CombinedOutput()
	return string(out), err
}

func TestTheRealPPPDaemonUnderstandsTheSettingsWeWrite(t *testing.T) {
	if _, err := exec.LookPath("pppd"); err != nil {
		t.Skip("pppd is not installed")
	}
	dir := t.TempDir()
	// the plugin and the interface name are read by the PPPoE plugin, which needs kernel support to start:
	// everything else is checked by pppd itself
	var kept []string
	for _, l := range strings.Split(string(mustPeer(t)), "\n") {
		if l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "plugin") || strings.HasPrefix(l, "nic-") || strings.HasPrefix(l, "rp_pppoe") {
			continue
		}
		kept = append(kept, l)
	}
	check := func(extra string) string {
		path := filepath.Join(dir, "peer")
		os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"+extra), 0600)
		out, _ := runInNamespace(t, "env", "HOME=/nonexistent", "timeout", "10", "pppd", "file", path, "nodetach", "/dev/null")
		return out
	}
	if out := check(""); strings.Contains(out, "unrecognized option") || strings.Contains(out, "In file") {
		t.Fatalf("pppd rejects what we write:\n%s", out)
	}
	// the control: pppd does complain about a setting it does not know
	if out := check("bogus-option-xyz\n"); !strings.Contains(out, "unrecognized option 'bogus-option-xyz'") {
		t.Skipf("this pppd could not be made to report a bad option, so the check above proves nothing:\n%s", out)
	}
}

func mustPeer(t *testing.T) []byte {
	cfg := wanCfg(pppoe)
	cfg.WanPPPoE.Service = "Internet"
	return []byte(text(t, cfg, PathPPPPeer))
}

func TestTheRealNftablesAcceptsTheWanRules(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("nft is not installed")
	}
	cases := map[string]config.NetworkConfig{
		"plain":              wanCfg(func(c *config.NetworkConfig) {}),
		"pppoe with a clamp": wanCfg(pppoe),
		"vlan":               wanCfg(func(c *config.NetworkConfig) { c.WanVlan = 835 }),
		"low mtu":            wanCfg(func(c *config.NetworkConfig) { c.WanMTU = 1280 }),
	}
	for name, cfg := range cases {
		path := filepath.Join(t.TempDir(), "nft.conf")
		os.WriteFile(path, []byte(text(t, cfg, PathNftables)), 0644)
		out, err := runInNamespace(t, "nft", "-c", "-f", path)
		if err != nil {
			if strings.Contains(out, "Operation not permitted") && !strings.Contains(out, "Error:") {
				t.Skipf("nft cannot check rules here: %s", out)
			}
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	// the control: a rule it does not understand fails
	bad := filepath.Join(t.TempDir(), "bad.conf")
	os.WriteFile(bad, []byte("table inet x { chain y { tcp option maxseg size bogus 5 } }\n"), 0644)
	if _, err := runInNamespace(t, "nft", "-c", "-f", bad); err == nil {
		t.Skip("nft accepts anything here, so the check above proves nothing")
	}
}

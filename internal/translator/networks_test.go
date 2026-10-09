package translator

import (
	"strings"
	"testing"

	"peakroute/internal/config"
)

func guestNet() config.Network {
	return config.Network{ID: "guest", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0",
		DHCP: &config.DHCPConfig{Enabled: true, RangeStart: "10.20.0.100", RangeEnd: "10.20.0.200", LeaseTime: "1h"}, Isolated: true}
}

func iotNet() config.Network {
	return config.Network{ID: "iot", Name: "IoT", VLAN: 30, IP: "10.30.0.1", Netmask: "255.255.255.0", Isolated: true}
}

// the wizard's two-port box with a guest and an IoT network
func netsCfg(nets ...config.Network) config.NetworkConfig {
	c := routerCfg() // eth0 WAN, eth1 LAN, 192.168.1.1/24 with DHCP
	c.LanInterfaces = []string{"eth1", "eth2"}
	c.Networks = nets
	return c
}

func content(t *testing.T, cfg config.NetworkConfig, path string) string {
	t.Helper()
	return artifactContent(t, mustRender(t, cfg), path)
}

func TestWithoutExtraNetworksNothingChanges(t *testing.T) {
	cfg := routerCfg()
	nft := content(t, cfg, PathNftables)
	if strings.Contains(nft, "br0") || strings.Contains(nft, "dport") {
		t.Errorf("the firewall of a one-network box gained rules:\n%s", nft)
	}
	if LanTarget(cfg) != "eth1" {
		t.Errorf("LanTarget = %q", LanTarget(cfg))
	}
	ifaces := content(t, cfg, PathInterfaces)
	if strings.Contains(ifaces, "vlan") || strings.Contains(ifaces, "pre-up") {
		t.Errorf("the interfaces file of a one-network box gained lines:\n%s", ifaces)
	}
	if strings.Contains(content(t, cfg, PathDnsmasqDHCP), "tag:") {
		t.Error("the DHCP file of a one-network box is tagged")
	}
}

func TestTheDefaultNetworkLivesOnTheBridgeOnlyWhenSeveralPortsCarryIt(t *testing.T) {
	one := routerCfg() // eth1 only
	one.Networks = []config.Network{guestNet()}
	if got := LanTarget(one); got != "eth1" {
		t.Errorf("one port carries the default network: LanTarget = %q", got)
	}
	if got := LanTarget(netsCfg(guestNet())); got != "br0" {
		t.Errorf("two ports carry it: LanTarget = %q", got)
	}
	// a port whose untagged network is another one does not count
	cfg := netsCfg(guestNet())
	cfg.PortNetworks = map[string]config.PortNetworks{"eth2": {Native: "guest"}}
	if got := LanTarget(cfg); got != "eth1" {
		t.Errorf("only eth1 carries the default network: LanTarget = %q", got)
	}
	// no port carries it: an empty bridge keeps its address
	cfg.PortNetworks = map[string]config.PortNetworks{"eth1": {Native: "guest"}, "eth2": {Native: "guest"}}
	if got := LanTarget(cfg); got != "br0" {
		t.Errorf("no port carries it: LanTarget = %q", got)
	}
	if got := content(t, cfg, PathInterfaces); !strings.Contains(got, "iface br0 inet static\n    address 192.168.1.1\n    netmask 255.255.255.0\n    bridge_ports none\n") {
		t.Errorf("%s", got)
	}
}

func TestInterfacesFileWithNetworks(t *testing.T) {
	cfg := netsCfg(guestNet(), iotNet())
	cfg.PortNetworks = map[string]config.PortNetworks{
		"eth1": {Tagged: []string{"iot", "guest"}}, // the default network untagged plus both VLANs
		"eth2": {Native: "guest"},                  // a port that belongs to the guest network
	}
	want := `auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp

auto eth1.20
iface eth1.20 inet manual
    vlan-raw-device eth1

auto eth1.30
iface eth1.30 inet manual
    vlan-raw-device eth1

auto eth1
iface eth1 inet static
    address 192.168.1.1
    netmask 255.255.255.0

auto br20
iface br20 inet static
    address 10.20.0.1
    netmask 255.255.255.0
    bridge_ports eth2 eth1.20

auto br30
iface br30 inet static
    address 10.30.0.1
    netmask 255.255.255.0
    bridge_ports eth1.30
`
	if got := content(t, cfg, PathInterfaces); got != want {
		t.Errorf("interfaces:\n%s\nwant:\n%s", got, want)
	}
}

func TestInterfacesFileWithSeveralDefaultPortsAndAnEmptyNetwork(t *testing.T) {
	cfg := netsCfg(guestNet()) // two ports, both on the default network; nothing carries the guest network
	want := `auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp

auto br0
iface br0 inet static
    address 192.168.1.1
    netmask 255.255.255.0
    bridge_ports eth1 eth2

auto br20
iface br20 inet static
    address 10.20.0.1
    netmask 255.255.255.0
    bridge_ports none
`
	if got := content(t, cfg, PathInterfaces); got != want {
		t.Errorf("interfaces:\n%s\nwant:\n%s", got, want)
	}
}

func TestTaggedInterfacesComeInVlanOrderAndTwoPortsCanTagTheSameNetwork(t *testing.T) {
	cfg := netsCfg(iotNet(), guestNet()) // listed iot (30) first on purpose
	cfg.PortNetworks = map[string]config.PortNetworks{
		"eth1": {Tagged: []string{"iot", "guest"}},
		"eth2": {Tagged: []string{"guest"}},
	}
	got := content(t, cfg, PathInterfaces)
	order := []string{"auto eth1.20", "auto eth1.30", "auto eth2.20", "auto br0", "auto br30", "auto br20"}
	last := -1
	for _, want := range order {
		i := strings.Index(got, want+"\n")
		if i < 0 || i < last {
			t.Fatalf("%q out of place in:\n%s", want, got)
		}
		last = i
	}
	if !strings.Contains(got, "iface br20 inet static\n    address 10.20.0.1\n    netmask 255.255.255.0\n    bridge_ports eth1.20 eth2.20\n") {
		t.Errorf("%s", got)
	}
}

func TestBridgedInterfacesKeepTheWanStanzaAsItWas(t *testing.T) {
	plain := content(t, routerCfg(), PathInterfaces)
	wanPart := plain[:strings.Index(plain, "auto eth1")]
	if !strings.HasPrefix(content(t, netsCfg(guestNet()), PathInterfaces), wanPart) {
		t.Error("the loopback and WAN stanzas differ between the two renderings")
	}
	static := netsCfg(guestNet())
	static.WanMode, static.WanIP, static.WanSubnet, static.WanGateway, static.WanDNS = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1", []string{"9.9.9.9"}
	got := content(t, static, PathInterfaces)
	if !strings.Contains(got, "iface eth0 inet static\n    address 203.0.113.10\n    netmask 255.255.255.0\n    gateway 203.0.113.1\n    dns-nameservers 9.9.9.9\n") {
		t.Errorf("%s", got)
	}
}

func TestBridgedDnsmasqFile(t *testing.T) {
	cfg := netsCfg(guestNet(), iotNet())
	want := `interface=br0
interface=br20
interface=br30
dhcp-range=set:lan,192.168.1.100,192.168.1.200,255.255.255.0,12h
dhcp-option=tag:lan,option:router,192.168.1.1
dhcp-option=tag:lan,option:dns-server,192.168.1.1
dhcp-range=set:guest,10.20.0.100,10.20.0.200,255.255.255.0,1h
dhcp-option=tag:guest,option:router,10.20.0.1
dhcp-option=tag:guest,option:dns-server,10.20.0.1
`
	if got := content(t, cfg, PathDnsmasqDHCP); got != want {
		t.Errorf("dnsmasq:\n%s\nwant:\n%s", got, want)
	}
}

func TestDnsmasqFileFollowsWhichNetworksUseDhcp(t *testing.T) {
	// the default network has no DHCP but the guest network does: the file exists, with only the guest range
	cfg := netsCfg(guestNet())
	cfg.DHCP = &config.DHCPConfig{Enabled: false}
	a, ok := hasArtifact(mustRender(t, cfg), PathDnsmasqDHCP)
	if !ok || a.Remove {
		t.Fatalf("%+v", a)
	}
	got := string(a.Content)
	if strings.Contains(got, "set:lan") || !strings.Contains(got, "set:guest") || !strings.Contains(got, "interface=br0\n") {
		t.Errorf("%s", got)
	}
	// nobody uses DHCP: the file goes away, as for a single network
	none := netsCfg(iotNet())
	none.DHCP = &config.DHCPConfig{Enabled: false}
	if a, _ := hasArtifact(mustRender(t, none), PathDnsmasqDHCP); !a.Remove {
		t.Errorf("%+v", a)
	}
}

func nftForward(t *testing.T, cfg config.NetworkConfig) []string {
	t.Helper()
	nft := content(t, cfg, PathNftables)
	fwd := nft[strings.Index(nft, "chain forward"):strings.Index(nft, "chain output")]
	var lines []string
	for _, l := range strings.Split(fwd, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "iifname") {
			lines = append(lines, l)
		}
	}
	return lines
}

func has(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestANewNetworkHasNoRouteToTheInternetUntilItIsAllowed(t *testing.T) {
	fwd := nftForward(t, netsCfg(guestNet()))
	for _, l := range fwd {
		if strings.Contains(l, "br20") {
			t.Fatalf("an isolated network with no internet got a forwarding rule: %s", l)
		}
	}
	if !has(fwd, `iifname "br0" oifname "eth0" accept`) {
		t.Error("the default network lost its internet access")
	}
	open := guestNet()
	open.Internet = true
	fwd = nftForward(t, netsCfg(open))
	if !has(fwd, `iifname "br20" oifname "eth0" accept`) {
		t.Errorf("internet access was not rendered: %v", fwd)
	}
	for _, l := range fwd {
		if strings.Contains(l, `oifname "br0"`) || strings.Contains(l, `iifname "br0" oifname "br20"`) {
			t.Errorf("internet access must not open the network to the others: %s", l)
		}
	}
}

func TestNetworksTalkOnlyWhenNeitherIsIsolated(t *testing.T) {
	open := func(n config.Network) config.Network { n.Isolated = false; return n }
	between := func(lines []string, a, b string) bool {
		return has(lines, `iifname "`+a+`" oifname "`+b+`" accept`) || has(lines, `iifname "`+b+`" oifname "`+a+`" accept`)
	}

	// the guest network is open but the default network is not isolated either: they talk, both ways
	fwd := nftForward(t, netsCfg(open(guestNet())))
	if !has(fwd, `iifname "br0" oifname "br20" accept`) || !has(fwd, `iifname "br20" oifname "br0" accept`) {
		t.Errorf("both directions are needed: %v", fwd)
	}

	// an isolated guest network cannot talk to anyone
	if between(nftForward(t, netsCfg(guestNet())), "br0", "br20") {
		t.Error("an isolated network can reach the default network")
	}

	// an isolated default network cannot talk to an open one either
	cfg := netsCfg(open(guestNet()))
	cfg.LanIsolated = true
	if between(nftForward(t, cfg), "br0", "br20") {
		t.Error("an isolated default network can reach another network")
	}

	// two open networks talk to each other and to the default network; an isolated third one to nobody
	cfg = netsCfg(open(guestNet()), open(iotNet()), func() config.Network {
		n := iotNet()
		n.ID, n.Name, n.VLAN, n.IP = "lab", "Lab", 40, "10.40.0.1"
		return n
	}())
	fwd = nftForward(t, cfg)
	if !between(fwd, "br20", "br30") || !between(fwd, "br0", "br30") || !between(fwd, "br0", "br20") {
		t.Errorf("open networks should reach each other: %v", fwd)
	}
	if between(fwd, "br40", "br0") || between(fwd, "br40", "br20") || between(fwd, "br40", "br30") {
		t.Errorf("the isolated network talks to others: %v", fwd)
	}
}

func TestOtherNetworksMayOnlyUseTheRoutersBasicServices(t *testing.T) {
	nft := content(t, netsCfg(guestNet()), PathNftables)
	input := nft[strings.Index(nft, "chain input"):strings.Index(nft, "chain forward")]
	for _, want := range []string{
		`iifname "br0" accept`, // the default network is trusted, as it always was
		`iifname "br20" udp dport { 53, 67 } accept`,
		`iifname "br20" tcp dport 53 accept`,
		`iifname "br20" icmp type echo-request accept`,
	} {
		if !strings.Contains(input, want) {
			t.Errorf("missing %q in:\n%s", want, input)
		}
	}
	if strings.Contains(input, `iifname "br20" accept`) {
		t.Error("another network was given the same access to the router as the default one")
	}
	if !strings.Contains(nft, "policy drop") || strings.Count(nft, "masquerade") != 1 {
		t.Errorf("%s", nft)
	}
}

func TestFullFirewallWithNetworks(t *testing.T) {
	open := guestNet()
	open.Internet, open.Isolated = true, false
	want := `#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;
        iifname "lo" accept
        ct state established,related accept
        ct state invalid drop
        iifname "br0" accept
        iifname "br20" udp dport { 53, 67 } accept
        iifname "br20" tcp dport 53 accept
        iifname "br20" icmp type echo-request accept
    }
    chain forward {
        type filter hook forward priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
        iifname "br0" oifname "eth0" accept
        iifname "br20" oifname "eth0" accept
        iifname "br0" oifname "br20" accept
        iifname "br20" oifname "br0" accept
    }
    chain output {
        type filter hook output priority filter; policy accept;
    }
}

table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "eth0" masquerade
    }
}
`
	if got := content(t, netsCfg(open), PathNftables); got != want {
		t.Errorf("nftables:\n%s\nwant:\n%s", got, want)
	}
}

func TestNotesAboutNetworksThatCannotWorkYet(t *testing.T) {
	cfg := netsCfg(guestNet())
	d := mustRender(t, cfg)
	if len(d.Notes) != 1 || !strings.Contains(d.Notes[0], "Guest") {
		t.Fatalf("%v", d.Notes)
	}
	cfg.PortNetworks = map[string]config.PortNetworks{"eth2": {Native: "guest"}}
	if d := mustRender(t, cfg); len(d.Notes) != 0 {
		t.Fatalf("a carried network still has a note: %v", d.Notes)
	}
	cfg.PortNetworks = map[string]config.PortNetworks{"eth1": {Tagged: []string{"guest"}}}
	if d := mustRender(t, cfg); len(d.Notes) != 0 {
		t.Fatalf("a tagged network still has a note: %v", d.Notes)
	}
	// every port moved away from the default network
	cfg.PortNetworks = map[string]config.PortNetworks{"eth1": {Native: "guest"}, "eth2": {Native: "guest"}}
	d = mustRender(t, cfg)
	if len(d.Notes) != 1 || !strings.Contains(d.Notes[0], "default network") {
		t.Fatalf("%v", d.Notes)
	}
	if len(mustRender(t, routerCfg()).Notes) != 0 {
		t.Error("a box without extra networks gained notes")
	}
}

func TestNetworkValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	orig := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = orig }()

	evil := []func(*config.NetworkConfig){
		func(c *config.NetworkConfig) { c.Networks[0].ID = "guest\ninterface=eth0" },
		func(c *config.NetworkConfig) { c.Networks[0].IP = "10.20.0.1\n    gateway 6.6.6.6" },
		func(c *config.NetworkConfig) { c.Networks[0].Netmask = "255.255.255.0; reboot" },
		func(c *config.NetworkConfig) { c.Networks[0].DHCP.RangeStart = "10.20.0.100,tag:evil" },
		func(c *config.NetworkConfig) { c.Networks[0].DHCP.LeaseTime = "1h\ndhcp-option=3,6.6.6.6" },
		func(c *config.NetworkConfig) {
			c.PortNetworks = map[string]config.PortNetworks{"eth1; reboot": {Native: "guest"}}
		},
		func(c *config.NetworkConfig) {
			c.PortNetworks = map[string]config.PortNetworks{"eth1": {Native: "guest\nbridge vlan flush"}}
		},
		func(c *config.NetworkConfig) {
			c.PortNetworks = map[string]config.PortNetworks{"eth1": {Tagged: []string{"x y"}}}
		},
	}
	for i, mutate := range evil {
		cfg := netsCfg(guestNet())
		mutate(&cfg)
		if d, err := Render(cfg, "ROUTER"); err == nil || len(d.Artifacts) != 0 {
			t.Errorf("case %d: an unsafe value reached the files (err=%v)", i, err)
		}
	}
	if _, err := Render(netsCfg(guestNet()), "ROUTER"); err != nil {
		t.Errorf("a safe configuration was refused: %v", err)
	}
}

func TestNetworkNamesNeverReachAnyFile(t *testing.T) {
	n := guestNet()
	n.Name = "Super Secret Guests"
	for _, a := range mustRender(t, netsCfg(n)).Artifacts {
		if strings.Contains(string(a.Content), "Secret") || strings.Contains(string(a.Content), "Guests") {
			t.Errorf("%s contains a network's name", a.Path)
		}
	}
}

// ---- plan ------------------------------------------------------------------------------

func TestAddingANetworkRestartsNetworkingTheFirewallAndTheDhcpServer(t *testing.T) {
	root := RootFS(t.TempDir())
	base := netsCfg()
	base.Networks = nil
	install(t, string(root), mustRender(t, base))

	plan, err := PlanChanges(root, mustRender(t, netsCfg(guestNet())))
	if err != nil {
		t.Fatal(err)
	}
	if got := actionSummary(plan); got != "restart networking, reload nftables, restart dnsmasq" {
		t.Fatalf("%q", got)
	}
}

func TestChangingASwitchOnlyReloadsTheFirewall(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, netsCfg(guestNet())))

	open := guestNet()
	open.Internet = true
	plan, _ := PlanChanges(root, mustRender(t, netsCfg(open)))
	if got := actionSummary(plan); got != "reload nftables" {
		t.Fatalf("%q: allowing the internet must not restart networking", got)
	}
	open.Isolated = false
	plan, _ = PlanChanges(root, mustRender(t, netsCfg(open)))
	if got := actionSummary(plan); got != "reload nftables" {
		t.Fatalf("%q", got)
	}
}

func TestRenamingANetworkChangesNothingOnTheSystem(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, netsCfg(guestNet())))
	renamed := guestNet()
	renamed.Name = "Visitors"
	if plan, _ := PlanChanges(root, mustRender(t, netsCfg(renamed))); !plan.Empty() {
		t.Fatalf("%+v", plan)
	}
}

func TestMovingAPortBetweenNetworksRestartsNetworkingAndDnsmasqOnly(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := netsCfg(guestNet())
	cfg.LanInterfaces = []string{"eth1", "eth2", "eth3"} // the default network stays on br0 with two ports
	install(t, string(root), mustRender(t, cfg))

	cfg.PortNetworks = map[string]config.PortNetworks{"eth3": {Native: "guest"}}
	plan, _ := PlanChanges(root, mustRender(t, cfg))
	// the firewall names networks, not ports, so it is untouched; dnsmasq follows networking because
	// restarting networking drops the address it is bound to
	if got := actionSummary(plan); got != "restart networking, restart dnsmasq" {
		t.Fatalf("%q", got)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != PathInterfaces {
		t.Fatalf("%+v", plan.Files)
	}
}

func TestTheDefaultNetworkChangesInterfaceWhenItsPortCountDoes(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := netsCfg(guestNet()) // eth1 and eth2 both default: br0
	install(t, string(root), mustRender(t, cfg))

	cfg.PortNetworks = map[string]config.PortNetworks{"eth2": {Native: "guest"}} // now eth1 alone: no bridge
	plan, _ := PlanChanges(root, mustRender(t, cfg))
	if got := actionSummary(plan); got != "restart networking, reload nftables, restart dnsmasq" {
		t.Fatalf("%q: the default network moves from br0 to eth1, which the firewall and DHCP files name", got)
	}
}

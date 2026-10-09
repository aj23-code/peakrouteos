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

func TestOneLanPortWithNetworksStillNeedsTheBridge(t *testing.T) {
	cfg := routerCfg()
	cfg.Networks = []config.Network{guestNet()}
	if LanTarget(cfg) != "br0" {
		t.Fatalf("LanTarget = %q", LanTarget(cfg))
	}
}

func TestBridgedInterfacesFile(t *testing.T) {
	cfg := netsCfg(guestNet(), iotNet())
	cfg.PortNetworks = map[string]config.PortNetworks{
		"eth1": {Tagged: []string{"iot", "guest"}}, // the default network untagged plus both VLANs
		"eth2": {Native: "guest"},                  // a port that belongs to the guest network
	}
	want := `auto lo
iface lo inet loopback

auto eth0
iface eth0 inet dhcp

auto br0
iface br0 inet static
    address 192.168.1.1
    netmask 255.255.255.0
    pre-up ip link add name br0 type bridge || true
    pre-up ip link set dev br0 type bridge vlan_filtering 1
    pre-up ip link set dev eth1 master br0
    pre-up ip link set dev eth1 up
    pre-up ip link set dev eth2 master br0
    pre-up ip link set dev eth2 up
    post-up bridge vlan add dev br0 vid 20 self
    post-up bridge vlan add dev br0 vid 30 self
    post-up bridge vlan add dev eth1 vid 20
    post-up bridge vlan add dev eth1 vid 30
    post-up bridge vlan add dev eth2 vid 20 pvid untagged
    post-up bridge vlan del dev eth2 vid 1
    post-down ip link del dev br0 || true

auto br0.20
iface br0.20 inet static
    address 10.20.0.1
    netmask 255.255.255.0
    pre-up ip link add link br0 name br0.20 type vlan id 20 || true
    post-down ip link del dev br0.20 || true

auto br0.30
iface br0.30 inet static
    address 10.30.0.1
    netmask 255.255.255.0
    pre-up ip link add link br0 name br0.30 type vlan id 30 || true
    post-down ip link del dev br0.30 || true
`
	if got := content(t, cfg, PathInterfaces); got != want {
		t.Errorf("interfaces:\n%s\nwant:\n%s", got, want)
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
interface=br0.20
interface=br0.30
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
		if strings.Contains(l, "br0.20") {
			t.Fatalf("an isolated network with no internet got a forwarding rule: %s", l)
		}
	}
	if !has(fwd, `iifname "br0" oifname "eth0" accept`) {
		t.Error("the default network lost its internet access")
	}
	open := guestNet()
	open.Internet = true
	fwd = nftForward(t, netsCfg(open))
	if !has(fwd, `iifname "br0.20" oifname "eth0" accept`) {
		t.Errorf("internet access was not rendered: %v", fwd)
	}
	for _, l := range fwd {
		if strings.Contains(l, `oifname "br0"`) || strings.Contains(l, `iifname "br0" oifname "br0.20"`) {
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
	if !has(fwd, `iifname "br0" oifname "br0.20" accept`) || !has(fwd, `iifname "br0.20" oifname "br0" accept`) {
		t.Errorf("both directions are needed: %v", fwd)
	}

	// an isolated guest network cannot talk to anyone
	if between(nftForward(t, netsCfg(guestNet())), "br0", "br0.20") {
		t.Error("an isolated network can reach the default network")
	}

	// an isolated default network cannot talk to an open one either
	cfg := netsCfg(open(guestNet()))
	cfg.LanIsolated = true
	if between(nftForward(t, cfg), "br0", "br0.20") {
		t.Error("an isolated default network can reach another network")
	}

	// two open networks talk to each other and to the default network; an isolated third one to nobody
	cfg = netsCfg(open(guestNet()), open(iotNet()), func() config.Network {
		n := iotNet()
		n.ID, n.Name, n.VLAN, n.IP = "lab", "Lab", 40, "10.40.0.1"
		return n
	}())
	fwd = nftForward(t, cfg)
	if !between(fwd, "br0.20", "br0.30") || !between(fwd, "br0", "br0.30") || !between(fwd, "br0", "br0.20") {
		t.Errorf("open networks should reach each other: %v", fwd)
	}
	if between(fwd, "br0.40", "br0") || between(fwd, "br0.40", "br0.20") || between(fwd, "br0.40", "br0.30") {
		t.Errorf("the isolated network talks to others: %v", fwd)
	}
}

func TestOtherNetworksMayOnlyUseTheRoutersBasicServices(t *testing.T) {
	nft := content(t, netsCfg(guestNet()), PathNftables)
	input := nft[strings.Index(nft, "chain input"):strings.Index(nft, "chain forward")]
	for _, want := range []string{
		`iifname "br0" accept`, // the default network is trusted, as it always was
		`iifname "br0.20" udp dport { 53, 67 } accept`,
		`iifname "br0.20" tcp dport 53 accept`,
		`iifname "br0.20" icmp type echo-request accept`,
	} {
		if !strings.Contains(input, want) {
			t.Errorf("missing %q in:\n%s", want, input)
		}
	}
	if strings.Contains(input, `iifname "br0.20" accept`) {
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
        iifname "br0.20" udp dport { 53, 67 } accept
        iifname "br0.20" tcp dport 53 accept
        iifname "br0.20" icmp type echo-request accept
    }
    chain forward {
        type filter hook forward priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
        iifname "br0" oifname "eth0" accept
        iifname "br0.20" oifname "eth0" accept
        iifname "br0" oifname "br0.20" accept
        iifname "br0.20" oifname "br0" accept
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

func TestMovingAPortBetweenNetworksRestartsOnlyNetworking(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := netsCfg(guestNet())
	install(t, string(root), mustRender(t, cfg))

	cfg.PortNetworks = map[string]config.PortNetworks{"eth2": {Native: "guest"}}
	plan, _ := PlanChanges(root, mustRender(t, cfg))
	// networking restarts, and the DHCP server follows it because its LAN address was dropped
	if got := actionSummary(plan); got != "restart networking, restart dnsmasq" {
		t.Fatalf("%q", got)
	}
}

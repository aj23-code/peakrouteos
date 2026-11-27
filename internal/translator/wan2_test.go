package translator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

// a router with a second internet connection on eth2
func twoWanCfg(edit func(*config.NetworkConfig)) config.NetworkConfig {
	c := routerCfg()
	c.Wan2 = &config.WanConfig{Interface: "eth2", Mode: "dhcp"}
	if edit != nil {
		edit(&c)
	}
	return c
}

func TestARouterWithOneConnectionWritesNothingForASecond(t *testing.T) {
	cfg := routerCfg()
	ifaces := content(t, cfg, PathInterfaces)
	if strings.Contains(ifaces, "metric") || strings.Contains(ifaces, PathUDHCPCWan) || strings.Contains(ifaces, "wan2") {
		t.Errorf("one connection, yet:\n%s", ifaces)
	}
	nft := content(t, cfg, PathNftables)
	if strings.Contains(nft, "{ \"") && strings.Contains(nft, "masquerade") && strings.Contains(nft, "oifname { ") {
		t.Errorf("a set for the internet with one connection:\n%s", nft)
	}
	if !strings.Contains(nft, `oifname "eth0" masquerade`) {
		t.Errorf("masquerade changed:\n%s", nft)
	}
	d := mustRender(t, cfg)
	if a, ok := hasArtifact(d, PathPPPPeer2); !ok || !a.Remove {
		t.Errorf("a file for a second PPPoE login would be left behind: %+v", a)
	}
}

func TestTwoDHCPConnectionsEachGetTheirOwnDefaultRouteMetric(t *testing.T) {
	ifaces := content(t, twoWanCfg(nil), PathInterfaces)
	first := section(t, ifaces, "iface eth0 inet manual")
	second := section(t, ifaces, "iface eth2 inet manual")
	for name, want := range map[string]struct{ sec, metric string }{"first": {first, "metric 100"}, "second": {second, "metric 200"}} {
		if !strings.Contains(want.sec, want.metric) || !strings.Contains(want.sec, "-s "+PathUDHCPCWan) {
			t.Errorf("%s connection:\n%s", name, want.sec)
		}
	}
	if strings.Contains(first, "peakroute-resolv") || !strings.Contains(second, "peakroute-resolv no") {
		t.Errorf("only the second keeps out of the resolver file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.Contains(ifaces, "auto eth2\n") {
		t.Errorf("the second port is not brought up:\n%s", ifaces)
	}
}

func TestPreferringTheSecondConnectionSwapsTheMetrics(t *testing.T) {
	ifaces := content(t, twoWanCfg(func(c *config.NetworkConfig) { c.WanPreferred = config.WanSecond }), PathInterfaces)
	if !strings.Contains(section(t, ifaces, "iface eth0 inet manual"), "metric 200") || !strings.Contains(section(t, ifaces, "iface eth2 inet manual"), "metric 100") {
		t.Errorf("\n%s", ifaces)
	}
}

func TestAStaticConnectionsGatewayRouteHasItsMetric(t *testing.T) {
	cfg := twoWanCfg(func(c *config.NetworkConfig) {
		c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway, c.WanDNS = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1", []string{"9.9.9.9"}
		c.Wan2.Mode, c.Wan2.IP, c.Wan2.Subnet, c.Wan2.Gateway = "static", "198.51.100.7", "255.255.255.0", "198.51.100.1"
	})
	ifaces := content(t, cfg, PathInterfaces)
	if s := section(t, ifaces, "iface eth0 inet static"); !strings.Contains(s, "gateway 203.0.113.1") || !strings.Contains(s, "metric 100") {
		t.Errorf("first:\n%s", s)
	}
	if s := section(t, ifaces, "iface eth2 inet static"); !strings.Contains(s, "address 198.51.100.7") || !strings.Contains(s, "gateway 198.51.100.1") || !strings.Contains(s, "metric 200") {
		t.Errorf("second:\n%s", s)
	}
}

func TestASecondPPPoESessionHasItsOwnProviderUnitAndMetric(t *testing.T) {
	cfg := twoWanCfg(func(c *config.NetworkConfig) {
		c.WanMode, c.WanPPPoE = config.WanPPPoE, &config.PPPoEConfig{Username: "first", Password: "one"}
		c.Wan2.Mode, c.Wan2.PPPoE, c.Wan2.Vlan = config.WanPPPoE, &config.PPPoEConfig{Username: "second", Password: "two"}, 835
	})
	d := mustRender(t, cfg)
	p1, ok1 := hasArtifact(d, PathPPPPeer)
	p2, ok2 := hasArtifact(d, PathPPPPeer2)
	if !ok1 || !ok2 || p1.Remove || p2.Remove || p1.Mode != 0600 || p2.Mode != 0600 {
		t.Fatalf("%+v %+v", p1, p2)
	}
	one, two := string(p1.Content), string(p2.Content)
	for _, want := range []string{"unit 0\n", "user \"first\"", "nic-eth0\n", "defaultroute-metric 100\n"} {
		if !strings.Contains(one, want) {
			t.Errorf("first:\n%s", one)
			break
		}
	}
	for _, want := range []string{"unit 1\n", "user \"second\"", "nic-eth2.835\n", "defaultroute-metric 200\n"} {
		if !strings.Contains(two, want) {
			t.Errorf("second:\n%s", two)
			break
		}
	}
	ifaces := content(t, cfg, PathInterfaces)
	if !strings.Contains(ifaces, "iface peakroute-wan inet ppp\n    provider peakroute-wan\n") || !strings.Contains(ifaces, "iface peakroute-wan2 inet ppp\n    provider peakroute-wan2\n") {
		t.Errorf("\n%s", ifaces)
	}
	if nft := content(t, cfg, PathNftables); !strings.Contains(nft, `oifname { "ppp0", "ppp1" } masquerade`) {
		t.Errorf("\n%s", nft)
	}
	// and a PPPoE login with the second connection gone is removed with it
	one2 := routerCfg()
	one2.WanMode, one2.WanPPPoE = config.WanPPPoE, &config.PPPoEConfig{Username: "first", Password: "one"}
	if a, _ := hasArtifact(mustRender(t, one2), PathPPPPeer2); !a.Remove {
		t.Error("the second login stays on the disk")
	}
}

func TestBothConnectionsAreNATedAndLetThroughTheFirewallAndNothingComesIn(t *testing.T) {
	nft := content(t, twoWanCfg(nil), PathNftables)
	for _, want := range []string{
		`oifname { "eth0", "eth2" } masquerade`,
		`oifname { "eth0", "eth2" } accept`, // the default network to the internet
	} {
		if !strings.Contains(nft, want) {
			t.Errorf("missing %q in\n%s", want, nft)
		}
	}
	in := nft[strings.Index(nft, "chain input"):strings.Index(nft, "chain forward")]
	if !strings.Contains(in, "policy drop") {
		t.Fatal("input does not drop")
	}
	for _, line := range strings.Split(in, "\n") {
		if strings.Contains(line, `"eth0"`) || strings.Contains(line, `"eth2"`) {
			t.Errorf("a line lets an internet port into the router: %s", line)
		}
	}
	if !strings.Contains(nft, `iifname != { "eth0", "eth2" } oifname { "eth0", "eth2" } add @up4`) {
		t.Errorf("the usage counters do not know both connections:\n%s", nft)
	}
}

func TestRulesAboutTheInternetCoverBothConnections(t *testing.T) {
	cfg := twoWanCfg(func(c *config.NetworkConfig) {
		f := config.FirewallConfig{Forwards: []config.PortForward{{ID: "pf1", Name: "web", Protocol: "tcp", ExternalPorts: "8080", ToIP: "192.168.1.10", InternalPorts: "80"}}}
		c.Firewall = &f
	})
	nft := content(t, cfg, PathNftables)
	if !strings.Contains(nft, `iifname { "eth0", "eth2" } meta l4proto tcp th dport 8080 counter dnat to 192.168.1.10:80`) {
		t.Errorf("a port forward is only on one connection:\n%s", nft)
	}
	if !strings.Contains(nft, `iifname { "eth0", "eth2" } ip daddr 192.168.1.10 meta l4proto tcp th dport 80 ct status dnat counter accept`) {
		t.Errorf("the forwarded traffic is let through on one connection only:\n%s", nft)
	}
}

func TestAMTUBelowTheUsualIsClampedOnTheConnectionThatHasIt(t *testing.T) {
	cfg := twoWanCfg(func(c *config.NetworkConfig) { c.Wan2.MTU = 1400 })
	nft := content(t, cfg, PathNftables)
	if !strings.Contains(nft, `oifname "eth2" tcp flags syn tcp option maxseg size > 1360`) || strings.Contains(nft, `oifname "eth0" tcp flags syn`) {
		t.Errorf("\n%s", nft)
	}
}

func TestTwoConnectionsAreRenderedToSafeValuesOnly(t *testing.T) {
	cfg := twoWanCfg(func(c *config.NetworkConfig) { c.Wan2.Interface = "eth2;rm -rf /" })
	if _, err := Render(cfg, "ROUTER"); err == nil {
		t.Fatal("an unsafe port name for the second connection was rendered")
	}
	cfg = twoWanCfg(func(c *config.NetworkConfig) {
		c.Wan2.Mode, c.Wan2.PPPoE = config.WanPPPoE, &config.PPPoEConfig{Username: "a\"b", Password: "x"}
	})
	if _, err := Render(cfg, "ROUTER"); err == nil {
		t.Fatal("an unsafe PPPoE login for the second connection was rendered")
	}
}

// section returns the stanza that starts at the line that holds start, up to the next blank line.
func section(t *testing.T, text, start string) string {
	t.Helper()
	i := strings.Index(text, start)
	if i < 0 {
		t.Fatalf("no %q in\n%s", start, text)
	}
	rest := text[i:]
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestTheRealNftablesAcceptsTheRulesetOfTwoConnections(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/nft"); err != nil {
		t.Skip("nft is not installed")
	}
	cases := map[string]config.NetworkConfig{
		"plain": twoWanCfg(nil),
		"pppoe on the second": twoWanCfg(func(c *config.NetworkConfig) {
			c.Wan2.Mode, c.Wan2.PPPoE, c.Wan2.MTU = config.WanPPPoE, &config.PPPoEConfig{Username: "u", Password: "p"}, 1400
		}),
		"port forward, firewall rule and VPN": twoWanCfg(func(c *config.NetworkConfig) {
			f := config.FirewallConfig{
				Forwards: []config.PortForward{{ID: "pf1", Name: "web", Protocol: "tcp", ExternalPorts: "8080", ToIP: "192.168.1.10", InternalPorts: "80"}},
				Rules:    []config.FirewallRule{{ID: "r1", Name: "dns", Action: "allow", From: config.FromWAN, To: config.ToRouter, Protocol: config.ProtoUDP, Ports: "53"}},
			}
			c.Firewall = &f
		}),
	}
	for name, cfg := range cases {
		path := filepath.Join(t.TempDir(), "nft.conf")
		if err := os.WriteFile(path, []byte(content(t, cfg, PathNftables)), 0644); err != nil {
			t.Fatal(err)
		}
		out, err := runInNamespace(t, "nft", "-c", "-f", path)
		if err != nil {
			if strings.Contains(out, "Operation not permitted") && !strings.Contains(out, "Error:") {
				t.Skipf("nft cannot check rules here: %s", out)
			}
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
}

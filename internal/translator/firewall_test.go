package translator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func fwCfg(rules []config.FirewallRule, forwards ...config.PortForward) config.NetworkConfig {
	c := wanCfg(func(c *config.NetworkConfig) {})
	c.WanInterface = "eth0"
	c.LanInterfaces = []string{"eth1"}
	c.Networks = []config.Network{
		{ID: "guest", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0", Internet: true},
		{ID: "iot", Name: "IoT", VLAN: 30, IP: "10.30.0.1", Netmask: "255.255.255.0"},
	}
	c.PortNetworks = map[string]config.PortNetworks{"eth1": {Tagged: []string{"guest", "iot"}}}
	c.Firewall = &config.FirewallConfig{Rules: rules, Forwards: forwards}
	return c
}

func sampleRules() ([]config.FirewallRule, []config.PortForward) {
	rules := []config.FirewallRule{
		{ID: "fw1", Name: "Printer", From: "lan", Action: "allow", Protocol: "tcp", To: "iot", Destination: "10.30.0.20", Ports: "9100,631"},
		{ID: "fw2", Name: "Camera", From: "iot", Action: "block", Protocol: "any", To: "internet", Source: "10.30.0.40", Log: true},
		{ID: "fw3", Name: "Ping", From: "wan", Action: "allow", Protocol: "icmp", To: "router"},
		{ID: "fw4", Name: "Refuse", From: "guest", Action: "reject", Protocol: "tcp+udp", To: "any", Ports: "53"},
		{ID: "fw5", Name: "Off", From: "lan", Action: "block", Protocol: "tcp", To: "iot", Disabled: true},
	}
	forwards := []config.PortForward{
		{ID: "pf1", Name: "Web", Protocol: "tcp", ExternalPorts: "443", ToIP: "192.168.1.50", InternalPorts: "8443", Sources: []string{"203.0.113.7", "198.51.100.0/24"}},
		{ID: "pf2", Name: "Game", Protocol: "tcp+udp", ExternalPorts: "5000-5010", ToIP: "10.30.0.5"},
		{ID: "pf3", Name: "Off", Protocol: "tcp", ExternalPorts: "9999", ToIP: "192.168.1.50", Disabled: true},
	}
	return rules, forwards
}

func TestTheRulesetIsUnchangedWithoutRulesAndForwards(t *testing.T) {
	for name, cfg := range map[string]config.NetworkConfig{
		"no section": wanCfg(func(c *config.NetworkConfig) {}),
		"an empty section": func() config.NetworkConfig {
			c := wanCfg(func(c *config.NetworkConfig) {})
			c.Firewall = &config.FirewallConfig{}
			return c
		}(),
		"only switched-off rules": func() config.NetworkConfig {
			c := fwCfg([]config.FirewallRule{{ID: "fw1", Name: "x", From: "lan", Action: "block", Protocol: "tcp", To: "iot", Disabled: true}},
				config.PortForward{ID: "pf1", Name: "x", Protocol: "tcp", ExternalPorts: "80", ToIP: "192.168.1.50", Disabled: true})
			return c
		}(),
	} {
		base := fwCfg(nil)
		base.Firewall = nil
		if name != "only switched-off rules" {
			base = cfg
			base.Firewall = nil
		}
		want := text(t, base, PathNftables)
		got := text(t, cfg, PathNftables)
		if got != want {
			t.Errorf("%s: the ruleset changed:\n%s\nwant\n%s", name, got, want)
		}
		if strings.Contains(got, "fw:") || strings.Contains(got, "prerouting") {
			t.Errorf("%s: firewall lines without any rule", name)
		}
	}
}

func TestTheRulesetWithRulesAndForwards(t *testing.T) {
	rules, fwds := sampleRules()
	got := text(t, fwCfg(rules, fwds...), PathNftables)
	want := `#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;
        iifname "lo" accept
        ct state established,related accept
        ct state invalid drop
        # always allowed, whatever the rules say: DHCP, and the default network's way into this router
        iifname { "eth1", "br20", "br30" } udp dport 67 accept
        iifname "eth1" tcp dport { 22, 443, 8443 } accept
        # firewall rules, in order
        # fw3: Allow ICMP, from the internet to this router
        iifname "eth0" meta l4proto icmp counter accept comment "fw:fw3"
        # fw4: Reject TCP and UDP 53, from Guest to anywhere
        iifname "br20" meta l4proto { tcp, udp } th dport 53 counter reject comment "fw:fw4"
        iifname "eth1" accept
        iifname "br20" udp dport { 53, 67 } accept
        iifname "br20" tcp dport 53 accept
        iifname "br20" icmp type echo-request accept
        iifname "br30" udp dport { 53, 67 } accept
        iifname "br30" tcp dport 53 accept
        iifname "br30" icmp type echo-request accept
    }
    chain forward {
        type filter hook forward priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
        # port forwards: the traffic they translate is let through
        iifname "eth0" ip daddr 192.168.1.50 meta l4proto tcp th dport 8443 ct status dnat counter accept comment "fw:pf1"
        iifname "eth0" ip daddr 10.30.0.5 meta l4proto { tcp, udp } th dport 5000-5010 ct status dnat counter accept comment "fw:pf2"
        # firewall rules, in order
        # fw1: Allow TCP 9100,631, from LAN to IoT (10.30.0.20)
        iifname "eth1" oifname "br30" ip daddr 10.30.0.20 tcp dport { 9100, 631 } counter accept comment "fw:fw1"
        # fw2: Block any protocol, from IoT (10.30.0.40) to the internet, logged
        iifname "br30" oifname "eth0" ip saddr 10.30.0.40 counter log prefix "fw:fw2 " limit rate 5/second drop comment "fw:fw2"
        # fw4: Reject TCP and UDP 53, from Guest to anywhere
        iifname "br20" meta l4proto { tcp, udp } th dport 53 counter reject comment "fw:fw4"
        iifname "eth1" oifname "eth0" accept
        iifname "br20" oifname "eth0" accept
        iifname "eth1" oifname "br20" accept
        iifname "br20" oifname "eth1" accept
        iifname "eth1" oifname "br30" accept
        iifname "br30" oifname "eth1" accept
        iifname "br20" oifname "br30" accept
        iifname "br30" oifname "br20" accept
    }
    chain output {
        type filter hook output priority filter; policy accept;
    }
}

table ip nat {
    chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        # pf1: TCP 443 from the internet to 192.168.1.50:8443, only from 203.0.113.7, 198.51.100.0/24
        iifname "eth0" ip saddr { 198.51.100.0/24, 203.0.113.7 } meta l4proto tcp th dport 443 counter dnat to 192.168.1.50:8443 comment "fw:pf1"
        # pf2: TCP and UDP 5000-5010 from the internet to 10.30.0.5:5000-5010
        iifname "eth0" meta l4proto { tcp, udp } th dport 5000-5010 counter dnat to 10.30.0.5 comment "fw:pf2"
    }
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "eth0" masquerade
    }
}
`
	got = withoutUsageTable(got)
	if got != want {
		t.Errorf("the ruleset differs:\n%s\nwant\n%s", got, want)
	}
}

func TestRuleParts(t *testing.T) {
	line := func(r config.FirewallRule) string {
		cfg := fwCfg([]config.FirewallRule{r})
		out := text(t, cfg, PathNftables)
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, `comment "fw:`+r.ID+`"`) {
				return strings.TrimSpace(l)
			}
		}
		t.Fatalf("no line for %s in\n%s", r.ID, out)
		return ""
	}
	base := config.FirewallRule{ID: "fw1", Name: "x", From: "lan", Action: "allow", Protocol: "any", To: "iot"}
	with := func(f func(*config.FirewallRule)) config.FirewallRule { r := base; f(&r); return r }
	cases := []struct {
		name string
		rule config.FirewallRule
		want string
	}{
		{"any protocol", base, `iifname "eth1" oifname "br30" counter accept comment "fw:fw1"`},
		{"tcp without ports", with(func(r *config.FirewallRule) { r.Protocol = "tcp" }), `iifname "eth1" oifname "br30" meta l4proto tcp counter accept comment "fw:fw1"`},
		{"udp one port", with(func(r *config.FirewallRule) { r.Protocol = "udp"; r.Ports = "53" }), `iifname "eth1" oifname "br30" udp dport 53 counter accept comment "fw:fw1"`},
		{"tcp a range", with(func(r *config.FirewallRule) { r.Protocol = "tcp"; r.Ports = "8000-8100" }), `iifname "eth1" oifname "br30" tcp dport 8000-8100 counter accept comment "fw:fw1"`},
		{"tcp+udp", with(func(r *config.FirewallRule) { r.Protocol = "tcp+udp" }), `iifname "eth1" oifname "br30" meta l4proto { tcp, udp } counter accept comment "fw:fw1"`},
		{"icmp", with(func(r *config.FirewallRule) { r.Protocol = "icmp" }), `iifname "eth1" oifname "br30" meta l4proto icmp counter accept comment "fw:fw1"`},
		{"source and destination", with(func(r *config.FirewallRule) { r.Source = "192.168.1.0/28"; r.Destination = "10.30.0.5" }),
			`iifname "eth1" oifname "br30" ip saddr 192.168.1.0/28 ip daddr 10.30.0.5 counter accept comment "fw:fw1"`},
		{"block", with(func(r *config.FirewallRule) { r.Action = "block" }), `iifname "eth1" oifname "br30" counter drop comment "fw:fw1"`},
		{"tcp reject", with(func(r *config.FirewallRule) { r.Action = "reject"; r.Protocol = "tcp" }), `iifname "eth1" oifname "br30" meta l4proto tcp counter reject with tcp reset comment "fw:fw1"`},
		{"udp reject", with(func(r *config.FirewallRule) { r.Action = "reject"; r.Protocol = "udp" }), `iifname "eth1" oifname "br30" meta l4proto udp counter reject comment "fw:fw1"`},
		{"any reject", with(func(r *config.FirewallRule) { r.Action = "reject" }), `iifname "eth1" oifname "br30" counter reject comment "fw:fw1"`},
		{"logged", with(func(r *config.FirewallRule) { r.Log = true }), `iifname "eth1" oifname "br30" counter log prefix "fw:fw1 " limit rate 5/second accept comment "fw:fw1"`},
		{"to the internet", with(func(r *config.FirewallRule) { r.To = "internet" }), `iifname "eth1" oifname "eth0" counter accept comment "fw:fw1"`},
		{"from another net", with(func(r *config.FirewallRule) { r.From = "guest"; r.To = "lan" }), `iifname "br20" oifname "eth1" counter accept comment "fw:fw1"`},
		{"from the internet", with(func(r *config.FirewallRule) { r.From = "wan"; r.To = "guest" }), `iifname "eth0" oifname "br20" counter accept comment "fw:fw1"`},
	}
	for _, c := range cases {
		if got := line(c.rule); got != c.want {
			t.Errorf("%s:\n%s\nwant\n%s", c.name, got, c.want)
		}
	}
}

func TestWhichChainARuleGoesIn(t *testing.T) {
	cfg := func(to string) string {
		return text(t, fwCfg([]config.FirewallRule{{ID: "fw1", Name: "x", From: "lan", Action: "allow", Protocol: "tcp", To: to, Ports: "80"}}), PathNftables)
	}
	chains := func(out string) (in, fw int) {
		cur := ""
		for _, l := range strings.Split(out, "\n") {
			switch {
			case strings.Contains(l, "chain input"):
				cur = "input"
			case strings.Contains(l, "chain forward"):
				cur = "forward"
			case strings.Contains(l, "chain output"), strings.Contains(l, "chain prerouting"), strings.Contains(l, "chain postrouting"):
				cur = ""
			}
			if strings.Contains(l, `comment "fw:fw1"`) {
				if cur == "input" {
					in++
				} else if cur == "forward" {
					fw++
				}
			}
		}
		return
	}
	for to, want := range map[string][2]int{"router": {1, 0}, "internet": {0, 1}, "iot": {0, 1}, "any": {1, 1}} {
		if in, fw := chains(cfg(to)); in != want[0] || fw != want[1] {
			t.Errorf("to %s: %d in the input chain, %d in the forward chain; want %v", to, in, fw, want)
		}
	}
}

func TestTheAntiLockoutLinesOnlyAppearWithRules(t *testing.T) {
	if out := text(t, fwCfg(nil), PathNftables); strings.Contains(out, "22, 443, 8443") {
		t.Error("anti-lockout lines without rules: the file must stay as it was")
	}
	rules, _ := sampleRules()
	out := text(t, fwCfg(rules), PathNftables)
	if !strings.Contains(out, `iifname "eth1" tcp dport { 22, 443, 8443 } accept`) || !strings.Contains(out, "udp dport 67 accept") {
		t.Errorf("anti-lockout and DHCP lines are missing:\n%s", out)
	}
	// they come before every rule
	lock, first := strings.Index(out, "22, 443, 8443"), strings.Index(out, `comment "fw:fw3"`)
	if lock < 0 || first < 0 || lock > first {
		t.Error("the anti-lockout line must come before the rules")
	}
}

func TestForwardsOnTheWANDevice(t *testing.T) {
	_, fwds := sampleRules()
	c := fwCfg(nil, fwds...)
	c.WanMode = "pppoe"
	c.WanPPPoE = &config.PPPoEConfig{Username: "u", Password: "p"}
	c.WanVlan = 835
	out := text(t, c, PathNftables)
	if !strings.Contains(out, `iifname "ppp0" ip saddr { 198.51.100.0/24, 203.0.113.7 } meta l4proto tcp th dport 443`) || strings.Contains(out, `iifname "eth0"`) {
		t.Errorf("forwards follow the device that carries the connection:\n%s", out)
	}
}

func TestOnlyForwardsAddAPreroutingChain(t *testing.T) {
	rules, fwds := sampleRules()
	if out := text(t, fwCfg(rules), PathNftables); strings.Contains(out, "prerouting") {
		t.Error("a prerouting chain without forwards")
	}
	if out := text(t, fwCfg(nil, fwds...), PathNftables); !strings.Contains(out, "prerouting") || strings.Contains(out, "# always allowed") {
		t.Errorf("forwards alone add the prerouting chain but no rules lines:\n%s", out)
	}
}

func TestTheGeneratedCommentsAreSafe(t *testing.T) {
	// the validation does not allow such names; the comment writer must cope even so
	saved := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = saved }()
	c := fwCfg([]config.FirewallRule{{ID: "fw1", Name: "Küche\ttest", From: "lan", Action: "allow", Protocol: "tcp", To: "guest", Ports: "80"}})
	c.Networks[0].Name = "Gäste\nWLAN"
	out := text(t, c, PathNftables)
	for _, l := range strings.Split(out, "\n") {
		for _, r := range l {
			if r > 0x7e || (r < 0x20 && r != '\t') {
				t.Fatalf("a non-ASCII or control character in the file: %q", l)
			}
		}
	}
	// the rule's name is not in the file at all; the network's name is, made plain
	if strings.Contains(out, "Küche") {
		t.Error("the rule name is in the file")
	}
}

func TestFirewallValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	saved := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = saved }()

	rule := func(f func(*config.FirewallRule)) config.NetworkConfig {
		r := config.FirewallRule{ID: "fw1", Name: "x", From: "lan", Action: "allow", Protocol: "tcp", To: "iot", Ports: "80"}
		f(&r)
		return fwCfg([]config.FirewallRule{r})
	}
	fwd := func(f func(*config.PortForward)) config.NetworkConfig {
		p := config.PortForward{ID: "pf1", Name: "x", Protocol: "tcp", ExternalPorts: "80", ToIP: "192.168.1.50"}
		f(&p)
		return fwCfg(nil, p)
	}
	evil := map[string]config.NetworkConfig{
		"id with a quote":     rule(func(r *config.FirewallRule) { r.ID = `fw1" accept #` }),
		"id with a newline":   rule(func(r *config.FirewallRule) { r.ID = "fw1\naccept" }),
		"source injecting":    rule(func(r *config.FirewallRule) { r.Source = "1.2.3.4\naccept" }),
		"source junk":         rule(func(r *config.FirewallRule) { r.Source = "any" }),
		"destination junk":    rule(func(r *config.FirewallRule) { r.Destination = "10.0.0.1 accept" }),
		"ports injecting":     rule(func(r *config.FirewallRule) { r.Ports = "80\naccept" }),
		"ports junk":          rule(func(r *config.FirewallRule) { r.Ports = "80;drop" }),
		"too many ports":      rule(func(r *config.FirewallRule) { r.Ports = "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16" }),
		"unknown source":      rule(func(r *config.FirewallRule) { r.From = "ghost" }),
		"unknown destination": rule(func(r *config.FirewallRule) { r.To = "ghost" }),
		"forward id":          fwd(func(p *config.PortForward) { p.ID = "pf1\"" }),
		"forward address":     fwd(func(p *config.PortForward) { p.ToIP = "192.168.1.50\naccept" }),
		"forward ports":       fwd(func(p *config.PortForward) { p.ExternalPorts = "80;" }),
		"forward two ports":   fwd(func(p *config.PortForward) { p.ExternalPorts = "80,81" }),
		"forward internal":    fwd(func(p *config.PortForward) { p.InternalPorts = "x" }),
		"forward source":      fwd(func(p *config.PortForward) { p.Sources = []string{"1.2.3.4 }"} }),
	}
	for name, cfg := range evil {
		if _, err := Render(cfg, "ROUTER"); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestTheRealNftablesAcceptsTheFirewallRuleset(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/nft"); err != nil {
		t.Skip("nft is not installed")
	}
	rules, fwds := sampleRules()
	cases := map[string]config.NetworkConfig{
		"rules and forwards": fwCfg(rules, fwds...),
		"rules only":         fwCfg(rules),
		"forwards only":      fwCfg(nil, fwds...),
		"over pppoe":         func() config.NetworkConfig { c := fwCfg(rules, fwds...); pppoe(&c); return c }(),
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
}

func TestRulesToAndFromTheInternetFollowTheDeviceThatCarriesIt(t *testing.T) {
	rules := []config.FirewallRule{
		{ID: "fw1", Name: "out", From: "lan", Action: "block", Protocol: "tcp", To: "internet", Ports: "25"},
		{ID: "fw2", Name: "in", From: "wan", Action: "allow", Protocol: "icmp", To: "router"},
	}
	for name, mutate := range map[string]func(*config.NetworkConfig){
		"over a vlan":  func(c *config.NetworkConfig) { c.WanVlan = 835 },
		"over pppoe":   func(c *config.NetworkConfig) { pppoe(c) },
		"pppoe + vlan": func(c *config.NetworkConfig) { pppoe(c); c.WanVlan = 835 },
	} {
		c := fwCfg(rules)
		mutate(&c)
		dev := c.WanDevice()
		out := text(t, c, PathNftables)
		for _, want := range []string{
			`iifname "eth1" oifname "` + dev + `" tcp dport 25 counter drop comment "fw:fw1"`,
			`iifname "` + dev + `" meta l4proto icmp counter accept comment "fw:fw2"`,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: no %q in\n%s", name, want, out)
			}
		}
		if dev != "eth0" && strings.Contains(out, `"eth0" meta l4proto icmp`) {
			t.Errorf("%s: a rule still names the port", name)
		}
	}
}

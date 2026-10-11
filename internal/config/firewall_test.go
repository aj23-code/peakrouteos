package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func fwNet(rules []FirewallRule, forwards ...PortForward) NetworkConfig {
	c := withNets(validNet(), guest(), iot())
	c.Firewall = &FirewallConfig{Rules: rules, Forwards: forwards}
	return c
}

func rule(id string, f func(*FirewallRule)) FirewallRule {
	r := FirewallRule{ID: id, Name: "Rule " + id, From: "lan", Action: ActionAllow, Protocol: ProtoTCP, To: "iot", Ports: "9100"}
	if f != nil {
		f(&r)
	}
	return r
}

func forward(id string, f func(*PortForward)) PortForward {
	p := PortForward{ID: id, Name: "Forward " + id, Protocol: ProtoTCP, ExternalPorts: "443", ToIP: "192.168.1.50"}
	if f != nil {
		f(&p)
	}
	return p
}

func TestFirewallSettingsThatAreAccepted(t *testing.T) {
	cases := map[string]NetworkConfig{
		"no firewall section": validNet(),
		"an empty section":    fwNet(nil),
		"a plain allow":       fwNet([]FirewallRule{rule("fw1", nil)}),
		"a block to the internet": fwNet([]FirewallRule{rule("fw1", func(r *FirewallRule) {
			r.From = "iot"
			r.Action = ActionBlock
			r.To = ToInternet
			r.Protocol = ProtoAny
			r.Ports = ""
			r.Source = "10.30.0.40"
		})}),
		"reject with a range": fwNet([]FirewallRule{rule("fw1", func(r *FirewallRule) {
			r.Action = ActionReject
			r.Source = "192.168.1.0/28"
			r.Destination = "10.30.0.0/24"
			r.Ports = "22,8000-8100"
		})}),
		"from the internet":                    fwNet([]FirewallRule{rule("fw1", func(r *FirewallRule) { r.From = FromWAN; r.To = ToRouter; r.Protocol = ProtoICMP; r.Ports = "" })}),
		"from the internet to lan":             fwNet([]FirewallRule{rule("fw1", func(r *FirewallRule) { r.From = FromWAN; r.To = "lan"; r.Destination = "192.168.1.50" })}),
		"to anywhere":                          fwNet([]FirewallRule{rule("fw1", func(r *FirewallRule) { r.To = ToAny; r.Ports = "53"; r.Protocol = ProtoTCPUDP })}),
		"switched off and logged":              fwNet([]FirewallRule{rule("fw1", func(r *FirewallRule) { r.Disabled = true; r.Log = true })}),
		"15 port entries":                      fwNet([]FirewallRule{rule("fw1", func(r *FirewallRule) { r.Ports = "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15" })}),
		"a name with accents":                  fwNet([]FirewallRule{rule("fw1", func(r *FirewallRule) { r.Name = "Küche – Drucker" })}),
		"a port forward":                       fwNet(nil, forward("pf1", nil)),
		"a forward of a range":                 fwNet(nil, forward("pf1", func(p *PortForward) { p.ExternalPorts = "5000-5010"; p.InternalPorts = "5000-5010" })),
		"a forward of a range, ports left out": fwNet(nil, forward("pf1", func(p *PortForward) { p.ExternalPorts = "5000-5010" })),
		"a single port moved":                  fwNet(nil, forward("pf1", func(p *PortForward) { p.ExternalPorts = "8080"; p.InternalPorts = "80" })),
		"a forward with sources":               fwNet(nil, forward("pf1", func(p *PortForward) { p.Sources = []string{"203.0.113.7", "198.51.100.0/24"} })),
		"a forward to another net":             fwNet(nil, forward("pf1", func(p *PortForward) { p.ToIP = "10.30.0.5"; p.Protocol = ProtoUDP })),
		"two forwards, two ports":              fwNet(nil, forward("pf1", nil), forward("pf2", func(p *PortForward) { p.ExternalPorts = "444" })),
		"tcp and udp on one port":              fwNet(nil, forward("pf1", nil), forward("pf2", func(p *PortForward) { p.Protocol = ProtoUDP })),
		"overlap but one is off":               fwNet(nil, forward("pf1", nil), forward("pf2", func(p *PortForward) { p.Disabled = true })),
		"a forward with a udp port":            fwNet(nil, forward("pf1", func(p *PortForward) { p.Protocol = ProtoTCPUDP; p.ExternalPorts = "51820" })),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestFirewallSettingsThatAreRefused(t *testing.T) {
	r := func(f func(*FirewallRule)) NetworkConfig { return fwNet([]FirewallRule{rule("fw1", f)}) }
	p := func(f func(*PortForward)) NetworkConfig { return fwNet(nil, forward("pf1", f)) }
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"bad id", r(func(x *FirewallRule) { x.ID = "FW 1" }), "firewall.rules[0].id"},
		{"empty id", r(func(x *FirewallRule) { x.ID = "" }), "firewall.rules[0].id"},
		{"duplicate id", fwNet([]FirewallRule{rule("fw1", nil), rule("fw1", nil)}), "firewall.rules[1].id"},
		{"no name", r(func(x *FirewallRule) { x.Name = "" }), "firewall.rules[0].name"},
		{"padded name", r(func(x *FirewallRule) { x.Name = " x " }), "firewall.rules[0].name"},
		{"name with a newline", r(func(x *FirewallRule) { x.Name = "a\nb" }), "firewall.rules[0].name"},
		{"61 character name", r(func(x *FirewallRule) { x.Name = strings.Repeat("a", 61) }), "firewall.rules[0].name"},
		{"unknown action", r(func(x *FirewallRule) { x.Action = "accept" }), "firewall.rules[0].action"},
		{"unknown protocol", r(func(x *FirewallRule) { x.Protocol = "sctp" }), "firewall.rules[0].protocol"},
		{"empty protocol", r(func(x *FirewallRule) { x.Protocol = ""; x.Ports = "" }), "firewall.rules[0].protocol"},
		{"unknown from", r(func(x *FirewallRule) { x.From = "ghost" }), "firewall.rules[0].from"},
		{"router as from", r(func(x *FirewallRule) { x.From = ToRouter }), "firewall.rules[0].from"},
		{"unknown to", r(func(x *FirewallRule) { x.To = "ghost" }), "firewall.rules[0].to"},
		{"internet to internet", r(func(x *FirewallRule) { x.From = FromWAN; x.To = ToInternet; x.Ports = "" }), "firewall.rules[0].to"},
		{"a network to itself", r(func(x *FirewallRule) { x.To = "lan" }), "firewall.rules[0].to"},
		{"source not an address", r(func(x *FirewallRule) { x.Source = "printer" }), "firewall.rules[0].source"},
		{"source with host bits", r(func(x *FirewallRule) { x.Source = "192.168.1.5/24" }), "firewall.rules[0].source"},
		{"source outside the network", r(func(x *FirewallRule) { x.Source = "10.0.0.5" }), "firewall.rules[0].source"},
		{"source ipv6", r(func(x *FirewallRule) { x.Source = "fe80::1" }), "firewall.rules[0].source"},
		{"destination outside the network", r(func(x *FirewallRule) { x.Destination = "192.168.1.5" }), "firewall.rules[0].destination"},
		{"destination not an address", r(func(x *FirewallRule) { x.Destination = "x/33" }), "firewall.rules[0].destination"},
		{"ports with icmp", r(func(x *FirewallRule) { x.Protocol = ProtoICMP }), "firewall.rules[0].ports"},
		{"ports with any", r(func(x *FirewallRule) { x.Protocol = ProtoAny }), "firewall.rules[0].ports"},
		{"port 0", r(func(x *FirewallRule) { x.Ports = "0" }), "firewall.rules[0].ports"},
		{"port 65536", r(func(x *FirewallRule) { x.Ports = "65536" }), "firewall.rules[0].ports"},
		{"a downward range", r(func(x *FirewallRule) { x.Ports = "90-80" }), "firewall.rules[0].ports"},
		{"a word", r(func(x *FirewallRule) { x.Ports = "http" }), "firewall.rules[0].ports"},
		{"an empty entry", r(func(x *FirewallRule) { x.Ports = "80,,443" }), "firewall.rules[0].ports"},
		{"spaces in the ports", r(func(x *FirewallRule) { x.Ports = "80, 443" }), "firewall.rules[0].ports"},
		{"16 port entries", r(func(x *FirewallRule) { x.Ports = "1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16" }), "firewall.rules[0].ports"},
		{"a leading zero", r(func(x *FirewallRule) { x.Ports = "080" }), "firewall.rules[0].ports"},

		{"forward bad id", p(func(x *PortForward) { x.ID = "" }), "firewall.forwards[0].id"},
		{"forward duplicate id", fwNet(nil, forward("pf1", nil), forward("pf1", func(x *PortForward) { x.ExternalPorts = "80" })), "firewall.forwards[1].id"},
		{"forward no name", p(func(x *PortForward) { x.Name = "" }), "firewall.forwards[0].name"},
		{"forward icmp", p(func(x *PortForward) { x.Protocol = ProtoICMP }), "firewall.forwards[0].protocol"},
		{"forward any", p(func(x *PortForward) { x.Protocol = ProtoAny }), "firewall.forwards[0].protocol"},
		{"forward no external port", p(func(x *PortForward) { x.ExternalPorts = "" }), "firewall.forwards[0].external_ports"},
		{"forward two external entries", p(func(x *PortForward) { x.ExternalPorts = "80,443" }), "firewall.forwards[0].external_ports"},
		{"forward bad port", p(func(x *PortForward) { x.ExternalPorts = "70000" }), "firewall.forwards[0].external_ports"},
		{"forward bad internal", p(func(x *PortForward) { x.InternalPorts = "x" }), "firewall.forwards[0].internal_ports"},
		{"a range moved to other numbers", p(func(x *PortForward) { x.ExternalPorts = "5000-5010"; x.InternalPorts = "6000-6010" }), "firewall.forwards[0].internal_ports"},
		{"one port against a range", p(func(x *PortForward) { x.ExternalPorts = "80"; x.InternalPorts = "80-90" }), "firewall.forwards[0].internal_ports"},
		{"forward counts differ", p(func(x *PortForward) { x.ExternalPorts = "5000-5010"; x.InternalPorts = "6000" }), "firewall.forwards[0].internal_ports"},
		{"forward not an address", p(func(x *PortForward) { x.ToIP = "nas" }), "firewall.forwards[0].to_ip"},
		{"forward to nowhere", p(func(x *PortForward) { x.ToIP = "172.16.0.5" }), "firewall.forwards[0].to_ip"},
		{"forward to the router", p(func(x *PortForward) { x.ToIP = "192.168.1.1" }), "firewall.forwards[0].to_ip"},
		{"forward to the network address", p(func(x *PortForward) { x.ToIP = "192.168.1.0" }), "firewall.forwards[0].to_ip"},
		{"forward to the broadcast", p(func(x *PortForward) { x.ToIP = "192.168.1.255" }), "firewall.forwards[0].to_ip"},
		{"forward source junk", p(func(x *PortForward) { x.Sources = []string{"office"} }), "firewall.forwards[0].sources"},
		{"forward 11 sources", p(func(x *PortForward) {
			for i := 1; i <= 11; i++ {
				x.Sources = append(x.Sources, "203.0.113."+itoa(i))
			}
		}), "firewall.forwards[0].sources"},
		{"forwards overlap", fwNet(nil, forward("pf1", func(x *PortForward) { x.ExternalPorts = "5000-5010" }), forward("pf2", func(x *PortForward) { x.ExternalPorts = "5010" })), "firewall.forwards[1].external_ports"},
		{"tcp+udp overlaps tcp", fwNet(nil, forward("pf1", nil), forward("pf2", func(x *PortForward) { x.Protocol = ProtoTCPUDP })), "firewall.forwards[1].external_ports"},
	}
	for _, c := range cases {
		err := c.cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !fieldsOf(t, err).Has(c.field) {
			t.Errorf("%s: %v, want an error on %s", c.name, err, c.field)
		}
	}
}

func TestTheLimitsOnRulesAndForwards(t *testing.T) {
	var rules []FirewallRule
	for i := 0; i < MaxFirewallRules; i++ {
		rules = append(rules, rule("fw"+itoa(i+1), func(r *FirewallRule) { r.Ports = itoa(1 + i%60000) }))
	}
	if err := fwNet(rules).Validate(); err != nil {
		t.Fatalf("%d rules: %v", len(rules), err)
	}
	rules = append(rules, rule("fw999", nil))
	if err := fwNet(rules).Validate(); err == nil || !fieldsOf(t, err).Has("firewall.rules") {
		t.Fatalf("%v", err)
	}
	var fwds []PortForward
	for i := 0; i < MaxPortForwards; i++ {
		fwds = append(fwds, forward("pf"+itoa(i+1), func(p *PortForward) { p.ExternalPorts = itoa(1000 + i) }))
	}
	if err := fwNet(nil, fwds...).Validate(); err != nil {
		t.Fatalf("%d forwards: %v", len(fwds), err)
	}
	fwds = append(fwds, forward("pf999", func(p *PortForward) { p.ExternalPorts = "9999" }))
	if err := fwNet(nil, fwds...).Validate(); err == nil || !fieldsOf(t, err).Has("firewall.forwards") {
		t.Fatalf("%v", err)
	}
}

func TestARuleThatNamesADeletedNetworkIsRefused(t *testing.T) {
	c := fwNet([]FirewallRule{rule("fw1", nil)})
	c.Networks = []Network{guest()} // IoT is gone, the rule still goes to it
	if err := c.Validate(); err == nil || !fieldsOf(t, err).Has("firewall.rules[0].to") {
		t.Fatalf("%v", err)
	}
}

func TestParsePorts(t *testing.T) {
	good := map[string][]PortRange{
		"80":             {{80, 80}},
		"80,443":         {{80, 80}, {443, 443}},
		"8000-8100,22":   {{8000, 8100}, {22, 22}},
		"1":              {{1, 1}},
		"65535":          {{65535, 65535}},
		"5-5":            {{5, 5}},
		" 80 , 443-444 ": {{80, 80}, {443, 444}},
	}
	for in, want := range good {
		got, err := ParsePorts(in, 15)
		if err != nil || len(got) != len(want) {
			t.Errorf("%q: %v %v", in, got, err)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: %v, want %v", in, got, want)
			}
		}
	}
	for _, bad := range []string{"", ",", "0", "65536", "-5", "5-", "a", "1-2-3", "9-1", "1.5", "+80", "0x50", "80,", "-"} {
		if _, err := ParsePorts(bad, 15); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := ParsePorts("1,2,3", 2); err == nil {
		t.Error("the entry limit is ignored")
	}
	if (PortRange{80, 80}).String() != "80" || (PortRange{1, 9}).String() != "1-9" || (PortRange{1, 9}).Count() != 9 {
		t.Error("PortRange text or count")
	}
	if NormalizePorts("80 , 443-450") != "80,443-450" || NormalizePorts("junk") != "junk" {
		t.Error("NormalizePorts")
	}
}

func TestParseAddrRange(t *testing.T) {
	for _, ok := range []string{"192.168.1.5", "192.168.1.0/24", "10.0.0.0/8", "0.0.0.0/0", "1.2.3.4/32"} {
		if _, good := ParseAddrRange(ok); !good {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", " 1.2.3.4", "1.2.3.4 ", "1.2.3", "1.2.3.4/33", "1.2.3.4/24", "fe80::1", "fe80::/64", "a.b.c.d", "1.2.3.4/", "1.2.3.4/x", "192.168.1.5/24"} {
		if _, good := ParseAddrRange(bad); good {
			t.Errorf("%q accepted", bad)
		}
	}
	if p, _ := ParseAddrRange("1.2.3.4"); p.Bits() != 32 {
		t.Error("a bare address is a /32")
	}
}

func TestRulesThatNeverMatch(t *testing.T) {
	shadows := func(rules ...FirewallRule) map[string]string {
		out := map[string]string{}
		for _, s := range fwNet(rules).Shadows() {
			out[s.Rule] = s.ShadowBy
		}
		return out
	}
	any := func(id string, f func(*FirewallRule)) FirewallRule {
		return rule(id, func(r *FirewallRule) {
			r.Protocol, r.Ports, r.To = ProtoAny, "", ToAny
			if f != nil {
				f(r)
			}
		})
	}

	// a broader rule above hides a narrower one, whatever the actions
	if got := shadows(any("fw1", nil), rule("fw2", nil)); got["fw2"] != "fw1" || len(got) != 1 {
		t.Errorf("any above a specific one: %v", got)
	}
	if got := shadows(any("fw1", func(r *FirewallRule) { r.Action = ActionBlock }), rule("fw2", nil)); got["fw2"] != "fw1" {
		t.Errorf("a block above an allow: %v", got)
	}
	// the other way round the narrow one does not hide the broad one
	if got := shadows(rule("fw1", nil), any("fw2", nil)); len(got) != 0 {
		t.Errorf("narrow above broad: %v", got)
	}
	// identical rules: the second is redundant
	if got := shadows(rule("fw1", nil), rule("fw2", nil)); got["fw2"] != "fw1" {
		t.Errorf("duplicates: %v", got)
	}
	// each part of the match matters
	cases := map[string][2]FirewallRule{
		"another source network": {rule("fw1", nil), rule("fw2", func(r *FirewallRule) { r.From = "guest" })},
		"another destination":    {rule("fw1", nil), rule("fw2", func(r *FirewallRule) { r.To = "guest" })},
		"another protocol":       {rule("fw1", nil), rule("fw2", func(r *FirewallRule) { r.Protocol = ProtoUDP })},
		"other ports":            {rule("fw1", nil), rule("fw2", func(r *FirewallRule) { r.Ports = "9101" })},
		"a narrower source above a broader one": {
			rule("fw1", func(r *FirewallRule) { r.Source = "192.168.1.5" }), rule("fw2", nil)},
		"a narrower destination above a broader one": {
			rule("fw1", func(r *FirewallRule) { r.Destination = "10.30.0.5" }), rule("fw2", nil)},
		"a narrower range above a broader one": {
			rule("fw1", func(r *FirewallRule) { r.Source = "192.168.1.0/28" }), rule("fw2", func(r *FirewallRule) { r.Source = "192.168.1.0/24" })},
		"fewer ports above more": {
			rule("fw1", nil), rule("fw2", func(r *FirewallRule) { r.Ports = "9100,9101" })},
		"tcp above tcp+udp": {
			rule("fw1", nil), rule("fw2", func(r *FirewallRule) { r.Protocol = ProtoTCPUDP })},
		"icmp above any": {
			any("fw1", func(r *FirewallRule) { r.Protocol = ProtoICMP }), any("fw2", nil)},
		"router above any": {
			any("fw1", func(r *FirewallRule) { r.To = ToRouter }), any("fw2", nil)},
	}
	for name, pair := range cases {
		if got := shadows(pair[0], pair[1]); len(got) != 0 {
			t.Errorf("%s: %v", name, got)
		}
	}
	// and what really is covered
	covered := map[string][2]FirewallRule{
		"a range covers a host": {
			rule("fw1", func(r *FirewallRule) { r.Source = "192.168.1.0/28" }), rule("fw2", func(r *FirewallRule) { r.Source = "192.168.1.5" })},
		"a range covers a smaller range": {
			rule("fw1", func(r *FirewallRule) { r.Destination = "10.30.0.0/24" }), rule("fw2", func(r *FirewallRule) { r.Destination = "10.30.0.0/28" })},
		"more ports cover fewer": {
			rule("fw1", func(r *FirewallRule) { r.Ports = "9000-9200" }), rule("fw2", nil)},
		"a list covers a member": {
			rule("fw1", func(r *FirewallRule) { r.Ports = "22,9100,80" }), rule("fw2", nil)},
		"tcp+udp covers udp": {
			rule("fw1", func(r *FirewallRule) { r.Protocol = ProtoTCPUDP }), rule("fw2", func(r *FirewallRule) { r.Protocol = ProtoUDP })},
		"no ports cover some": {
			rule("fw1", func(r *FirewallRule) { r.Ports = "" }), rule("fw2", nil)},
		"any covers icmp": {
			any("fw1", nil), any("fw2", func(r *FirewallRule) { r.Protocol = ProtoICMP })},
	}
	for name, pair := range covered {
		if got := shadows(pair[0], pair[1]); got["fw2"] != "fw1" {
			t.Errorf("%s: %v", name, got)
		}
	}
	// a switched-off rule neither hides nor is reported
	if got := shadows(any("fw1", func(r *FirewallRule) { r.Disabled = true }), rule("fw2", nil)); len(got) != 0 {
		t.Errorf("off above: %v", got)
	}
	if got := shadows(any("fw1", nil), rule("fw2", func(r *FirewallRule) { r.Disabled = true })); len(got) != 0 {
		t.Errorf("off below: %v", got)
	}
	// the first rule that hides it is named
	if got := shadows(any("fw1", nil), any("fw2", nil), rule("fw3", nil)); got["fw3"] != "fw1" || got["fw2"] != "fw1" {
		t.Errorf("%v", got)
	}
	if (NetworkConfig{}).Shadows() != nil {
		t.Error("no firewall, no shadows")
	}
}

func TestFirewallJSONAndClone(t *testing.T) {
	if plain, _ := json.Marshal(validNet()); strings.Contains(string(plain), "firewall") {
		t.Errorf("nothing about the firewall is written when there is none: %s", plain)
	}
	a := fwNet([]FirewallRule{rule("fw1", nil)}, forward("pf1", func(p *PortForward) { p.Sources = []string{"203.0.113.7"} }))
	b, _ := json.Marshal(a)
	var back NetworkConfig
	if err := json.Unmarshal(b, &back); err != nil || back.Firewall == nil || len(back.Firewall.Rules) != 1 || back.Firewall.Forwards[0].Sources[0] != "203.0.113.7" {
		t.Fatalf("%v %+v", err, back.Firewall)
	}
	c := a.clone()
	c.Firewall.Rules[0].Name = "changed"
	c.Firewall.Forwards[0].Sources[0] = "9.9.9.9"
	if a.Firewall.Rules[0].Name == "changed" || a.Firewall.Forwards[0].Sources[0] != "203.0.113.7" {
		t.Fatal("the copy shares its firewall settings with the original")
	}
	if validNet().clone().Firewall != nil {
		t.Error("a copy gained a firewall section")
	}
	if len((NetworkConfig{}).FirewallSettings().Rules) != 0 {
		t.Error("an absent section reads as empty")
	}
	if r, ok := a.Firewall.FindRule("fw1"); !ok || r.ID != "fw1" {
		t.Error("FindRule")
	}
	if _, ok := a.Firewall.FindRule("nope"); ok {
		t.Error("FindRule found a ghost")
	}
	if p, ok := a.Firewall.FindForward("pf1"); !ok || p.ID != "pf1" {
		t.Error("FindForward")
	}
}

func TestFirewallReviewLines(t *testing.T) {
	base := withNets(validNet(), guest(), iot())
	with := func(rules []FirewallRule, fwds ...PortForward) NetworkConfig {
		c := base.clone()
		c.Firewall = &FirewallConfig{Rules: rules, Forwards: fwds}
		return c
	}
	line := func(a, b NetworkConfig, field string) []Change {
		var out []Change
		for _, c := range DiffNetwork(a, b) {
			if c.Field == field {
				out = append(out, c)
			}
		}
		return out
	}
	r1 := rule("fw1", nil)
	r1.Name = "Printer"

	// added
	got := line(base, with([]FirewallRule{r1}), "firewall_rules")
	if len(got) != 1 || got[0].Label != "Firewall rule Printer" || got[0].Old != "(none)" || got[0].New != "Allow TCP 9100, from LAN to IoT" {
		t.Fatalf("added: %+v", got)
	}
	// nothing changed
	if got := DiffNetwork(with([]FirewallRule{r1}), with([]FirewallRule{r1})); len(got) != 0 {
		t.Fatalf("unchanged: %+v", got)
	}
	// changed, and renamed
	r2 := r1
	r2.Action, r2.Source, r2.Log = ActionBlock, "192.168.1.5", true
	got = line(with([]FirewallRule{r1}), with([]FirewallRule{r2}), "firewall_rules")
	if len(got) != 1 || got[0].Old != "Allow TCP 9100, from LAN to IoT" || got[0].New != "Block TCP 9100, from LAN (192.168.1.5) to IoT, logged" {
		t.Fatalf("changed: %+v", got)
	}
	r3 := r1
	r3.Name = "Print server"
	got = line(with([]FirewallRule{r1}), with([]FirewallRule{r3}), "firewall_rules")
	if len(got) != 1 || got[0].Label != "Firewall rule Print server" || !strings.HasPrefix(got[0].Old, `"Printer": `) {
		t.Fatalf("renamed: %+v", got)
	}
	// removed
	got = line(with([]FirewallRule{r1}), base, "firewall_rules")
	if len(got) != 1 || got[0].New != "(removed)" || got[0].Label != "Firewall rule Printer" {
		t.Fatalf("removed: %+v", got)
	}
	// switched off
	r4 := r1
	r4.Disabled = true
	if got := line(with([]FirewallRule{r1}), with([]FirewallRule{r4}), "firewall_rules"); len(got) != 1 || !strings.HasSuffix(got[0].New, ", switched off") {
		t.Fatalf("off: %+v", got)
	}
	// the words for destinations, the internet and anywhere
	w := rule("fw2", func(r *FirewallRule) { r.From = FromWAN; r.To = ToRouter; r.Protocol = ProtoICMP; r.Ports = "" })
	if got := base.clone().RuleText(w); got != "Allow ICMP, from the internet to this router" {
		t.Fatalf("%q", got)
	}
	if got := base.RuleText(rule("fw3", func(r *FirewallRule) {
		r.From = "guest"
		r.To = ToAny
		r.Protocol = ProtoAny
		r.Ports = ""
		r.Action = ActionReject
	})); got != "Reject any protocol, from Guest to anywhere" {
		t.Fatalf("%q", got)
	}

	// order
	ra, rb := rule("fw1", nil), rule("fw2", func(r *FirewallRule) { r.Ports = "631" })
	ra.Name, rb.Name = "First", "Second"
	got = line(with([]FirewallRule{ra, rb}), with([]FirewallRule{rb, ra}), "firewall_order")
	if len(got) != 1 || got[0].Old != "First, then Second" || got[0].New != "Second, then First" {
		t.Fatalf("order: %+v", got)
	}
	if got := line(with([]FirewallRule{ra}), with([]FirewallRule{ra, rb}), "firewall_order"); len(got) != 0 {
		t.Fatalf("adding at the end does not reorder: %+v", got)
	}
	if got := line(with([]FirewallRule{ra, rb}), with([]FirewallRule{rb}), "firewall_order"); len(got) != 0 {
		t.Fatalf("removing does not reorder: %+v", got)
	}

	// forwards
	pf := forward("pf1", func(p *PortForward) { p.Name = "Web"; p.Sources = []string{"203.0.113.7"} })
	got = line(base, with(nil, pf), "firewall_forwards")
	if len(got) != 1 || got[0].Label != "Port forward Web" || got[0].New != "TCP 443 from the internet to 192.168.1.50:443, only from 203.0.113.7" {
		t.Fatalf("forward added: %+v", got)
	}
	pf2 := pf
	pf2.InternalPorts, pf2.Sources = "8443", nil
	got = line(with(nil, pf), with(nil, pf2), "firewall_forwards")
	if len(got) != 1 || got[0].New != "TCP 443 from the internet to 192.168.1.50:8443" {
		t.Fatalf("forward changed: %+v", got)
	}
	if got := line(with(nil, pf), base, "firewall_forwards"); len(got) != 1 || got[0].New != "(removed)" {
		t.Fatalf("forward removed: %+v", got)
	}
	// a change of only the sources, or only the internal ports, is a change
	onlySources := pf
	onlySources.Sources = []string{"203.0.113.8"}
	if got := line(with(nil, pf), with(nil, onlySources), "firewall_forwards"); len(got) != 1 {
		t.Fatalf("only the sources changed: %+v", got)
	}
	onlyInternal := pf
	onlyInternal.InternalPorts = "8443"
	if got := line(with(nil, pf), with(nil, onlyInternal), "firewall_forwards"); len(got) != 1 {
		t.Fatalf("only the internal port changed: %+v", got)
	}
	pf3 := pf
	pf3.Name = "Website"
	if got := line(with(nil, pf), with(nil, pf3), "firewall_forwards"); len(got) != 1 || !strings.HasPrefix(got[0].Old, `"Web": `) {
		t.Fatalf("forward renamed: %+v", got)
	}
	if got := line(with(nil, pf), with(nil, func() PortForward { p := pf; p.Disabled = true; return p }()), "firewall_forwards"); len(got) != 1 || !strings.HasSuffix(got[0].New, ", switched off") {
		t.Fatalf("forward off: %+v", got)
	}
}

package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func guest() Network {
	return Network{ID: "guest", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0",
		DHCP: &DHCPConfig{Enabled: true, RangeStart: "10.20.0.100", RangeEnd: "10.20.0.200", LeaseTime: "1h"}, Isolated: true}
}

func iot() Network {
	return Network{ID: "iot", Name: "IoT", VLAN: 30, IP: "10.30.0.1", Netmask: "255.255.255.0", Isolated: true}
}

func withNets(c NetworkConfig, nets ...Network) NetworkConfig { c.Networks = nets; return c }

func twoPorts(c NetworkConfig) NetworkConfig {
	c.LanInterfaces = []string{"eth1", "eth2"}
	return c
}

func TestValidNetworks(t *testing.T) {
	multi := twoPorts(withNets(validNet(), guest(), iot()))
	multi.PortNetworks = map[string]PortNetworks{
		"eth1": {Tagged: []string{"guest", "iot"}},
		"eth2": {Native: "guest"},
	}
	cases := map[string]NetworkConfig{
		"no extra networks":        validNet(),
		"one network":              withNets(validNet(), guest()),
		"two networks":             withNets(validNet(), guest(), iot()),
		"ports carrying them":      multi,
		"no DHCP on a network":     withNets(validNet(), iot()),
		"open to other networks":   withNets(validNet(), func() Network { n := guest(); n.Isolated = false; n.Internet = true; return n }()),
		"a static WAN beside them": withNets(staticWan(validNet()), guest()),
		"a port entry that says only the default": func() NetworkConfig {
			c := withNets(validNet(), guest())
			c.PortNetworks = map[string]PortNetworks{"eth1": {Native: "lan"}}
			return c
		}(),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInvalidNetworks(t *testing.T) {
	mut := func(f func(*Network)) NetworkConfig { n := guest(); f(&n); return withNets(validNet(), n) }
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"bad id", mut(func(n *Network) { n.ID = "Guest Net" }), "networks[0].id"},
		{"empty id", mut(func(n *Network) { n.ID = "" }), "networks[0].id"},
		{"id is the default's", mut(func(n *Network) { n.ID = "lan" }), "networks[0].id"},
		{"id with injection", mut(func(n *Network) { n.ID = "a\nb" }), "networks[0].id"},
		{"duplicate id", withNets(validNet(), guest(), func() Network { n := iot(); n.ID = "guest"; return n }()), "networks[1].id"},
		{"empty name", mut(func(n *Network) { n.Name = "  " }), "networks[0].name"},
		{"long name", mut(func(n *Network) { n.Name = strings.Repeat("a", 33) }), "networks[0].name"},
		{"name with markup", mut(func(n *Network) { n.Name = "<i>x</i>" }), "networks[0].name"},
		{"name used twice", withNets(validNet(), guest(), func() Network { n := iot(); n.Name = "guest"; return n }()), "networks[1].name"},
		{"name is the default's", mut(func(n *Network) { n.Name = "Default" }), "networks[0].name"},
		{"vlan 1", mut(func(n *Network) { n.VLAN = 1 }), "networks[0].vlan"},
		{"vlan 0", mut(func(n *Network) { n.VLAN = 0 }), "networks[0].vlan"},
		{"vlan too large", mut(func(n *Network) { n.VLAN = 4095 }), "networks[0].vlan"},
		{"vlan used twice", withNets(validNet(), guest(), func() Network { n := iot(); n.VLAN = 20; return n }()), "networks[1].vlan"},
		{"bad ip", mut(func(n *Network) { n.IP = "10.20.0.300" }), "networks[0].ip"},
		{"ip is the network address", mut(func(n *Network) { n.IP = "10.20.0.0" }), "networks[0].ip"},
		{"bad netmask", mut(func(n *Network) { n.Netmask = "255.0.255.0" }), "networks[0].netmask"},
		{"overlaps the LAN", mut(func(n *Network) { n.IP = "192.168.1.129"; n.Netmask = "255.255.255.128" }), "networks[0].netmask"},
		{"overlaps a static WAN", withNets(staticWan(validNet()), func() Network { n := guest(); n.IP = "203.0.113.129"; n.Netmask = "255.255.255.128"; return n }()), "networks[0].netmask"},
		{"overlaps another network", withNets(validNet(), guest(), func() Network { n := iot(); n.IP = "10.20.0.129"; n.Netmask = "255.255.255.128"; return n }()), "networks[1].netmask"},
		{"dhcp range outside", mut(func(n *Network) { n.DHCP.RangeStart = "10.99.0.5" }), "networks[0].dhcp.range_start"},
		{"dhcp range reversed", mut(func(n *Network) { n.DHCP.RangeStart, n.DHCP.RangeEnd = "10.20.0.200", "10.20.0.100" }), "networks[0].dhcp.range_end"},
		{"dhcp range covers the router", mut(func(n *Network) { n.DHCP.RangeStart = "10.20.0.1" }), "networks[0].dhcp.range_start"},
		{"dhcp lease", mut(func(n *Network) { n.DHCP.LeaseTime = "soon" }), "networks[0].dhcp.lease_time"},
		{"default DHCP not managed", func() NetworkConfig { c := withNets(validNet(), guest()); c.DHCP = nil; return c }(), "dhcp_enabled"},
		{"too many", func() NetworkConfig {
			c := validNet()
			for i := 0; i < 17; i++ {
				n := guest()
				n.ID, n.Name, n.VLAN, n.DHCP = "n"+itoa(i), "Net "+itoa(i), 100+i, nil
				n.IP = "10." + itoa(i+40) + ".0.1"
				c.Networks = append(c.Networks, n)
			}
			return c
		}(), "networks"},
	}
	for _, c := range cases {
		err := c.cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted, want an error on %s", c.name, c.field)
			continue
		}
		if ve := fieldsOf(t, err); !ve.Has(c.field) {
			t.Errorf("%s: errors were %v, want one on %s", c.name, ve, c.field)
		}
	}
}

func TestInvalidPortNetworks(t *testing.T) {
	with := func(p map[string]PortNetworks) NetworkConfig {
		c := twoPorts(withNets(validNet(), guest(), iot()))
		c.PortNetworks = p
		return c
	}
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"not a LAN port", with(map[string]PortNetworks{"eth9": {Native: "guest"}}), "port_networks.eth9"},
		{"the WAN port", with(map[string]PortNetworks{"eth0": {Native: "guest"}}), "port_networks.eth0"},
		{"native does not exist", with(map[string]PortNetworks{"eth1": {Native: "nope"}}), "port_networks.eth1.native"},
		{"tagged does not exist", with(map[string]PortNetworks{"eth1": {Tagged: []string{"nope"}}}), "port_networks.eth1.tagged"},
		{"default network tagged", with(map[string]PortNetworks{"eth1": {Native: "guest", Tagged: []string{"lan"}}}), "port_networks.eth1.tagged"},
		{"native also tagged", with(map[string]PortNetworks{"eth1": {Native: "guest", Tagged: []string{"guest"}}}), "port_networks.eth1.tagged"},
		{"tagged twice", with(map[string]PortNetworks{"eth1": {Tagged: []string{"iot", "iot"}}}), "port_networks.eth1.tagged"},
		{"injection", with(map[string]PortNetworks{"eth1": {Tagged: []string{"iot\nbridge vlan flush"}}}), "port_networks.eth1.tagged"},
	}
	for _, c := range cases {
		err := c.cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted, want an error on %s", c.name, c.field)
			continue
		}
		if ve := fieldsOf(t, err); !ve.Has(c.field) {
			t.Errorf("%s: errors were %v, want one on %s", c.name, ve, c.field)
		}
	}
}

func TestNetworksJSONRoundTripAndSafeDefaults(t *testing.T) {
	c := twoPorts(withNets(validNet(), guest()))
	c.LanIsolated = true
	c.PortNetworks = map[string]PortNetworks{"eth2": {Native: "guest"}, "eth1": {Tagged: []string{"guest"}}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var out NetworkConfig
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Networks) != 1 || out.Networks[0].ID != "guest" || !out.Networks[0].Isolated || out.Networks[0].Internet ||
		!out.LanIsolated || out.PortNetworks["eth2"].Native != "guest" || out.PortNetworks["eth1"].Tagged[0] != "guest" {
		t.Fatalf("%+v", out)
	}
	// both switches are always written, so reading the file never has to guess
	if !strings.Contains(string(b), `"isolated":true`) || !strings.Contains(string(b), `"internet":false`) {
		t.Fatalf("%s", b)
	}
	// without extra networks none of it is written
	plain, _ := json.Marshal(validNet())
	for _, k := range []string{"networks", "port_networks", "lan_isolated"} {
		if strings.Contains(string(plain), k) {
			t.Errorf("%q written for a configuration without networks: %s", k, plain)
		}
	}
}

func TestCloneDoesNotShareNetworks(t *testing.T) {
	a := withNets(validNet(), guest())
	a.PortNetworks = map[string]PortNetworks{"eth1": {Tagged: []string{"guest"}}}
	b := a.clone()
	b.Networks[0].Name = "changed"
	b.Networks[0].DHCP.RangeStart = "10.20.0.50"
	b.PortNetworks["eth1"].Tagged[0] = "other"
	b.PortNetworks["eth9"] = PortNetworks{}
	if a.Networks[0].Name != "Guest" || a.Networks[0].DHCP.RangeStart != "10.20.0.100" || a.PortNetworks["eth1"].Tagged[0] != "guest" || len(a.PortNetworks) != 1 {
		t.Fatalf("the copy shares its networks with the original: %+v", a)
	}
}

func TestPortNetworksHelpers(t *testing.T) {
	if (PortNetworks{}).NativeID() != "lan" || (PortNetworks{Native: "guest"}).NativeID() != "guest" {
		t.Error("NativeID")
	}
	if !(PortNetworks{}).IsDefault() || !(PortNetworks{Native: "lan"}).IsDefault() || (PortNetworks{Native: "guest"}).IsDefault() || (PortNetworks{Tagged: []string{"x"}}).IsDefault() {
		t.Error("IsDefault")
	}
	if validNet().Bridged() || !withNets(validNet(), guest()).Bridged() {
		t.Error("Bridged")
	}
	if n, ok := withNets(validNet(), guest()).FindNetwork("guest"); !ok || n.VLAN != 20 {
		t.Error("FindNetwork")
	}
	if _, ok := validNet().FindNetwork("guest"); ok {
		t.Error("FindNetwork found a network that is not there")
	}
}

// ---- diff -----------------------------------------------------------------------------

func changesOf(a, b NetworkConfig, field string) []Change {
	var out []Change
	for _, c := range DiffNetwork(a, b) {
		if c.Field == field {
			out = append(out, c)
		}
	}
	return out
}

func TestNetworkDiff(t *testing.T) {
	base := validNet()
	added := changesOf(base, withNets(base, guest()), "networks")
	if len(added) != 1 || added[0].Label != "Network Guest" || added[0].Old != "(none)" ||
		added[0].New != "VLAN 20, router 10.20.0.1 / 255.255.255.0, DHCP 10.20.0.100 to 10.20.0.200, lease 1h, no internet, isolated from other networks" {
		t.Fatalf("%+v", added)
	}
	removed := changesOf(withNets(base, guest()), base, "networks")
	if len(removed) != 1 || removed[0].New != "(removed)" {
		t.Fatalf("%+v", removed)
	}

	open := guest()
	open.Internet, open.Isolated = true, false
	changed := changesOf(withNets(base, guest()), withNets(base, open), "networks")
	if len(changed) != 1 || !strings.Contains(changed[0].Old, "no internet") || !strings.Contains(changed[0].New, "internet allowed") ||
		!strings.Contains(changed[0].New, "can reach other networks") {
		t.Fatalf("%+v", changed)
	}

	renamed := guest()
	renamed.Name = "Visitors"
	rn := changesOf(withNets(base, guest()), withNets(base, renamed), "networks")
	if len(rn) != 1 || !strings.Contains(rn[0].Label, "Guest") || !strings.Contains(rn[0].Label, "Visitors") {
		t.Fatalf("a rename should be one change: %+v", rn)
	}

	if got := changesOf(withNets(base, guest(), iot()), withNets(base, iot(), guest()), "networks"); len(got) != 0 {
		t.Fatalf("order is not a change: %+v", got)
	}
	// no DHCP versus a disabled one is the same thing
	a, b := iot(), iot()
	b.DHCP = &DHCPConfig{Enabled: false}
	if got := changesOf(withNets(base, a), withNets(base, b), "networks"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestLanIsolationDiff(t *testing.T) {
	a, b := validNet(), validNet()
	b.LanIsolated = true
	ch := changesOf(a, b, "lan_isolated")
	if len(ch) != 1 || ch[0].Old != "not isolated" || ch[0].New != "isolated from other networks" {
		t.Fatalf("%+v", ch)
	}
}

func TestPortNetworksDiff(t *testing.T) {
	base := twoPorts(withNets(validNet(), guest(), iot()))
	moved := base.clone()
	moved.PortNetworks = map[string]PortNetworks{"eth2": {Native: "guest", Tagged: []string{"iot"}}}

	ch := changesOf(base, moved, "port_networks")
	if len(ch) != 1 || ch[0].Label != "Networks on port eth2" || ch[0].Old != "native LAN (default)" || ch[0].New != "native Guest, tagged IoT" {
		t.Fatalf("%+v", ch)
	}
	// several tagged networks are listed in a fixed order whatever order they were saved in
	both := base.clone()
	both.PortNetworks = map[string]PortNetworks{"eth1": {Tagged: []string{"iot", "guest"}}}
	if ch := changesOf(base, both, "port_networks"); len(ch) != 1 || ch[0].New != "native LAN (default), tagged Guest, IoT" {
		t.Fatalf("%+v", ch)
	}
	// an explicit "lan" entry says the same as no entry
	same := base.clone()
	same.PortNetworks = map[string]PortNetworks{"eth1": {Native: "lan"}}
	if got := changesOf(base, same, "port_networks"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	// a port that becomes a LAN port with a non-default network is shown
	grown := validNet()
	grown = withNets(grown, guest())
	became := grown.clone()
	became.LanInterfaces = []string{"eth1", "eth2"}
	became.PortNetworks = map[string]PortNetworks{"eth2": {Native: "guest"}}
	ch = changesOf(grown, became, "port_networks")
	if len(ch) != 1 || ch[0].Old != "(not a LAN port)" || ch[0].New != "native Guest" {
		t.Fatalf("%+v", ch)
	}
	// a port that stops being a LAN port is the Ports page's change, not this one's
	left := moved.clone()
	left.LanInterfaces = []string{"eth1"}
	left.PortNetworks = nil // the Ports page drops the entry along with the role
	if got := changesOf(moved, left, "port_networks"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

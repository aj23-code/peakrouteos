package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func pppoeNet() NetworkConfig {
	c := validNet()
	c.WanMode = "pppoe"
	c.WanPPPoE = &PPPoEConfig{Username: "user@isp.example", Password: "s3cret-pass"}
	return c
}

func TestTheConnectionTypeAndItsInterfaces(t *testing.T) {
	cases := []struct {
		name         string
		mode         string
		vlan         int
		kind         string
		link, device string
	}{
		{"empty is DHCP", "", 0, "dhcp", "eth0", "eth0"},
		{"upper case", "STATIC", 0, "static", "eth0", "eth0"},
		{"vlan", "dhcp", 100, "dhcp", "eth0.100", "eth0.100"},
		{"vlan 1 is still a VLAN", "dhcp", 1, "dhcp", "eth0.1", "eth0.1"},
		{"pppoe", "pppoe", 0, "pppoe", "eth0", "ppp0"},
		{"pppoe over a vlan", "PPPoE", 835, "pppoe", "eth0.835", "ppp0"},
		{"unknown reads as DHCP", "l2tp", 0, "dhcp", "eth0", "eth0"},
	}
	for _, c := range cases {
		cfg := NetworkConfig{WanInterface: "eth0", WanMode: c.mode, WanVlan: c.vlan}
		if cfg.WanKind() != c.kind || cfg.WanLink() != c.link || cfg.WanDevice() != c.device {
			t.Errorf("%s: %s %s %s", c.name, cfg.WanKind(), cfg.WanLink(), cfg.WanDevice())
		}
	}
	if (NetworkConfig{WanVlan: 10}).WanLink() != "" {
		t.Error("a VLAN on no port is no interface")
	}
}

func TestTheUsualMTUFollowsTheType(t *testing.T) {
	if got := (NetworkConfig{}).EffectiveWanMTU(); got != 1500 {
		t.Errorf("dhcp %d", got)
	}
	if got := (NetworkConfig{WanMode: "pppoe"}).EffectiveWanMTU(); got != 1492 {
		t.Errorf("pppoe %d", got)
	}
	if got := (NetworkConfig{WanMode: "pppoe", WanMTU: 1400}).EffectiveWanMTU(); got != 1400 {
		t.Errorf("set %d", got)
	}
}

func TestWanSettingsThatAreAccepted(t *testing.T) {
	cases := map[string]NetworkConfig{
		"pppoe":                  pppoeNet(),
		"pppoe with a service":   func() NetworkConfig { c := pppoeNet(); c.WanPPPoE.Service = "Internet"; return c }(),
		"vlan":                   func() NetworkConfig { c := validNet(); c.WanVlan = 4094; return c }(),
		"vlan 1":                 func() NetworkConfig { c := validNet(); c.WanVlan = 1; return c }(),
		"mtu 576":                func() NetworkConfig { c := validNet(); c.WanMTU = 576; return c }(),
		"jumbo mtu":              func() NetworkConfig { c := validNet(); c.WanMTU = 9000; return c }(),
		"pppoe mtu 1492":         func() NetworkConfig { c := pppoeNet(); c.WanMTU = 1492; return c }(),
		"cloned mac":             func() NetworkConfig { c := validNet(); c.WanMAC = "02:11:22:33:44:55"; return c }(),
		"host name":              func() NetworkConfig { c := validNet(); c.Hostname = "my-router1"; return c }(),
		"host name with static":  func() NetworkConfig { c := staticWan(validNet()); c.Hostname = "my-router1"; return c }(),
		"host name with pppoe":   func() NetworkConfig { c := pppoeNet(); c.Hostname = "my-router1"; return c }(),
		"dns over dhcp":          func() NetworkConfig { c := validNet(); c.WanDNS = []string{"9.9.9.9", "149.112.112.112"}; return c }(),
		"dns over pppoe":         func() NetworkConfig { c := pppoeNet(); c.WanDNS = []string{"1.1.1.1"}; return c }(),
		"63 character host name": func() NetworkConfig { c := validNet(); c.Hostname = strings.Repeat("a", 63); return c }(),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWanSettingsThatAreRefused(t *testing.T) {
	mut := func(f func(*NetworkConfig)) NetworkConfig { c := validNet(); f(&c); return c }
	pp := func(f func(*NetworkConfig)) NetworkConfig { c := pppoeNet(); f(&c); return c }
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"vlan 4095", mut(func(c *NetworkConfig) { c.WanVlan = 4095 }), "wan_vlan"},
		{"negative vlan", mut(func(c *NetworkConfig) { c.WanVlan = -1 }), "wan_vlan"},
		{"vlan makes the name too long", mut(func(c *NetworkConfig) { c.WanInterface = "enp0s31f6xxxxx"; c.WanVlan = 100 }), "wan_vlan"},
		{"mtu too small", mut(func(c *NetworkConfig) { c.WanMTU = 575 }), "wan_mtu"},
		{"mtu too big", mut(func(c *NetworkConfig) { c.WanMTU = 9001 }), "wan_mtu"},
		{"pppoe mtu over 1492", pp(func(c *NetworkConfig) { c.WanMTU = 1493 }), "wan_mtu"},
		{"mac upper case", mut(func(c *NetworkConfig) { c.WanMAC = "02:11:22:33:44:AA" }), "wan_mac"},
		{"mac with dashes", mut(func(c *NetworkConfig) { c.WanMAC = "02-11-22-33-44-aa" }), "wan_mac"},
		{"mac too short", mut(func(c *NetworkConfig) { c.WanMAC = "02:11:22:33:44" }), "wan_mac"},
		{"group mac", mut(func(c *NetworkConfig) { c.WanMAC = "01:11:22:33:44:55" }), "wan_mac"},
		{"zero mac", mut(func(c *NetworkConfig) { c.WanMAC = "00:00:00:00:00:00" }), "wan_mac"},
		{"mac not hex", mut(func(c *NetworkConfig) { c.WanMAC = "zz:11:22:33:44:55" }), "wan_mac"},
		{"host name with a space", mut(func(c *NetworkConfig) { c.Hostname = "my router" }), "hostname"},
		{"host name starting with a dash", mut(func(c *NetworkConfig) { c.Hostname = "-router" }), "hostname"},
		{"host name ending with a dash", mut(func(c *NetworkConfig) { c.Hostname = "router-" }), "hostname"},
		{"64 character host name", mut(func(c *NetworkConfig) { c.Hostname = strings.Repeat("a", 64) }), "hostname"},
		{"host name injecting a line", mut(func(c *NetworkConfig) { c.Hostname = "a\nb" }), "hostname"},
		{"unsafe host name with static addressing", func() NetworkConfig { c := staticWan(validNet()); c.Hostname = "a b"; return c }(), "hostname"},
		{"dns not an address", mut(func(c *NetworkConfig) { c.WanDNS = []string{"dns.example"} }), "wan_dns"},
		{"four dns servers", mut(func(c *NetworkConfig) { c.WanDNS = []string{"1.1.1.1", "1.0.0.1", "9.9.9.9", "8.8.8.8"} }), "wan_dns"},
		{"pppoe without a user", pp(func(c *NetworkConfig) { c.WanPPPoE.Username = "" }), "pppoe_username"},
		{"pppoe without settings", pp(func(c *NetworkConfig) { c.WanPPPoE = nil }), "pppoe_username"},
		{"pppoe without a password", pp(func(c *NetworkConfig) { c.WanPPPoE.Password = "" }), "pppoe_password"},
		{"pppoe user with a quote", pp(func(c *NetworkConfig) { c.WanPPPoE.Username = `a"b` }), "pppoe_username"},
		{"pppoe password with a backslash", pp(func(c *NetworkConfig) { c.WanPPPoE.Password = `a\b` }), "pppoe_password"},
		{"pppoe password with a newline", pp(func(c *NetworkConfig) { c.WanPPPoE.Password = "a\nplugin evil.so" }), "pppoe_password"},
		{"pppoe user not ASCII", pp(func(c *NetworkConfig) { c.WanPPPoE.Username = "ü" }), "pppoe_username"},
		{"pppoe user too long", pp(func(c *NetworkConfig) { c.WanPPPoE.Username = strings.Repeat("a", 65) }), "pppoe_username"},
		{"pppoe service with a quote", pp(func(c *NetworkConfig) { c.WanPPPoE.Service = `a"b` }), "pppoe_service"},
		{"pppoe settings without pppoe", mut(func(c *NetworkConfig) { c.WanPPPoE = &PPPoEConfig{Username: "u", Password: "p"} }), "wan_pppoe"},
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

func TestPPPoEAcceptsEverythingASafePasswordMayContain(t *testing.T) {
	c := pppoeNet()
	c.WanPPPoE.Password = "A-z_0~9!@#$%^&*()+={}[]|:;'<>,.?/ `"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestACloneIsIndependentOfThePPPoESettings(t *testing.T) {
	a := pppoeNet()
	b := a.clone()
	b.WanPPPoE.Password = "other"
	if a.WanPPPoE.Password != "s3cret-pass" {
		t.Fatal("the copy shares the PPPoE settings with the original")
	}
	if validNet().clone().WanPPPoE != nil {
		t.Fatal("a copy gained PPPoE settings")
	}
}

func TestWanSettingsAreWrittenOnlyWhenUsed(t *testing.T) {
	plain, _ := json.Marshal(validNet())
	for _, key := range []string{"wan_vlan", "wan_mtu", "wan_mac", "hostname", "wan_pppoe"} {
		if strings.Contains(string(plain), key) {
			t.Errorf("%s written for a plain configuration: %s", key, plain)
		}
	}
}

func TestWanReviewLines(t *testing.T) {
	base := validNet()
	line := func(a, b NetworkConfig, field string) *Change {
		for _, c := range DiffNetwork(a, b) {
			if c.Field == field {
				c := c
				return &c
			}
		}
		return nil
	}
	with := func(f func(*NetworkConfig)) NetworkConfig { c := validNet(); f(&c); return c }

	if c := line(base, with(func(c *NetworkConfig) { c.WanVlan = 835 }), "wan_vlan"); c == nil || c.Old != "none" || c.New != "835" || c.Label != "WAN VLAN" {
		t.Errorf("vlan %+v", c)
	}
	// the MTU is compared by what is in effect: setting the usual value changes nothing
	if c := line(base, with(func(c *NetworkConfig) { c.WanMTU = 1500 }), "wan_mtu"); c != nil {
		t.Errorf("the usual MTU written out is no change: %+v", c)
	}
	if c := line(base, with(func(c *NetworkConfig) { c.WanMTU = 1400 }), "wan_mtu"); c == nil || c.Old != "1500" || c.New != "1400" {
		t.Errorf("mtu %+v", c)
	}
	if c := line(base, pppoeNet(), "wan_mtu"); c == nil || c.Old != "1500" || c.New != "1492" {
		t.Errorf("switching to PPPoE changes the usual MTU: %+v", c)
	}
	if c := line(base, with(func(c *NetworkConfig) { c.WanMAC = "02:11:22:33:44:55" }), "wan_mac"); c == nil || c.Old != "the port's own" || c.New != "02:11:22:33:44:55" {
		t.Errorf("mac %+v", c)
	}
	if c := line(base, with(func(c *NetworkConfig) { c.Hostname = "gw" }), "hostname"); c == nil || c.Old != "(none)" || c.New != "gw" {
		t.Errorf("host name %+v", c)
	}
	if c := line(base, pppoeNet(), "pppoe_username"); c == nil || c.New != "user@isp.example" {
		t.Errorf("user %+v", c)
	}
	if c := line(base, pppoeNet(), "wan_mode"); c == nil || c.New != "pppoe" {
		t.Errorf("mode %+v", c)
	}

	// the password is never shown
	for _, ch := range DiffNetwork(base, pppoeNet()) {
		if strings.Contains(ch.Old+ch.New+ch.Label, "s3cret") {
			t.Fatalf("the password is in the review: %+v", ch)
		}
	}
	if c := line(base, pppoeNet(), "pppoe_password"); c == nil || c.Old != "(none)" || c.New != "set" {
		t.Errorf("new password %+v", c)
	}
	changed := pppoeNet()
	changed.WanPPPoE.Password = "another-one"
	if c := line(pppoeNet(), changed, "pppoe_password"); c == nil || c.Old != "set" || c.New != "changed" {
		t.Errorf("changed password %+v", c)
	}
	if c := line(pppoeNet(), pppoeNet(), "pppoe_password"); c != nil {
		t.Errorf("unchanged password: %+v", c)
	}
	removed := pppoeNet()
	removed.WanPPPoE = nil
	if c := line(pppoeNet(), removed, "pppoe_password"); c == nil || c.Old != "set" || c.New != "(none)" {
		t.Errorf("removed password %+v", c)
	}
}

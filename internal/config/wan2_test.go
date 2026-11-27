package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func twoWans() NetworkConfig {
	c := validNet()
	c.Wan2 = &WanConfig{Interface: "eth2", Mode: "dhcp"}
	return c
}

// errorFields are the fields a validation error names.
func errorFields(err error) map[string]bool {
	out := map[string]bool{}
	var ve ValidationError
	if errors.As(err, &ve) {
		for _, e := range ve {
			out[e.Field] = true
		}
	}
	return out
}

func TestARouterWithOneConnectionIsAsItWas(t *testing.T) {
	c := validNet()
	if c.HasWan2() || len(c.WanViews()) != 1 || c.WanMetric() != 0 || c.WanDevice() != "eth0" || len(c.WanDevices()) != 1 {
		t.Fatalf("%+v", c.WanDevices())
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	// a stored configuration without the second connection reads and writes as before: the keys are not there
	if b, _ := json.Marshal(c); strings.Contains(string(b), "wan2") || strings.Contains(string(b), "wan_preferred") {
		t.Fatalf("one connection writes keys for a second: %s", b)
	}
	if !c.WanIsPreferred(WanFirst) || c.WanIsPreferred(WanSecond) {
		t.Error("preferred")
	}
}

func TestTheSecondConnectionHasItsOwnDeviceMetricAndPPPInterface(t *testing.T) {
	c := twoWans()
	if !c.HasWan2() || len(c.WanViews()) != 2 {
		t.Fatal("no second connection")
	}
	if got := strings.Join(c.WanDevices(), ","); got != "eth0,eth2" {
		t.Fatalf("devices %s", got)
	}
	first, _ := c.WanView(WanFirst)
	second, _ := c.WanView(WanSecond)
	if first.WanMetric() != WanMetricPreferred || second.WanMetric() != WanMetricOther {
		t.Errorf("metrics %d %d", first.WanMetric(), second.WanMetric())
	}
	c.WanPreferred = WanSecond
	first, _ = c.WanView(WanFirst)
	second, _ = c.WanView(WanSecond)
	if first.WanMetric() != WanMetricOther || second.WanMetric() != WanMetricPreferred {
		t.Errorf("after preferring the second: %d %d", first.WanMetric(), second.WanMetric())
	}
	// a PPPoE session of each has its own interface and provider
	c.WanMode, c.WanPPPoE = WanPPPoE, &PPPoEConfig{Username: "a", Password: "b"}
	c.Wan2.Mode, c.Wan2.PPPoE = WanPPPoE, &PPPoEConfig{Username: "c", Password: "d"}
	if got := strings.Join(c.WanDevices(), ","); got != "ppp0,ppp1" {
		t.Fatalf("devices %s", got)
	}
	second, _ = c.WanView(WanSecond)
	if second.WanPPPProvider() != "peakroute-wan2" || second.WanPPPoE.Username != "c" || !second.IsSecondWan() {
		t.Errorf("%+v", second.WanPPPoE)
	}
	// a VLAN names the device
	c.Wan2.Mode, c.Wan2.PPPoE, c.Wan2.Vlan = WanDHCP, nil, 835
	if got := c.WanDevices()[1]; got != "eth2.835" {
		t.Errorf("device %s", got)
	}
}

func TestChangingACopyNeverChangesTheSecondConnectionOfTheOriginal(t *testing.T) {
	c := twoWans()
	c.Wan2.DNS = []string{"9.9.9.9"}
	c.Wan2.PPPoE = &PPPoEConfig{Username: "u"}
	d := c.clone()
	d.Wan2.DNS[0] = "1.1.1.1"
	d.Wan2.PPPoE.Username = "other"
	d.Wan2.Interface = "eth9"
	if c.Wan2.DNS[0] != "9.9.9.9" || c.Wan2.PPPoE.Username != "u" || c.Wan2.Interface != "eth2" {
		t.Fatalf("%+v", c.Wan2)
	}
	v, _ := c.WanView(WanSecond)
	v.WanDNS[0] = "8.8.8.8"
	if c.Wan2.DNS[0] != "9.9.9.9" {
		t.Error("a view shares its list with the original")
	}
}

func TestTheSecondConnectionIsValidatedWithTheFirstOnesRules(t *testing.T) {
	cases := map[string]struct {
		change func(*NetworkConfig)
		field  string
	}{
		"the same port as the first":  {func(c *NetworkConfig) { c.Wan2.Interface = "eth0" }, "wan2.interface"},
		"a LAN port":                  {func(c *NetworkConfig) { c.Wan2.Interface = "eth1" }, "wan2.interface"},
		"not an interface name":       {func(c *NetworkConfig) { c.Wan2.Interface = "eth 2" }, "wan2.interface"},
		"an unknown type":             {func(c *NetworkConfig) { c.Wan2.Mode = "carrier-pigeon" }, "wan2.mode"},
		"a static address that isn't": {func(c *NetworkConfig) { c.Wan2.Mode, c.Wan2.IP, c.Wan2.Subnet = "static", "300.1.1.1", "255.255.255.0" }, "wan2.ip"},
		"a bad VLAN":                  {func(c *NetworkConfig) { c.Wan2.Vlan = 5000 }, "wan2.vlan"},
		"a bad MTU":                   {func(c *NetworkConfig) { c.Wan2.MTU = 10 }, "wan2.mtu"},
		"a bad MAC":                   {func(c *NetworkConfig) { c.Wan2.MAC = "nope" }, "wan2.mac"},
		"PPPoE without a login":       {func(c *NetworkConfig) { c.Wan2.Mode = "pppoe" }, "wan2.pppoe_username"},
		"PPPoE settings with DHCP":    {func(c *NetworkConfig) { c.Wan2.PPPoE = &PPPoEConfig{Username: "x"} }, "wan2.pppoe"},
		"a preference for nothing":    {func(c *NetworkConfig) { c.Wan2 = nil; c.WanPreferred = "wan2" }, "wan_preferred"},
		"an unknown preference":       {func(c *NetworkConfig) { c.WanPreferred = "wan3" }, "wan_preferred"},
		"the same subnet as the LAN": {func(c *NetworkConfig) {
			c.Wan2.Mode, c.Wan2.IP, c.Wan2.Subnet, c.Wan2.Gateway = "static", "192.168.1.9", "255.255.255.0", "192.168.1.254"
		}, "wan2.subnet"},
		"the same subnet as the first": {func(c *NetworkConfig) {
			c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1"
			c.Wan2.Mode, c.Wan2.IP, c.Wan2.Subnet, c.Wan2.Gateway = "static", "203.0.113.20", "255.255.255.0", "203.0.113.1"
		}, "wan2.subnet"},
	}
	for name, tc := range cases {
		c := twoWans()
		tc.change(&c)
		if !errorFields(c.Validate())[tc.field] {
			t.Errorf("%s: no error for %s: %v", name, tc.field, c.Validate())
		}
	}
	// what is right passes, in every type
	for _, mode := range []string{"dhcp", "pppoe"} {
		c := twoWans()
		c.Wan2.Mode = mode
		if mode == "pppoe" {
			c.Wan2.PPPoE = &PPPoEConfig{Username: "u", Password: "p"}
		}
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", mode, err)
		}
	}
	c := twoWans()
	c.WanPreferred = WanSecond
	c.Wan2.Mode, c.Wan2.IP, c.Wan2.Subnet, c.Wan2.Gateway, c.Wan2.DNS = "static", "198.51.100.7", "255.255.255.0", "198.51.100.1", []string{"9.9.9.9"}
	if err := c.Validate(); err != nil {
		t.Errorf("static: %v", err)
	}
}

func TestTheSecondConnectionShowsInTheReviewOfChanges(t *testing.T) {
	active := validNet()
	draft := twoWans()
	draft.Wan2.Mode, draft.Wan2.IP, draft.Wan2.Subnet, draft.Wan2.Gateway = "static", "198.51.100.7", "255.255.255.0", "198.51.100.1"
	draft.WanPreferred = WanSecond
	got := map[string]Change{}
	for _, c := range DiffNetwork(active, draft) {
		got[c.Field] = c
	}
	if c := got["wan2"]; c.Old != "(none)" || !strings.Contains(c.New, "eth2") || !strings.Contains(c.New, "198.51.100.7") {
		t.Errorf("wan2: %+v", c)
	}
	if c := got["wan2.ip"]; c.New != "198.51.100.7" {
		t.Errorf("ip: %+v", c)
	}
	if c := got["wan_preferred"]; c.New != "the second connection" {
		t.Errorf("preferred: %+v", c)
	}
	// the password is never shown
	draft.Wan2.Mode, draft.Wan2.PPPoE = "pppoe", &PPPoEConfig{Username: "u", Password: "s3cret"}
	for _, c := range DiffNetwork(active, draft) {
		if strings.Contains(c.Old, "s3cret") || strings.Contains(c.New, "s3cret") {
			t.Fatalf("the password is in the review: %+v", c)
		}
	}
	if got := DiffNetwork(draft, draft); len(got) != 0 {
		t.Errorf("no change shows %+v", got)
	}
}

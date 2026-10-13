package config

import (
	"strings"
	"testing"
)

func apNet(edit func(*NetworkConfig)) NetworkConfig {
	c := NetworkConfig{Hostname: "ap-hall", Country: "US", AP: &APConfig{Enabled: true, Ports: []string{"eth0", "eth1"}}}
	if edit != nil {
		edit(&c)
	}
	return c
}

func TestValidAccessPoints(t *testing.T) {
	cases := map[string]NetworkConfig{
		"two ports, DHCP": apNet(nil),
		"a fixed address": apNet(func(c *NetworkConfig) {
			c.AP.Mode, c.AP.IP, c.AP.Netmask, c.AP.Gateway, c.AP.DNS = APStatic, "192.168.1.2", "255.255.255.0", "192.168.1.1", []string{"192.168.1.1"}
		}),
		"no gateway": apNet(func(c *NetworkConfig) { c.AP.Mode, c.AP.IP, c.AP.Netmask = APStatic, "10.0.0.2", "255.0.0.0" }),
		"a wireless extender (no cable)": apNet(func(c *NetworkConfig) {
			c.AP.Ports = nil
			c.Wifi = &WifiConfig{Stations: []WifiStation{linkStn()}}
		}),
		"with Wi-Fi": apNet(func(c *NetworkConfig) {
			c.Wifi = &WifiConfig{Radios: []WifiRadio{radio24()}, Networks: []WifiNetwork{func() WifiNetwork { n := homeWifi(); n.Bands = []string{Band24}; n.BandSteering = false; return n }()}}
		}),
		"router settings left over are not looked at": apNet(func(c *NetworkConfig) {
			c.WanInterface, c.LanIP, c.LanSubnet = "!!", "not an address", "nope"
			c.DHCP = &DHCPConfig{Enabled: true, RangeStart: "x"}
		}),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !c.IsAP() || c.OperationMode() != "AP" {
			t.Errorf("%s: not an access point", name)
		}
	}
}

func TestInvalidAccessPoints(t *testing.T) {
	static := func(f func(*APConfig)) NetworkConfig {
		return apNet(func(c *NetworkConfig) {
			c.AP.Mode, c.AP.IP, c.AP.Netmask = APStatic, "192.168.1.2", "255.255.255.0"
			f(c.AP)
		})
	}
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"no ports", apNet(func(c *NetworkConfig) { c.AP.Ports = nil }), "ap.ports"},
		{"a port that is not a name", apNet(func(c *NetworkConfig) { c.AP.Ports = []string{"eth0;reboot"} }), "ap.ports"},
		{"a port twice", apNet(func(c *NetworkConfig) { c.AP.Ports = []string{"eth0", "eth0"} }), "ap.ports"},
		{"a port that is also a link", apNet(func(c *NetworkConfig) {
			c.AP.Ports = []string{"wlan1"}
			c.Wifi = &WifiConfig{Stations: []WifiStation{linkStn()}}
		}), "ap.ports"},
		{"an unknown way to get an address", apNet(func(c *NetworkConfig) { c.AP.Mode = "magic" }), "ap.mode"},
		{"no address", static(func(a *APConfig) { a.IP = "" }), "ap.ip"},
		{"the network address", static(func(a *APConfig) { a.IP = "192.168.1.0" }), "ap.ip"},
		{"the recovery range", static(func(a *APConfig) { a.IP, a.Netmask = "192.168.77.20", "255.255.255.0" }), "ap.ip"},
		{"a netmask that is not one", static(func(a *APConfig) { a.Netmask = "255.0.255.0" }), "ap.netmask"},
		{"a gateway outside", static(func(a *APConfig) { a.Gateway = "10.0.0.1" }), "ap.gateway"},
		{"the device as its own gateway", static(func(a *APConfig) { a.Gateway = "192.168.1.2" }), "ap.gateway"},
		{"a name server that is not one", static(func(a *APConfig) { a.DNS = []string{"nope"} }), "ap.dns"},
		{"a country that is not one", apNet(func(c *NetworkConfig) { c.Country = "ZZ" }), "country"},
		{"Wi-Fi without a country", apNet(func(c *NetworkConfig) {
			c.Country = ""
			c.Wifi = &WifiConfig{Radios: []WifiRadio{radio24()}, Networks: []WifiNetwork{func() WifiNetwork { n := homeWifi(); n.Bands = []string{Band24}; n.BandSteering = false; return n }()}}
		}), "country"},
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

func TestAnAccessPointThatIsSwitchedOffIsARouter(t *testing.T) {
	c := validNet()
	c.AP = &APConfig{Enabled: false, Ports: nil}
	if c.IsAP() || c.OperationMode() != "ROUTER" {
		t.Fatal("a disabled access point setup made it an access point")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the router checks do not run: %v", err)
	}
	// and the setup of an access point is kept while it is a router
	if c.APSettings().Enabled {
		t.Fatal("enabled")
	}
}

func TestTheReviewSaysTheDeviceChangesMode(t *testing.T) {
	changes := DiffNetwork(validNet(), apNet(nil))
	var mode, net *Change
	for i := range changes {
		switch changes[i].Field {
		case "ap_enabled":
			mode = &changes[i]
		case "ap":
			net = &changes[i]
		}
	}
	if mode == nil || mode.Old != "router" || mode.New != "access point" || mode.Label != "Operation mode" {
		t.Fatalf("%+v", changes)
	}
	if net == nil || !strings.Contains(net.New, "address from DHCP") || !strings.Contains(net.New, "eth0, eth1") {
		t.Fatalf("%+v", net)
	}
	// a change of address alone
	b := apNet(func(c *NetworkConfig) { c.AP.Mode, c.AP.IP, c.AP.Netmask = APStatic, "192.168.1.2", "255.255.255.0" })
	for _, ch := range DiffNetwork(apNet(nil), b) {
		if ch.Field == "ap_enabled" {
			t.Fatal("the mode did not change")
		}
	}
	if len(DiffNetwork(apNet(nil), apNet(nil))) != 0 {
		t.Fatal("a change from nothing")
	}
}

func TestACopyOfAnAccessPointSharesNothing(t *testing.T) {
	c := apNet(nil)
	d := c.clone()
	d.AP.Ports[0] = "changed"
	if c.AP.Ports[0] == "changed" {
		t.Fatal("shared")
	}
	if got := c.APBridgeMembers(); len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

func tagged(vlan int, id, name string) WifiNetwork {
	n := homeWifi()
	n.ID, n.SSID, n.Bands, n.BandSteering, n.VLAN = id, name, []string{Band24}, false, vlan
	return n
}

func TestAWifiNetworkOfAnAccessPointCanBeOnAVLAN(t *testing.T) {
	c := apNet(func(c *NetworkConfig) {
		c.Wifi = &WifiConfig{Radios: []WifiRadio{radio24()}, Networks: []WifiNetwork{tagged(0, "w1", "Home"), tagged(20, "w2", "Guest"), tagged(20, "w3", "Guest 2"), tagged(30, "w4", "Cameras")}}
	})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := c.APVLANs(); len(got) != 2 || got[0] != 20 || got[1] != 30 {
		t.Fatalf("%v", got)
	}
	// a network that is switched off does not need its VLAN
	c.Wifi.Networks[3].Disabled = true
	if got := c.APVLANs(); len(got) != 1 || got[0] != 20 {
		t.Fatalf("%v", got)
	}
	for _, bad := range []int{1, -5, 4095, 70000} {
		c := apNet(func(c *NetworkConfig) {
			c.Wifi = &WifiConfig{Radios: []WifiRadio{radio24()}, Networks: []WifiNetwork{tagged(bad, "w1", "Home")}}
		})
		err := c.Validate()
		if err == nil || !fieldsOf(t, err).Has("wifi.networks[0].vlan") {
			t.Errorf("VLAN %d: %v", bad, err)
		}
	}
}

func TestARouterKeepsToLocalNetworksAndRefusesAWifiVLAN(t *testing.T) {
	c := withWifi(validNet(), "US", []WifiRadio{radio24()}, tagged(20, "w1", "Home"))
	c.Wifi.Networks[0].Network = "lan"
	err := c.Validate()
	if err == nil || !fieldsOf(t, err).Has("wifi.networks[0].vlan") {
		t.Fatalf("%v", err)
	}
	c.Wifi.Networks[0].VLAN = 0
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestTheReviewNamesTheVLANOfAWifiNetworkOnAnAccessPoint(t *testing.T) {
	old := apNet(func(c *NetworkConfig) {
		c.Wifi = &WifiConfig{Radios: []WifiRadio{radio24()}, Networks: []WifiNetwork{tagged(0, "w1", "Guest")}}
	})
	b := apNet(func(c *NetworkConfig) {
		c.Wifi = &WifiConfig{Radios: []WifiRadio{radio24()}, Networks: []WifiNetwork{tagged(20, "w1", "Guest")}}
	})
	for _, ch := range DiffNetwork(old, b) {
		if ch.Field == "wifi_networks" {
			if !strings.Contains(ch.Old, "untagged network") || !strings.Contains(ch.New, "VLAN 20") || strings.Contains(ch.New, "joins") {
				t.Fatalf("%+v", ch)
			}
			return
		}
	}
	t.Fatal("the change is not in the review")
}

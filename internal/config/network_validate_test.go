package config

import (
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"testing"
)

func validNet() NetworkConfig {
	return NetworkConfig{
		WanInterface:  "eth0",
		WanMode:       "dhcp",
		LanInterfaces: []string{"eth1"},
		LanIP:         "192.168.1.1",
		LanSubnet:     "255.255.255.0",
		DHCP:          &DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"},
	}
}

func staticWan(c NetworkConfig) NetworkConfig {
	c.WanMode = "static"
	c.WanIP = "203.0.113.10"
	c.WanSubnet = "255.255.255.0"
	c.WanGateway = "203.0.113.1"
	c.WanDNS = []string{"1.1.1.1", "9.9.9.9"}
	return c
}

func fieldsOf(t *testing.T, err error) ValidationError {
	t.Helper()
	var ve ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error = %v, want a ValidationError", err)
	}
	return ve
}

func TestValidConfigurationsPass(t *testing.T) {
	cases := map[string]NetworkConfig{
		"dhcp wan with dhcp server": validNet(),
		"static wan":                staticWan(validNet()),
		"multiple LAN ports":        func() NetworkConfig { c := validNet(); c.LanInterfaces = []string{"eth1", "eth2", "eth3"}; return c }(),
		"dhcp section absent":       func() NetworkConfig { c := validNet(); c.DHCP = nil; return c }(),
		"dhcp disabled needs no range": func() NetworkConfig {
			c := validNet()
			c.DHCP = &DHCPConfig{Enabled: false}
			return c
		}(),
		"larger private subnet": func() NetworkConfig {
			c := validNet()
			c.LanIP, c.LanSubnet = "10.0.0.1", "255.255.0.0"
			c.DHCP = &DHCPConfig{Enabled: true, RangeStart: "10.0.1.10", RangeEnd: "10.0.200.250", LeaseTime: "1d"}
			return c
		}(),
		"wan mode in upper case": func() NetworkConfig { c := validNet(); c.WanMode = "DHCP"; return c }(),
		"vlan style name":        func() NetworkConfig { c := validNet(); c.LanInterfaces = []string{"eth1.10"}; return c }(),
		"infinite lease":         func() NetworkConfig { c := validNet(); c.DHCP.LeaseTime = "infinite"; return c }(),
	}
	for name, cfg := range cases {
		if err := cfg.Validate(); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}
}

// A configuration with only the basics (what older setups wrote). Existing installs must stay committable.
func TestConfigWrittenByTheFirstBootWizardIsValid(t *testing.T) {
	const wizardJSON = `{
    "wan_interface": "enp1s0",
    "wan_mode": "dhcp",
    "wan_ip": "",
    "wan_subnet": "",
    "wan_gateway": "",
    "wan_dns": [],
    "lan_interfaces": ["enp2s0", "enp3s0"],
    "lan_ip": "192.168.1.1",
    "lan_subnet": "255.255.255.0",
    "interfaces": [],
    "managed_switches": []
}`
	var cfg NetworkConfig
	if err := json.Unmarshal([]byte(wizardJSON), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.DHCP != nil {
		t.Fatal("a wizard config has no dhcp section, so it must load as nil (not managed yet)")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("wizard-written config rejected: %v", err)
	}
}

func TestInvalidConfigurationsAreRejectedWithTheRightField(t *testing.T) {
	type mut func(*NetworkConfig)
	cases := []struct {
		name  string
		apply mut
		field string
	}{
		// WAN
		{"empty WAN interface", func(c *NetworkConfig) { c.WanInterface = "" }, "wan_interface"},
		{"WAN name with a space", func(c *NetworkConfig) { c.WanInterface = "eth 0" }, "wan_interface"},
		{"WAN name with a newline", func(c *NetworkConfig) { c.WanInterface = "eth0\niface evil inet dhcp" }, "wan_interface"},
		{"WAN name too long", func(c *NetworkConfig) { c.WanInterface = "abcdefghijklmnop" }, "wan_interface"},
		{"WAN name starting with a dash", func(c *NetworkConfig) { c.WanInterface = "-eth0" }, "wan_interface"},
		{"WAN name with quote and brace", func(c *NetworkConfig) { c.WanInterface = `eth0"}` }, "wan_interface"},
		{"unknown WAN mode", func(c *NetworkConfig) { c.WanMode = "l2tp" }, "wan_mode"},
		{"static WAN without an IP", func(c *NetworkConfig) { *c = staticWan(*c); c.WanIP = "" }, "wan_ip"},
		{"static WAN with CIDR in the IP", func(c *NetworkConfig) { *c = staticWan(*c); c.WanIP = "203.0.113.10/24" }, "wan_ip"},
		{"static WAN with IPv6", func(c *NetworkConfig) { *c = staticWan(*c); c.WanIP = "2001:db8::1" }, "wan_ip"},
		{"static WAN network address", func(c *NetworkConfig) { *c = staticWan(*c); c.WanIP = "203.0.113.0" }, "wan_ip"},
		{"static WAN broadcast address", func(c *NetworkConfig) { *c = staticWan(*c); c.WanIP = "203.0.113.255" }, "wan_ip"},
		{"static WAN without a netmask", func(c *NetworkConfig) { *c = staticWan(*c); c.WanSubnet = "" }, "wan_subnet"},
		{"static WAN gateway outside the subnet", func(c *NetworkConfig) { *c = staticWan(*c); c.WanGateway = "198.51.100.1" }, "wan_gateway"},
		{"static WAN gateway equals the IP", func(c *NetworkConfig) { *c = staticWan(*c); c.WanGateway = c.WanIP }, "wan_gateway"},
		{"static WAN junk gateway", func(c *NetworkConfig) { *c = staticWan(*c); c.WanGateway = "gateway" }, "wan_gateway"},
		{"static WAN junk DNS", func(c *NetworkConfig) { *c = staticWan(*c); c.WanDNS = []string{"1.1.1.1; rm -rf /"} }, "wan_dns"},
		{"static WAN too many DNS servers", func(c *NetworkConfig) {
			*c = staticWan(*c)
			c.WanDNS = []string{"1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4"}
		}, "wan_dns"},

		// LAN
		{"no LAN ports", func(c *NetworkConfig) { c.LanInterfaces = nil }, "lan_interfaces"},
		{"empty LAN port name", func(c *NetworkConfig) { c.LanInterfaces = []string{""} }, "lan_interfaces"},
		{"bad LAN port name", func(c *NetworkConfig) { c.LanInterfaces = []string{"eth1;reboot"} }, "lan_interfaces"},
		{"WAN also used as a LAN port", func(c *NetworkConfig) { c.LanInterfaces = []string{"eth0"} }, "lan_interfaces"},
		{"duplicate LAN port", func(c *NetworkConfig) { c.LanInterfaces = []string{"eth1", "eth1"} }, "lan_interfaces"},
		{"empty LAN IP", func(c *NetworkConfig) { c.LanIP = "" }, "lan_ip"},
		{"LAN IP with CIDR", func(c *NetworkConfig) { c.LanIP = "192.168.1.1/24" }, "lan_ip"},
		{"LAN IP out of range", func(c *NetworkConfig) { c.LanIP = "192.168.1.256" }, "lan_ip"},
		{"LAN IP is the network address", func(c *NetworkConfig) { c.LanIP = "192.168.1.0" }, "lan_ip"},
		{"LAN IP is the broadcast address", func(c *NetworkConfig) { c.LanIP = "192.168.1.255" }, "lan_ip"},
		{"empty LAN netmask", func(c *NetworkConfig) { c.LanSubnet = "" }, "lan_subnet"},
		{"non-contiguous netmask", func(c *NetworkConfig) { c.LanSubnet = "255.0.255.0" }, "lan_subnet"},
		{"netmask given as a prefix length", func(c *NetworkConfig) { c.LanSubnet = "24" }, "lan_subnet"},
		{"netmask /31 is too small", func(c *NetworkConfig) { c.LanSubnet = "255.255.255.254" }, "lan_subnet"},
		{"netmask /7 is too large", func(c *NetworkConfig) { c.LanSubnet = "254.0.0.0" }, "lan_subnet"},
		{"LAN overlaps a static WAN", func(c *NetworkConfig) {
			*c = staticWan(*c)
			c.LanIP, c.LanSubnet = "203.0.113.129", "255.255.255.128"
			c.DHCP = nil
		}, "lan_subnet"},
		{"static WAN overlaps the LAN (reported on the WAN too)", func(c *NetworkConfig) {
			*c = staticWan(*c)
			c.LanIP, c.LanSubnet = "203.0.113.129", "255.255.255.128"
			c.DHCP = nil
		}, "wan_subnet"},

		// DHCP
		{"DHCP without a start", func(c *NetworkConfig) { c.DHCP.RangeStart = "" }, "dhcp_range_start"},
		{"DHCP without an end", func(c *NetworkConfig) { c.DHCP.RangeEnd = "" }, "dhcp_range_end"},
		{"DHCP start outside the LAN", func(c *NetworkConfig) { c.DHCP.RangeStart = "10.0.0.50" }, "dhcp_range_start"},
		{"DHCP end outside the LAN", func(c *NetworkConfig) { c.DHCP.RangeEnd = "192.168.2.50" }, "dhcp_range_end"},
		{"DHCP start is the network address", func(c *NetworkConfig) { c.DHCP.RangeStart = "192.168.1.0" }, "dhcp_range_start"},
		{"DHCP end is the broadcast address", func(c *NetworkConfig) { c.DHCP.RangeEnd = "192.168.1.255" }, "dhcp_range_end"},
		{"DHCP range reversed", func(c *NetworkConfig) { c.DHCP.RangeStart, c.DHCP.RangeEnd = "192.168.1.200", "192.168.1.100" }, "dhcp_range_end"},
		{"DHCP range covers the router address", func(c *NetworkConfig) { c.DHCP.RangeStart, c.DHCP.RangeEnd = "192.168.1.1", "192.168.1.50" }, "dhcp_range_start"},
		{"DHCP lease missing", func(c *NetworkConfig) { c.DHCP.LeaseTime = "" }, "dhcp_lease_time"},
		{"DHCP lease without a unit", func(c *NetworkConfig) { c.DHCP.LeaseTime = "12" }, "dhcp_lease_time"},
		{"DHCP lease shorter than dnsmasq's minimum", func(c *NetworkConfig) { c.DHCP.LeaseTime = "60s" }, "dhcp_lease_time"},
		{"DHCP lease with injection", func(c *NetworkConfig) { c.DHCP.LeaseTime = "12h\ndhcp-option=3,1.2.3.4" }, "dhcp_lease_time"},
		{"DHCP lease zero", func(c *NetworkConfig) { c.DHCP.LeaseTime = "0h" }, "dhcp_lease_time"},
	}

	for _, c := range cases {
		cfg := validNet()
		c.apply(&cfg)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted, want a %s error", c.name, c.field)
			continue
		}
		if ve := fieldsOf(t, err); !ve.Has(c.field) {
			t.Errorf("%s: errors were %v, want one on %s", c.name, ve, c.field)
		}
	}
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	cfg := NetworkConfig{WanInterface: "bad name", LanIP: "nope", LanSubnet: "also nope"}
	ve := fieldsOf(t, cfg.Validate())

	for _, field := range []string{"wan_interface", "lan_interfaces", "lan_ip", "lan_subnet"} {
		if !ve.Has(field) {
			t.Errorf("missing an error for %s in %v", field, ve)
		}
	}
	if !strings.Contains(ve.Error(), "lan_ip:") {
		t.Errorf("Error() should name the fields: %q", ve.Error())
	}
}

func TestParseLeaseTime(t *testing.T) {
	cases := []struct {
		in   string
		secs int64
		ok   bool
	}{
		{"2m", 120, true}, {"30m", 1800, true}, {"12h", 43200, true}, {"1d", 86400, true},
		{"120s", 120, true}, {"infinite", -1, true},
		{"119s", 119, false}, {"1m", 60, false}, {"", 0, false}, {"h", 0, false},
		{"12", 0, false}, {"12 h", 0, false}, {"-5m", 0, false}, {"1.5h", 0, false},
		{"12H", 0, false}, {"1000000h", 0, false},
	}
	for _, c := range cases {
		secs, ok := ParseLeaseTime(c.in)
		if ok != c.ok || (ok && secs != c.secs) {
			t.Errorf("ParseLeaseTime(%q) = (%d, %v), want (%d, %v)", c.in, secs, ok, c.secs, c.ok)
		}
	}
}

func TestInterfaceNameValidation(t *testing.T) {
	good := []string{"eth0", "enp0s31f6", "wlp2s0", "br0", "eth0.10", "a", "lan_1", "some-port", "123456789012345"}
	bad := []string{"", " eth0", "eth0 ", "eth 0", "eth0\n", "eth0;", "$(id)", "../eth0", "eth/0", "-eth0", ".eth0",
		"1234567890123456", "eth0\x00", "ét0", `eth"0`}
	for _, n := range good {
		if !IsValidInterfaceName(n) {
			t.Errorf("%q should be a valid interface name", n)
		}
	}
	for _, n := range bad {
		if IsValidInterfaceName(n) {
			t.Errorf("%q must not be accepted as an interface name", n)
		}
	}
}

func TestBroadcastAndNetworkAddresses(t *testing.T) {
	cases := []struct{ cidr, network, broadcast string }{
		{"192.168.1.77/24", "192.168.1.0", "192.168.1.255"},
		{"10.1.2.3/16", "10.1.0.0", "10.1.255.255"},
		{"172.16.5.9/30", "172.16.5.8", "172.16.5.11"},
		{"10.0.0.1/8", "10.0.0.0", "10.255.255.255"},
	}
	for _, c := range cases {
		p := mustPrefix(t, c.cidr)
		if got := networkAddr(p).String(); got != c.network {
			t.Errorf("networkAddr(%s) = %s, want %s", c.cidr, got, c.network)
		}
		if got := broadcastAddr(p).String(); got != c.broadcast {
			t.Errorf("broadcastAddr(%s) = %s, want %s", c.cidr, got, c.broadcast)
		}
	}
}

func mustPrefix(t *testing.T, cidr string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A non-contiguous mask is rejected with the right explanation, not as "out of range"
func TestNonContiguousNetmaskGetsAMeaningfulMessage(t *testing.T) {
	for _, mask := range []string{"255.0.255.0", "255.255.0.255", "0.255.255.255"} {
		cfg := validNet()
		cfg.LanSubnet = mask
		ve := fieldsOf(t, cfg.Validate())
		for _, e := range ve {
			if e.Field == "lan_subnet" && !strings.Contains(e.Message, "must be a netmask such as") {
				t.Errorf("%s: message = %q, want it to explain this is not a valid netmask", mask, e.Message)
			}
		}
		if !ve.Has("lan_subnet") {
			t.Errorf("%s was accepted", mask)
		}
	}
}

func TestReservationRules(t *testing.T) {
	with := func(rs ...Reservation) NetworkConfig {
		c := validNet()
		c.Reservations = rs
		return c
	}
	good := Reservation{MAC: "f8:94:c2:a4:13:a7", IP: "192.168.1.50", Name: "printer"}
	if err := with(good).Validate(); err != nil {
		t.Fatalf("a good reservation was refused: %v", err)
	}
	// inside the dynamic range is fine too (the server then never hands that address to anyone else)
	if err := with(Reservation{MAC: "f8:94:c2:a4:13:a7", IP: "192.168.1.150"}).Validate(); err != nil {
		t.Fatalf("inside the range: %v", err)
	}
	for name, c := range map[string]struct {
		cfg   NetworkConfig
		field string
	}{
		"not a MAC":             {with(Reservation{MAC: "nope", IP: "192.168.1.50"}), "reservations[0].mac"},
		"a multicast MAC":       {with(Reservation{MAC: "01:00:5e:00:00:01", IP: "192.168.1.50"}), "reservations[0].mac"},
		"an upper case MAC":     {with(Reservation{MAC: "F8:94:C2:A4:13:A7", IP: "192.168.1.50"}), "reservations[0].mac"},
		"the same MAC twice":    {with(good, Reservation{MAC: good.MAC, IP: "192.168.1.51"}), "reservations[1].mac"},
		"the same address":      {with(good, Reservation{MAC: "48:b0:2d:f1:fe:ca", IP: good.IP}), "reservations[1].ip"},
		"the same name":         {with(good, Reservation{MAC: "48:b0:2d:f1:fe:ca", IP: "192.168.1.51", Name: "PRINTER"}), "reservations[1].name"},
		"not an address":        {with(Reservation{MAC: good.MAC, IP: "192.168.1.50/24"}), "reservations[0].ip"},
		"outside every network": {with(Reservation{MAC: good.MAC, IP: "10.1.1.1"}), "reservations[0].ip"},
		"the router itself":     {with(Reservation{MAC: good.MAC, IP: "192.168.1.1"}), "reservations[0].ip"},
		"the network address":   {with(Reservation{MAC: good.MAC, IP: "192.168.1.0"}), "reservations[0].ip"},
		"the broadcast":         {with(Reservation{MAC: good.MAC, IP: "192.168.1.255"}), "reservations[0].ip"},
		"a name with a space":   {with(Reservation{MAC: good.MAC, IP: "192.168.1.50", Name: "my printer"}), "reservations[0].name"},
		"a name ending in -":    {with(Reservation{MAC: good.MAC, IP: "192.168.1.50", Name: "printer-"}), "reservations[0].name"},
	} {
		err := c.cfg.Validate()
		var ve ValidationError
		if !errors.As(err, &ve) || !ve.Has(c.field) {
			t.Errorf("%s: no error on %s (%v)", name, c.field, err)
		}
	}

	// with the network's DHCP server off, there is nothing to keep the address
	off := with(good)
	off.DHCP.Enabled = false
	var ve ValidationError
	if err := off.Validate(); !errors.As(err, &ve) || !ve.Has("reservations[0].ip") {
		t.Errorf("DHCP off: %v", err)
	}
}

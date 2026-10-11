package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func dnsNet(f func(*NetworkConfig)) NetworkConfig {
	c := withNets(validNet(), guest(), iot())
	c.DNS = &DNSConfig{}
	if f != nil {
		f(&c)
	}
	return c
}

func host(id, name, addr string) DNSHost { return DNSHost{ID: id, Name: name, Address: addr} }

func TestDNSSettingsThatAreAccepted(t *testing.T) {
	cases := map[string]NetworkConfig{
		"nothing set":          validNet(),
		"an empty section":     dnsNet(nil),
		"a domain":             dnsNet(func(c *NetworkConfig) { c.Domain = "lan" }),
		"a longer domain":      dnsNet(func(c *NetworkConfig) { c.Domain = "home.example.com" }),
		"a domain with digits": dnsNet(func(c *NetworkConfig) { c.Domain = "a1-b2.example.org" }),
		"a local name":         dnsNet(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{host("h1", "nas", "192.168.1.50")} }),
		"a full local name":    dnsNet(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{host("h1", "home.example.com", "192.168.1.50")} }),
		"the same name once off": dnsNet(func(c *NetworkConfig) {
			c.DNS.Hosts = []DNSHost{host("h1", "nas", "192.168.1.50"), {ID: "h2", Name: "nas", Address: "192.168.1.51", Disabled: true}}
		}),
		"a public address as a name": dnsNet(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{host("h1", "partner", "203.0.113.9")} }),
		"a forward": dnsNet(func(c *NetworkConfig) {
			c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "corp.example.com", Servers: []string{"10.0.0.5"}}}
		}),
		"a forward of 3 servers": dnsNet(func(c *NetworkConfig) {
			c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "corp.example.com", Servers: []string{"10.0.0.5", "10.0.0.6", "10.0.0.7"}}}
		}),
		"a forward outside the local domain": dnsNet(func(c *NetworkConfig) {
			c.Domain = "lan"
			c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "lanx", Servers: []string{"10.0.0.5"}}, {ID: "f2", Domain: "corp.example.com", Servers: []string{"10.0.0.5"}}}
		}),
		"the same domain forwarded once off": dnsNet(func(c *NetworkConfig) {
			c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "a.example", Servers: []string{"10.0.0.5"}}, {ID: "f2", Domain: "a.example", Servers: []string{"10.0.0.6"}, Disabled: true}}
		}),
		"servers for the default network": dnsNet(func(c *NetworkConfig) { c.LanDNSServers = []string{"192.168.1.5", "192.168.1.6"} }),
		"servers for another network": dnsNet(func(c *NetworkConfig) {
			c.Networks[0].DNSServers = []string{"10.20.0.5"}
			c.Networks[1].DNSServers = []string{"1.1.1.1", "9.9.9.9", "8.8.8.8"}
		}),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDNSSettingsThatAreRefused(t *testing.T) {
	hostCase := func(f func(*DNSHost)) NetworkConfig {
		h := host("h1", "nas", "192.168.1.50")
		f(&h)
		return dnsNet(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h} })
	}
	fwdCase := func(f func(*DNSForward)) NetworkConfig {
		x := DNSForward{ID: "f1", Domain: "corp.example.com", Servers: []string{"10.0.0.5"}}
		f(&x)
		return dnsNet(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{x} })
	}
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"domain in capitals", dnsNet(func(c *NetworkConfig) { c.Domain = "LAN" }), "domain"},
		{"domain with a space", dnsNet(func(c *NetworkConfig) { c.Domain = "my lan" }), "domain"},
		{"domain with an underscore", dnsNet(func(c *NetworkConfig) { c.Domain = "my_lan" }), "domain"},
		{"domain with a leading dot", dnsNet(func(c *NetworkConfig) { c.Domain = ".lan" }), "domain"},
		{"domain with a trailing dot", dnsNet(func(c *NetworkConfig) { c.Domain = "lan." }), "domain"},
		{"domain with an empty label", dnsNet(func(c *NetworkConfig) { c.Domain = "a..b" }), "domain"},
		{"domain starting with a hyphen", dnsNet(func(c *NetworkConfig) { c.Domain = "-lan" }), "domain"},
		{"domain ending with a hyphen", dnsNet(func(c *NetworkConfig) { c.Domain = "lan-" }), "domain"},
		{"a 64 character label", dnsNet(func(c *NetworkConfig) { c.Domain = strings.Repeat("a", 64) }), "domain"},
		{"a name over 253 characters", dnsNet(func(c *NetworkConfig) { c.Domain = strings.Repeat("abcdefgh.", 30) + "com" }), "domain"},
		{"domain local", dnsNet(func(c *NetworkConfig) { c.Domain = "local" }), "domain"},
		{"domain localhost", dnsNet(func(c *NetworkConfig) { c.Domain = "localhost" }), "domain"},
		{"domain example", dnsNet(func(c *NetworkConfig) { c.Domain = "example" }), "domain"},
		{"domain injecting a line", dnsNet(func(c *NetworkConfig) { c.Domain = "lan\nserver=1.1.1.1" }), "domain"},

		{"host bad id", hostCase(func(h *DNSHost) { h.ID = "H 1" }), "dns.hosts[0].id"},
		{"host duplicate id", dnsNet(func(c *NetworkConfig) {
			c.DNS.Hosts = []DNSHost{host("h1", "a", "192.168.1.5"), host("h1", "b", "192.168.1.6")}
		}), "dns.hosts[1].id"},
		{"host no name", hostCase(func(h *DNSHost) { h.Name = "" }), "dns.hosts[0].name"},
		{"host name in capitals", hostCase(func(h *DNSHost) { h.Name = "NAS" }), "dns.hosts[0].name"},
		{"host name with a space", hostCase(func(h *DNSHost) { h.Name = "my nas" }), "dns.hosts[0].name"},
		{"host name injecting", hostCase(func(h *DNSHost) { h.Name = "nas,1.2.3.4\nserver=9.9.9.9" }), "dns.hosts[0].name"},
		{"host name twice", dnsNet(func(c *NetworkConfig) {
			c.DNS.Hosts = []DNSHost{host("h1", "nas", "192.168.1.5"), host("h2", "nas", "192.168.1.6")}
		}), "dns.hosts[1].name"},
		{"host no address", hostCase(func(h *DNSHost) { h.Address = "" }), "dns.hosts[0].address"},
		{"host name as address", hostCase(func(h *DNSHost) { h.Address = "nas.lan" }), "dns.hosts[0].address"},
		{"host ipv6 address", hostCase(func(h *DNSHost) { h.Address = "fd00::5" }), "dns.hosts[0].address"},
		{"host zero address", hostCase(func(h *DNSHost) { h.Address = "0.0.0.0" }), "dns.hosts[0].address"},
		{"host loopback address", hostCase(func(h *DNSHost) { h.Address = "127.0.0.1" }), "dns.hosts[0].address"},
		{"host multicast address", hostCase(func(h *DNSHost) { h.Address = "224.0.0.1" }), "dns.hosts[0].address"},

		{"forward bad id", fwdCase(func(f *DNSForward) { f.ID = "" }), "dns.forwards[0].id"},
		{"forward duplicate id", dnsNet(func(c *NetworkConfig) {
			c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "a.example", Servers: []string{"10.0.0.5"}}, {ID: "f1", Domain: "b.example", Servers: []string{"10.0.0.5"}}}
		}), "dns.forwards[1].id"},
		{"forward no domain", fwdCase(func(f *DNSForward) { f.Domain = "" }), "dns.forwards[0].domain"},
		{"forward domain in capitals", fwdCase(func(f *DNSForward) { f.Domain = "Corp.Example.com" }), "dns.forwards[0].domain"},
		{"forward domain twice", dnsNet(func(c *NetworkConfig) {
			c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "a.example", Servers: []string{"10.0.0.5"}}, {ID: "f2", Domain: "a.example", Servers: []string{"10.0.0.6"}}}
		}), "dns.forwards[1].domain"},
		{"forward of the local domain", dnsNet(func(c *NetworkConfig) {
			c.Domain = "lan"
			c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "lan", Servers: []string{"10.0.0.5"}}}
		}), "dns.forwards[0].domain"},
		{"forward inside the local domain", dnsNet(func(c *NetworkConfig) {
			c.Domain = "home.example.com"
			c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "lab.home.example.com", Servers: []string{"10.0.0.5"}}}
		}), "dns.forwards[0].domain"},
		{"forward without servers", fwdCase(func(f *DNSForward) { f.Servers = nil }), "dns.forwards[0].servers"},
		{"forward four servers", fwdCase(func(f *DNSForward) { f.Servers = []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"} }), "dns.forwards[0].servers"},
		{"forward server not an address", fwdCase(func(f *DNSForward) { f.Servers = []string{"dns.corp"} }), "dns.forwards[0].servers"},
		{"forward server loopback", fwdCase(func(f *DNSForward) { f.Servers = []string{"127.0.0.1"} }), "dns.forwards[0].servers"},
		{"forward server twice", fwdCase(func(f *DNSForward) { f.Servers = []string{"10.0.0.5", "10.0.0.5"} }), "dns.forwards[0].servers"},

		{"default network server not an address", dnsNet(func(c *NetworkConfig) { c.LanDNSServers = []string{"pihole"} }), "lan_dns_servers"},
		{"default network four servers", dnsNet(func(c *NetworkConfig) { c.LanDNSServers = []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4"} }), "lan_dns_servers"},
		{"default network server unspecified", dnsNet(func(c *NetworkConfig) { c.LanDNSServers = []string{"0.0.0.0"} }), "lan_dns_servers"},
		{"default network server broadcast", dnsNet(func(c *NetworkConfig) { c.LanDNSServers = []string{"255.255.255.255"} }), "lan_dns_servers"},
		{"default network server multicast", dnsNet(func(c *NetworkConfig) { c.LanDNSServers = []string{"224.0.0.251"} }), "lan_dns_servers"},
		{"default network server twice", dnsNet(func(c *NetworkConfig) { c.LanDNSServers = []string{"10.0.0.5", "10.0.0.5"} }), "lan_dns_servers"},
		{"other network server loopback", dnsNet(func(c *NetworkConfig) { c.Networks[1].DNSServers = []string{"127.0.0.53"} }), "networks[1].dns_servers"},
		{"other network server injecting", dnsNet(func(c *NetworkConfig) { c.Networks[0].DNSServers = []string{"1.1.1.1\ndhcp-option=3,6.6.6.6"} }), "networks[0].dns_servers"},
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

func TestTheDNSLimits(t *testing.T) {
	var hosts []DNSHost
	for i := 1; i <= MaxDNSHosts; i++ {
		hosts = append(hosts, host("h"+itoa(i), "host"+itoa(i), "192.168.1.50"))
	}
	c := dnsNet(func(c *NetworkConfig) { c.DNS.Hosts = hosts })
	if err := c.Validate(); err != nil {
		t.Fatalf("%d names: %v", len(hosts), err)
	}
	c.DNS.Hosts = append(c.DNS.Hosts, host("h999", "one-too-many", "192.168.1.51"))
	if err := c.Validate(); err == nil || !fieldsOf(t, err).Has("dns.hosts") {
		t.Fatalf("%v", err)
	}
	var fwds []DNSForward
	for i := 1; i <= MaxDNSForwards; i++ {
		fwds = append(fwds, DNSForward{ID: "f" + itoa(i), Domain: "d" + itoa(i) + ".example", Servers: []string{"10.0.0.5"}})
	}
	c = dnsNet(func(c *NetworkConfig) { c.DNS.Forwards = fwds })
	if err := c.Validate(); err != nil {
		t.Fatalf("%d forwards: %v", len(fwds), err)
	}
	c.DNS.Forwards = append(c.DNS.Forwards, DNSForward{ID: "f999", Domain: "last.example", Servers: []string{"10.0.0.5"}})
	if err := c.Validate(); err == nil || !fieldsOf(t, err).Has("dns.forwards") {
		t.Fatalf("%v", err)
	}
}

func TestTheLengthOfANameIsLimitedTo253Characters(t *testing.T) {
	l63, l61 := strings.Repeat("a", 63), strings.Repeat("b", 61)
	max := l63 + "." + l63 + "." + l63 + "." + l61 // 63+1+63+1+63+1+61 = 253
	if len(max) != 253 || !ValidDNSName(max) {
		t.Errorf("a name of %d characters is refused", len(max))
	}
	if long := max + "b"; len(long) != 254 || ValidDNSName(long) {
		t.Errorf("a name of %d characters is accepted", len(long))
	}
}

func TestADisabledNameDoesNotBlockTheSameNameLaterOn(t *testing.T) {
	c := dnsNet(func(c *NetworkConfig) {
		c.DNS.Hosts = []DNSHost{{ID: "h1", Name: "nas", Address: "192.168.1.50", Disabled: true}, host("h2", "nas", "192.168.1.51")}
	})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidDNSName(t *testing.T) {
	for _, ok := range []string{"a", "nas", "a-b", "a.b", "a1.b2.c3", "1abc", "xn--bcher-kva.example", strings.Repeat("a", 63), strings.Repeat("a.", 100) + "a"} {
		if !ValidDNSName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", ".", "a.", ".a", "a..b", "-a", "a-", "A", "a_b", "a b", "ü", "a/b", strings.Repeat("a", 64), strings.Repeat("a.", 127) + "a"} {
		if ValidDNSName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDNSJSONAndClone(t *testing.T) {
	if plain, _ := json.Marshal(validNet()); strings.Contains(string(plain), "dns") || strings.Contains(string(plain), "domain") {
		t.Errorf("nothing about DNS is written when nothing is set: %s", plain)
	}
	a := dnsNet(func(c *NetworkConfig) {
		c.Domain = "lan"
		c.LanDNSServers = []string{"192.168.1.5"}
		c.Networks[0].DNSServers = []string{"10.20.0.5"}
		c.DNS.Hosts = []DNSHost{host("h1", "nas", "192.168.1.50")}
		c.DNS.Forwards = []DNSForward{{ID: "f1", Domain: "corp.example.com", Servers: []string{"10.0.0.5"}}}
	})
	b, _ := json.Marshal(a)
	var back NetworkConfig
	if err := json.Unmarshal(b, &back); err != nil || back.Domain != "lan" || back.DNS.Hosts[0].Name != "nas" || back.DNS.Forwards[0].Servers[0] != "10.0.0.5" ||
		back.LanDNSServers[0] != "192.168.1.5" || back.Networks[0].DNSServers[0] != "10.20.0.5" {
		t.Fatalf("%v %+v", err, back)
	}
	c := a.clone()
	c.DNS.Hosts[0].Name = "changed"
	c.DNS.Forwards[0].Servers[0] = "9.9.9.9"
	c.LanDNSServers[0] = "9.9.9.9"
	c.Networks[0].DNSServers[0] = "9.9.9.9"
	if a.DNS.Hosts[0].Name != "nas" || a.DNS.Forwards[0].Servers[0] != "10.0.0.5" || a.LanDNSServers[0] != "192.168.1.5" || a.Networks[0].DNSServers[0] != "10.20.0.5" {
		t.Fatal("the copy shares its DNS settings with the original")
	}
	if cl := validNet().clone(); cl.DNS != nil || cl.LanDNSServers != nil {
		t.Errorf("a copy gained DNS settings: %+v %v", cl.DNS, cl.LanDNSServers)
	}
	if (NetworkConfig{}).NetworkDNSServers("lan") != nil || len(a.NetworkDNSServers("lan")) != 1 || len(a.NetworkDNSServers("net2")) != 0 || len(a.NetworkDNSServers("guest")) != 1 {
		t.Errorf("NetworkDNSServers")
	}
	if h, ok := a.DNS.FindHost("h1"); !ok || h.Name != "nas" {
		t.Error("FindHost")
	}
	if _, ok := a.DNS.FindHost("nope"); ok {
		t.Error("FindHost found a ghost")
	}
	if f, ok := a.DNS.FindForward("f1"); !ok || f.Domain != "corp.example.com" {
		t.Error("FindForward")
	}
	if len((NetworkConfig{}).DNSSettings().Hosts) != 0 {
		t.Error("an absent section reads as empty")
	}
}

func TestANetworksServersAreComparedAndCopied(t *testing.T) {
	a, b := guest(), guest()
	if !a.Equal(b) {
		t.Fatal("equal networks differ")
	}
	b.DNSServers = []string{"10.20.0.5"}
	if a.Equal(b) {
		t.Fatal("a network with other DNS servers is the same network")
	}
	c := b.clone()
	c.DNSServers[0] = "9.9.9.9"
	if b.DNSServers[0] != "10.20.0.5" {
		t.Fatal("the copy shares the servers")
	}
}

func TestDNSReviewLines(t *testing.T) {
	base := withNets(validNet(), guest())
	with := func(f func(*NetworkConfig)) NetworkConfig { c := base.clone(); c.DNS = &DNSConfig{}; f(&c); return c }
	line := func(a, b NetworkConfig, field string) []Change {
		var out []Change
		for _, c := range DiffNetwork(a, b) {
			if c.Field == field {
				out = append(out, c)
			}
		}
		return out
	}

	if got := line(base, with(func(c *NetworkConfig) { c.Domain = "lan" }), "domain"); len(got) != 1 || got[0].Label != "Local domain" || got[0].Old != "(none)" || got[0].New != "lan" {
		t.Errorf("domain: %+v", got)
	}
	got := line(base, with(func(c *NetworkConfig) { c.LanDNSServers = []string{"192.168.1.5", "192.168.1.6"} }), "lan_dns_servers")
	if len(got) != 1 || got[0].Old != "this router" || got[0].New != "192.168.1.5, 192.168.1.6" {
		t.Errorf("default network servers: %+v", got)
	}
	// another network: part of its line
	b := with(func(c *NetworkConfig) { c.Networks[0].DNSServers = []string{"10.20.0.5"} })
	if got := line(base, b, "networks"); len(got) != 1 || !strings.HasSuffix(got[0].New, ", DNS 10.20.0.5") || strings.Contains(got[0].Old, "DNS") {
		t.Errorf("network servers: %+v", got)
	}

	h := host("h1", "nas", "192.168.1.50")
	if got := line(base, with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h} }), "dns_hosts"); len(got) != 1 || got[0].Label != "Local name nas" || got[0].Old != "(none)" || got[0].New != "nas is 192.168.1.50" {
		t.Errorf("host added: %+v", got)
	}
	h2 := h
	h2.Address = "192.168.1.51"
	if got := line(with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h} }), with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h2} }), "dns_hosts"); len(got) != 1 || got[0].Old != "nas is 192.168.1.50" || got[0].New != "nas is 192.168.1.51" {
		t.Errorf("host changed: %+v", got)
	}
	h3 := h
	h3.Name = "server"
	if got := line(with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h} }), with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h3} }), "dns_hosts"); len(got) != 1 || !strings.HasPrefix(got[0].Old, `"nas": `) || got[0].Label != "Local name server" {
		t.Errorf("host renamed: %+v", got)
	}
	if got := line(with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h} }), base, "dns_hosts"); len(got) != 1 || got[0].New != "(removed)" {
		t.Errorf("host removed: %+v", got)
	}
	h4 := h
	h4.Disabled = true
	if got := line(with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h} }), with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h4} }), "dns_hosts"); len(got) != 1 || !strings.HasSuffix(got[0].New, ", switched off") {
		t.Errorf("host off: %+v", got)
	}
	if got := DiffNetwork(with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h} }), with(func(c *NetworkConfig) { c.DNS.Hosts = []DNSHost{h} })); len(got) != 0 {
		t.Errorf("unchanged: %+v", got)
	}

	f := DNSForward{ID: "f1", Domain: "corp.example.com", Servers: []string{"10.0.0.5"}}
	if got := line(base, with(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{f} }), "dns_forwards"); len(got) != 1 || got[0].Label != "Domain forwarding corp.example.com" || got[0].New != "corp.example.com is asked of 10.0.0.5" {
		t.Errorf("forward added: %+v", got)
	}
	f2 := f
	f2.Servers = []string{"10.0.0.5", "10.0.0.6"}
	if got := line(with(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{f} }), with(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{f2} }), "dns_forwards"); len(got) != 1 || got[0].New != "corp.example.com is asked of 10.0.0.5, 10.0.0.6" {
		t.Errorf("forward servers changed: %+v", got)
	}
	f3 := f
	f3.Domain = "corp2.example.com"
	if got := line(with(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{f} }), with(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{f3} }), "dns_forwards"); len(got) != 1 {
		t.Errorf("forward domain changed: %+v", got)
	}
	f4 := f
	f4.Disabled = true
	if got := line(with(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{f} }), with(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{f4} }), "dns_forwards"); len(got) != 1 || !strings.HasSuffix(got[0].New, ", switched off") {
		t.Errorf("forward off: %+v", got)
	}
	if got := line(with(func(c *NetworkConfig) { c.DNS.Forwards = []DNSForward{f} }), base, "dns_forwards"); len(got) != 1 || got[0].New != "(removed)" {
		t.Errorf("forward removed: %+v", got)
	}
}

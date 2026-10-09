package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func withRoutes(c NetworkConfig, routes ...Route) NetworkConfig { c.Routes = routes; return c }

func goodRoute() Route {
	return Route{Name: "Lab VPN", Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: 100}
}

func TestValidRoutesPass(t *testing.T) {
	cases := map[string]NetworkConfig{
		"one route via the LAN": withRoutes(validNet(), goodRoute()),
		"no routes":             validNet(),
		"no name, no metric":    withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.50"}),
		"host route":            withRoutes(validNet(), Route{Destination: "172.16.5.9/32", Gateway: "192.168.1.50"}),
		"turned off":            withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Disabled: true}),
		"same network, two metrics": withRoutes(validNet(),
			Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: 10},
			Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.51", Metric: 20}),
		"a route covering the LAN (the connected route is more specific)": withRoutes(validNet(), Route{Destination: "192.168.0.0/16", Gateway: "192.168.1.50"}),
		"gateway inside the destination":                                  withRoutes(validNet(), Route{Destination: "192.168.0.0/16", Gateway: "192.168.1.50"}),
		"via the static WAN":                                              withRoutes(staticWan(validNet()), Route{Destination: "198.51.100.0/24", Gateway: "203.0.113.77"}),
		"via an unknown gateway on a DHCP uplink":                         withRoutes(validNet(), Route{Destination: "198.51.100.0/24", Gateway: "100.64.0.1"}),
		"empty mode counts as DHCP":                                       withRoutes(func() NetworkConfig { c := validNet(); c.WanMode = ""; return c }(), Route{Destination: "198.51.100.0/24", Gateway: "100.64.0.1"}),
		"name punctuation":                                                withRoutes(validNet(), Route{Name: "Site A (VPN) / 2nd, ok+1", Destination: "10.20.0.0/16", Gateway: "192.168.1.50"}),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInvalidRoutesAreRejectedOnTheRightField(t *testing.T) {
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"empty destination", withRoutes(validNet(), Route{Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"destination without a prefix", withRoutes(validNet(), Route{Destination: "10.20.0.0", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"destination as a netmask", withRoutes(validNet(), Route{Destination: "10.20.0.0/255.255.0.0", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"IPv6 destination", withRoutes(validNet(), Route{Destination: "2001:db8::/32", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"destination with a zone", withRoutes(validNet(), Route{Destination: "fe80::1%eth0/64", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"destination with host bits", withRoutes(validNet(), Route{Destination: "10.20.0.7/16", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"the default route", withRoutes(validNet(), Route{Destination: "0.0.0.0/0", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"destination is the LAN", withRoutes(validNet(), Route{Destination: "192.168.1.0/24", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"destination inside the LAN", withRoutes(validNet(), Route{Destination: "192.168.1.128/25", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"destination is the static WAN", withRoutes(staticWan(validNet()), Route{Destination: "203.0.113.0/24", Gateway: "203.0.113.77"}), "routes[0].destination"},
		{"destination with injection", withRoutes(validNet(), Route{Destination: "10.20.0.0/16\nip route flush all", Gateway: "192.168.1.50"}), "routes[0].destination"},
		{"empty gateway", withRoutes(validNet(), Route{Destination: "10.20.0.0/16"}), "routes[0].gateway"},
		{"gateway not an address", withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "router"}), "routes[0].gateway"},
		{"gateway with CIDR", withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.50/24"}), "routes[0].gateway"},
		{"gateway with injection", withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.50 dev eth9"}), "routes[0].gateway"},
		{"gateway is the router's LAN address", withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.1"}), "routes[0].gateway"},
		{"gateway is the router's WAN address", withRoutes(staticWan(validNet()), Route{Destination: "198.51.100.0/24", Gateway: "203.0.113.10"}), "routes[0].gateway"},
		{"gateway is the LAN broadcast address", withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.255"}), "routes[0].gateway"},
		{"gateway is the LAN network address", withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.0"}), "routes[0].gateway"},
		{"gateway not reachable (static WAN)", withRoutes(staticWan(validNet()), Route{Destination: "10.20.0.0/16", Gateway: "100.64.0.1"}), "routes[0].gateway"},
		{"negative metric", withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: -1}), "routes[0].metric"},
		{"huge metric", withRoutes(validNet(), Route{Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: 100000}), "routes[0].metric"},
		{"name too long", withRoutes(validNet(), Route{Name: strings.Repeat("a", 41), Destination: "10.20.0.0/16", Gateway: "192.168.1.50"}), "routes[0].name"},
		{"name with a newline", withRoutes(validNet(), Route{Name: "a\nb", Destination: "10.20.0.0/16", Gateway: "192.168.1.50"}), "routes[0].name"},
		{"name with markup", withRoutes(validNet(), Route{Name: "<b>x</b>", Destination: "10.20.0.0/16", Gateway: "192.168.1.50"}), "routes[0].name"},
		{"duplicate", withRoutes(validNet(), goodRoute(), goodRoute()), "routes[1].destination"},
		{"the second route is the bad one", withRoutes(validNet(), goodRoute(), Route{Destination: "nope", Gateway: "192.168.1.50"}), "routes[1].destination"},
		{"too many routes", func() NetworkConfig {
			c := validNet()
			for i := 0; i < 65; i++ {
				c.Routes = append(c.Routes, Route{Destination: "10." + itoa(i) + ".0.0/16", Gateway: "192.168.1.50"})
			}
			return c
		}(), "routes"},
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

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestDisabledRoutesAreValidatedToo(t *testing.T) {
	c := withRoutes(validNet(), Route{Destination: "nope", Gateway: "192.168.1.50", Disabled: true})
	if c.Validate() == nil {
		t.Fatal("a turned-off route with a bad destination was accepted")
	}
}

func TestRoutesJSONRoundTrip(t *testing.T) {
	in := withRoutes(validNet(), goodRoute(), Route{Destination: "172.16.0.0/12", Gateway: "192.168.1.51", Disabled: true})
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"routes"`) || strings.Count(string(b), `"disabled":true`) != 1 {
		t.Fatalf("%s", b)
	}
	var out NetworkConfig
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Routes) != 2 || out.Routes[0] != in.Routes[0] || out.Routes[1] != in.Routes[1] {
		t.Fatalf("%+v", out.Routes)
	}
	// without routes the key is not written at all
	if b, _ := json.Marshal(validNet()); strings.Contains(string(b), "routes") {
		t.Fatalf("%s", b)
	}
}

func TestCloneDoesNotShareRoutes(t *testing.T) {
	a := withRoutes(validNet(), goodRoute())
	b := a.clone()
	b.Routes[0].Gateway = "192.168.1.99"
	if a.Routes[0].Gateway != "192.168.1.50" {
		t.Fatal("the copy shares its routes with the original")
	}
}

// ---- diff -----------------------------------------------------------------------------

func routeChanges(a, b []Route) []Change {
	var out []Change
	for _, c := range DiffNetwork(withRoutes(validNet(), a...), withRoutes(validNet(), b...)) {
		if c.Field == "routes" {
			out = append(out, c)
		}
	}
	return out
}

func TestRouteDiff(t *testing.T) {
	r1 := Route{Name: "Lab", Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: 100}
	r2 := Route{Destination: "172.16.0.0/12", Gateway: "192.168.1.51"}

	if got := routeChanges(nil, nil); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	if got := routeChanges([]Route{r1, r2}, []Route{r2, r1}); len(got) != 0 {
		t.Fatalf("reordering is not a change: %+v", got)
	}

	added := routeChanges(nil, []Route{r1})
	if len(added) != 1 || added[0].Old != "(none)" || added[0].Label != "Static route to 10.20.0.0/16 (metric 100)" ||
		added[0].New != "via 192.168.1.50, metric 100 [Lab]" {
		t.Fatalf("%+v", added)
	}

	removed := routeChanges([]Route{r1}, nil)
	if len(removed) != 1 || removed[0].New != "(removed)" || removed[0].Old != "via 192.168.1.50, metric 100 [Lab]" {
		t.Fatalf("%+v", removed)
	}

	moved := r1
	moved.Gateway = "192.168.1.60"
	changed := routeChanges([]Route{r1}, []Route{moved})
	if len(changed) != 1 || !strings.Contains(changed[0].Old, "192.168.1.50") || !strings.Contains(changed[0].New, "192.168.1.60") {
		t.Fatalf("%+v", changed)
	}

	off := r1
	off.Disabled = true
	if got := routeChanges([]Route{r1}, []Route{off}); len(got) != 1 || !strings.Contains(got[0].New, "turned off") {
		t.Fatalf("%+v", got)
	}

	// a different metric is a different route: one removed, one added
	other := r1
	other.Metric = 200
	if got := routeChanges([]Route{r1}, []Route{other}); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestRouteChangesComeAfterTheSettingsAndSurviveTheManager(t *testing.T) {
	a := validNet()
	b := withRoutes(validNet(), goodRoute())
	b.LanIP = "192.168.1.2"
	changes := DiffNetwork(a, b)
	if len(changes) != 2 || changes[0].Field != "lan_ip" || changes[1].Field != "routes" {
		t.Fatalf("%+v", changes)
	}
}

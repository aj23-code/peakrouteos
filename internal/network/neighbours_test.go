package network

import (
	"context"
	"reflect"
	"testing"

	"peakroute/internal/config"
)

func staticCfg() config.NetworkConfig {
	return config.NetworkConfig{
		LanIP: "192.168.1.1", LanSubnet: "255.255.255.0",
		Networks: []config.Network{{ID: "net2", Name: "IoT", IP: "10.20.0.1", Netmask: "255.255.255.0"}},
	}
}

func TestTheNeighbourTableGivesIPv4DevicesThatAnswered(t *testing.T) {
	out := `[{"dst":"192.168.1.50","dev":"br0","lladdr":"F8:94:C2:A4:13:A7","state":["REACHABLE"]},
	 {"dst":"fe80::1","dev":"br0","lladdr":"f8:94:c2:a4:13:a7","state":["STALE"]},
	 {"dst":"192.168.1.60","dev":"br0","state":["FAILED"]},
	 {"dst":"192.168.1.61","dev":"br0","lladdr":"aa:bb:cc:dd:ee:ff","state":["INCOMPLETE"]},
	 {"dst":"192.168.1.62","dev":"br0","lladdr":"48:b0:2d:f1:fe:ca","state":["STALE"]}]`
	got := ReadNeighbours(t.Context(), &fakeRunner{neigh: out})
	want := []Neighbour{{"192.168.1.50", "f8:94:c2:a4:13:a7", "br0"}, {"192.168.1.62", "48:b0:2d:f1:fe:ca", "br0"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%+v, want %+v", got, want)
	}
	if ReadNeighbours(context.Background(), nil) != nil || ReadNeighbours(t.Context(), &fakeRunner{neigh: "nope"}) != nil {
		t.Error("something came out of nothing")
	}
}

func TestDevicesWithAnAddressOfTheirOwnAreListedAsStatic(t *testing.T) {
	leases := []ClientLease{{MACAddress: "f8:94:c2:a4:13:a7", IPAddress: "192.168.1.119"}}
	neighbours := []Neighbour{
		{"192.168.1.119", "f8:94:c2:a4:13:a7", "br0"}, // has a lease
		{"192.168.1.119", "02:00:00:00:00:09", "br0"}, // somebody else on a leased address: the lease wins
		{"192.168.1.30", "f8:94:c2:a4:13:a7", "br0"},  // a leased device's hardware address with another address: listed by its lease
		{"192.168.1.1", "02:00:00:00:00:01", "br0"},   // the router itself
		{"192.168.1.200", "02:aa:bb:cc:dd:01", "br0"}, // static, on its own network
		{"10.20.0.77", "02:aa:bb:cc:dd:02", "br0"},    // an IoT address seen on the default network's bridge
		{"10.20.0.78", "02:aa:bb:cc:dd:03", "br20"},   // an IoT address on the IoT bridge
		{"203.0.113.1", "02:00:00:00:00:02", "eth0"},  // the upstream router: not one of the router's networks
		{"192.168.1.200", "02:aa:bb:cc:dd:01", "br0"}, // the same entry twice
	}
	devNet := func(dev string) string { return map[string]string{"br0": "LAN", "br20": "IoT"}[dev] }
	got := StaticClients(leases, neighbours, staticCfg(), devNet)
	type row struct{ ip, network, seenOn string }
	var rows []row
	for _, c := range got {
		if !c.Static || c.ExpiryText != "Static" {
			t.Errorf("not marked static: %+v", c)
		}
		rows = append(rows, row{c.IPAddress, c.Network, c.SeenOn})
	}
	want := []row{{"10.20.0.77", "IoT", "LAN"}, {"10.20.0.78", "IoT", ""}, {"192.168.1.200", "LAN", ""}}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("%+v, want %+v", rows, want)
	}
}

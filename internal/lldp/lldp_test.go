package lldp

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const oneNeighbour = `{"lldp": {"interface": {"eth1": {"via": "LLDP", "rid": "1", "age": "0 day, 00:04:12",
 "chassis": {"switch-hall": {"id": {"type": "mac", "value": "00:11:22:33:44:55"}, "descr": "UniFi Switch 8 POE, 6.5.59", "mgmt-ip": "192.168.1.2",
   "capability": [{"type": "Bridge", "enabled": true}, {"type": "Router", "enabled": false}]}},
 "port": {"id": {"type": "ifname", "value": "Port 7"}, "descr": "Port 7", "ttl": "120"},
 "vlan": {"vlan-id": "1", "pvid": true, "value": "default"}}}}}`

const severalNeighbours = `{"lldp": {"interface": [
 {"eth2": {"via": "LLDP", "chassis": {"ap-kitchen": {"id": {"type": "mac", "value": "aa:bb:cc:00:00:09"}, "mgmt-ip": ["192.168.1.9", "fe80::1"],
    "capability": [{"type": "Wlan", "enabled": true}]}},
   "port": {"id": {"type": "mac", "value": "aa:bb:cc:00:00:09"}, "descr": "eth0"}}},
 {"eth1": {"via": "CDP", "chassis": {"id": {"type": "ifname", "value": "SW2"}, "descr": "a switch with no name"},
   "port": {"id": {"type": "ifname", "value": "Gi0/1"}, "descr": "uplink to the router"},
   "vlan": [{"vlan-id": "20", "pvid": false}, {"vlan-id": "30", "pvid": true}]}}
]}}`

func TestOneNeighbourIsReadWithItsNamePortAndCapabilities(t *testing.T) {
	ns, err := Parse([]byte(oneNeighbour))
	if err != nil || len(ns) != 1 {
		t.Fatalf("%v %+v", err, ns)
	}
	n := ns[0]
	if n.LocalPort != "eth1" || n.Name != "switch-hall" || n.ChassisID != "00:11:22:33:44:55" || n.PortID != "Port 7" || n.PortDesc != "" ||
		n.Description != "UniFi Switch 8 POE, 6.5.59" || len(n.MgmtIPs) != 1 || n.MgmtIPs[0] != "192.168.1.2" || n.Via != "LLDP" || n.VLAN != "1" ||
		len(n.Capabilities) != 1 || n.Capabilities[0] != "Bridge" || n.Age == "" {
		t.Fatalf("%+v", n)
	}
}

func TestSeveralNeighboursAndTheOddShapesOfLldpdsJSON(t *testing.T) {
	ns, err := Parse([]byte(severalNeighbours))
	if err != nil || len(ns) != 2 {
		t.Fatalf("%v %+v", err, ns)
	}
	// ordered by the router's port
	if ns[0].LocalPort != "eth1" || ns[1].LocalPort != "eth2" {
		t.Fatalf("%+v", ns)
	}
	// a system with no name has no key above its chassis: its chassis id stands in, and a CDP neighbour is said to be one
	sw := ns[0]
	if sw.Name != "" || sw.ChassisID != "SW2" || sw.Via != "CDP" || sw.PortID != "Gi0/1" || sw.PortDesc != "uplink to the router" || sw.VLAN != "30" {
		t.Errorf("%+v", sw)
	}
	// a list of addresses, and a capability given as the lone object
	ap := ns[1]
	if ap.Name != "ap-kitchen" || len(ap.MgmtIPs) != 2 || len(ap.Capabilities) != 1 || ap.Capabilities[0] != "Wlan" || ap.PortID != "aa:bb:cc:00:00:09" || ap.PortDesc != "eth0" {
		t.Errorf("%+v", ap)
	}
}

func TestNothingHeardAndGarbageAreNotNeighbours(t *testing.T) {
	for _, in := range []string{`{}`, `{"lldp": {}}`, `{"lldp": {"interface": []}}`, `{"lldp": null}`} {
		if ns, err := Parse([]byte(in)); err != nil || len(ns) != 0 {
			t.Errorf("%s: %v %+v", in, err, ns)
		}
	}
	for _, in := range []string{``, `nope`, `[1,2]`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%q was accepted", in)
		}
	}
	// strange values do not stop the rest from being read
	ns, err := Parse([]byte(`{"lldp": {"interface": {"eth1": {"chassis": "text", "port": 5, "vlan": "x"}}}}`))
	if err != nil || len(ns) != 1 || ns[0].LocalPort != "eth1" {
		t.Errorf("%v %+v", err, ns)
	}
}

type fakeRun struct {
	out string
	err error
	got []string
}

func (f *fakeRun) Run(ctx context.Context, name string, args ...string) (string, error) {
	f.got = append([]string{name}, args...)
	return f.out, f.err
}

func TestReadAsksLldpcliForTheDetailsInJSON(t *testing.T) {
	f := &fakeRun{out: oneNeighbour}
	ns, err := Read(context.Background(), f)
	if err != nil || len(ns) != 1 || strings.Join(f.got, " ") != "lldpcli -f json show neighbors details" {
		t.Fatalf("%v %+v %v", err, ns, f.got)
	}
	if _, err := Read(context.Background(), &fakeRun{err: errors.New("lldpd is not running")}); err == nil {
		t.Error("an error was lost")
	}
}

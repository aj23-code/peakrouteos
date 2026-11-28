package devices

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"peakroute/internal/config"
	"peakroute/internal/filtering"
	"peakroute/internal/fwlog"
)

func policyConfig(filterLan bool) config.NetworkConfig {
	return config.NetworkConfig{
		LanIP: "192.168.1.1", LanSubnet: "255.255.255.0", LanFilter: filterLan,
		Networks: []config.Network{{ID: "iot", Name: "IoT", IP: "10.0.5.1", Netmask: "255.255.255.0"}},
		Firewall: &config.FirewallConfig{
			Rules: []config.FirewallRule{
				{ID: "fw1", Name: "Whole LAN out", From: "lan", Action: "allow", Protocol: "any", To: "internet"},
				{ID: "fw2", Name: "Block the laptop's SSH", From: "lan", Source: "192.168.1.48/29", Action: "block", Protocol: "tcp", Ports: "22", To: "any", Log: true},
				{ID: "fw3", Name: "Printer from IoT", From: "iot", Action: "allow", Protocol: "tcp", To: "lan", Destination: "192.168.1.50", Ports: "631"},
				{ID: "fw4", Name: "Other device", From: "lan", Source: "192.168.1.51", Action: "block", Protocol: "any", To: "internet"},
				{ID: "fw5", Name: "IoT only", From: "iot", Action: "allow", Protocol: "any", To: "internet"},
				{ID: "fw6", Name: "Into the LAN", From: "wan", Action: "block", Protocol: "any", To: "lan"},
			},
			Forwards: []config.PortForward{
				{ID: "pf1", Name: "Web", Protocol: "tcp", ExternalPorts: "8080", ToIP: "192.168.1.50", InternalPorts: "80"},
				{ID: "pf2", Name: "Elsewhere", Protocol: "tcp", ExternalPorts: "8081", ToIP: "192.168.1.51"},
			},
		},
	}
}

func TestTheFirewallRulesThatApplyToADeviceAreFoundNamedOnesFirst(t *testing.T) {
	rep := FirewallFor(policyConfig(false), "192.168.1.50")
	if rep.Network != "lan" || rep.NetworkName != "LAN" {
		t.Fatalf("%+v", rep)
	}
	var got []string
	for _, r := range rep.Rules {
		got = append(got, r.ID+":"+r.Role+":"+r.Scope)
	}
	// fw2 names a range that holds it; fw3 names it as a destination; fw1 and fw6 are for the whole LAN; fw4 is another device's; fw5 is IoT's
	want := []string{"fw2:source:device", "fw3:destination:device", "fw1:source:network", "fw6:destination:network"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v", got)
		}
	}
	if !rep.Rules[0].Log || rep.Rules[0].Action != "block" || rep.Rules[0].Text == "" {
		t.Errorf("%+v", rep.Rules[0])
	}
	if len(rep.Forwards) != 1 || rep.Forwards[0].ID != "pf1" || rep.Forwards[0].Text == "" {
		t.Errorf("%+v", rep.Forwards)
	}
	// a device on another network, one that is not on any, and a bad address
	if r := FirewallFor(policyConfig(false), "10.0.5.7"); r.Network != "iot" || len(r.Rules) != 2 || r.Rules[0].ID != "fw3" || r.Rules[1].ID != "fw5" {
		t.Errorf("%+v", r)
	}
	if r := FirewallFor(policyConfig(false), "172.16.0.9"); r.Network != "" || len(r.Rules) != 0 {
		t.Errorf("%+v", r)
	}
	if r := FirewallFor(policyConfig(false), "nonsense"); len(r.Rules) != 0 || r.Rules == nil {
		t.Errorf("%+v", r)
	}
}

type fakeLog struct {
	events []fwlog.Event
	err    error
	q      fwlog.Query
}

func (f *fakeLog) Query(q fwlog.Query) ([]fwlog.Event, error) { f.q = q; return f.events, f.err }

func TestTheFirewallLogOfADeviceHoldsOnlyItsOwnPackets(t *testing.T) {
	s, _ := rig(t)
	cfg := policyConfig(false)
	s.Config = func() config.NetworkConfig { return cfg }
	fl := &fakeLog{events: []fwlog.Event{
		{ID: 3, Time: 1000, Last: 2000, Rule: "fw2", Src: "192.168.1.50", Dst: "203.0.113.9", DstPort: 22, Proto: "tcp", Count: 4},
		{ID: 2, Rule: "fw2", Src: "192.168.1.500", Dst: "203.0.113.9"}, // not this device: its address is part of a longer one
		{ID: 1, Rule: "gone", Src: "198.51.100.2", Dst: "192.168.1.50", DstPort: 443, Proto: "tcp", Count: 1},
	}}
	s.FirewallLog = fl
	rec, b := get(t, s.HandleFirewall, "/x", "a8:bb:cc:00:00:01")
	if rec.Code != http.StatusOK || b["log_known"] != true || b["ip"] != "192.168.1.50" {
		t.Fatalf("%d %+v", rec.Code, b)
	}
	if fl.q.Search != "192.168.1.50" || fl.q.Since == 0 {
		t.Errorf("%+v", fl.q)
	}
	log := b["log"].([]any)
	if len(log) != 2 {
		t.Fatalf("%+v", log)
	}
	a, c := log[0].(map[string]any), log[1].(map[string]any)
	if a["direction"] != "from" || a["peer"] != "203.0.113.9" || a["name"] != "Block the laptop's SSH" || a["action"] != "block" || a["count"].(float64) != 4 ||
		c["direction"] != "to" || c["name"] != "gone" || c["action"] != nil {
		t.Errorf("%+v %+v", a, c)
	}
	// a log that cannot be read is said, not an empty list
	fl.err = errors.New("db")
	if _, b = get(t, s.HandleFirewall, "/x", "a8:bb:cc:00:00:01"); b["log_known"] != false {
		t.Errorf("%+v", b)
	}
	if rec, _ = get(t, s.HandleFirewall, "/x", "aa:aa:aa:aa:aa:aa"); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
}

func TestTheDNSSectionSaysWhyThereIsNothingToShow(t *testing.T) {
	s, _ := rig(t)
	asked := 0
	s.DNS = func(ctx context.Context, ip string) (filtering.Report, error) {
		asked++
		return filtering.Report{Seen: 5, Blocked: 1, Queries: []filtering.Query{{Name: "ads.example", Blocked: true}}}, nil
	}
	// filtering off: nothing is asked
	s.Config = func() config.NetworkConfig { return policyConfig(false) }
	_, b := get(t, s.HandleDNS, "/x", "a8:bb:cc:00:00:01")
	if b["filtering"] != false || b["filtered"] != false || b["queries"] != nil || asked != 0 {
		t.Fatalf("%+v", b)
	}
	// filtering on for another network only
	cfg := policyConfig(false)
	cfg.Networks[0].Filter = true
	s.Config = func() config.NetworkConfig { return cfg }
	_, b = get(t, s.HandleDNS, "/x", "a8:bb:cc:00:00:01")
	if b["filtering"] != true || b["filtered"] != false || asked != 0 {
		t.Fatalf("%+v", b)
	}
	// on for the device's network
	s.Config = func() config.NetworkConfig { return policyConfig(true) }
	_, b = get(t, s.HandleDNS, "/x", "a8:bb:cc:00:00:01")
	if b["filtered"] != true || b["network"] != "LAN" || b["seen"].(float64) != 5 || len(b["queries"].([]any)) != 1 || asked != 1 {
		t.Fatalf("%+v", b)
	}
	// AdGuard Home not answering
	s.DNS = func(ctx context.Context, ip string) (filtering.Report, error) {
		return filtering.Report{}, errors.New("refused")
	}
	_, b = get(t, s.HandleDNS, "/x", "a8:bb:cc:00:00:01")
	if b["error"] == nil || b["queries"] != nil {
		t.Fatalf("%+v", b)
	}
}

package netdevices

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var clock = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

type fakeDriver struct {
	mu   sync.Mutex
	recs map[string]Record
	errs map[string]error
	got  []Device
}

func (f *fakeDriver) Poll(ctx context.Context, d Device) (Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, d)
	if err := f.errs[d.ID]; err != nil {
		return Record{}, err
	}
	return f.recs[d.ID], nil
}

func snmpDev(name, addr string) Device {
	return Device{Name: name, Address: addr, Kind: KindSNMP, Enabled: true, SNMP: &SNMPAuth{Version: "2c", Community: "public"}}
}

func rig(t *testing.T) (*Service, *fakeDriver) {
	t.Helper()
	st, err := OpenStore(filepath.Join(t.TempDir(), "network_devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDriver{recs: map[string]Record{}, errs: map[string]error{}}
	now := clock
	return &Service{Store: st, Drivers: map[string]Driver{KindSNMP: f, KindAP: f}, Now: func() time.Time { return now }}, f
}

func TestTheStoreKeepsDevicesInAFileOnlyItsOwnerCanReadAndChecksThem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network_devices.json")
	st, _ := OpenStore(path)
	d, err := st.Add(snmpDev("Hall switch", "192.168.1.2"))
	if err != nil || d.ID != "nd1" {
		t.Fatalf("%v %+v", err, d)
	}
	st.Add(snmpDev("Garage switch", "switch2.lan"))
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("%v %v", err, fi)
	}
	// reopened: the same, and ids carry on
	st2, err := OpenStore(path)
	if err != nil || len(st2.List()) != 2 {
		t.Fatalf("%v", err)
	}
	if d3, _ := st2.Add(snmpDev("Third", "192.168.1.4")); d3.ID != "nd3" {
		t.Errorf("%s", d3.ID)
	}
	if err := st2.Delete("nd2"); err != nil {
		t.Fatal(err)
	}
	if d4, _ := st2.Add(snmpDev("Fourth", "192.168.1.5")); d4.ID != "nd4" {
		t.Errorf("an id was used again: %s", d4.ID)
	}
	if err := st2.Delete("nd99"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%v", err)
	}
	// a file that is not what it should be is an error (it is not written over)
	os.WriteFile(path, []byte("{broken"), 0600)
	if _, err := OpenStore(path); err == nil {
		t.Error("a broken file was accepted")
	}
}

func TestADeviceIsCheckedBeforeItIsStored(t *testing.T) {
	st, _ := OpenStore("")
	for name, mod := range map[string]func(*Device){
		"name":           func(d *Device) { d.Name = "" },
		"name ":          func(d *Device) { d.Name = "bad;name" },
		"address":        func(d *Device) { d.Address = "" },
		"address ":       func(d *Device) { d.Address = "127.0.0.1" },
		"address  ":      func(d *Device) { d.Address = "not a host!" },
		"address   ":     func(d *Device) { d.Address = "::1" },
		"kind":           func(d *Device) { d.Kind = "telnet" },
		"snmp":           func(d *Device) { d.SNMP = nil },
		"snmp.version":   func(d *Device) { d.SNMP.Version = "1" },
		"snmp.community": func(d *Device) { d.SNMP.Community = "has space" },
		"snmp.port":      func(d *Device) { d.SNMP.Port = 70000 },
		"snmp.user":      func(d *Device) { d.SNMP = &SNMPAuth{Version: "3"} },
		"snmp.auth_pass": func(d *Device) { d.SNMP = &SNMPAuth{Version: "3", User: "u", AuthProto: "SHA", AuthPass: "short"} },
		"snmp.priv_proto": func(d *Device) {
			d.SNMP = &SNMPAuth{Version: "3", User: "u", PrivProto: "AES", PrivPass: "longenough1"}
		},
		"snmp.priv_pass": func(d *Device) {
			d.SNMP = &SNMPAuth{Version: "3", User: "u", AuthProto: "SHA", AuthPass: "longenough1", PrivProto: "AES", PrivPass: "x"}
		},
	} {
		d := snmpDev("A switch", "192.168.1.2")
		mod(&d)
		_, err := st.Add(d)
		var errs Errors
		if !errors.As(err, &errs) {
			t.Errorf("%s: accepted: %v", name, err)
			continue
		}
		found := false
		for _, e := range errs {
			found = found || e.Field == strings.TrimSpace(name)
		}
		if !found {
			t.Errorf("%s: %v", name, errs)
		}
	}
	// what is right is accepted: v3 with everything, and a host name
	good := Device{Name: "Core (v3)", Address: "core.lan", Kind: KindSNMP, Enabled: true, SNMP: &SNMPAuth{Version: "3", User: "monitor", AuthProto: "SHA256", AuthPass: "authpass123", PrivProto: "AES", PrivPass: "privpass123"}}
	if _, err := st.Add(good); err != nil {
		t.Fatal(err)
	}
	// the same name or the same device twice
	if _, err := st.Add(snmpDev("core (v3)", "10.0.0.1")); err == nil {
		t.Error("the same name twice")
	}
	if _, err := st.Add(Device{Name: "Other", Address: "core.lan", Kind: KindSNMP, Enabled: true, SNMP: &SNMPAuth{Version: "2c", Community: "x"}}); err == nil {
		t.Error("the same device twice")
	}
	for i := 0; i < MaxDevices; i++ {
		st.Add(snmpDev("d"+strings.Repeat("x", i%10)+string(rune('a'+i%26))+string(rune('a'+i/26)), "10.9.0."+string(rune('0'+i%10))+string(rune('0'+i/10))))
	}
	if len(st.List()) > MaxDevices {
		t.Errorf("%d devices", len(st.List()))
	}
}

func TestPollingKeepsTheLastGoodRecordWhenADeviceStopsAnswering(t *testing.T) {
	s, f := rig(t)
	d, _ := s.Store.Add(snmpDev("Hall", "192.168.1.2"))
	f.recs[d.ID] = Record{Name: "hall", Ports: []Port{{Index: 1, Name: "Port 1", Up: true}}, Clients: []Client{{MAC: "a8:bb:cc:00:00:01", Port: "Port 1"}}}
	st := s.Poll(context.Background(), d)
	if !st.Reachable || st.Record == nil || st.Record.Name != "hall" || st.Since.IsZero() || st.LastOK.IsZero() {
		t.Fatalf("%+v", st)
	}
	f.errs[d.ID] = errors.New("timeout")
	st = s.Poll(context.Background(), d)
	if st.Reachable || st.Error != "timeout" || st.Record == nil || st.Record.Name != "hall" {
		t.Fatalf("a device that stops answering lost its record: %+v", st)
	}
	// a device taken out is forgotten at the next round; a disabled one is not polled
	s.Store.Delete(d.ID)
	off := snmpDev("Off", "192.168.1.3")
	off.Enabled = false
	od, _ := s.Store.Add(off)
	f.got = nil
	s.PollAll(context.Background())
	if len(f.got) != 0 || s.StatusOf(d.ID).Record != nil {
		t.Errorf("%+v %+v", f.got, s.StatusOf(d.ID))
	}
	_ = od
	// a kind with no reader says so
	s.Drivers = map[string]Driver{}
	e, _ := s.Store.Add(snmpDev("NoReader", "192.168.1.9"))
	if st := s.Poll(context.Background(), e); st.Reachable || !strings.Contains(st.Error, "no reader") {
		t.Errorf("%+v", st)
	}
}

func TestAHardwareAddressIsPlacedOnTheEndOfTheLine(t *testing.T) {
	s, f := rig(t)
	core, _ := s.Store.Add(snmpDev("Core", "192.168.1.2"))
	edge, _ := s.Store.Add(snmpDev("Edge", "192.168.1.3"))
	mac := "a8:bb:cc:00:00:01"
	// the core has learned the device on the port that leads to the edge switch (with many addresses); the edge on its own port 4 (with one)
	f.recs[core.ID] = Record{Ports: []Port{{Name: "Port 8", Up: true, MACs: 12, Neighbour: &Neighbour{Name: "Edge", Capabilities: []string{"Bridge"}}}}, Clients: []Client{{MAC: mac, Port: "Port 8"}}}
	f.recs[edge.ID] = Record{Ports: []Port{{Name: "Port 4", Alias: "desk", Up: true, SpeedMbps: 1000, VLAN: 20, MACs: 1}}, Clients: []Client{{MAC: mac, Port: "Port 4"}}}
	s.PollAll(context.Background())
	loc := s.Locate("A8:BB:CC:00:00:01")
	if loc == nil || loc.DeviceName != "Edge" || loc.Port != "Port 4" || loc.Alias != "desk" || loc.SpeedMbps != 1000 || loc.VLAN != 20 || loc.Uplink || loc.Stale {
		t.Fatalf("%+v", loc)
	}
	// with no LLDP the port that has learned fewer addresses is the likelier edge
	r := f.recs[core.ID]
	r.Ports[0].Neighbour = nil
	f.recs[core.ID] = r
	s.PollAll(context.Background())
	if loc = s.Locate(mac); loc.DeviceName != "Edge" {
		t.Errorf("%+v", loc)
	}
	// only behind a switch we cannot see into: the port that leads there, said to be an uplink
	delete(f.recs, edge.ID)
	f.errs[edge.ID] = errors.New("down")
	s.Store.Delete(edge.ID)
	r.Ports[0].Neighbour = &Neighbour{Name: "Edge", Capabilities: []string{"Bridge"}}
	f.recs[core.ID] = r
	s.PollAll(context.Background())
	if loc = s.Locate(mac); loc == nil || loc.DeviceName != "Core" || !loc.Uplink || loc.Neighbour == nil || loc.Neighbour.Name != "Edge" {
		t.Errorf("%+v", loc)
	}
	// an unknown address is nowhere, and a device that is not answering now is the last choice and said to be stale
	if s.Locate("a8:bb:cc:99:99:99") != nil {
		t.Error("an unknown address was placed")
	}
	f.errs[core.ID] = errors.New("down")
	s.PollAll(context.Background())
	if loc = s.Locate(mac); loc == nil || !loc.Stale {
		t.Errorf("%+v", loc)
	}
}

func TestAnAccessPointThatHasADeviceAssociatedIsItsEdgeAndShowsItsWifi(t *testing.T) {
	s, f := rig(t)
	sw, _ := s.Store.Add(snmpDev("Switch", "192.168.1.2"))
	ap := snmpDev("AP hall", "192.168.1.9")
	ap.Kind = KindAP
	ap.SNMP, ap.AP = nil, &APAuth{Token: strings.Repeat("t", 43)}
	apd, _ := s.Store.Add(ap)
	mac := "a8:bb:cc:00:00:01"
	sig := -61
	f.recs[sw.ID] = Record{Ports: []Port{{Name: "Port 7", Up: true, MACs: 4, Neighbour: &Neighbour{Name: "AP hall", Capabilities: []string{"WLAN AP"}}}}, Clients: []Client{{MAC: mac, Port: "Port 7"}}}
	f.recs[apd.ID] = Record{Ports: []Port{{Name: "wlan1", Up: true, MACs: 3}}, Clients: []Client{{MAC: mac, Port: "wlan1", SSID: "Home", Band: "5 GHz", Signal: &sig}}}
	s.PollAll(context.Background())
	loc := s.Locate(mac)
	if loc == nil || loc.DeviceName != "AP hall" || loc.Client.SSID != "Home" || loc.Client.Signal == nil || *loc.Client.Signal != -61 || loc.Kind != KindAP {
		t.Fatalf("%+v", loc)
	}
}

// ---- HTTP

func call(t *testing.T, s *Service, h http.HandlerFunc, method, body, id string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func TestTheAPIAddsChangesAndRemovesDevicesAndNeverSendsASecret(t *testing.T) {
	s, f := rig(t)
	f.recs["nd1"] = Record{Name: "hall", Description: "a switch", UptimeSec: 60, Ports: []Port{{Name: "Port 1", Up: true}}, Clients: []Client{{MAC: "a8:bb:cc:00:00:01", Port: "Port 1"}}}
	rec, v := call(t, s, s.HandleCollection, http.MethodPost, `{"name":"Hall switch","address":"192.168.1.2","kind":"snmp","snmp":{"version":"2c","community":"s3cretcommunity"}}`, "")
	if rec.Code != http.StatusCreated || v["id"] != "nd1" || v["enabled"] != true {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cretcommunity") {
		t.Fatal("the community was sent back")
	}
	if sv := v["snmp"].(map[string]any); sv["community_set"] != true || sv["version"] != "2c" {
		t.Fatalf("%+v", sv)
	}
	// it is read in the background: wait for the answer
	for i := 0; i < 100 && !s.StatusOf("nd1").Reachable; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	rec, v = call(t, s, s.HandleItem, http.MethodGet, "", "nd1")
	if rec.Code != http.StatusOK || v["reachable"] != true || v["system_name"] != "hall" || v["clients"].(float64) != 1 || len(v["ports"].([]any)) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// changing it without the secret keeps the secret; with one, replaces it
	rec, _ = call(t, s, s.HandleItem, http.MethodPut, `{"name":"Hall switch (1st floor)","address":"192.168.1.2","snmp":{"version":"2c"}}`, "nd1")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if d, _ := s.Store.Get("nd1"); d.Name != "Hall switch (1st floor)" || d.SNMP.Community != "s3cretcommunity" {
		t.Fatalf("%+v", d.SNMP)
	}
	call(t, s, s.HandleItem, http.MethodPut, `{"name":"Hall switch (1st floor)","address":"192.168.1.2","snmp":{"version":"2c","community":"newcommunity"}}`, "nd1")
	if d, _ := s.Store.Get("nd1"); d.SNMP.Community != "newcommunity" {
		t.Fatalf("%+v", d.SNMP)
	}
	// switching to v3 drops the community
	call(t, s, s.HandleItem, http.MethodPut, `{"name":"Hall switch (1st floor)","address":"192.168.1.2","snmp":{"version":"3","user":"mon","auth_proto":"SHA","auth_pass":"authpass123"}}`, "nd1")
	if d, _ := s.Store.Get("nd1"); d.SNMP.Community != "" || d.SNMP.User != "mon" || d.SNMP.AuthPass != "authpass123" || d.SNMP.PrivPass != "" {
		t.Fatalf("%+v", d.SNMP)
	}
	// mistakes are reported on their fields and change nothing
	rec, v = call(t, s, s.HandleItem, http.MethodPut, `{"name":"x","address":"127.0.0.1","snmp":{"version":"2c","community":"c"}}`, "nd1")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"address"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if d, _ := s.Store.Get("nd1"); d.Address != "192.168.1.2" {
		t.Error("a refused change was stored")
	}
	if rec, _ = call(t, s, s.HandleCollection, http.MethodPost, `{"name":"x","address":"1.2.3.4","kind":"snmp","snmp":{"version":"2c","community":"c"},"extra":1}`, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown field was accepted: %d", rec.Code)
	}
	// the list, a poll now, and removal
	if _, v = call(t, s, s.HandleCollection, http.MethodGet, "", ""); len(v["devices"].([]any)) != 1 || v["max"].(float64) != MaxDevices {
		t.Fatalf("%+v", v)
	}
	if rec, v = call(t, s, s.HandlePoll, http.MethodPost, "", "nd1"); rec.Code != http.StatusOK || v["polled"] != true {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec, _ = call(t, s, s.HandleItem, http.MethodDelete, "", "nd1"); rec.Code != http.StatusNoContent {
		t.Fatalf("%d", rec.Code)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if rec, _ = call(t, s, s.HandleItem, m, "", "nd1"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", m, rec.Code)
		}
	}
	if rec, _ = call(t, s, s.HandlePoll, http.MethodGet, "", "nd1"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("%d", rec.Code)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

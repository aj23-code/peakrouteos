package netdevices

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// a device as a table of OIDs
type table map[string]snmpValue

type fakeSession struct {
	t      table
	noGet  bool
	closed bool
}

func (f *fakeSession) Get(oids ...string) (map[string]snmpValue, error) {
	if f.noGet {
		return nil, errors.New("timeout")
	}
	out := map[string]snmpValue{}
	for _, o := range oids {
		if v, ok := f.t[o]; ok {
			out[o] = v
		}
	}
	return out, nil
}

func (f *fakeSession) Walk(root string, fn func(oid string, v snmpValue)) error {
	var keys []string
	for k := range f.t {
		if strings.HasPrefix(k, root+".") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fn(k, f.t[k])
	}
	return nil
}
func (f *fakeSession) Close() error { f.closed = true; return nil }

func num(n uint64) snmpValue  { return snmpValue{Num: n, Int: int64(n)} }
func txt(s string) snmpValue  { return snmpValue{Bytes: []byte(s), Text: true} }
func raw(b ...byte) snmpValue { return snmpValue{Bytes: b, Text: true} }

// eightPortSwitch: ports 1-6 are cables, port 7 has an access point on it, port 8 leads to another switch, interface 9 is the CPU.
func eightPortSwitch() table {
	t := table{
		oidSysName: txt("switch-hall"), oidSysDescr: txt("UniFi Switch 8 POE"), oidSysUpTime: num(8640000), // 1 day in hundredths of a second
	}
	for i := 1; i <= 9; i++ {
		idx := strconv.Itoa(i)
		typ := uint64(6)
		if i == 9 {
			typ = 53 // propVirtual: not a port
		}
		t[oidIfType+"."+idx] = num(typ)
		t[oidIfName+"."+idx] = txt("Port " + idx)
		t[oidIfDescr+"."+idx] = txt("eth" + idx)
		t[oidIfOperStatus+"."+idx] = num(1)
		t[oidIfHighSpeed+"."+idx] = num(1000)
		t[oidIfHCIn+"."+idx] = num(uint64(1000 * i))
		t[oidIfHCOut+"."+idx] = num(uint64(2000 * i))
		t[oidBasePortIfIndex+"."+idx] = num(uint64(i))
	}
	t[oidIfOperStatus+".3"] = num(2) // port 3 is down
	t[oidIfHighSpeed+".2"] = num(100)
	t[oidIfAlias+".2"] = txt("printer")
	t[oidIfAlias+".1"] = txt("Port 1") // an alias that only repeats the name
	t[oidIfInErrors+".4"], t[oidIfOutErrors+".4"] = num(3), num(4)
	// 802.1Q forwarding table: vlan.mac -> bridge port
	t[oidQTpFdbPort+".1.168.187.204.0.0.1"] = num(1) // a8:bb:cc:00:00:01 on port 1, VLAN 1
	t[oidQTpFdbPort+".20.168.187.204.0.0.2"] = num(2)
	t[oidQTpFdbPort+".1.168.187.204.0.0.3"] = num(8) // three devices behind the other switch
	t[oidQTpFdbPort+".1.168.187.204.0.0.4"] = num(8)
	t[oidQTpFdbPort+".1.168.187.204.0.0.5"] = num(8)
	t[oidQTpFdbPort+".1.168.187.204.0.0.6"] = num(7) // a Wi-Fi client behind the access point
	t[oidQTpFdbPort+".1.168.187.204.0.0.1"] = num(1) // seen on the same port again: counted once
	t[oidQTpFdbPort+".1.1.2.3.4.5"] = num(99)        // a port that is no port here
	t[oidQPvid+".2"] = num(20)
	// LLDP: the access point on port 7 and a switch on port 8 (the port number is the interface number here)
	for _, c := range []struct {
		local, name, port, desc string
		caps                    byte
	}{{"7", "ap-kitchen", "eth0", "PeakRoute AP", 0x10}, {"8", "switch-2", "Port 3", "downstairs", 0x20}} {
		k := "0." + c.local + ".1"
		t[oidLldpRemChassisType+"."+k] = num(4)
		t[oidLldpRemChassisID+"."+k] = raw(0xaa, 0xbb, 0xcc, 0, 0, 0x09)
		t[oidLldpRemPortType+"."+k] = num(5)
		t[oidLldpRemPortID+"."+k] = txt(c.port)
		t[oidLldpRemPortDesc+"."+k] = txt(c.desc)
		t[oidLldpRemSysName+"."+k] = txt(c.name)
		t[oidLldpRemSysDesc+"."+k] = txt("something")
		t[oidLldpRemSysCap+"."+k] = raw(c.caps)
	}
	return t
}

func poll(t *testing.T, tb table) (Record, error) {
	t.Helper()
	s := &SNMPDriver{Dial: func(context.Context, Device) (snmpSession, error) { return &fakeSession{t: tb}, nil },
		Now: func() time.Time { return time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC) }}
	return s.Poll(context.Background(), Device{ID: "nd1", Address: "192.168.1.2", Kind: KindSNMP})
}

func TestASwitchIsReadIntoPortsAddressesAndNeighbours(t *testing.T) {
	rec, err := poll(t, eightPortSwitch())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name != "switch-hall" || rec.Description != "UniFi Switch 8 POE" || rec.UptimeSec != 86400 || rec.PolledAt.IsZero() {
		t.Fatalf("%+v", rec)
	}
	// the CPU interface is not a port
	if len(rec.Ports) != 8 {
		t.Fatalf("%d ports: %+v", len(rec.Ports), rec.Ports)
	}
	p := func(i int) Port { return rec.Ports[i-1] }
	if p(1).Name != "Port 1" || !p(1).Up || p(1).SpeedMbps != 1000 || p(1).InBytes != 1000 || p(1).OutBytes != 2000 || p(1).Alias != "" || p(1).MACs != 1 {
		t.Errorf("port 1: %+v", p(1))
	}
	if p(2).Alias != "printer" || p(2).SpeedMbps != 100 || p(2).VLAN != 20 || p(2).MACs != 1 {
		t.Errorf("port 2: %+v", p(2))
	}
	if p(3).Up || p(4).Errors != 7 {
		t.Errorf("port 3 %+v port 4 %+v", p(3), p(4))
	}
	if p(8).MACs != 3 || p(7).MACs != 1 {
		t.Errorf("addresses: port 7 %d, port 8 %d", p(7).MACs, p(8).MACs)
	}
	ap, sw := p(7).Neighbour, p(8).Neighbour
	if ap == nil || ap.Name != "ap-kitchen" || ap.PortID != "eth0" || ap.ChassisID != "aa:bb:cc:00:00:09" || ap.PortDesc != "PeakRoute AP" || len(ap.Capabilities) != 1 || ap.Capabilities[0] != "WLAN AP" || !ap.Bridging() {
		t.Errorf("%+v", ap)
	}
	if sw == nil || sw.Name != "switch-2" || sw.Capabilities[0] != "Bridge" || !sw.Bridging() || p(1).Neighbour != nil {
		t.Errorf("%+v", sw)
	}
	// the learned addresses, each on its port (the one seen twice once; the port that is no port left out)
	byMAC := map[string]Client{}
	for _, c := range rec.Clients {
		byMAC[c.MAC] = c
	}
	if len(rec.Clients) != 6 || byMAC["a8:bb:cc:00:00:01"].Port != "Port 1" || byMAC["a8:bb:cc:00:00:02"].VLAN != 20 || byMAC["a8:bb:cc:00:00:06"].Port != "Port 7" {
		t.Errorf("%+v", rec.Clients)
	}
}

func TestADeviceWithOnlyTheOldBridgeTableIsReadFromItsLearnedEntries(t *testing.T) {
	tb := table{oidSysName: txt("old"), oidSysDescr: txt("a smart switch"), oidSysUpTime: num(100)}
	for _, i := range []string{"1", "2"} {
		tb[oidIfType+"."+i], tb[oidIfDescr+"."+i], tb[oidIfOperStatus+"."+i], tb[oidIfSpeed+"."+i] = num(6), txt("gi"+i), num(1), num(1_000_000_000)
		tb[oidBasePortIfIndex+"."+i] = num(uint64(map[string]int{"1": 10, "2": 11}[i])) // the bridge numbers differ from the interface numbers here
	}
	tb[oidIfType+".10"], tb[oidIfDescr+".10"], tb[oidIfOperStatus+".10"] = num(6), txt("gi10"), num(1)
	tb[oidIfType+".11"], tb[oidIfDescr+".11"], tb[oidIfOperStatus+".11"] = num(6), txt("gi11"), num(1)
	delete(tb, oidIfType+".1")
	delete(tb, oidIfType+".2")
	tb[oidTpFdbPort+".168.187.204.0.0.1"], tb[oidTpFdbStatus+".168.187.204.0.0.1"] = num(1), num(3) // learned
	tb[oidTpFdbPort+".168.187.204.0.0.2"], tb[oidTpFdbStatus+".168.187.204.0.0.2"] = num(2), num(4) // the switch's own: not a device
	tb[oidTpFdbPort+".168.187.204.0.0.3"], tb[oidTpFdbStatus+".168.187.204.0.0.3"] = num(2), num(3)
	rec, err := poll(t, tb)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Ports) != 2 || rec.Ports[0].Name != "gi10" || rec.Ports[0].SpeedMbps != 0 || len(rec.Clients) != 2 {
		t.Fatalf("%+v", rec)
	}
	for _, c := range rec.Clients {
		if c.MAC == "a8:bb:cc:00:00:02" {
			t.Errorf("an address that is not learned is listed: %+v", c)
		}
	}
	if rec.Clients[0].Port != "gi10" || rec.Clients[1].Port != "gi11" || rec.Clients[0].VLAN != 0 {
		t.Errorf("%+v", rec.Clients)
	}
}

func TestADeviceThatDoesNotAnswerOrIsNotASwitchIsAnError(t *testing.T) {
	s := &SNMPDriver{Dial: func(context.Context, Device) (snmpSession, error) { return &fakeSession{t: table{}, noGet: true}, nil }}
	if _, err := s.Poll(context.Background(), Device{}); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("%v", err)
	}
	// it answers the system questions but has no interface table
	if _, err := poll(t, table{oidSysName: txt("a printer"), oidSysDescr: txt("laser"), oidSysUpTime: num(1)}); err == nil || !strings.Contains(err.Error(), "no interface table") {
		t.Errorf("%v", err)
	}
	// the dial itself failing, and the session is closed otherwise
	s = &SNMPDriver{Dial: func(context.Context, Device) (snmpSession, error) { return nil, errors.New("no route") }}
	if _, err := s.Poll(context.Background(), Device{}); err == nil {
		t.Error("a failed dial was not an error")
	}
	f := &fakeSession{t: eightPortSwitch()}
	s = &SNMPDriver{Dial: func(context.Context, Device) (snmpSession, error) { return f, nil }}
	s.Poll(context.Background(), Device{})
	if !f.closed {
		t.Error("the session was left open")
	}
}

func TestTheLLDPIdsAndCapabilitiesAreReadTheWayTheyAreWritten(t *testing.T) {
	if got := lldpID([]byte{1, 2, 3, 4, 5, 6}, 4, true); got != "01:02:03:04:05:06" {
		t.Errorf("%q", got)
	}
	if got := lldpID([]byte("Gi1/0/12"), 5, false); got != "Gi1/0/12" {
		t.Errorf("%q", got)
	}
	if got := lldpID([]byte{0x00, 0x01, 0x02}, 7, false); got != "000102" {
		t.Errorf("%q", got)
	}
	if got := lldpCaps([]byte{0x28}); strings.Join(got, ",") != "Bridge,Router" {
		t.Errorf("%v", got)
	}
	if lldpCaps(nil) != nil || len(lldpCaps([]byte{0})) != 0 {
		t.Error("no capabilities")
	}
	var nilN *Neighbour
	if nilN.Bridging() || (&Neighbour{Capabilities: []string{"Station"}}).Bridging() {
		t.Error("a station bridges")
	}
}

package devhistory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

func memStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.Now = func() time.Time { return t0 }
	return s
}

func TestUsageIsKeptByTheMinuteAndAddedUp(t *testing.T) {
	s := memStore(t)
	mac := "AA:BB:CC:00:00:01" // upper case is the same device
	for i := 0; i < 5; i++ {
		if err := s.AddUsage(mac, t0.Add(time.Duration(i)*time.Minute+10*time.Second), 100, 1000); err != nil {
			t.Fatal(err)
		}
	}
	s.AddUsage(mac, t0.Add(10*time.Second), 5, 50) // the same minute again: added
	s.AddUsage("aa:bb:cc:00:00:02", t0, 7, 7)      // another device
	s.AddUsage(mac, t0, 0, 0)                      // nothing: no row

	pts, err := s.Usage("aa:bb:cc:00:00:01", t0, t0.Add(10*time.Minute), time.Minute)
	if err != nil || len(pts) != 5 {
		t.Fatalf("%v %+v", err, pts)
	}
	if pts[0].T != t0.Unix() || pts[0].Up != 105 || pts[0].Down != 1050 || pts[1].Up != 100 {
		t.Errorf("%+v", pts[:2])
	}
	// five buckets of a minute become one of five
	one, _ := s.Usage(mac, t0, t0.Add(10*time.Minute), 5*time.Minute)
	if len(one) != 1 || one[0].Up != 505 || one[0].Down != 5050 {
		t.Errorf("%+v", one)
	}
	// the range is half open and the other device is not in it
	if none, _ := s.Usage(mac, t0.Add(5*time.Minute), t0.Add(time.Hour), time.Minute); len(none) != 0 {
		t.Errorf("%+v", none)
	}
}

func TestOldMinutesAreFoldedIntoFiveMinuteRowsAndVeryOldOnesDropped(t *testing.T) {
	s := memStore(t)
	mac := "aa:bb:cc:00:00:01"
	old := t0.Add(-30 * time.Hour)
	for i := 0; i < 10; i++ { // ten minutes a day and a half ago
		s.AddUsage(mac, old.Add(time.Duration(i)*time.Minute), 10, 100)
	}
	s.AddUsage(mac, t0.Add(-time.Hour), 1, 2)         // an hour ago: stays as it is
	s.AddUsage(mac, t0.Add(-31*24*time.Hour), 99, 99) // past the retention
	s.AddEvent(Event{MAC: mac, At: t0.Add(-31 * 24 * time.Hour).Unix(), Online: true})
	s.AddEvent(Event{MAC: mac, At: t0.Add(-time.Hour).Unix(), Online: true})
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	var rows int
	s.db.QueryRow(`SELECT COUNT(*) FROM usage_min`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("%d one-minute rows are left", rows)
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM usage_5m`).Scan(&rows)
	if rows < 2 || rows > 3 {
		t.Fatalf("%d five-minute rows", rows)
	}
	// nothing is lost by the folding, and what is past the retention is gone
	pts, _ := s.Usage(mac, t0.Add(-40*24*time.Hour), t0, time.Hour)
	var up, down uint64
	for _, p := range pts {
		up += p.Up
		down += p.Down
	}
	if up != 101 || down != 1002 {
		t.Errorf("up %d down %d: %+v", up, down, pts)
	}
	if ev, _ := s.Events(mac, t0.Add(-40*24*time.Hour), 0); len(ev) != 1 {
		t.Errorf("%+v", ev)
	}
	// folding twice adds nothing more
	s.Compact()
	pts2, _ := s.Usage(mac, t0.Add(-40*24*time.Hour), t0, time.Hour)
	var up2 uint64
	for _, p := range pts2 {
		up2 += p.Up
	}
	if up2 != 101 {
		t.Errorf("a second compaction changed the totals: %d", up2)
	}
}

func TestEventsComeNewestFirstAndLastEventsFindsTheLatestOfEachDevice(t *testing.T) {
	s := memStore(t)
	for i, on := range []bool{true, false, true} {
		s.AddEvent(Event{MAC: "aa:00:00:00:00:01", At: t0.Add(time.Duration(i) * time.Hour).Unix(), Online: on, Connection: "wifi", SSID: "Home"})
	}
	s.AddEvent(Event{MAC: "aa:00:00:00:00:02", At: t0.Unix(), Online: false})
	ev, err := s.Events("aa:00:00:00:00:01", t0, 0)
	if err != nil || len(ev) != 3 || !ev[0].Online || ev[1].Online || ev[0].At <= ev[1].At || ev[0].SSID != "Home" {
		t.Fatalf("%v %+v", err, ev)
	}
	if two, _ := s.Events("aa:00:00:00:00:01", t0, 2); len(two) != 2 {
		t.Errorf("%+v", two)
	}
	last, err := s.LastEvents()
	if err != nil || len(last) != 2 || !last["aa:00:00:00:00:01"].Online || last["aa:00:00:00:00:01"].At != t0.Add(2*time.Hour).Unix() || last["aa:00:00:00:00:02"].Online {
		t.Fatalf("%v %+v", err, last)
	}
	// a removed device leaves nothing
	s.AddUsage("aa:00:00:00:00:01", t0, 1, 1)
	if err := s.Forget("AA:00:00:00:00:01"); err != nil {
		t.Fatal(err)
	}
	if ev, _ := s.Events("aa:00:00:00:00:01", t0, 0); len(ev) != 0 {
		t.Errorf("%+v", ev)
	}
	if pts, _ := s.Usage("aa:00:00:00:00:01", t0, t0.Add(time.Hour), time.Minute); len(pts) != 0 {
		t.Errorf("%+v", pts)
	}
	if last, _ = s.LastEvents(); len(last) != 1 {
		t.Errorf("another device lost its history: %+v", last)
	}
}

func TestTheHistorySurvivesAReopenedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device_history.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.AddUsage("aa:00:00:00:00:01", t0, 5, 6)
	s.AddEvent(Event{MAC: "aa:00:00:00:00:01", At: t0.Unix(), Online: true})
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if pts, _ := s2.Usage("aa:00:00:00:00:01", t0, t0.Add(time.Hour), time.Minute); len(pts) != 1 || pts[0].Down != 6 {
		t.Errorf("%+v", pts)
	}
	if ev, _ := s2.Events("aa:00:00:00:00:01", t0, 0); len(ev) != 1 {
		t.Errorf("%+v", ev)
	}
}

// ---- the sampler -----------------------------------------------------------------------------------------------------------

type rig struct {
	s       *Sampler
	bytes   map[string][2]uint64
	clients []Client
	now     time.Time
}

func newRig(t *testing.T) *rig {
	r := &rig{bytes: map[string][2]uint64{}, now: t0}
	r.s = &Sampler{Store: memStore(t),
		Directions: func() map[string][2]uint64 {
			out := map[string][2]uint64{}
			for k, v := range r.bytes {
				out[k] = v
			}
			return out
		},
		Clients: func(context.Context) []Client { return r.clients },
		Now:     func() time.Time { return r.now }}
	return r
}

func (r *rig) step() {
	r.now = r.now.Add(time.Minute)
	r.s.Sample(context.Background())
}

func TestTheSamplerWritesWhatEachDeviceUsedSinceTheLastLook(t *testing.T) {
	r := newRig(t)
	mac := "aa:00:00:00:00:01"
	r.bytes[mac] = [2]uint64{1000, 9000} // counted before the daemon looked: the starting point, not a minute of traffic
	r.step()
	if pts, _ := r.s.Store.Usage(mac, t0, t0.Add(time.Hour), time.Minute); len(pts) != 0 {
		t.Fatalf("the first look wrote %+v", pts)
	}
	r.bytes[mac] = [2]uint64{1500, 12000}
	r.step()
	r.step() // nothing new
	r.bytes[mac] = [2]uint64{1500, 12500}
	r.bytes["aa:00:00:00:00:02"] = [2]uint64{40, 60} // a device that appears: all of it is new
	r.step()
	pts, _ := r.s.Store.Usage(mac, t0, t0.Add(time.Hour), time.Minute)
	if len(pts) != 2 || pts[0].Up != 500 || pts[0].Down != 3000 || pts[1].Up != 0 || pts[1].Down != 500 {
		t.Fatalf("%+v", pts)
	}
	if pts, _ := r.s.Store.Usage("aa:00:00:00:00:02", t0, t0.Add(time.Hour), time.Minute); len(pts) != 1 || pts[0].Up != 40 {
		t.Fatalf("%+v", pts)
	}
	// counters that went down (the device was forgotten and came back) are a new starting point, not a huge number
	r.bytes[mac] = [2]uint64{10, 10}
	r.step()
	pts, _ = r.s.Store.Usage(mac, t0, t0.Add(time.Hour), time.Minute)
	for _, p := range pts {
		if p.Up > 1000 || p.Down > 10000 {
			t.Fatalf("%+v", pts)
		}
	}
}

func TestTheSamplerRecordsWhenADeviceComesGoesAndMoves(t *testing.T) {
	r := newRig(t)
	mac := "aa:00:00:00:00:01"
	set := func(status, conn, ssid, band, network string) {
		r.clients = []Client{{MAC: mac, IP: "192.168.1.50", Status: status, Connection: conn, SSID: ssid, Band: band, Network: network}}
		r.step()
	}
	set("unknown", "unknown", "", "", "") // nothing is known: nothing is written
	set("online", "wifi", "Home", "5 GHz", "LAN")
	set("online", "wifi", "Home", "5 GHz", "LAN")   // no change
	set("online", "wifi", "Home", "2.4 GHz", "LAN") // moved to another band
	set("online", "wired", "", "", "LAN")           // plugged in
	set("offline", "wired", "", "", "LAN")          // gone
	set("offline", "wired", "", "", "LAN")
	set("online", "wired", "", "", "IoT") // back, on another network
	ev, _ := r.s.Store.Events(mac, t0, 0)
	if len(ev) != 5 {
		t.Fatalf("%d events: %+v", len(ev), ev)
	}
	// newest first
	if !ev[0].Online || ev[0].Network != "IoT" || ev[1].Online || ev[2].Connection != "wired" || ev[3].Band != "2.4 GHz" || ev[4].SSID != "Home" || ev[4].IP != "192.168.1.50" {
		t.Fatalf("%+v", ev)
	}
	// a restart: what was known is read back, so nothing is written twice
	s2 := &Sampler{Store: r.s.Store, Directions: r.s.Directions, Clients: r.s.Clients, Now: r.s.Now}
	s2.Sample(context.Background())
	if ev2, _ := r.s.Store.Events(mac, t0, 0); len(ev2) != 5 {
		t.Fatalf("a restart wrote an event: %d", len(ev2))
	}
}

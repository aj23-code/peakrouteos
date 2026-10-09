package wanhealth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// minute of the test day, as a time
func at(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

// fill records `sent` probes in minute m, `lost` of them lost.
func fill(s *Store, series string, m, sent, lost int) {
	for i := 0; i < sent; i++ {
		s.Record(Probe{At: at(m).Add(time.Duration(i) * 4 * time.Second), Series: series, RTT: 10 * time.Millisecond, OK: i >= lost})
	}
}

// up/down helpers: a full minute of 12 probes
func up(s *Store, from, to int) {
	for m := from; m < to; m++ {
		fill(s, SeriesDNS, m, 12, 0)
	}
}
func down(s *Store, from, to int) {
	for m := from; m < to; m++ {
		fill(s, SeriesDNS, m, 12, 12)
	}
}

func pctNear(t *testing.T, got *float64, want float64, what string) {
	t.Helper()
	near(t, got, want, what)
}

func TestAllUp(t *testing.T) {
	s := memStore(t)
	up(s, 0, 60)
	a, err := s.Availability(at(61))
	if err != nil {
		t.Fatal(err)
	}
	pctNear(t, a.Uptime24h, 100, "24h")
	pctNear(t, a.Uptime30d, 100, "30d")
	if a.OutageCount != 0 || len(a.Outages) != 0 || a.Outages == nil {
		t.Fatalf("%+v", a)
	}
}

func TestNothingObservedMeansNoPercentage(t *testing.T) {
	s := memStore(t)
	a, _ := s.Availability(at(10))
	if a.Uptime24h != nil || a.Uptime30d != nil || a.Outages == nil || len(a.Outages) != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestAnOutageHasStartAndLength(t *testing.T) {
	s := memStore(t)
	up(s, 0, 10)
	down(s, 10, 13) // three minutes
	up(s, 13, 20)
	a, _ := s.Availability(at(30))
	if a.OutageCount != 1 || len(a.Outages) != 1 {
		t.Fatalf("%+v", a)
	}
	o := a.Outages[0]
	if o.Start != at(10).UnixMilli() || o.Seconds != 180 || o.Ongoing {
		t.Fatalf("%+v", o)
	}
	pctNear(t, a.Uptime30d, 17*100.0/20, "uptime")
}

func TestOutagesAreListedNewestFirst(t *testing.T) {
	s := memStore(t)
	up(s, 0, 5)
	down(s, 5, 6)
	up(s, 6, 10)
	down(s, 10, 12)
	up(s, 12, 20)
	a, _ := s.Availability(at(40))
	if a.OutageCount != 2 || a.Outages[0].Start != at(10).UnixMilli() || a.Outages[1].Start != at(5).UnixMilli() {
		t.Fatalf("%+v", a.Outages)
	}
	if a.Outages[0].Seconds != 120 || a.Outages[1].Seconds != 60 {
		t.Fatalf("%+v", a.Outages)
	}
}

func TestAMissingMinuteSplitsAnOutage(t *testing.T) {
	s := memStore(t)
	down(s, 0, 2)
	// minute 2 has no data at all (the daemon was not running), then it is down again
	down(s, 3, 5)
	a, _ := s.Availability(at(60))
	if a.OutageCount != 2 {
		t.Fatalf("a gap in the data joined two outages: %+v", a.Outages)
	}
	pctNear(t, a.Uptime30d, 0, "only observed minutes count")
}

func TestShortMinutesSayNothing(t *testing.T) {
	s := memStore(t)
	up(s, 0, 5)
	fill(s, SeriesDNS, 5, 2, 2) // two lost probes: not enough to call it an outage
	up(s, 6, 10)
	a, _ := s.Availability(at(60))
	if a.OutageCount != 0 {
		t.Fatalf("%+v", a.Outages)
	}
	pctNear(t, a.Uptime30d, 100, "short minute not observed")

	// and a short minute does not continue an outage either
	s2 := memStore(t)
	down(s2, 0, 2)
	fill(s2, SeriesDNS, 2, 1, 1)
	down(s2, 3, 4)
	if a, _ := s2.Availability(at(60)); a.OutageCount != 2 {
		t.Fatalf("%+v", a.Outages)
	}
}

func TestPartialLossIsNotAnOutage(t *testing.T) {
	s := memStore(t)
	up(s, 0, 5)
	fill(s, SeriesDNS, 5, 12, 11) // one probe got through
	fill(s, SeriesDNS, 6, 12, 6)
	up(s, 7, 10)
	a, _ := s.Availability(at(60))
	if a.OutageCount != 0 {
		t.Fatalf("%+v", a.Outages)
	}
	pctNear(t, a.Uptime30d, 100, "uptime counts a minute as up if anything answered")
}

func TestOngoingOutage(t *testing.T) {
	s := memStore(t)
	up(s, 0, 10)
	down(s, 10, 14) // minute 13 is the current one
	a, _ := s.Availability(at(13).Add(30 * time.Second))
	if len(a.Outages) != 1 || !a.Outages[0].Ongoing || a.Outages[0].Seconds != 240 {
		t.Fatalf("%+v", a.Outages)
	}
	// a few minutes later with no new data it is not "now" any more
	a, _ = s.Availability(at(20))
	if a.Outages[0].Ongoing {
		t.Fatal("an outage that ended in the past is shown as ongoing")
	}
}

func TestAnOutageFollowedByUpIsNotOngoing(t *testing.T) {
	s := memStore(t)
	down(s, 10, 12)
	fill(s, SeriesDNS, 12, 4, 0) // back up, minute still in progress
	a, _ := s.Availability(at(12).Add(20 * time.Second))
	if len(a.Outages) != 1 || a.Outages[0].Ongoing {
		t.Fatalf("%+v", a.Outages)
	}
}

func TestOldOutageCountsForThirtyDaysButNotForADay(t *testing.T) {
	s := memStore(t)
	twoDays := 2 * 24 * 60
	down(s, 0, 10)
	up(s, 10, 20)
	// one fresh hour, 2 days later
	for m := twoDays; m < twoDays+60; m++ {
		fill(s, SeriesDNS, m, 12, 0)
	}
	a, _ := s.Availability(at(twoDays + 61))
	pctNear(t, a.Uptime24h, 100, "24h only sees the fresh hour")
	pctNear(t, a.Uptime30d, 70*100.0/80, "30d sees both")
	if a.OutageCount != 1 {
		t.Fatalf("%+v", a)
	}
	// beyond 30 days it is forgotten
	a, _ = s.Availability(at(31 * 24 * 60))
	if a.OutageCount != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestOutageListIsCappedButCounted(t *testing.T) {
	s := memStore(t)
	m := 0
	for i := 0; i < 25; i++ {
		down(s, m, m+1)
		up(s, m+1, m+3)
		m += 3
	}
	a, _ := s.Availability(at(m + 10))
	if a.OutageCount != 25 || len(a.Outages) != maxOutagesListed {
		t.Fatalf("count %d, listed %d", a.OutageCount, len(a.Outages))
	}
	if a.Outages[0].Start != at(72).UnixMilli() { // the newest, not the oldest
		t.Fatalf("newest listed is %d", a.Outages[0].Start)
	}
}

func TestOnlyTheDNSSeriesCounts(t *testing.T) {
	s := memStore(t)
	up(s, 0, 10)
	for m := 0; m < 10; m++ {
		fill(s, SeriesGateway, m, 12, 12) // gateway dead the whole time
	}
	a, _ := s.Availability(at(20))
	if a.OutageCount != 0 {
		t.Fatalf("a gateway that ignores pings made an outage: %+v", a.Outages)
	}
	pctNear(t, a.Uptime30d, 100, "uptime")
}

func TestAvailabilityAcrossDatabaseAndRAM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	up(s, 0, 5)
	down(s, 5, 8)
	s.Close()

	s2 := fileStore(t, path) // restart: the first minutes are only in the database
	defer s2.Close()
	down(s2, 8, 10) // the outage continues in RAM
	up(s2, 10, 15)
	now := at(16)
	before, _ := s2.Availability(now)
	s2.writeDirty()
	after, _ := s2.Availability(now)

	for _, a := range []Availability{before, after} {
		if a.OutageCount != 1 || a.Outages[0].Seconds != 300 || a.Outages[0].Start != at(5).UnixMilli() {
			t.Fatalf("outage spanning the database and RAM: %+v", a.Outages)
		}
		pctNear(t, a.Uptime30d, 10*100.0/15, "uptime counts every minute once")
	}
}

func TestAvailabilityIsCachedBriefly(t *testing.T) {
	p := newProber(t, &fakePinger{results: []error{nil}}, tgt)
	up(p.Store, 0, 5)
	a1, _ := p.availability(at(6))
	pctNear(t, a1.Uptime30d, 100, "first")

	down(p.Store, 5, 10)
	a2, _ := p.availability(at(6).Add(5 * time.Second)) // within the cache time
	pctNear(t, a2.Uptime30d, 100, "cached")
	a3, _ := p.availability(at(6).Add(availabilityTTL + time.Second))
	if a3.Uptime30d == nil || *a3.Uptime30d >= 100 {
		t.Fatalf("not recomputed after the cache time: %v", a3.Uptime30d)
	}
}

func TestHistoryEndpointIncludesAvailability(t *testing.T) {
	p := newProber(t, &fakePinger{results: []error{nil}, rtt: time.Millisecond}, tgt)
	now := time.Now()
	for m := 5; m >= 3; m-- {
		for i := 0; i < 12; i++ {
			p.Store.Record(Probe{At: now.Add(-time.Duration(m) * time.Minute).Add(time.Duration(i) * time.Second), Series: SeriesDNS, OK: false})
		}
	}
	for m := 2; m >= 0; m-- {
		for i := 0; i < 12; i++ {
			p.Store.Record(Probe{At: now.Add(-time.Duration(m) * time.Minute).Add(-time.Duration(i) * time.Second), Series: SeriesDNS, RTT: time.Millisecond, OK: true})
		}
	}
	rec := httptest.NewRecorder()
	p.HandleHistory(rec, httptest.NewRequest(http.MethodGet, "/x?range=1h", nil))
	var body struct {
		Availability struct {
			Uptime24h   *float64 `json:"uptime_24h"`
			OutageCount int      `json:"outage_count"`
			Outages     []struct {
				Start, Seconds int64
				Ongoing        bool
			}
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != 200 {
		t.Fatalf("%d %v %s", rec.Code, err, rec.Body)
	}
	if body.Availability.OutageCount != 1 || len(body.Availability.Outages) != 1 || body.Availability.Outages[0].Seconds < 120 || body.Availability.Uptime24h == nil || *body.Availability.Uptime24h >= 100 {
		t.Fatalf("%s", rec.Body.String())
	}
}

func TestTwentyFourHourUptimeUsesOnlyTheLastDay(t *testing.T) {
	s := memStore(t)
	twoDays := 2 * 24 * 60
	up(s, 0, 100) // old, all up
	up(s, twoDays, twoDays+59)
	down(s, twoDays+59, twoDays+60)
	a, _ := s.Availability(at(twoDays + 61))
	pctNear(t, a.Uptime24h, 59*100.0/60, "24h")
	pctNear(t, a.Uptime30d, 159*100.0/160, "30d")
}

func TestAnotherSeriesInRAMDoesNotHideStoredMinutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	defer s.Close()
	down(s, 5, 8) // an outage that is only in the database now
	s.writeDirty()
	s.expire(at(120))
	fill(s, SeriesGateway, 0, 12, 0) // an older gateway minute still in RAM
	a, _ := s.Availability(at(30))
	if a.OutageCount != 1 || a.Outages[0].Seconds != 180 {
		t.Fatalf("%+v", a.Outages)
	}
}

func TestRAMHasTheLatestWordOnAMinuteThatIsAlsoStored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	defer s.Close()
	fill(s, SeriesDNS, 5, 3, 3) // the first probes of the minute fail...
	s.writeDirty()              // ...and are written
	for i := 0; i < 9; i++ {    // then the link comes back
		s.Record(Probe{At: at(5).Add(time.Duration(30+i) * time.Second), Series: SeriesDNS, RTT: time.Millisecond, OK: true})
	}
	a, _ := s.Availability(at(6))
	if a.OutageCount != 0 {
		t.Fatalf("the stored, older picture of the minute won: %+v", a.Outages)
	}
}

func TestADeadDNSServerIsAnOutageWhateverTheGatewayDoes(t *testing.T) {
	s := memStore(t)
	down(s, 0, 3)
	for m := 0; m < 3; m++ {
		fill(s, SeriesGateway, m, 12, 0)
	}
	if a, _ := s.Availability(at(10)); a.OutageCount != 1 {
		t.Fatalf("%+v", a.Outages)
	}
}

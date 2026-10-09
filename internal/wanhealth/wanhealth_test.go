package wanhealth

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func probe(at time.Time, rtt time.Duration, ok bool) Probe {
	return Probe{At: at, Series: SeriesDNS, RTT: rtt, OK: ok}
}

func memStore(t *testing.T) *Store {
	t.Helper()
	s, err := open("", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func fileStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func near(t *testing.T, got *float64, want float64, what string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: got nil, want %v", what, want)
	}
	if d := *got - want; d > 0.0005 || d < -0.0005 {
		t.Fatalf("%s: got %v, want %v", what, *got, want)
	}
}

func TestMinuteAveragesAndLoss(t *testing.T) {
	s := memStore(t)
	s.Record(probe(t0, 10*time.Millisecond, true))
	s.Record(probe(t0.Add(5*time.Second), 30*time.Millisecond, true))
	s.Record(probe(t0.Add(10*time.Second), 0, false))
	s.Record(probe(t0.Add(15*time.Second), 20*time.Millisecond, true))

	pts, sum, err := s.History(SeriesDNS, Ranges["1h"], t0.Add(time.Minute))
	if err != nil || len(pts) != 1 {
		t.Fatalf("points %v err %v", pts, err)
	}
	p := pts[0]
	if p.Sent != 4 || p.Lost != 1 || p.LossPct != 25 {
		t.Fatalf("counts %+v", p)
	}
	near(t, p.AvgMS, 20, "avg") // lost probes do not drag the average down
	near(t, p.MinMS, 10, "min")
	near(t, p.MaxMS, 30, "max")
	if sum.Sent != 4 || sum.LossPct != 25 {
		t.Fatalf("summary %+v", sum)
	}
}

func TestAllLostGivesNoLatency(t *testing.T) {
	s := memStore(t)
	s.Record(probe(t0, 0, false))
	s.Record(probe(t0.Add(5*time.Second), 0, false))
	pts, sum, _ := s.History(SeriesDNS, Ranges["1h"], t0.Add(time.Minute))
	if len(pts) != 1 || pts[0].AvgMS != nil || pts[0].MinMS != nil || pts[0].LossPct != 100 {
		t.Fatalf("%+v", pts)
	}
	if sum.AvgMS != nil || sum.LossPct != 100 {
		t.Fatalf("%+v", sum)
	}
}

func TestBucketsAreWeightedByReplies(t *testing.T) {
	s := memStore(t)
	// minute 1: one probe at 10 ms. minute 2: three probes at 50 ms. One 15 minute bucket: avg 40 ms, not 30.
	s.Record(probe(t0, 10*time.Millisecond, true))
	for i := 0; i < 3; i++ {
		s.Record(probe(t0.Add(time.Minute+time.Duration(i)*time.Second), 50*time.Millisecond, true))
	}
	pts, _, _ := s.History(SeriesDNS, Ranges["24h"], t0.Add(2*time.Minute))
	if len(pts) != 1 {
		t.Fatalf("want one bucket, got %d", len(pts))
	}
	near(t, pts[0].AvgMS, 40, "weighted avg")
	near(t, pts[0].MinMS, 10, "min")
	near(t, pts[0].MaxMS, 50, "max")
}

func TestWindowExcludesOldMinutes(t *testing.T) {
	s := memStore(t)
	s.Record(probe(t0, time.Millisecond, true))
	s.Record(probe(t0.Add(2*time.Hour), time.Millisecond, true))
	pts, _, _ := s.History(SeriesDNS, Ranges["1h"], t0.Add(2*time.Hour+time.Minute))
	if len(pts) != 1 {
		t.Fatalf("an old probe is shown: %d points", len(pts))
	}
}

func TestRawRangeKeepsEveryProbe(t *testing.T) {
	s := memStore(t)
	for i := 0; i < 5; i++ {
		s.Record(probe(t0.Add(time.Duration(i)*5*time.Second), time.Duration(i+1)*time.Millisecond, true))
	}
	pts, sum, _ := s.History(SeriesDNS, Ranges["5m"], t0.Add(30*time.Second))
	if len(pts) != 5 || sum.Sent != 5 {
		t.Fatalf("points %d, summary %+v", len(pts), sum)
	}
	near(t, pts[2].AvgMS, 3, "third probe")
	// older than five minutes: out of the window
	pts, _, _ = s.History(SeriesDNS, Ranges["5m"], t0.Add(10*time.Minute))
	if len(pts) != 0 {
		t.Fatalf("expired probes shown: %d", len(pts))
	}
}

func TestRingIsBounded(t *testing.T) {
	s := memStore(t)
	for i := 0; i < ringCapacity+50; i++ {
		s.Record(probe(t0.Add(time.Duration(i)*time.Second), time.Millisecond, true))
	}
	if n := len(s.Recent(SeriesDNS, 10000)); n != ringCapacity {
		t.Fatalf("ring holds %d probes", n)
	}
}

func TestPersistedAcrossRestartAndNotCountedTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	for i := 0; i < 3; i++ {
		s.Record(probe(t0.Add(time.Duration(i)*5*time.Second), 10*time.Millisecond, true))
	}
	// A query while the minute is only in RAM sees it once, and still once after it is written
	now := t0.Add(30 * time.Second)
	pts, _, _ := s.History(SeriesDNS, Ranges["1h"], now)
	if len(pts) != 1 || pts[0].Sent != 3 {
		t.Fatalf("before write: %+v", pts)
	}
	s.writeDirty()
	pts, _, _ = s.History(SeriesDNS, Ranges["1h"], now)
	if len(pts) != 1 || pts[0].Sent != 3 {
		t.Fatalf("after write: %+v", pts)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := fileStore(t, path)
	defer s2.Close()
	pts, sum, _ := s2.History(SeriesDNS, Ranges["1h"], now)
	if len(pts) != 1 || pts[0].Sent != 3 || sum.Sent != 3 {
		t.Fatalf("after restart: %+v %+v", pts, sum)
	}
	near(t, pts[0].AvgMS, 10, "avg after restart")

	// New probes after the restart land in a later minute and sit beside the stored one
	s2.Record(probe(t0.Add(70*time.Second), 20*time.Millisecond, true))
	pts, _, _ = s2.History(SeriesDNS, Ranges["1h"], t0.Add(2*time.Minute))
	if len(pts) != 2 || pts[0].Sent != 3 || pts[1].Sent != 1 {
		t.Fatalf("stored + new: %+v", pts)
	}
}

func TestCloseWritesPendingMinute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	s.Record(probe(t0, 5*time.Millisecond, true))
	s.Close() // no flush tick has happened: Close must write it

	s2 := fileStore(t, path)
	defer s2.Close()
	pts, _, _ := s2.History(SeriesDNS, Ranges["1h"], t0.Add(time.Minute))
	if len(pts) != 1 {
		t.Fatalf("pending minute lost on close: %+v", pts)
	}
}

func TestPurgeDropsOldRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	defer s.Close()
	old := t0.AddDate(0, 0, -(retentionDays + 1))
	s.Record(probe(old, time.Millisecond, true))
	s.Record(probe(t0, time.Millisecond, true))
	s.writeDirty()
	s.purge(t0)
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM wan_health`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after purge: %d (%v)", n, err)
	}
}

func TestExpireKeepsUnwrittenMinutes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	defer s.Close()
	s.Record(probe(t0, time.Millisecond, true))
	s.expire(t0.Add(time.Hour)) // long past, but never written: must stay until it is
	if len(s.mins) != 1 {
		t.Fatal("an unwritten minute was dropped")
	}
	s.writeDirty()
	s.expire(t0.Add(time.Hour))
	if len(s.mins) != 0 {
		t.Fatal("a written, old minute was kept")
	}
}

// ---- prober ----------------------------------------------------------------------------

type fakePinger struct {
	mu      sync.Mutex
	results []error // consumed in order; the last repeats
	rtt     time.Duration
	calls   int
	addrs   []netip.Addr
	noICMP  bool // pretend real pings are not permitted
}

func (f *fakePinger) Ping(ctx context.Context, a netip.Addr, d time.Duration) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.addrs = append(f.addrs, a)
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	if err := f.results[i]; err != nil {
		return 0, err
	}
	return f.rtt, nil
}
func (f *fakePinger) Method() string { return "icmp" }
func (f *fakePinger) ICMP() bool     { return !f.noICMP }

func newProber(t *testing.T, pinger Pinger, target func() (Target, bool)) *Prober {
	return &Prober{Store: memStore(t), Pinger: pinger, Target: target, Every: 5 * time.Second, Timeout: time.Second}
}

func tgt() (Target, bool) {
	return Target{Addr: netip.MustParseAddr("1.1.1.1"), Source: "test"}, true
}

func TestProberStates(t *testing.T) {
	fail := errors.New("no reply")
	fp := &fakePinger{results: []error{nil, fail, fail, fail, nil}, rtt: 12 * time.Millisecond}
	p := newProber(t, fp, tgt)
	ctx := context.Background()

	if p.Status().State != StateUnknown {
		t.Fatal("before any probe the state is unknown")
	}
	step := func(want string) {
		t.Helper()
		p.probe(ctx, t0)
		if got := p.Status().State; got != want {
			t.Fatalf("state %q, want %q", got, want)
		}
	}
	step(StateOnline)
	step(StateOnline) // one lost probe is not an outage
	step(StateOnline) // nor two
	step(StateOffline)
	step(StateOnline) // a single reply brings it back
	st := p.Status()
	if st.RTTMS == nil || *st.RTTMS != 12 || st.Target != "1.1.1.1" || st.Method != "icmp" || st.Every != 5 {
		t.Fatalf("status %+v", st)
	}
	if got := p.Store.Recent(SeriesDNS, 10); len(got) != 5 || got[1].OK || !got[4].OK {
		t.Fatalf("recorded %+v", got)
	}
}

func TestProberFailureKeepsLastRTTEmpty(t *testing.T) {
	p := newProber(t, &fakePinger{results: []error{errors.New("x")}}, tgt)
	p.probe(context.Background(), t0)
	if st := p.Status(); st.RTTMS != nil || st.At == 0 {
		t.Fatalf("%+v", st)
	}
}

func TestProberNotConfigured(t *testing.T) {
	fp := &fakePinger{results: []error{nil}}
	p := newProber(t, fp, func() (Target, bool) { return Target{}, false })
	p.probe(context.Background(), t0)
	if st := p.Status(); st.State != StateNotConfigured || st.Problem == "" {
		t.Fatalf("%+v", st)
	}
	if fp.calls != 0 || len(p.Store.Recent(SeriesDNS, 5)) != 0 {
		t.Fatal("nothing should be probed or recorded without a target")
	}
}

func TestProberFollowsTargetChange(t *testing.T) {
	addr := "1.1.1.1"
	fp := &fakePinger{results: []error{nil}, rtt: time.Millisecond}
	p := newProber(t, fp, func() (Target, bool) {
		return Target{Addr: netip.MustParseAddr(addr)}, true
	})
	p.probe(context.Background(), t0)
	addr = "9.9.9.9"
	p.probe(context.Background(), t0.Add(5*time.Second))
	if p.Status().Target != "9.9.9.9" {
		t.Fatal("target change not followed")
	}
	// both measurements belong to one series even though the address changed
	if len(fp.addrs) != 2 || fp.addrs[0].String() != "1.1.1.1" || fp.addrs[1].String() != "9.9.9.9" {
		t.Fatalf("pinged %v", fp.addrs)
	}
	if got := p.Store.Recent(SeriesDNS, 5); len(got) != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestProberCancelledProbeIsNotLoss(t *testing.T) {
	p := newProber(t, &fakePinger{results: []error{context.Canceled}}, tgt)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.probe(ctx, t0)
	if len(p.Store.Recent(SeriesDNS, 5)) != 0 {
		t.Fatal("a probe cancelled by shutdown was counted as lost")
	}
}

func TestProberRunStopsAndProbesImmediately(t *testing.T) {
	fp := &fakePinger{results: []error{nil}, rtt: time.Millisecond}
	p := newProber(t, fp, tgt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for {
		fp.mu.Lock()
		n := fp.calls
		fp.mu.Unlock()
		if n >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no immediate probe")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// ---- HTTP -------------------------------------------------------------------------------

func TestHistoryEndpoint(t *testing.T) {
	p := newProber(t, &fakePinger{results: []error{nil}, rtt: 8 * time.Millisecond}, tgt)
	now := time.Now()
	p.Store.Record(probe(now.Add(-2*time.Minute), 10*time.Millisecond, true))
	p.Store.Record(probe(now.Add(-time.Minute), 0, false))
	p.probe(context.Background(), now)

	get := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.HandleHistory(rec, httptest.NewRequest(http.MethodGet, "/x"+q, nil))
		return rec
	}
	rec := get("?range=1h")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	type series struct {
		Timestamps []int64
		AvgMS      []*float64 `json:"avg_ms"`
		LossPct    []float64  `json:"loss_pct"`
		Summary    struct{ Sent, Lost int64 }
	}
	var body struct {
		Range   string
		Status  struct{ State string }
		DNS     series
		Gateway series
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	dns := body.DNS
	if body.Range != "1h" || len(dns.Timestamps) < 2 || len(dns.AvgMS) != len(dns.Timestamps) || len(dns.LossPct) != len(dns.Timestamps) {
		t.Fatalf("%s", rec.Body.String())
	}
	if dns.Summary.Sent != 3 || dns.Summary.Lost != 1 || body.Status.State != StateOnline {
		t.Fatalf("%+v", body)
	}
	if body.Gateway.Summary.Sent != 0 || len(body.Gateway.Timestamps) != 0 {
		t.Fatalf("no gateway was probed, yet: %+v", body.Gateway)
	}
	hasNull, hasLoss := false, false
	for i := range dns.AvgMS {
		hasNull = hasNull || dns.AvgMS[i] == nil
		hasLoss = hasLoss || dns.LossPct[i] == 100
	}
	if !hasNull || !hasLoss {
		t.Fatal("the fully lost minute should have a null latency and 100% loss")
	}

	if get("").Code != 200 { // default range
		t.Fatal("default range")
	}
	if get("?range=7y").Code != http.StatusBadRequest {
		t.Fatal("unknown range accepted")
	}
	rec = httptest.NewRecorder()
	p.HandleHistory(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatal("POST allowed")
	}
}

func TestHistoryEndpointEmptyListsAreArrays(t *testing.T) {
	p := newProber(t, &fakePinger{results: []error{nil}}, tgt)
	rec := httptest.NewRecorder()
	p.HandleHistory(rec, httptest.NewRequest(http.MethodGet, "/x?range=30d", nil))
	var raw map[string]json.RawMessage
	json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, name := range []string{"dns", "gateway"} {
		var ser map[string]json.RawMessage
		json.Unmarshal(raw[name], &ser)
		for _, k := range []string{"timestamps", "avg_ms", "min_ms", "max_ms", "loss_pct"} {
			if string(ser[k]) != "[]" {
				t.Fatalf("%s.%s = %s, want []", name, k, ser[k])
			}
		}
	}
}

// ---- probes -----------------------------------------------------------------------------

func TestEchoRequestChecksum(t *testing.T) {
	b := echoRequest(0x1234, 7)
	if b[0] != 8 || binary.BigEndian.Uint16(b[4:]) != 0x1234 || binary.BigEndian.Uint16(b[6:]) != 7 {
		t.Fatalf("% x", b)
	}
	if icmpChecksum(b) != 0 { // a correct checksum makes the whole packet sum to zero
		t.Fatalf("bad checksum, residue %x", icmpChecksum(b))
	}
}

func TestEchoReplyMatching(t *testing.T) {
	reply := echoRequest(5, 9)
	reply[0] = 0
	if !echoReplyMatches(reply, 5, 9, true) {
		t.Fatal("matching reply rejected")
	}
	if echoReplyMatches(reply, 6, 9, true) {
		t.Fatal("wrong id accepted on a raw socket")
	}
	if !echoReplyMatches(reply, 6, 9, false) {
		t.Fatal("datagram sockets rewrite the id: it must not be checked")
	}
	if echoReplyMatches(reply, 5, 10, true) {
		t.Fatal("wrong sequence accepted")
	}
	req := echoRequest(5, 9) // type 8: our own request echoed back by a raw socket
	if echoReplyMatches(req, 5, 9, true) {
		t.Fatal("a request counted as a reply")
	}
	if echoReplyMatches([]byte{0, 0, 0}, 5, 9, true) {
		t.Fatal("short packet accepted")
	}
}

func TestPingICMPLoopback(t *testing.T) {
	rtt, err := pingICMP(context.Background(), netip.MustParseAddr("127.0.0.1"), time.Second)
	if errors.Is(err, ErrNoPermission) {
		t.Skip("this process may not send ICMP echo requests")
	}
	if err != nil || rtt <= 0 || rtt > time.Second {
		t.Fatalf("rtt %v err %v", rtt, err)
	}
}

// fakeDNS answers every query (as a resolver would), or stays silent.
func fakeDNS(t *testing.T, answer bool) (netip.Addr, uint16) {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if answer && n >= 12 {
				resp := append([]byte(nil), buf[:n]...)
				resp[2] |= 0x80                 // QR: response
				resp[3] = (resp[3] &^ 0x0f) | 5 // REFUSED still counts: the resolver answered
				pc.WriteTo(resp, from)
			}
		}
	}()
	return netip.MustParseAddr("127.0.0.1"), uint16(pc.LocalAddr().(*net.UDPAddr).Port)
}

func TestDNSQueryShape(t *testing.T) {
	q := dnsQuery(0xabcd)
	if len(q) != 17 || binary.BigEndian.Uint16(q) != 0xabcd || q[2] != 0x01 || q[5] != 1 || q[12] != 0 || q[14] != 2 {
		t.Fatalf("% x", q)
	}
}

func TestQueryDNSMeasuresTheAnswer(t *testing.T) {
	addr, port := fakeDNS(t, true)
	rtt, err := queryDNS(context.Background(), netip.AddrPortFrom(addr, port), time.Second)
	if err != nil || rtt <= 0 || rtt > time.Second {
		t.Fatalf("rtt %v err %v", rtt, err)
	}
}

func TestQueryDNSIgnoresAnswersThatAreNotForUs(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	go func() {
		buf := make([]byte, 512)
		n, from, err := pc.ReadFrom(buf)
		if err != nil || n < 12 {
			return
		}
		wrongID := append([]byte(nil), buf[:n]...)
		wrongID[0] ^= 0xff
		wrongID[2] |= 0x80
		pc.WriteTo(wrongID, from) // someone else's answer
		notAResponse := append([]byte(nil), buf[:n]...)
		pc.WriteTo(notAResponse, from) // our own query echoed back: QR bit not set
	}()
	port := uint16(pc.LocalAddr().(*net.UDPAddr).Port)
	_, err = queryDNS(context.Background(), netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port), 300*time.Millisecond)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want a timeout (no valid answer arrived)", err)
	}
}

func TestQueryDNSTimesOutWithoutAnAnswer(t *testing.T) {
	addr, port := fakeDNS(t, false)
	start := time.Now()
	_, err := queryDNS(context.Background(), netip.AddrPortFrom(addr, port), 200*time.Millisecond)
	if !errors.Is(err, ErrTimeout) || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestQueryDNSStopsWhenCancelled(t *testing.T) {
	addr, port := fakeDNS(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := queryDNS(ctx, netip.AddrPortFrom(addr, port), 5*time.Second)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestAutoPingerUsesICMPWhenAllowed(t *testing.T) {
	p := &AutoPinger{
		icmp: func(context.Context, netip.Addr, time.Duration) (time.Duration, error) {
			return 7 * time.Millisecond, nil
		},
		dns: func(context.Context, netip.Addr, time.Duration) (time.Duration, error) {
			t.Fatal("DNS used though ping works")
			return 0, nil
		},
	}
	rtt, err := p.Ping(context.Background(), netip.MustParseAddr("1.1.1.1"), time.Second)
	if err != nil || rtt != 7*time.Millisecond || p.Method() != "icmp" {
		t.Fatalf("%v %v %q", rtt, err, p.Method())
	}
}

func TestAutoPingerKeepsICMPWhenAHostIsSimplySilent(t *testing.T) {
	p := &AutoPinger{
		icmp: func(context.Context, netip.Addr, time.Duration) (time.Duration, error) { return 0, ErrTimeout },
		dns: func(context.Context, netip.Addr, time.Duration) (time.Duration, error) {
			t.Fatal("fell back to DNS on a mere timeout")
			return 0, nil
		},
	}
	if _, err := p.Ping(context.Background(), netip.MustParseAddr("1.1.1.1"), time.Second); !errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v", err)
	}
	if p.Method() != "icmp" {
		t.Fatalf("method %q", p.Method())
	}
}

func TestAutoPingerFallsBackToDNSWhenICMPIsDeniedAndRemembers(t *testing.T) {
	icmpCalls := 0
	p := &AutoPinger{
		icmp: func(context.Context, netip.Addr, time.Duration) (time.Duration, error) {
			icmpCalls++
			return 0, ErrNoPermission
		},
		dns: func(context.Context, netip.Addr, time.Duration) (time.Duration, error) {
			return 9 * time.Millisecond, nil
		},
	}
	for i := 0; i < 3; i++ {
		rtt, err := p.Ping(context.Background(), netip.MustParseAddr("1.1.1.1"), time.Second)
		if err != nil || rtt != 9*time.Millisecond || p.Method() != "dns" {
			t.Fatalf("round %d: %v %v %q", i, rtt, err, p.Method())
		}
	}
	if icmpCalls != 1 {
		t.Fatalf("ICMP was retried %d times after being denied", icmpCalls)
	}
}

// ---- gateway series --------------------------------------------------------------------

func gwTarget() (Target, bool) {
	return Target{Addr: netip.MustParseAddr("192.168.0.1"), Source: "default gateway"}, true
}

func TestSeriesAreKeptApart(t *testing.T) {
	s := memStore(t)
	s.Record(Probe{At: t0, Series: SeriesDNS, RTT: 40 * time.Millisecond, OK: true})
	s.Record(Probe{At: t0, Series: SeriesGateway, RTT: 2 * time.Millisecond, OK: true})
	s.Record(Probe{At: t0.Add(5 * time.Second), Series: SeriesGateway, OK: false})

	dns, dsum, _ := s.History(SeriesDNS, Ranges["1h"], t0.Add(time.Minute))
	gw, gsum, _ := s.History(SeriesGateway, Ranges["1h"], t0.Add(time.Minute))
	if len(dns) != 1 || dsum.Sent != 1 || dsum.Lost != 0 {
		t.Fatalf("dns %+v %+v", dns, dsum)
	}
	if len(gw) != 1 || gsum.Sent != 2 || gsum.Lost != 1 {
		t.Fatalf("gateway %+v %+v", gw, gsum)
	}
	near(t, dns[0].AvgMS, 40, "dns avg")
	near(t, gw[0].AvgMS, 2, "gateway avg")

	raw, _, _ := s.History(SeriesGateway, Ranges["5m"], t0.Add(30*time.Second))
	if len(raw) != 2 {
		t.Fatalf("raw gateway probes: %d", len(raw))
	}
	if len(s.Recent(SeriesDNS, 10)) != 1 || len(s.Recent(SeriesGateway, 10)) != 2 {
		t.Fatal("the raw rings are shared")
	}
}

func TestSeriesArePersistedSeparately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	s.Record(Probe{At: t0, Series: SeriesDNS, RTT: 40 * time.Millisecond, OK: true})
	s.Record(Probe{At: t0, Series: SeriesGateway, RTT: 2 * time.Millisecond, OK: true})
	s.Close()

	s2 := fileStore(t, path)
	defer s2.Close()
	now := t0.Add(time.Minute)
	dns, _, _ := s2.History(SeriesDNS, Ranges["1h"], now)
	gw, _, _ := s2.History(SeriesGateway, Ranges["1h"], now)
	if len(dns) != 1 || len(gw) != 1 {
		t.Fatalf("%+v %+v", dns, gw)
	}
	near(t, dns[0].AvgMS, 40, "dns")
	near(t, gw[0].AvgMS, 2, "gateway")
}

func TestMigratesRowsKeyedByAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	min := t0.Unix() / 60 * 60
	for _, q := range []string{
		`CREATE TABLE wan_health (target TEXT NOT NULL, minute INTEGER NOT NULL, sent INTEGER NOT NULL, lost INTEGER NOT NULL, rtt_sum_us INTEGER NOT NULL, rtt_min_us INTEGER NOT NULL, rtt_max_us INTEGER NOT NULL, PRIMARY KEY (target, minute))`,
		fmt.Sprintf(`INSERT INTO wan_health VALUES ('9.9.9.9', %d, 12, 0, 240000, 15000, 30000)`, min),
		fmt.Sprintf(`INSERT INTO wan_health VALUES ('1.1.1.1', %d, 12, 12, 0, 0, 0)`, min), // same minute, another address
		fmt.Sprintf(`INSERT INTO wan_health VALUES ('1.1.1.1', %d, 12, 0, 120000, 5000, 15000)`, min+60),
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	s := fileStore(t, path)
	defer s.Close()
	pts, sum, err := s.History(SeriesDNS, Ranges["1h"], t0.Add(5*time.Minute))
	if err != nil || len(pts) != 2 || sum.Sent != 24 {
		t.Fatalf("old rows were not carried over as DNS history: %+v %+v %v", pts, sum, err)
	}
	if gw, _, _ := s.History(SeriesGateway, Ranges["1h"], t0.Add(5*time.Minute)); len(gw) != 0 {
		t.Fatalf("old rows leaked into the gateway series: %+v", gw)
	}
	// opening it again (already migrated) changes nothing
	s.Close()
	s2 := fileStore(t, path)
	defer s2.Close()
	if pts, _, _ := s2.History(SeriesDNS, Ranges["1h"], t0.Add(5*time.Minute)); len(pts) != 2 {
		t.Fatalf("second open: %+v", pts)
	}
}

func TestProberProbesBothAndKeepsStatesApart(t *testing.T) {
	fp := &fakePinger{results: []error{nil}, rtt: 3 * time.Millisecond}
	p := newProber(t, fp, tgt)
	p.Gateway = gwTarget
	p.probe(context.Background(), t0)

	if len(fp.addrs) != 2 || fp.addrs[0].String() != "1.1.1.1" || fp.addrs[1].String() != "192.168.0.1" {
		t.Fatalf("pinged %v: the DNS server first, then the gateway", fp.addrs)
	}
	st := p.Status()
	if st.State != StateOnline || st.Gateway.State != StateOnline || st.Gateway.Target != "192.168.0.1" || st.Gateway.RTTMS == nil {
		t.Fatalf("%+v", st)
	}
	if len(p.Store.Recent(SeriesDNS, 5)) != 1 || len(p.Store.Recent(SeriesGateway, 5)) != 1 {
		t.Fatal("each series should have one probe")
	}
}

func TestGatewayNeedsARealPing(t *testing.T) {
	fp := &fakePinger{results: []error{nil}, noICMP: true}
	p := newProber(t, fp, tgt)
	p.Gateway = gwTarget
	p.probe(context.Background(), t0)
	if len(fp.addrs) != 1 {
		t.Fatalf("pinged %v: with only DNS timing available the gateway must be left alone", fp.addrs)
	}
	if st := p.Status(); st.Gateway.State != StateUnavailable || st.Gateway.Problem == "" || st.State != StateOnline {
		t.Fatalf("%+v", st)
	}
}

func TestGatewayStates(t *testing.T) {
	p := newProber(t, &fakePinger{results: []error{nil}}, tgt)
	p.probe(context.Background(), t0) // no Gateway function at all
	if st := p.Status(); st.Gateway.State != StateNotConfigured || st.Gateway.Problem == "" {
		t.Fatalf("%+v", st.Gateway)
	}
	p.Gateway = func() (Target, bool) { return Target{}, false }
	p.probe(context.Background(), t0)
	if st := p.Status(); st.Gateway.State != StateNotConfigured {
		t.Fatalf("%+v", st.Gateway)
	}
}

func TestGatewayOutageNeedsThreeFailures(t *testing.T) {
	// DNS answers (call 1, 3, 5...); the gateway fails every time (call 2, 4, 6...)
	calls := 0
	pinger := pingerFunc(func(a netip.Addr) error {
		calls++
		if a.String() == "192.168.0.1" {
			return errors.New("no reply")
		}
		return nil
	})
	p := newProber(t, pinger, tgt)
	p.Gateway = gwTarget
	want := []string{StateUnknown, StateUnknown, StateOffline}
	for i, w := range want {
		p.probe(context.Background(), t0.Add(time.Duration(i)*5*time.Second))
		if got := p.Status().Gateway.State; got != w {
			t.Fatalf("after %d failures: %q, want %q", i+1, got, w)
		}
	}
	if p.Status().State != StateOnline {
		t.Fatal("the DNS series must not follow the gateway")
	}
}

type pingerFunc func(netip.Addr) error

func (f pingerFunc) Ping(ctx context.Context, a netip.Addr, d time.Duration) (time.Duration, error) {
	if err := f(a); err != nil {
		return 0, err
	}
	return time.Millisecond, nil
}
func (pingerFunc) Method() string { return "icmp" }
func (pingerFunc) ICMP() bool     { return true }

func TestDiagnosis(t *testing.T) {
	for _, c := range []struct {
		dns, gw string
		want    string // a word the message must contain; empty: no message
	}{
		{StateOffline, StateOnline, "beyond the gateway"},
		{StateOffline, StateOffline, "cannot reach its gateway"},
		{StateOnline, StateOffline, "ignore pings"},
		{StateOnline, StateOnline, ""},
		{StateOffline, StateUnknown, ""},
		{StateOffline, StateNotConfigured, ""},
		{StateOffline, StateUnavailable, ""},
		{StateOnline, StateUnavailable, ""},
		{StateUnknown, StateOffline, ""},
	} {
		got := diagnose(c.dns, c.gw)
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("dns %s, gateway %s: %q, want %q", c.dns, c.gw, got, c.want)
		}
	}
}

func TestStatusCarriesTheDiagnosis(t *testing.T) {
	pinger := pingerFunc(func(a netip.Addr) error {
		if a.String() == "1.1.1.1" {
			return errors.New("no reply") // the DNS server is gone, the gateway is fine
		}
		return nil
	})
	p := newProber(t, pinger, tgt)
	p.Gateway = gwTarget
	for i := 0; i < 3; i++ {
		p.probe(context.Background(), t0.Add(time.Duration(i)*5*time.Second))
	}
	st := p.Status()
	if st.State != StateOffline || st.Gateway.State != StateOnline || !strings.Contains(st.Diagnosis, "beyond the gateway") {
		t.Fatalf("%+v", st)
	}
}

func TestHistoryEndpointHasBothSeries(t *testing.T) {
	fp := &fakePinger{results: []error{nil}, rtt: 4 * time.Millisecond}
	p := newProber(t, fp, tgt)
	p.Gateway = gwTarget
	p.probe(context.Background(), time.Now())
	rec := httptest.NewRecorder()
	p.HandleHistory(rec, httptest.NewRequest(http.MethodGet, "/x?range=5m", nil))
	var body struct {
		Status struct {
			Gateway struct{ State, Target string }
		}
		DNS     struct{ Timestamps []int64 }
		Gateway struct{ Timestamps []int64 }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.DNS.Timestamps) != 1 || len(body.Gateway.Timestamps) != 1 || body.Status.Gateway.Target != "192.168.0.1" {
		t.Fatalf("%s", rec.Body.String())
	}
}

func TestOneSeriesRAMDoesNotHideAnotherSeriesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	s := fileStore(t, path)
	defer s.Close()
	// a gateway minute that is only in the database (written, then forgotten by RAM)...
	s.Record(Probe{At: t0.Add(time.Minute), Series: SeriesGateway, RTT: 2 * time.Millisecond, OK: true})
	s.writeDirty()
	s.expire(t0.Add(time.Hour))
	// ...while the DNS series still holds an older, unwritten minute in RAM
	s.Record(Probe{At: t0, Series: SeriesDNS, RTT: 30 * time.Millisecond, OK: true})

	gw, _, _ := s.History(SeriesGateway, Ranges["1h"], t0.Add(10*time.Minute))
	if len(gw) != 1 {
		t.Fatalf("gateway history lost because of the DNS series' RAM: %+v", gw)
	}
}

func TestMigrationDropsRowsItCannotAttribute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.db")
	old, _ := sql.Open("sqlite", path)
	min := t0.Unix() / 60 * 60
	for _, q := range []string{
		`CREATE TABLE wan_health (target TEXT NOT NULL, minute INTEGER NOT NULL, sent INTEGER NOT NULL, lost INTEGER NOT NULL, rtt_sum_us INTEGER NOT NULL, rtt_min_us INTEGER NOT NULL, rtt_max_us INTEGER NOT NULL, PRIMARY KEY (target, minute))`,
		fmt.Sprintf(`INSERT INTO wan_health VALUES ('9.9.9.9', %d, 12, 0, 240000, 15000, 30000)`, min),
		fmt.Sprintf(`INSERT INTO wan_health VALUES ('1.1.1.1', %d, 12, 12, 0, 0, 0)`, min),
	} {
		old.Exec(q)
	}
	old.Close()
	s := fileStore(t, path)
	defer s.Close()
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM wan_health WHERE series NOT IN ('dns','gateway')`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows keep a series name that nothing reads", n)
	}
}

func TestSeriesLeavesNotConfiguredOnTheFirstFailure(t *testing.T) {
	have := false
	fp := &fakePinger{results: []error{errors.New("no reply")}}
	p := newProber(t, fp, func() (Target, bool) {
		if !have {
			return Target{}, false
		}
		return tgt()
	})
	p.probe(context.Background(), t0)
	if p.Status().State != StateNotConfigured {
		t.Fatal("setup")
	}
	have = true
	p.probe(context.Background(), t0.Add(5*time.Second)) // first failed probe: not an outage yet, but no longer "not set up"
	if got := p.Status().State; got != StateUnknown {
		t.Fatalf("state %q", got)
	}
}

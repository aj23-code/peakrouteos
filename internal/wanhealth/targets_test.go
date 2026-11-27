package wanhealth

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// a pinger that answers or not by address
type addrPinger struct {
	mu   sync.Mutex
	up   map[netip.Addr]bool
	seen map[netip.Addr]int
}

func newAddrPinger(up ...string) *addrPinger {
	p := &addrPinger{up: map[netip.Addr]bool{}, seen: map[netip.Addr]int{}}
	for _, a := range up {
		p.up[netip.MustParseAddr(a)] = true
	}
	return p
}
func (p *addrPinger) set(a string, up bool) {
	p.mu.Lock()
	p.up[netip.MustParseAddr(a)] = up
	p.mu.Unlock()
}
func (p *addrPinger) Ping(ctx context.Context, a netip.Addr, d time.Duration) (time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen[a]++
	if p.up[a] {
		return 8 * time.Millisecond, nil
	}
	return 0, errors.New("no reply")
}
func (p *addrPinger) Method() string { return "icmp" }
func (p *addrPinger) ICMP() bool     { return true }

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

func withTargets(t *testing.T, pinger Pinger, dns string, extras ...string) *Prober {
	p := newProber(t, pinger, func() (Target, bool) {
		if dns == "" {
			return Target{}, false
		}
		return Target{Addr: addr(dns), Source: "DNS server from your provider"}, true
	})
	p.Extra = func() []Target {
		var out []Target
		for _, e := range extras {
			out = append(out, Target{Addr: addr(e), Source: "added by you"})
		}
		return out
	}
	return p
}

func rounds(p *Prober, n int) {
	for i := 0; i < n; i++ {
		p.probe(context.Background(), t0.Add(time.Duration(i)*5*time.Second))
	}
}

func TestAConnectionIsDownOnlyWhenEveryTargetHasFailedSeveralTimes(t *testing.T) {
	fp := newAddrPinger("203.0.113.53", "8.8.8.8")
	p := withTargets(t, fp, "203.0.113.53", "8.8.8.8")
	rounds(p, 1)
	if st := p.Status(); st.State != StateOnline || len(st.Targets) != 2 {
		t.Fatalf("%+v", st)
	}
	// the provider's DNS server stops answering but the other target does: the connection is up
	fp.set("203.0.113.53", false)
	rounds(p, 6)
	if st := p.Status(); st.State != StateOnline {
		t.Fatalf("one target down made the connection down: %+v", st)
	}
	// the other fails too: down, but only after as many rounds as the settings say
	fp.set("8.8.8.8", false)
	p.probe(context.Background(), t0)
	p.probe(context.Background(), t0)
	if st := p.Status(); st.State != StateOnline {
		t.Fatalf("down after two failed rounds: %+v", st)
	}
	p.probe(context.Background(), t0)
	if st := p.Status(); st.State != StateOffline {
		t.Fatalf("not down after three failed rounds: %+v", st)
	}
	// and one answer brings it back
	fp.set("8.8.8.8", true)
	rounds(p, 1)
	if st := p.Status(); st.State != StateOnline {
		t.Fatalf("%+v", st)
	}
}

func TestATargetThatHasNeverAnsweredIsLeftOutOfTheDecision(t *testing.T) {
	// the provider's DNS server does not answer ping, the public one does
	fp := newAddrPinger("1.1.1.1")
	p := withTargets(t, fp, "203.0.113.53", "1.1.1.1")
	rounds(p, 10)
	st := p.Status()
	if st.State != StateOnline {
		t.Fatalf("a server that ignores ping made the connection look down: %+v", st)
	}
	var dns, public TargetStatus
	for _, tt := range st.Targets {
		if tt.Addr == "203.0.113.53" {
			dns = tt
		} else {
			public = tt
		}
	}
	if dns.Counts || dns.Note == "" || !public.Counts || public.State != StateOnline {
		t.Fatalf("%+v %+v", dns, public)
	}
	// the line dies: the one that counts fails, and that is enough
	fp.set("1.1.1.1", false)
	rounds(p, 3)
	if got := p.Status().State; got != StateOffline {
		t.Fatalf("%s", got)
	}
}

func TestAConnectionThatIsDownFromTheStartIsFoundDown(t *testing.T) {
	// nothing has ever answered: every target counts, so the connection is found down and not "unknown" for ever
	p := withTargets(t, newAddrPinger(), "203.0.113.53", "1.1.1.1", "9.9.9.9")
	rounds(p, 3)
	if st := p.Status(); st.State != StateOffline {
		t.Fatalf("%+v", st)
	}
}

func TestTargetsNeedNotIncludeTheDNSServer(t *testing.T) {
	p := withTargets(t, newAddrPinger("1.1.1.1"), "", "1.1.1.1")
	rounds(p, 1)
	if st := p.Status(); st.State != StateOnline || len(st.Targets) != 1 {
		t.Fatalf("%+v", st)
	}
	// with nothing to probe at all there is nothing to say
	q := newProber(t, newAddrPinger(), func() (Target, bool) { return Target{}, false })
	rounds(q, 2)
	if st := q.Status(); st.State != StateNotConfigured {
		t.Fatalf("%+v", st)
	}
}

func TestTheSameAddressIsNeverProbedTwiceInARound(t *testing.T) {
	fp := newAddrPinger("1.1.1.1")
	p := withTargets(t, fp, "1.1.1.1", "1.1.1.1", "1.1.1.1")
	rounds(p, 1)
	if fp.seen[addr("1.1.1.1")] != 1 || len(p.Status().Targets) != 1 {
		t.Fatalf("%d %+v", fp.seen[addr("1.1.1.1")], p.Status().Targets)
	}
}

func TestChangedSettingsAreFollowedAtOnce(t *testing.T) {
	fp := newAddrPinger("8.8.8.8", "1.1.1.1")
	extras := []string{"8.8.8.8"}
	p := newProber(t, fp, func() (Target, bool) { return Target{}, false })
	p.Extra = func() []Target {
		var out []Target
		for _, e := range extras {
			out = append(out, Target{Addr: addr(e)})
		}
		return out
	}
	down := 2
	p.DownAfter = func() int { return down }
	p.EveryFn = func() time.Duration { return 9 * time.Second }
	rounds(p, 1)
	if p.Status().Every != 9 {
		t.Errorf("interval %d", p.Status().Every)
	}
	extras = []string{"1.1.1.1"} // the first is dropped, another added
	rounds(p, 1)
	if st := p.Status(); len(st.Targets) != 1 || st.Targets[0].Addr != "1.1.1.1" {
		t.Fatalf("%+v", st.Targets)
	}
	fp.set("1.1.1.1", false)
	rounds(p, 2) // down after two, as set
	if st := p.Status(); st.State != StateOffline {
		t.Fatalf("%+v", st)
	}
}

func TestTheSecondConnectionHasSeriesOfItsOwn(t *testing.T) {
	fp := newAddrPinger("203.0.113.53")
	p := withTargets(t, fp, "203.0.113.53")
	p.ID = "wan2"
	p.Gateway = func() (Target, bool) { return Target{Addr: addr("203.0.113.1")}, true }
	rounds(p, 2)
	if len(p.Store.Recent("wan2.dns", 10)) != 2 || len(p.Store.Recent("wan2.gateway", 10)) != 2 {
		t.Fatalf("the second connection's probes are not in its own series")
	}
	if len(p.Store.Recent(SeriesDNS, 10)) != 0 || len(p.Store.Recent(SeriesGateway, 10)) != 0 {
		t.Fatal("the first connection's series got the second's probes")
	}
}

func TestProbesOfSeveralTargetsRunTogether(t *testing.T) {
	// four targets that each take a timeout to fail must not make a round four timeouts long
	slow := &slowPinger{delay: 150 * time.Millisecond}
	p := withTargets(t, slow, "203.0.113.53", "8.8.8.8", "1.1.1.1", "9.9.9.9")
	start := time.Now()
	rounds(p, 1)
	if took := time.Since(start); took > 400*time.Millisecond {
		t.Fatalf("a round took %v for four targets that take 150 ms each", took)
	}
}

type slowPinger struct{ delay time.Duration }

func (s *slowPinger) Ping(ctx context.Context, a netip.Addr, d time.Duration) (time.Duration, error) {
	time.Sleep(s.delay)
	return 0, errors.New("no reply")
}
func (s *slowPinger) Method() string { return "icmp" }
func (s *slowPinger) ICMP() bool     { return true }

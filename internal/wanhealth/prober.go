// Package wanhealth watches an internet connection: every few seconds it pings the upstream DNS
// resolver, the targets the administrator added and the gateway in front of this router, keeps the
// results as per-minute history (like the traffic history) and answers whether the connection is up
// and, if not, where it breaks. One Prober watches one connection (docs/wan-failover.md, section 4).
package wanhealth

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

// Target is what to ping and where it came from.
type Target struct {
	Addr   netip.Addr
	Source string // human readable: "DNS server you configured", "default gateway"
}

// State of a series as the prober sees it.
const (
	StateUnknown       = "unknown" // no probe has finished yet
	StateOnline        = "online"
	StateOffline       = "offline"        // the last few probes all failed
	StateNotConfigured = "not_configured" // nothing to probe yet
	StateUnavailable   = "unavailable"    // cannot be probed here (the gateway needs a real ping)
)

// offlineAfter is how many probes in a row must fail before a series counts as down (unless the connection's settings say otherwise).
const offlineAfter = 3

// FirstID names the first connection: its series are called "dns" and "gateway", as they always were. The second's are "wan2.dns" and
// "wan2.gateway".
const FirstID = "wan"

type Prober struct {
	Store  *Store
	Pinger Pinger
	// ID names the connection: "" or FirstID for the first, "wan2" for the second. It decides the names of the history series.
	ID      string
	Target  func() (Target, bool) // the upstream DNS server
	Gateway func() (Target, bool) // the default gateway; nil: not probed
	// Extra are the other targets: the ones the administrator added and the public fallbacks (nil: none). They are asked every round,
	// so a change of the settings is followed at once.
	Extra     func() []Target
	Every     time.Duration        // between probes
	EveryFn   func() time.Duration // when set, asked before every wait: the connection's own setting
	DownAfter func() int           // when set: how many probes in a row must fail (offlineAfter otherwise)
	Timeout   time.Duration        // per probe

	avail   availabilityCache
	mu      sync.Mutex
	dns     seriesState
	gw      seriesState
	extras  map[netip.Addr]*seriesState
	order   []netip.Addr // the extra targets in the order they were given
	decided bool         // there is at least one target to decide by
	state   string       // the decision: online, offline or unknown
	method  string
}

// series is the name of one of this connection's history series.
func (p *Prober) series(base string) string {
	if p.ID == "" || p.ID == FirstID {
		return base
	}
	return p.ID + "." + base
}

func (p *Prober) downAfterN() int {
	if p.DownAfter != nil {
		if n := p.DownAfter(); n > 0 {
			return n
		}
	}
	return offlineAfter
}

func (p *Prober) interval() time.Duration {
	if p.EveryFn != nil {
		if d := p.EveryFn(); d > 0 {
			return d
		}
	}
	return p.Every
}

type seriesState struct {
	state    string
	problem  string
	target   Target
	have     bool
	failures int // in a row
	everOK   bool
	lastRTT  time.Duration
	lastOK   bool
	lastAt   time.Time
}

// SeriesStatus is the state of one probed target.
type SeriesStatus struct {
	State   string   `json:"state"`
	Problem string   `json:"problem,omitempty"` // why there is nothing to show
	Target  string   `json:"target,omitempty"`
	Source  string   `json:"source,omitempty"`
	RTTMS   *float64 `json:"rtt_ms"` // the latest answered probe
	At      int64    `json:"at"`     // Unix ms of the latest probe
}

// TargetStatus is one of the addresses a connection is probed against.
type TargetStatus struct {
	Addr   string   `json:"addr"`
	Source string   `json:"source"` // where it comes from: the provider's DNS server, added by the administrator, a public address
	State  string   `json:"state"`  // online, offline or unknown
	RTTMS  *float64 `json:"rtt_ms"`
	Counts bool     `json:"counts"` // it takes part in deciding whether the connection is down
	Note   string   `json:"note,omitempty"`
}

// Status is what the page shows next to the chart. The embedded fields describe the DNS server (its latency is what the chart draws),
// except for the state: that is the verdict over every target that counts.
type Status struct {
	SeriesStatus
	Method    string         `json:"method,omitempty"` // "icmp" (ping) or "dns" (a DNS query, when ping is not permitted)
	Every     int            `json:"interval_seconds"`
	Gateway   SeriesStatus   `json:"gateway"`
	Targets   []TargetStatus `json:"targets"`
	Diagnosis string         `json:"diagnosis,omitempty"` // where the connection breaks, when the two disagree
}

// Run probes until ctx ends. It probes right away so the page has data soon after start.
func (p *Prober) Run(ctx context.Context) {
	p.probe(ctx, time.Now())
	for {
		t := time.NewTimer(p.interval())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case now := <-t.C:
			p.probe(ctx, now)
		}
	}
}

func (p *Prober) probe(ctx context.Context, now time.Time) {
	tgt, ok := p.Target()
	extras := p.extraTargets(tgt, ok)

	// the DNS server and the other targets are asked together, so that a round lasts one timeout and not one per target
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.probeSeries(ctx, now, SeriesDNS, &p.dns, tgt, ok, "No internet port or DNS server is configured yet.")
	}()
	for _, e := range extras {
		wg.Add(1)
		go func(e Target) {
			defer wg.Done()
			p.probeExtra(ctx, now, e)
		}(e)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	p.method = p.Pinger.Method()
	p.mu.Unlock()

	// The gateway is only worth measuring with a real ping: many gateways do not run a DNS server,
	// so a DNS query to one would report an outage that is not there.
	var gtgt Target
	gok := false
	if p.Gateway != nil {
		gtgt, gok = p.Gateway()
	}
	switch {
	case !gok:
		p.setState(&p.gw, StateNotConfigured, "No gateway is known yet.")
	case !p.Pinger.ICMP():
		p.setState(&p.gw, StateUnavailable, "The gateway is only measured with a real ping, which is not permitted here.")
	default:
		p.probeSeries(ctx, now, SeriesGateway, &p.gw, gtgt, true, "")
	}
	p.decide()
}

// extraTargets are the targets besides the DNS server, without the ones that repeat it or each other.
func (p *Prober) extraTargets(dns Target, haveDNS bool) []Target {
	if p.Extra == nil {
		p.mu.Lock()
		p.extras, p.order = nil, nil
		p.mu.Unlock()
		return nil
	}
	var out []Target
	seen := map[netip.Addr]bool{}
	if haveDNS {
		seen[dns.Addr] = true
	}
	for _, t := range p.Extra() {
		if t.Addr.IsValid() && !seen[t.Addr] {
			seen[t.Addr] = true
			out = append(out, t)
		}
	}
	// forget the targets that are no longer asked about, and make room for the new ones
	p.mu.Lock()
	keep := map[netip.Addr]*seriesState{}
	p.order = p.order[:0]
	for _, t := range out {
		st := p.extras[t.Addr]
		if st == nil {
			st = &seriesState{}
		}
		st.target, st.have = t, true
		keep[t.Addr] = st
		p.order = append(p.order, t.Addr)
	}
	p.extras = keep
	p.mu.Unlock()
	return out
}

func (p *Prober) probeExtra(ctx context.Context, now time.Time, t Target) {
	rtt, err := p.Pinger.Ping(ctx, t.Addr, p.Timeout)
	if ctx.Err() != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.extras[t.Addr]
	if st == nil {
		return
	}
	st.record(now, rtt, err == nil)
}

// record notes the outcome of a probe.
func (st *seriesState) record(now time.Time, rtt time.Duration, good bool) {
	st.lastAt, st.lastOK, st.lastRTT = now, good, rtt
	if good {
		st.failures, st.everOK = 0, true
	} else {
		st.failures++
	}
}

// decide gives the verdict over every target that counts. A target counts once it has answered: many providers' DNS servers do not
// answer a ping, and a healthy connection must not look down for that. Until anything has answered, all targets count (so a
// connection that is down from the start is found down). The verdict is online when any target that counts answered last time, offline
// when every one of them has failed as many times in a row as the settings say, and otherwise stays as it was.
func (p *Prober) decide() {
	n := p.downAfterN()
	p.mu.Lock()
	defer p.mu.Unlock()
	cands := p.candidates()
	p.decided = len(cands) > 0
	if !p.decided {
		return
	}
	any := false
	for _, c := range cands {
		any = any || c.everOK
	}
	online, allDown, deciding := false, true, 0
	for _, c := range cands {
		if any && !c.everOK {
			continue
		}
		deciding++
		if c.lastOK {
			online = true
		}
		if c.failures < n {
			allDown = false
		}
	}
	switch {
	case online:
		p.state = StateOnline
	case deciding > 0 && allDown:
		p.state = StateOffline
	case p.state == "":
		p.state = StateUnknown
	}
}

// candidates are the targets that have been probed: the DNS server and the others. The caller holds the lock.
func (p *Prober) candidates() []*seriesState {
	var out []*seriesState
	if p.dns.have && !p.dns.lastAt.IsZero() {
		out = append(out, &p.dns)
	}
	for _, a := range p.order {
		if st := p.extras[a]; st != nil && !st.lastAt.IsZero() {
			out = append(out, st)
		}
	}
	return out
}

func (p *Prober) probeSeries(ctx context.Context, now time.Time, series string, st *seriesState, tgt Target, ok bool, missing string) {
	p.mu.Lock()
	st.target, st.have = tgt, ok
	p.mu.Unlock()
	if !ok {
		p.setState(st, StateNotConfigured, missing)
		return
	}

	rtt, err := p.Pinger.Ping(ctx, tgt.Addr, p.Timeout)
	if ctx.Err() != nil {
		return // shutting down: not a lost probe
	}
	good := err == nil
	p.Store.Record(Probe{At: now, Series: p.series(series), RTT: rtt, OK: good})

	p.mu.Lock()
	defer p.mu.Unlock()
	st.record(now, rtt, good)
	if good {
		st.state, st.problem = StateOnline, ""
		return
	}
	if st.failures >= p.downAfterN() {
		st.state, st.problem = StateOffline, ""
	} else if st.state == "" || st.state == StateNotConfigured || st.state == StateUnavailable {
		st.state, st.problem = StateUnknown, ""
	}
}

func (p *Prober) setState(st *seriesState, state, problem string) {
	p.mu.Lock()
	st.state, st.problem = state, problem
	p.mu.Unlock()
}

func (st *seriesState) status() SeriesStatus {
	out := SeriesStatus{State: st.state, Problem: st.problem}
	if out.State == "" {
		out.State = StateUnknown
	}
	if st.have {
		out.Target, out.Source = st.target.Addr.String(), st.target.Source
	}
	if !st.lastAt.IsZero() {
		out.At = st.lastAt.UnixMilli()
		if st.lastOK {
			v := float64(st.lastRTT.Microseconds()) / 1000
			out.RTTMS = &v
		}
	}
	return out
}

// targetStatus describes one probed address; anyEver says whether any target has answered yet.
func (p *Prober) targetStatus(st *seriesState, anyEver bool) TargetStatus {
	t := TargetStatus{Addr: st.target.Addr.String(), Source: st.target.Source, State: StateUnknown, Counts: !anyEver || st.everOK}
	switch {
	case st.lastAt.IsZero():
	case st.lastOK:
		t.State = StateOnline
		v := float64(st.lastRTT.Microseconds()) / 1000
		t.RTTMS = &v
	case st.failures >= p.downAfterN():
		t.State = StateOffline
	}
	if !t.Counts {
		t.Note = "has not answered yet, so it is left out of the decision (some servers do not answer a ping)"
	}
	return t
}

// Status reports the current state.
func (p *Prober) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{SeriesStatus: p.dns.status(), Method: p.method, Every: int(p.interval() / time.Second), Gateway: p.gw.status(), Targets: []TargetStatus{}}
	if p.decided {
		s.State, s.Problem = p.state, ""
	}
	anyEver := false
	for _, c := range p.candidates() {
		anyEver = anyEver || c.everOK
	}
	if p.dns.have {
		s.Targets = append(s.Targets, p.targetStatus(&p.dns, anyEver))
	}
	for _, a := range p.order {
		if st := p.extras[a]; st != nil {
			s.Targets = append(s.Targets, p.targetStatus(st, anyEver))
		}
	}
	s.Diagnosis = diagnose(s.State, s.Gateway.State)
	return s
}

// diagnose says where the connection breaks when the DNS server and the gateway tell different
// stories. Nothing is said while they agree that things are fine.
func diagnose(dns, gateway string) string {
	switch {
	case dns == StateOffline && gateway == StateOnline:
		return "This router can reach its gateway but not the DNS server, so the problem is beyond the gateway: the modem, the provider or the line."
	case dns == StateOffline && gateway == StateOffline:
		return "This router cannot reach its gateway either. Check the cable and the modem or upstream router."
	case dns == StateOnline && gateway == StateOffline:
		return "The gateway does not answer pings, but the internet works. Some modems and provider routers ignore pings."
	}
	return ""
}

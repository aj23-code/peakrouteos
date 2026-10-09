// Package wanhealth watches the internet connection: every few seconds it pings the upstream DNS
// resolver and the gateway in front of this router, keeps the results as per-minute history (like
// the traffic history) and answers whether the connection is up and, if not, where it breaks.
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

// offlineAfter is how many probes in a row must fail before a series counts as down.
const offlineAfter = 3

type Prober struct {
	Store   *Store
	Pinger  Pinger
	Target  func() (Target, bool) // the upstream DNS server
	Gateway func() (Target, bool) // the default gateway; nil: not probed
	Every   time.Duration         // between probes
	Timeout time.Duration         // per probe

	mu     sync.Mutex
	dns    seriesState
	gw     seriesState
	method string
}

type seriesState struct {
	state    string
	problem  string
	target   Target
	have     bool
	failures int // in a row
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

// Status is what the page shows next to the chart. The embedded fields describe the DNS server,
// which is what "online" means.
type Status struct {
	SeriesStatus
	Method    string       `json:"method,omitempty"` // "icmp" (ping) or "dns" (a DNS query, when ping is not permitted)
	Every     int          `json:"interval_seconds"`
	Gateway   SeriesStatus `json:"gateway"`
	Diagnosis string       `json:"diagnosis,omitempty"` // where the connection breaks, when the two disagree
}

// Run probes until ctx ends. It probes right away so the page has data soon after start.
func (p *Prober) Run(ctx context.Context) {
	p.probe(ctx, time.Now())
	t := time.NewTicker(p.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			p.probe(ctx, now)
		}
	}
}

func (p *Prober) probe(ctx context.Context, now time.Time) {
	tgt, ok := p.Target()
	p.probeSeries(ctx, now, SeriesDNS, &p.dns, tgt, ok, "No internet port or DNS server is configured yet.")
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
	p.Store.Record(Probe{At: now, Series: series, RTT: rtt, OK: good})

	p.mu.Lock()
	defer p.mu.Unlock()
	st.lastAt, st.lastOK, st.lastRTT = now, good, rtt
	if good {
		st.failures = 0
		st.state, st.problem = StateOnline, ""
		return
	}
	st.failures++
	if st.failures >= offlineAfter {
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

// Status reports the current state.
func (p *Prober) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{SeriesStatus: p.dns.status(), Method: p.method, Every: int(p.Every / time.Second), Gateway: p.gw.status()}
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

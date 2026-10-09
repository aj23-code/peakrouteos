// Package wanhealth watches the internet connection: it pings the upstream DNS resolver every few
// seconds, keeps the results as per-minute history (like the traffic history) and answers whether
// the connection is up.
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
	Source string // human readable: "configured DNS server", "from your provider (DHCP)"
}

// State of the connection as the prober sees it.
const (
	StateUnknown       = "unknown" // no probe has finished yet
	StateOnline        = "online"
	StateOffline       = "offline"        // the last few probes all failed
	StateNotConfigured = "not_configured" // no WAN port or resolver to ping
)

// offlineAfter is how many probes in a row must fail before the connection counts as down.
const offlineAfter = 3

type Prober struct {
	Store   *Store
	Pinger  Pinger
	Target  func() (Target, bool)
	Every   time.Duration // between probes
	Timeout time.Duration // per probe

	mu       sync.Mutex
	state    string
	problem  string
	target   Target
	haveTgt  bool
	failures int // in a row
	lastRTT  time.Duration
	lastOK   bool
	lastAt   time.Time
	method   string
}

// Status is what the page shows next to the chart.
type Status struct {
	State   string   `json:"state"`
	Problem string   `json:"problem,omitempty"` // why it is not configured
	Target  string   `json:"target,omitempty"`
	Source  string   `json:"source,omitempty"`
	Method  string   `json:"method,omitempty"` // "icmp" (ping) or "dns" (a DNS query, when ping is not permitted)
	RTTMS   *float64 `json:"rtt_ms"`           // the latest answered probe
	At      int64    `json:"at"`               // Unix ms of the latest probe
	Every   int      `json:"interval_seconds"`
}

// Run probes until ctx ends. It records a probe right away so the page has data soon after start.
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
	p.mu.Lock()
	p.target, p.haveTgt = tgt, ok
	p.mu.Unlock()
	if !ok {
		p.set(StateNotConfigured, "No internet port or DNS server is configured yet.")
		return
	}

	rtt, err := p.Pinger.Ping(ctx, tgt.Addr, p.Timeout)
	if ctx.Err() != nil {
		return // shutting down: not a lost probe
	}
	good := err == nil
	p.Store.Record(Probe{At: now, Target: tgt.Addr.String(), RTT: rtt, OK: good})

	p.mu.Lock()
	p.method = p.Pinger.Method()
	p.lastAt, p.lastOK, p.lastRTT = now, good, rtt
	if good {
		p.failures = 0
		p.state, p.problem = StateOnline, ""
	} else {
		p.failures++
		if p.failures >= offlineAfter {
			p.state, p.problem = StateOffline, ""
		} else if p.state == "" {
			p.state = StateUnknown
		}
	}
	p.mu.Unlock()
}

func (p *Prober) set(state, problem string) {
	p.mu.Lock()
	p.state, p.problem = state, problem
	p.mu.Unlock()
}

// Status reports the current state.
func (p *Prober) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Status{State: p.state, Problem: p.problem, Method: p.method, Every: int(p.Every / time.Second)}
	if st.State == "" {
		st.State = StateUnknown
	}
	if p.haveTgt {
		st.Target, st.Source = p.target.Addr.String(), p.target.Source
	}
	if !p.lastAt.IsZero() {
		st.At = p.lastAt.UnixMilli()
		if p.lastOK {
			v := float64(p.lastRTT.Microseconds()) / 1000
			st.RTTMS = &v
		}
	}
	return st
}

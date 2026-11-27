// Package failover moves the traffic between two internet connections (docs/wan-failover.md, section 5). The preferred connection carries
// it; when that one is down (the verdict of its health check, internal/wanhealth) the other one does, and it goes back once the preferred
// one has been up for the recovery time. It works by the metric of each connection's default route: the one that carries the traffic has
// 100, one that is up and waiting 200, one that is down 1000. A down connection keeps its route, so its health check can still reach out
// through it and see it come back.
package failover

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"peakroute/internal/config"
)

// The metrics of the default routes.
const (
	MetricActive  = config.WanMetricPreferred
	MetricStandby = config.WanMetricOther
	MetricDown    = 1000
)

// What a connection's health check can say (the states of internal/wanhealth).
const (
	StateOnline  = "online"
	StateOffline = "offline"
)

// Runner runs programs (the daemon's own runner).
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Event is one change of the connection that carries the traffic.
type Event struct {
	Time   time.Time `json:"time"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Reason string    `json:"reason"`
}

// Status is what the pages show.
type Status struct {
	Active     string            `json:"active"`    // the connection that carries the traffic
	Preferred  string            `json:"preferred"` // the one the settings prefer
	FailedOver bool              `json:"failed_over"`
	Since      *time.Time        `json:"since,omitempty"`  // when the active connection took over
	Reason     string            `json:"reason,omitempty"` // why
	States     map[string]string `json:"states"`           // each connection's verdict
	Recovering int               `json:"recovering_seconds,omitempty"`
	Pinned     string            `json:"pinned,omitempty"` // the connection that was switched to by hand, until "automatic" is chosen
	Events     []Event           `json:"events"`           // newest first
}

// Controller decides which connection carries the traffic and keeps the routes that way.
type Controller struct {
	Config func() config.NetworkConfig
	State  func(id string) string // the health check's verdict on a connection
	Runner Runner
	Now    func() time.Time
	Every  time.Duration // between looks (2 s when zero)
	RunDir string        // where the table of firewall rules is written (/run/peakroute when empty)
	// OnChange, when set, is called (in a goroutine of its own) after the traffic has moved to the other connection, with the routes
	// already changed: the services that have to follow (names, tunnels) are told here.
	OnChange func(Event)
	// EventsPath, when set, is a small file the last changes are kept in (read when the controller starts, written at each change).
	EventsPath string

	mu         sync.Mutex
	active     string
	since      time.Time
	reason     string
	preferred  string
	upSince    map[string]time.Time
	states     map[string]string
	events     []Event
	pinned     string // a connection the administrator switched to by hand ("": the settings decide); not kept over a restart
	pinChanged bool   // the pin was set or cleared since the last look: the traffic moves at once
	loaded     bool   // the event file was read
	lbApplied  string // the firewall table that is loaded
	managed    bool   // tables, rules and the firewall table of balance.go were made
}

func logf(format string, args ...any) { log.Printf("[failover] "+format, args...) }

const maxEvents = 30

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Run looks every few seconds until ctx ends.
func (c *Controller) Run(ctx context.Context) {
	every := c.Every
	if every <= 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// name is how a connection is called to people.
func name(id string) string {
	if id == config.WanSecond {
		return "Internet 2"
	}
	return "Internet"
}

func other(id string) string {
	if id == config.WanSecond {
		return config.WanFirst
	}
	return config.WanSecond
}

func preferredOf(cfg config.NetworkConfig) string {
	if cfg.WanPreferred == config.WanSecond {
		return config.WanSecond
	}
	return config.WanFirst
}

// decide says which connection should carry the traffic: active is the one that does now ("" at the start), preferred the one the
// settings prefer. A connection that is not yet judged is not down. The recovery time is counted from when the preferred connection
// came up (upSince).
func decide(active, preferred string, state map[string]string, upSince map[string]time.Time, now time.Time, set config.FailoverSettings) (string, string) {
	down := func(id string) bool { return state[id] == StateOffline }
	s := other(preferred)
	switch {
	case active == "":
		if down(preferred) && !down(s) {
			return s, fmt.Sprintf("%s is down", name(preferred))
		}
		return preferred, ""
	case active == preferred:
		if down(preferred) && !down(s) {
			return s, fmt.Sprintf("%s is down", name(preferred))
		}
	default:
		if down(active) && !down(preferred) {
			return preferred, fmt.Sprintf("%s is down", name(active))
		}
		if set.Failback && state[preferred] == StateOnline {
			if t, ok := upSince[preferred]; ok && now.Sub(t) >= set.Recovery {
				return preferred, fmt.Sprintf("%s has been up for %d s", name(preferred), int(set.Recovery/time.Second))
			}
		}
	}
	return active, ""
}

// Step looks once: it reads the verdicts, decides, and brings the routes to what is decided.
func (c *Controller) Step(ctx context.Context) {
	cfg := c.Config()
	if !cfg.HasWan2() {
		c.mu.Lock()
		managed := c.managed
		c.mu.Unlock()
		if managed {
			c.cleanup(ctx) // the second connection was taken away: so are the tables and marks that served it
		}
		return
	}
	now := c.now()
	set := cfg.FailoverSettings()
	ids := []string{config.WanFirst, config.WanSecond}

	c.mu.Lock()
	c.loadEvents()
	pref := preferredOf(cfg)
	if c.pinned != "" {
		pref = c.pinned // switched by hand: it is the preferred connection until the administrator says automatic (it is still left if it is down)
	}
	if c.upSince == nil {
		c.upSince = map[string]time.Time{}
	}
	states := map[string]string{}
	for _, id := range ids {
		st := c.State(id)
		states[id] = st
		if st == StateOnline {
			if _, ok := c.upSince[id]; !ok {
				c.upSince[id] = now
			}
		} else {
			delete(c.upSince, id)
		}
	}
	c.states = states
	prev, changedPref := c.active, false
	// a changed preference is taken at once, with no waiting: the administrator asked for it (unless that connection is down)
	if (c.preferred != "" && c.preferred != pref || c.pinChanged) && prev != "" {
		prev, changedPref = "", true
	}
	c.preferred = pref
	next, why := decide(prev, pref, states, c.upSince, now, set)
	if changedPref && why == "" {
		why = "the preferred connection was changed"
		if c.pinned != "" {
			why = "switched by hand"
		} else if c.pinChanged {
			why = "back to automatic"
		}
	}
	c.pinChanged = false
	left := c.active
	if next != c.active {
		if c.active != "" {
			c.events = append([]Event{{Time: now, From: c.active, To: next, Reason: why}}, c.events...)
			if len(c.events) > maxEvents {
				c.events = c.events[:maxEvents]
			}
			log.Printf("[failover] the traffic moves from %s to %s: %s", c.active, next, why)
			c.saveEvents()
		}
		c.active, c.since, c.reason = next, now, why
	}
	active := c.active
	var moved *Event
	if left != "" && left != active && len(c.events) > 0 {
		e := c.events[0]
		moved = &e
	}
	c.mu.Unlock()

	devices := map[string]string{}
	for _, id := range ids {
		if v, ok := cfg.WanView(id); ok {
			devices[id] = v.WanDevice()
		}
	}
	c.apply(ctx, devices, active, states)
	c.syncBalance(ctx, cfg, devices, states)
	if moved != nil && c.OnChange != nil {
		go c.OnChange(*moved)
	}
	if left != "" && left != active {
		// connections that were translated to the address of the connection that was left cannot go on: forget them, so that the devices
		// start again (through the new one) instead of waiting for a timeout
		c.forget(ctx, devices[left])
	}
}

// desired is the metric each connection's default route should have.
func desired(active string, states map[string]string, id string) int {
	switch {
	case id == active:
		return MetricActive
	case states[id] == StateOffline:
		return MetricDown
	}
	return MetricStandby
}

type route struct {
	via    string
	dev    string
	metric int
}

// parseRoutes reads `ip -4 -o route show default`.
func parseRoutes(out string) []route {
	var rs []route
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "default" {
			continue
		}
		var r route
		for i := 1; i+1 < len(f); i++ {
			switch f[i] {
			case "via":
				r.via = f[i+1]
			case "dev":
				r.dev = f[i+1]
			case "metric":
				r.metric, _ = strconv.Atoi(f[i+1])
			}
		}
		if r.dev != "" {
			rs = append(rs, r)
		}
	}
	return rs
}

func routeArgs(verb string, r route, metric int) []string {
	args := []string{"-4", "route", verb, "default"}
	if r.via != "" {
		args = append(args, "via", r.via)
	}
	return append(args, "dev", r.dev, "metric", strconv.Itoa(metric))
}

// apply gives each connection's default route its metric: it adds the route with the metric wanted (from the gateway of the route there
// is) and takes the ones with other metrics away. A connection with no default route (no lease yet) is left alone.
func (c *Controller) apply(ctx context.Context, devices map[string]string, active string, states map[string]string) {
	out, err := c.Runner.Run(ctx, "ip", "-4", "-o", "route", "show", "default")
	if err != nil {
		return
	}
	routes := parseRoutes(out)
	for id, dev := range devices {
		want := desired(active, states, id)
		var mine []route
		for _, r := range routes {
			if r.dev == dev {
				mine = append(mine, r)
			}
		}
		if len(mine) == 0 {
			continue
		}
		have := false
		for _, r := range mine {
			if r.metric == want {
				have = true
			}
		}
		if !have {
			if _, err := c.Runner.Run(ctx, "ip", routeArgs("replace", mine[0], want)...); err != nil {
				log.Printf("[failover] could not set the route of %s to metric %d: %v", dev, want, err)
				continue
			}
		}
		for _, r := range mine {
			if r.metric != want {
				c.Runner.Run(ctx, "ip", routeArgs("del", r, r.metric)...)
			}
		}
	}
}

// forget ends the connections that were translated to the address of the device that was left.
func (c *Controller) forget(ctx context.Context, dev string) {
	if dev == "" {
		return
	}
	out, err := c.Runner.Run(ctx, "ip", "-4", "-o", "addr", "show", "dev", dev)
	if err != nil {
		return
	}
	for _, ip := range addrsOf(out) {
		c.Runner.Run(ctx, "conntrack", "-D", "--reply-dst", ip.String()) // exit status 1: there were none
	}
}

// addrsOf reads the IPv4 addresses of `ip -4 -o addr show`.
func addrsOf(out string) []netip.Addr {
	var as []netip.Addr
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "inet" {
				if p, err := netip.ParsePrefix(f[i+1]); err == nil {
					as = append(as, p.Addr())
				} else if a, err := netip.ParseAddr(f[i+1]); err == nil {
					as = append(as, a)
				}
			}
		}
	}
	return as
}

// Pin switches the traffic to a connection by hand ("wan" or "wan2"), or back to automatic ("": the settings decide again). The
// connection is the preferred one from now on, at once and without waiting for a recovery time; if it is down the traffic still goes
// to the other. It is not kept over a restart of the daemon.
func (c *Controller) Pin(id string) {
	c.mu.Lock()
	c.pinned, c.pinChanged = id, true
	c.mu.Unlock()
}

// Status describes what is happening now.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadEvents()
	st := Status{Active: c.active, Preferred: c.preferred, Pinned: c.pinned, States: map[string]string{}, Events: append([]Event{}, c.events...)}
	for k, v := range c.states {
		st.States[k] = v
	}
	if st.Active == "" {
		st.Active = st.Preferred
	}
	st.FailedOver = st.Active != "" && st.Preferred != "" && st.Active != st.Preferred
	if !c.since.IsZero() {
		t := c.since
		st.Since, st.Reason = &t, c.reason
	}
	return st
}

// RecoveringIn says how many seconds are left before the traffic goes back to the preferred connection (0: not waiting for that).
func (c *Controller) RecoveringIn(cfg config.NetworkConfig) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := cfg.FailoverSettings()
	pref := preferredOf(cfg)
	if !set.Failback || c.active == "" || c.active == pref || c.states[pref] != StateOnline {
		return 0
	}
	t, ok := c.upSince[pref]
	if !ok {
		return 0
	}
	left := set.Recovery - c.now().Sub(t)
	if left <= 0 {
		return 0
	}
	return int(left/time.Second) + 1
}

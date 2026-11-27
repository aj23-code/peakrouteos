package config

import (
	"fmt"
	"net/netip"
	"time"
)

// How an internet connection is watched (docs/wan-failover.md, section 4). A connection is probed through itself: every few seconds a ping
// goes to each target, and the connection is down when every target that counts has failed several times in a row. The targets are the
// connection's upstream DNS servers (as they are now), the ones the administrator adds, and, unless turned off, two public addresses.

// Limits and defaults.
const (
	DefaultProbeEvery = 5 // seconds between probes
	DefaultDownAfter  = 3 // failed probes in a row before a target counts as down
	MinProbeEvery     = 2
	MaxProbeEvery     = 60
	MinDownAfter      = 2
	MaxDownAfter      = 20
	MaxProbeTargets   = 8
)

// PublicProbeTargets are the addresses a connection is probed against besides its own (Cloudflare's and Quad9's resolvers).
var PublicProbeTargets = []string{"1.1.1.1", "9.9.9.9"}

// ProbeConfig is one connection's health check settings; every field has a default, and nothing is written for the defaults.
type ProbeConfig struct {
	Targets     []string `json:"targets,omitempty"`      // addresses the administrator added (IPv4)
	NoFallbacks bool     `json:"no_fallbacks,omitempty"` // do not use the public addresses
	Every       int      `json:"every_seconds,omitempty"`
	DownAfter   int      `json:"down_after,omitempty"`
}

func (p ProbeConfig) clone() ProbeConfig {
	p.Targets = append([]string(nil), p.Targets...)
	return p
}

// ProbeSettings are a connection's health check settings with the defaults filled in.
type ProbeSettings struct {
	Targets   []netip.Addr
	Fallbacks bool
	Every     time.Duration
	DownAfter int
}

// ProbeSettings returns the health check settings of the connection this view is of.
func (c NetworkConfig) ProbeSettings() ProbeSettings {
	s := ProbeSettings{Fallbacks: true, Every: DefaultProbeEvery * time.Second, DownAfter: DefaultDownAfter}
	p := c.WanProbe
	if p == nil {
		return s
	}
	for _, t := range p.Targets {
		if a, err := netip.ParseAddr(t); err == nil && a.Is4() {
			s.Targets = append(s.Targets, a)
		}
	}
	s.Fallbacks = !p.NoFallbacks
	if p.Every > 0 {
		s.Every = time.Duration(p.Every) * time.Second
	}
	if p.DownAfter > 0 {
		s.DownAfter = p.DownAfter
	}
	return s
}

// ProbeText describes the settings in a line, for the review of changes.
func (c NetworkConfig) ProbeText() string {
	s := c.ProbeSettings()
	text := fmt.Sprintf("every %d s, down after %d", int(s.Every/time.Second), s.DownAfter)
	if len(s.Targets) > 0 {
		text += ", also " + list(addrStrings(s.Targets))
	}
	if !s.Fallbacks {
		text += ", without the public addresses"
	}
	return text
}

func addrStrings(a []netip.Addr) []string {
	out := make([]string, len(a))
	for i, x := range a {
		out[i] = x.String()
	}
	return out
}

// validateProbe checks the health check settings of the connection this view is of (the field names are the first connection's;
// validateWan2 renames them).
func (c NetworkConfig) validateProbe(add func(field, format string, args ...any)) {
	p := c.WanProbe
	if p == nil {
		return
	}
	if len(p.Targets) > MaxProbeTargets {
		add("wan_probe.targets", "at most %d addresses can be added", MaxProbeTargets)
	}
	seen := map[netip.Addr]bool{}
	for _, t := range p.Targets {
		a, err := netip.ParseAddr(t)
		switch {
		case err != nil || !a.Is4():
			add("wan_probe.targets", "%q is not an IPv4 address such as 8.8.8.8", t)
		case a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
			add("wan_probe.targets", "%s cannot be used: it is not an address on the internet", t)
		case seen[a]:
			add("wan_probe.targets", "%s is listed twice", t)
		}
		seen[a] = true
	}
	if p.Every != 0 && (p.Every < MinProbeEvery || p.Every > MaxProbeEvery) {
		add("wan_probe.every_seconds", "must be between %d and %d seconds", MinProbeEvery, MaxProbeEvery)
	}
	if p.DownAfter != 0 && (p.DownAfter < MinDownAfter || p.DownAfter > MaxDownAfter) {
		add("wan_probe.down_after", "must be between %d and %d", MinDownAfter, MaxDownAfter)
	}
}

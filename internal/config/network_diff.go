package config

import (
	"fmt"
	"strings"
)

// Change is one user-visible difference between two network configurations.
type Change struct {
	Field string `json:"field"` // JSON key, e.g. "lan_ip"
	Label string `json:"label"` // human-readable name for the review screen
	Old   string `json:"old"`
	New   string `json:"new"`
}

type networkSetting struct {
	field, label string
	value        func(NetworkConfig) string
}

const (
	valueNone   = "(none)"
	valueNotSet = "(not set)"
)

func text(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return valueNone
	}
	return s
}

func list(items []string) string {
	if len(items) == 0 {
		return valueNone
	}
	return strings.Join(items, ", ")
}

func dhcpValue(c NetworkConfig, get func(*DHCPConfig) string) string {
	if c.DHCP == nil {
		return valueNotSet
	}
	return get(c.DHCP)
}

// The settings a person can change, in the order they are shown. Deliberately absent: the
// discovered hardware inventory (Interfaces), which is detected rather than chosen, and the
// unused managed-switch placeholders. A NIC being plugged in is not a pending change.
var networkSettings = []networkSetting{
	{"wan_interface", "WAN interface", func(c NetworkConfig) string { return text(c.WanInterface) }},
	{"wan_mode", "WAN addressing", func(c NetworkConfig) string {
		if m := strings.ToLower(strings.TrimSpace(c.WanMode)); m != "" {
			return m
		}
		return "dhcp" // the default when unset
	}},
	{"wan_ip", "WAN IP address", func(c NetworkConfig) string { return text(c.WanIP) }},
	{"wan_subnet", "WAN netmask", func(c NetworkConfig) string { return text(c.WanSubnet) }},
	{"wan_gateway", "WAN gateway", func(c NetworkConfig) string { return text(c.WanGateway) }},
	{"wan_dns", "WAN DNS servers", func(c NetworkConfig) string { return list(c.WanDNS) }},
	{"lan_interfaces", "LAN ports", func(c NetworkConfig) string { return list(c.LanInterfaces) }},
	{"lan_ip", "LAN IP address", func(c NetworkConfig) string { return text(c.LanIP) }},
	{"lan_subnet", "LAN netmask", func(c NetworkConfig) string { return text(c.LanSubnet) }},
	{"dhcp_enabled", "DHCP server", func(c NetworkConfig) string {
		return dhcpValue(c, func(d *DHCPConfig) string {
			if d.Enabled {
				return "enabled"
			}
			return "disabled"
		})
	}},
	{"dhcp_range_start", "DHCP range start", func(c NetworkConfig) string {
		return dhcpValue(c, func(d *DHCPConfig) string { return text(d.RangeStart) })
	}},
	{"dhcp_range_end", "DHCP range end", func(c NetworkConfig) string {
		return dhcpValue(c, func(d *DHCPConfig) string { return text(d.RangeEnd) })
	}},
	{"dhcp_lease_time", "DHCP lease time", func(c NetworkConfig) string {
		return dhcpValue(c, func(d *DHCPConfig) string { return text(d.LeaseTime) })
	}},
}

// DiffNetwork lists what differs between the active configuration and a candidate, in a
// stable order. It returns nil when there is nothing to commit.
func DiffNetwork(active, candidate NetworkConfig) []Change {
	var changes []Change
	for _, s := range networkSettings {
		before, after := s.value(active), s.value(candidate)
		if before != after {
			changes = append(changes, Change{Field: s.field, Label: s.label, Old: before, New: after})
		}
	}
	return append(changes, diffRoutes(active.Routes, candidate.Routes)...)
}

// routeText describes what a route does, for the review screen.
func routeText(r Route) string {
	out := "via " + text(r.Gateway)
	if r.Metric > 0 {
		out += fmt.Sprintf(", metric %d", r.Metric)
	}
	if r.Disabled {
		out += " (turned off)"
	}
	if n := strings.TrimSpace(r.Name); n != "" {
		out += " [" + n + "]"
	}
	return out
}

// diffRoutes reports one change per route that was added, removed or changed. A route is the
// same route while its destination and metric stay the same; the rest of it can change.
func diffRoutes(active, candidate []Route) []Change {
	key := func(r Route) string { return fmt.Sprintf("%s|%d", strings.TrimSpace(r.Destination), r.Metric) }
	label := func(r Route) string {
		l := "Static route to " + text(r.Destination)
		if r.Metric > 0 {
			l += fmt.Sprintf(" (metric %d)", r.Metric)
		}
		return l
	}

	before := map[string]Route{}
	for _, r := range active {
		before[key(r)] = r
	}

	var changes []Change
	seen := map[string]bool{}
	for _, r := range candidate {
		k := key(r)
		seen[k] = true
		old, existed := before[k]
		switch {
		case !existed:
			changes = append(changes, Change{Field: "routes", Label: label(r), Old: valueNone, New: routeText(r)})
		case old != r:
			changes = append(changes, Change{Field: "routes", Label: label(r), Old: routeText(old), New: routeText(r)})
		}
	}
	for _, r := range active {
		if !seen[key(r)] {
			changes = append(changes, Change{Field: "routes", Label: label(r), Old: routeText(r), New: "(removed)"})
		}
	}
	return changes
}

// StagedChanges returns what committing the candidate would change (nil without a candidate).
func (m *Manager) StagedChanges() []Change {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.hasStaged {
		return nil
	}
	return DiffNetwork(m.network, m.staged)
}

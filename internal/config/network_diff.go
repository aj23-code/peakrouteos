package config

import (
	"fmt"
	"sort"
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
	{"lan_isolated", "Default network isolation", func(c NetworkConfig) string {
		if c.LanIsolated {
			return "isolated from other networks"
		}
		return "not isolated"
	}},
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
	changes = append(changes, diffWifi(active, candidate)...)
	changes = append(changes, diffNetworks(active, candidate)...)
	changes = append(changes, diffPortNetworks(active, candidate)...)
	return append(changes, diffRoutes(active.Routes, candidate.Routes)...)
}

func onOff(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

// networkText describes an additional network on one line, for the review screen.
func networkText(n Network) string {
	dhcp := "no DHCP"
	if d := n.DHCP; d != nil && d.Enabled {
		dhcp = fmt.Sprintf("DHCP %s to %s, lease %s", d.RangeStart, d.RangeEnd, d.LeaseTime)
	}
	return fmt.Sprintf("VLAN %d, router %s / %s, %s, %s, %s", n.VLAN, text(n.IP), text(n.Netmask), dhcp,
		onOff(n.Internet, "internet allowed", "no internet"), onOff(n.Isolated, "isolated from other networks", "can reach other networks"))
}

func networkLabel(n Network) string {
	if name := strings.TrimSpace(n.Name); name != "" {
		return "Network " + name
	}
	return "Network " + n.ID
}

// diffNetworks reports one change per additional network that was added, removed or changed. A
// network is the same network while its id stays the same, so renaming one is a change, not a
// removal and an addition.
func diffNetworks(active, candidate NetworkConfig) []Change {
	before := map[string]Network{}
	for _, n := range active.Networks {
		before[n.ID] = n
	}
	var changes []Change
	seen := map[string]bool{}
	for _, n := range candidate.Networks {
		seen[n.ID] = true
		old, existed := before[n.ID]
		switch {
		case !existed:
			changes = append(changes, Change{Field: "networks", Label: networkLabel(n), Old: valueNone, New: networkText(n)})
		case networkText(old) != networkText(n) || strings.TrimSpace(old.Name) != strings.TrimSpace(n.Name):
			label := networkLabel(n)
			if oldName := strings.TrimSpace(old.Name); oldName != strings.TrimSpace(n.Name) {
				label = networkLabel(old) + " (now " + strings.TrimSpace(n.Name) + ")"
			}
			changes = append(changes, Change{Field: "networks", Label: label, Old: networkText(old), New: networkText(n)})
		}
	}
	for _, n := range active.Networks {
		if !seen[n.ID] {
			changes = append(changes, Change{Field: "networks", Label: networkLabel(n), Old: networkText(n), New: "(removed)"})
		}
	}
	return changes
}

// portNetworksText says which networks a LAN port carries, using the network names.
func portNetworksText(c NetworkConfig, port string) string {
	name := func(id string) string {
		if id == DefaultNetworkID {
			return "LAN (default)"
		}
		if n, ok := c.FindNetwork(id); ok && strings.TrimSpace(n.Name) != "" {
			return strings.TrimSpace(n.Name)
		}
		return id
	}
	pn := c.PortNetworks[port]
	out := "native " + name(pn.NativeID())
	if len(pn.Tagged) > 0 {
		tagged := make([]string, len(pn.Tagged))
		for i, t := range pn.Tagged {
			tagged[i] = name(t)
		}
		sort.Strings(tagged)
		out += ", tagged " + strings.Join(tagged, ", ")
	}
	return out
}

// diffPortNetworks reports a change for each LAN port whose networks differ. A port that stops being
// a LAN port is the Ports page's business (its role); one that becomes a LAN port is shown with the
// networks it will carry.
func diffPortNetworks(active, candidate NetworkConfig) []Change {
	isLan := func(c NetworkConfig, p string) bool {
		for _, l := range c.LanInterfaces {
			if l == p {
				return true
			}
		}
		return false
	}
	ports := map[string]bool{}
	for p := range active.PortNetworks {
		ports[p] = true
	}
	for p := range candidate.PortNetworks {
		ports[p] = true
	}
	names := make([]string, 0, len(ports))
	for p := range ports {
		names = append(names, p)
	}
	sort.Strings(names)

	var changes []Change
	for _, p := range names {
		if !isLan(candidate, p) {
			continue
		}
		before := "(not a LAN port)"
		if isLan(active, p) {
			before = portNetworksText(active, p)
		}
		if after := portNetworksText(candidate, p); before != after {
			changes = append(changes, Change{Field: "port_networks", Label: "Networks on port " + p, Old: before, New: after})
		}
	}
	return changes
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

// ---- Wi-Fi -----------------------------------------------------------------------------------------
//
// The review shows what changes, never a passphrase: a changed one is reported as "new password".

func radioText(r WifiRadio) string {
	channel := "automatic channel"
	if r.Channel != 0 {
		channel = fmt.Sprintf("channel %d", r.Channel)
	}
	out := fmt.Sprintf("%s, %s, %d MHz", r.Band, channel, r.Width)
	if r.Disabled {
		out += " (switched off)"
	}
	return out
}

func localNetworkName(c NetworkConfig, id string) string {
	if id == DefaultNetworkID {
		return "LAN"
	}
	if n, ok := c.FindNetwork(id); ok && strings.TrimSpace(n.Name) != "" {
		return strings.TrimSpace(n.Name)
	}
	return id
}

func wifiNetworkText(c NetworkConfig, n WifiNetwork) string {
	security := map[string]string{SecurityWPA2: "WPA2", SecurityWPA3: "WPA3", SecurityWPA2WPA3: "WPA2/WPA3"}[n.Security]
	if security == "" {
		security = text(n.Security)
	}
	parts := []string{"joins " + localNetworkName(c, n.Network), security, strings.Join(n.Bands, " + ")}
	if n.BandSteering {
		parts = append(parts, "band steering")
	}
	if n.Hidden {
		parts = append(parts, "hidden")
	}
	if n.ClientIsolation {
		parts = append(parts, "devices isolated from each other")
	}
	if n.Disabled {
		parts = append(parts, "switched off")
	}
	return strings.Join(parts, ", ")
}

func diffWifi(active, candidate NetworkConfig) []Change {
	a, b := active.WifiSettings(), candidate.WifiSettings()
	var changes []Change

	if a.Country != b.Country {
		changes = append(changes, Change{Field: "wifi_country", Label: "Wi-Fi country", Old: text(a.Country), New: text(b.Country)})
	}

	// adapters, by name
	before := map[string]WifiRadio{}
	for _, r := range a.Radios {
		before[r.Name] = r
	}
	seen := map[string]bool{}
	for _, r := range b.Radios {
		seen[r.Name] = true
		old, existed := before[r.Name]
		switch {
		case !existed:
			changes = append(changes, Change{Field: "wifi_radios", Label: "Wi-Fi adapter " + r.Name, Old: valueNone, New: radioText(r)})
		case old != r:
			changes = append(changes, Change{Field: "wifi_radios", Label: "Wi-Fi adapter " + r.Name, Old: radioText(old), New: radioText(r)})
		}
	}
	for _, r := range a.Radios {
		if !seen[r.Name] {
			changes = append(changes, Change{Field: "wifi_radios", Label: "Wi-Fi adapter " + r.Name, Old: radioText(r), New: "(removed)"})
		}
	}

	// Wi-Fi networks, by id
	prev := map[string]WifiNetwork{}
	for _, n := range a.Networks {
		prev[n.ID] = n
	}
	seenNet := map[string]bool{}
	label := func(n WifiNetwork) string { return "Wi-Fi network " + text(n.SSID) }
	for _, n := range b.Networks {
		seenNet[n.ID] = true
		old, existed := prev[n.ID]
		if !existed {
			changes = append(changes, Change{Field: "wifi_networks", Label: label(n), Old: valueNone, New: wifiNetworkText(candidate, n) + ", password set"})
			continue
		}
		oldText, newText := wifiNetworkText(active, old), wifiNetworkText(candidate, n)
		passwordChanged := old.Passphrase != n.Passphrase
		if oldText == newText && !passwordChanged && old.SSID == n.SSID {
			continue
		}
		l := label(n)
		if old.SSID != n.SSID {
			l = label(old) + " (now " + text(n.SSID) + ")"
		}
		if passwordChanged {
			newText += ", new password"
		}
		changes = append(changes, Change{Field: "wifi_networks", Label: l, Old: oldText, New: newText})
	}
	for _, n := range a.Networks {
		if !seenNet[n.ID] {
			changes = append(changes, Change{Field: "wifi_networks", Label: label(n), Old: wifiNetworkText(active, n), New: "(removed)"})
		}
	}
	return changes
}

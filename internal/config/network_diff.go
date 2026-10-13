package config

import (
	"fmt"
	"sort"
	"strconv"
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
	{"hostname", "Host name", func(c NetworkConfig) string { return text(c.Hostname) }},
	{"send_hostname", "Host name sent to the provider", func(c NetworkConfig) string { return onOff(c.SendHostname, "yes (with DHCP)", "no") }},
	{"timezone", "Time zone", func(c NetworkConfig) string {
		if c.Timezone == "" {
			return "not set (the machine keeps its own)"
		}
		return c.Timezone
	}},
	{"ntp_disabled", "Time from time servers", func(c NetworkConfig) string { return onOff(c.NTPDisabled, "off", "on") }},
	{"ntp_servers", "Time servers", func(c NetworkConfig) string {
		if len(c.NTPServers) == 0 {
			return "the default (" + strings.Join(DefaultNTPServers, ", ") + ")"
		}
		return strings.Join(c.NTPServers, ", ")
	}},
	{"syslog_servers", "Syslog servers", func(c NetworkConfig) string { return c.SyslogText() }},
	{"syslog_sends", "Syslog sends", func(c NetworkConfig) string {
		if len(c.SyslogSettings().Servers) == 0 {
			return valueNone
		}
		return c.SyslogSendsText()
	}},
	{"country", "Country", func(c NetworkConfig) string { return text(c.Country) }},
	{"domain", "Local domain", func(c NetworkConfig) string { return text(c.Domain) }},
	{"lan_dns_servers", "DNS told to the default network", func(c NetworkConfig) string { return serversText(c.LanDNSServers) }},
}

// The internet connection's carrier settings (VLAN, MTU, MAC, host name, PPPoE), shown right after the
// addressing lines. The PPPoE password is handled apart (diffWanSecret): it is never shown.
var wanCarrierSettings = []networkSetting{
	{"wan_vlan", "WAN VLAN", func(c NetworkConfig) string {
		if c.WanVlan == 0 {
			return "none"
		}
		return strconv.Itoa(c.WanVlan)
	}},
	{"wan_mtu", "WAN MTU", func(c NetworkConfig) string { return strconv.Itoa(c.EffectiveWanMTU()) }},
	{"wan_mac", "WAN MAC address", func(c NetworkConfig) string {
		if c.WanMAC == "" {
			return "the port's own"
		}
		return c.WanMAC
	}},
	{"pppoe_username", "PPPoE user name", func(c NetworkConfig) string {
		if c.WanPPPoE == nil {
			return valueNone
		}
		return text(c.WanPPPoE.Username)
	}},
	{"pppoe_service", "PPPoE service name", func(c NetworkConfig) string {
		if c.WanPPPoE == nil {
			return valueNone
		}
		return text(c.WanPPPoE.Service)
	}},
}

// diffWanSecret reports a change of the PPPoE password without showing either value.
func diffWanSecret(active, candidate NetworkConfig) []Change {
	pw := func(c NetworkConfig) string {
		if c.WanPPPoE == nil {
			return ""
		}
		return c.WanPPPoE.Password
	}
	a, b := pw(active), pw(candidate)
	if a == b {
		return nil
	}
	change := Change{Field: "pppoe_password", Label: "PPPoE password", Old: "set", New: "changed"}
	if a == "" {
		change.Old, change.New = valueNone, "set"
	} else if b == "" {
		change.New = valueNone
	}
	return []Change{change}
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
	for _, s := range wanCarrierSettings {
		if before, after := s.value(active), s.value(candidate); before != after {
			changes = append(changes, Change{Field: s.field, Label: s.label, Old: before, New: after})
		}
	}
	changes = append(changes, diffWanSecret(active, candidate)...)
	changes = append(changes, diffAP(active, candidate)...)
	changes = append(changes, diffWifi(active, candidate)...)
	changes = append(changes, diffStations(active, candidate)...)
	changes = append(changes, diffFirewall(active, candidate)...)
	changes = append(changes, diffDNS(active, candidate)...)
	changes = append(changes, diffDDNS(active, candidate)...)
	changes = append(changes, diffVPN(active, candidate)...)
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
	t := fmt.Sprintf("VLAN %d, router %s / %s, %s, %s, %s", n.VLAN, text(n.IP), text(n.Netmask), dhcp,
		onOff(n.Internet, "internet allowed", "no internet"), onOff(n.Isolated, "isolated from other networks", "can reach other networks"))
	if len(n.DNSServers) > 0 {
		t += ", DNS " + strings.Join(n.DNSServers, ", ")
	}
	return t
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
	if n.WDS {
		parts = append(parts, "accepts point-to-point links")
	}
	if n.Disabled {
		parts = append(parts, "switched off")
	}
	return strings.Join(parts, ", ")
}

func diffWifi(active, candidate NetworkConfig) []Change {
	a, b := active.WifiSettings(), candidate.WifiSettings()
	var changes []Change

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

func stationText(c NetworkConfig, st WifiStation) string {
	security := map[string]string{SecurityWPA2: "WPA2", SecurityWPA3: "WPA3", SecurityWPA2WPA3: "WPA2/WPA3"}[st.Security]
	if security == "" {
		security = text(st.Security)
	}
	role := "internet connection"
	if st.Role == StationBridge {
		role = "link into " + localNetworkName(c, st.Network)
	}
	parts := []string{"joins " + text(st.SSID), security, role}
	if st.Hidden {
		parts = append(parts, "hidden network")
	}
	if st.Disabled {
		parts = append(parts, "switched off")
	}
	return strings.Join(parts, ", ")
}

func diffStations(active, candidate NetworkConfig) []Change {
	a, b := active.WifiSettings(), candidate.WifiSettings()
	var changes []Change
	label := func(st WifiStation) string { return "Wi-Fi connection " + st.Name }
	before := map[string]WifiStation{}
	for _, st := range a.Stations {
		before[st.Name] = st
	}
	seen := map[string]bool{}
	for _, st := range b.Stations {
		seen[st.Name] = true
		old, existed := before[st.Name]
		if !existed {
			changes = append(changes, Change{Field: "wifi_stations", Label: label(st), Old: valueNone, New: stationText(candidate, st) + ", password set"})
			continue
		}
		oldText, newText := stationText(active, old), stationText(candidate, st)
		if oldText == newText && old.Passphrase == st.Passphrase {
			continue
		}
		if old.Passphrase != st.Passphrase {
			newText += ", new password"
		}
		changes = append(changes, Change{Field: "wifi_stations", Label: label(st), Old: oldText, New: newText})
	}
	for _, st := range a.Stations {
		if !seen[st.Name] {
			changes = append(changes, Change{Field: "wifi_stations", Label: label(st), Old: stationText(active, st), New: "(removed)"})
		}
	}
	return changes
}

// diffFirewall describes rule and port forward changes in words: one line per rule added, removed or changed
// (a rename is one change), and one line when the order of the rules changed.
func diffFirewall(active, candidate NetworkConfig) []Change {
	a, b := active.FirewallSettings(), candidate.FirewallSettings()
	var changes []Change

	oldRules := map[string]FirewallRule{}
	for _, r := range a.Rules {
		oldRules[r.ID] = r
	}
	newRules := map[string]FirewallRule{}
	for _, r := range b.Rules {
		newRules[r.ID] = r
	}
	for _, r := range b.Rules {
		old, had := oldRules[r.ID]
		label := "Firewall rule " + r.Name
		switch {
		case !had:
			changes = append(changes, Change{Field: "firewall_rules", Label: label, Old: valueNone, New: candidate.RuleText(r)})
		case old != r:
			before := active.RuleText(old)
			if old.Name != r.Name {
				before = fmt.Sprintf("%q: %s", old.Name, before)
			}
			changes = append(changes, Change{Field: "firewall_rules", Label: label, Old: before, New: candidate.RuleText(r)})
		}
	}
	for _, r := range a.Rules {
		if _, kept := newRules[r.ID]; !kept {
			changes = append(changes, Change{Field: "firewall_rules", Label: "Firewall rule " + r.Name, Old: active.RuleText(r), New: "(removed)"})
		}
	}
	// the order of the rules that exist both before and after
	var before, after []string
	for _, r := range a.Rules {
		if _, kept := newRules[r.ID]; kept {
			before = append(before, r.ID)
		}
	}
	for _, r := range b.Rules {
		if _, had := oldRules[r.ID]; had {
			after = append(after, r.ID)
		}
	}
	if strings.Join(before, ",") != strings.Join(after, ",") {
		names := func(ids []string, c NetworkConfig) string {
			var out []string
			for _, id := range ids {
				if r, ok := c.FirewallSettings().FindRule(id); ok {
					out = append(out, r.Name)
				}
			}
			return strings.Join(out, ", then ")
		}
		changes = append(changes, Change{Field: "firewall_order", Label: "Firewall rule order", Old: names(before, active), New: names(after, candidate)})
	}

	oldFwd := map[string]PortForward{}
	for _, p := range a.Forwards {
		oldFwd[p.ID] = p
	}
	newFwd := map[string]PortForward{}
	for _, p := range b.Forwards {
		newFwd[p.ID] = p
	}
	for _, p := range b.Forwards {
		old, had := oldFwd[p.ID]
		label := "Port forward " + p.Name
		switch {
		case !had:
			changes = append(changes, Change{Field: "firewall_forwards", Label: label, Old: valueNone, New: candidate.ForwardText(p)})
		case !portForwardEqual(old, p):
			before := active.ForwardText(old)
			if old.Name != p.Name {
				before = fmt.Sprintf("%q: %s", old.Name, before)
			}
			changes = append(changes, Change{Field: "firewall_forwards", Label: label, Old: before, New: candidate.ForwardText(p)})
		}
	}
	for _, p := range a.Forwards {
		if _, kept := newFwd[p.ID]; !kept {
			changes = append(changes, Change{Field: "firewall_forwards", Label: "Port forward " + p.Name, Old: active.ForwardText(p), New: "(removed)"})
		}
	}
	return changes
}

func portForwardEqual(a, b PortForward) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Disabled == b.Disabled && a.Protocol == b.Protocol &&
		a.ExternalPorts == b.ExternalPorts && a.ToIP == b.ToIP && a.InternalPorts == b.InternalPorts &&
		strings.Join(a.Sources, ",") == strings.Join(b.Sources, ",")
}

// diffDNS describes local name and forwarded domain changes in words: one line per entry added, removed or changed.
func diffDNS(active, candidate NetworkConfig) []Change {
	a, b := active.DNSSettings(), candidate.DNSSettings()
	var changes []Change

	oldHosts := map[string]DNSHost{}
	for _, h := range a.Hosts {
		oldHosts[h.ID] = h
	}
	newHosts := map[string]bool{}
	for _, h := range b.Hosts {
		newHosts[h.ID] = true
		old, had := oldHosts[h.ID]
		switch {
		case !had:
			changes = append(changes, Change{Field: "dns_hosts", Label: "Local name " + h.Name, Old: valueNone, New: HostText(h)})
		case old != h:
			before := HostText(old)
			if old.Name != h.Name {
				before = fmt.Sprintf("%q: %s", old.Name, before)
			}
			changes = append(changes, Change{Field: "dns_hosts", Label: "Local name " + h.Name, Old: before, New: HostText(h)})
		}
	}
	for _, h := range a.Hosts {
		if !newHosts[h.ID] {
			changes = append(changes, Change{Field: "dns_hosts", Label: "Local name " + h.Name, Old: HostText(h), New: "(removed)"})
		}
	}

	oldFwd := map[string]DNSForward{}
	for _, f := range a.Forwards {
		oldFwd[f.ID] = f
	}
	newFwd := map[string]bool{}
	for _, f := range b.Forwards {
		newFwd[f.ID] = true
		old, had := oldFwd[f.ID]
		switch {
		case !had:
			changes = append(changes, Change{Field: "dns_forwards", Label: "Domain forwarding " + f.Domain, Old: valueNone, New: DNSForwardText(f)})
		case old.Domain != f.Domain || old.Disabled != f.Disabled || strings.Join(old.Servers, ",") != strings.Join(f.Servers, ","):
			changes = append(changes, Change{Field: "dns_forwards", Label: "Domain forwarding " + f.Domain, Old: DNSForwardText(old), New: DNSForwardText(f)})
		}
	}
	for _, f := range a.Forwards {
		if !newFwd[f.ID] {
			changes = append(changes, Change{Field: "dns_forwards", Label: "Domain forwarding " + f.Domain, Old: DNSForwardText(f), New: "(removed)"})
		}
	}
	return changes
}

// diffDDNS describes Dynamic DNS changes in words: one line per name added, removed or changed. A password is never shown: a
// change of it is "set" or "changed".
func diffDDNS(active, candidate NetworkConfig) []Change {
	a, b := active.DDNSSettings(), candidate.DDNSSettings()
	var changes []Change
	label := func(e DDNSEntry) string { return "Dynamic DNS " + e.Hostname }
	same := func(x, y DDNSEntry) bool { x.Password, y.Password = "", ""; return x == y }
	newIDs := map[string]bool{}
	for _, e := range b.Entries {
		newIDs[e.ID] = true
		old, had := a.FindDDNS(e.ID)
		switch {
		case !had:
			changes = append(changes, Change{Field: "ddns", Label: label(e), Old: valueNone, New: DDNSText(e)})
		case !same(old, e):
			before := DDNSText(old)
			if old.Hostname != e.Hostname {
				before = fmt.Sprintf("%q: %s", old.Hostname, before)
			}
			changes = append(changes, Change{Field: "ddns", Label: label(e), Old: before, New: DDNSText(e)})
		}
		if had && old.Password != e.Password {
			change := Change{Field: "ddns_password", Label: "Dynamic DNS password of " + e.Hostname, Old: "set", New: "changed"}
			if old.Password == "" {
				change.Old, change.New = valueNone, "set"
			} else if e.Password == "" {
				change.New = valueNone
			}
			changes = append(changes, change)
		}
	}
	for _, e := range a.Entries {
		if !newIDs[e.ID] {
			changes = append(changes, Change{Field: "ddns", Label: label(e), Old: DDNSText(e), New: "(removed)"})
		}
	}
	return changes
}

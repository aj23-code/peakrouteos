package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The firewall's rules and port forwards. The model, the order rules are applied in and the reasons for them are
// written down in docs/firewall.md; this file is the stored form, its validation and the words used to describe it.

// Where traffic comes from or goes to, besides a Local Network's id.
const (
	FromWAN    = "wan"      // traffic arriving from the internet
	ToAny      = "any"      // this router and everything behind it
	ToRouter   = "router"   // this router itself
	ToInternet = "internet" // out through the internet connection
)

// What a rule does with the traffic it matches.
const (
	ActionAllow  = "allow"
	ActionBlock  = "block"  // drop silently
	ActionReject = "reject" // refuse: the sender is told at once
)

// Protocols a rule can match.
const (
	ProtoAny    = "any"
	ProtoTCP    = "tcp"
	ProtoUDP    = "udp"
	ProtoTCPUDP = "tcp+udp"
	ProtoICMP   = "icmp"
)

const (
	MaxFirewallRules    = 200
	MaxPortForwards     = 50
	maxRuleName         = 60
	maxPortEntries      = 15
	MaxForwardSources   = 10
	maxPortForwardPorts = 1
)

// FirewallRule is one rule. It belongs to the place traffic comes from (From), and says where it goes (To). Rules
// are applied top to bottom within their source: the first match decides.
type FirewallRule struct {
	ID          string `json:"id"` // fw1, fw2...
	Name        string `json:"name"`
	Disabled    bool   `json:"disabled,omitempty"`
	From        string `json:"from"`                  // a Local Network's id, or "wan"
	Action      string `json:"action"`                // allow, block or reject
	Protocol    string `json:"protocol"`              // any, tcp, udp, tcp+udp or icmp
	Source      string `json:"source,omitempty"`      // an IPv4 address or range inside the From network; empty: any
	To          string `json:"to"`                    // any, router, internet, or a Local Network's id
	Destination string `json:"destination,omitempty"` // an IPv4 address or range; empty: any
	Ports       string `json:"ports,omitempty"`       // destination ports with tcp, udp or both: "80,443,8000-8100"
	Log         bool   `json:"log,omitempty"`
}

// PortForward sends connections that arrive from the internet on the router's ports to a device inside.
type PortForward struct {
	ID            string   `json:"id"` // pf1, pf2...
	Name          string   `json:"name"`
	Disabled      bool     `json:"disabled,omitempty"`
	Protocol      string   `json:"protocol"`                 // tcp, udp or tcp+udp
	ExternalPorts string   `json:"external_ports"`           // "443" or "5000-5010"
	ToIP          string   `json:"to_ip"`                    // a device inside one of the Local Networks
	InternalPorts string   `json:"internal_ports,omitempty"` // same count as the external ones; empty: the same numbers
	Sources       []string `json:"sources,omitempty"`        // only these addresses or ranges may use it; empty: anyone
}

// FirewallConfig is everything the Firewall pages edit.
type FirewallConfig struct {
	Rules    []FirewallRule `json:"rules,omitempty"`
	Forwards []PortForward  `json:"forwards,omitempty"`
}

func (f FirewallConfig) clone() FirewallConfig {
	f.Rules = append([]FirewallRule(nil), f.Rules...)
	if len(f.Forwards) > 0 {
		fw := make([]PortForward, len(f.Forwards))
		for i, p := range f.Forwards {
			p.Sources = append([]string(nil), p.Sources...)
			fw[i] = p
		}
		f.Forwards = fw
	} else {
		f.Forwards = nil
	}
	return f
}

// FirewallSettings returns the firewall's rules and forwards, empty when there are none.
func (c NetworkConfig) FirewallSettings() FirewallConfig {
	if c.Firewall == nil {
		return FirewallConfig{}
	}
	return *c.Firewall
}

// FindRule returns a rule by id.
func (f FirewallConfig) FindRule(id string) (FirewallRule, bool) {
	for _, r := range f.Rules {
		if r.ID == id {
			return r, true
		}
	}
	return FirewallRule{}, false
}

// FindForward returns a port forward by id.
func (f FirewallConfig) FindForward(id string) (PortForward, bool) {
	for _, p := range f.Forwards {
		if p.ID == id {
			return p, true
		}
	}
	return PortForward{}, false
}

// ---- networks as the firewall sees them ----------------------------------------------------------------------

// networkName is how a Local Network (or a VPN) is called in sentences.
func (c NetworkConfig) networkName(id string) string {
	if id == DefaultNetworkID {
		return "LAN"
	}
	if n, ok := c.FindNetwork(id); ok {
		return strings.TrimSpace(n.Name)
	}
	if z, ok := c.FindVPNZone(id); ok {
		return z.Name
	}
	return id
}

// ZonePrefixes returns the addresses behind a Local Network or a VPN (none for anything else).
func (c NetworkConfig) ZonePrefixes(id string) []netip.Prefix {
	if p, ok := c.NetworkPrefix(id); ok {
		return []netip.Prefix{p}
	}
	if z, ok := c.FindVPNZone(id); ok {
		return z.Prefixes
	}
	return nil
}

func prefixesContain(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func prefixesText(ps []netip.Prefix) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.String()
	}
	return strings.Join(parts, ", ")
}

// NetworkPrefix returns the addresses of a Local Network (the default one included).
func (c NetworkConfig) NetworkPrefix(id string) (netip.Prefix, bool) {
	ip, mask := c.LanIP, c.LanSubnet
	if id != DefaultNetworkID {
		n, ok := c.FindNetwork(id)
		if !ok {
			return netip.Prefix{}, false
		}
		ip, mask = n.IP, n.Netmask
	}
	addr, ok := parseIPv4(ip)
	ones, maskOK := parseNetmask(mask)
	if !ok || !maskOK {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(addr, ones).Masked(), true
}

func (c NetworkConfig) isNetwork(id string) bool {
	if id == DefaultNetworkID {
		return true
	}
	if _, ok := c.FindNetwork(id); ok {
		return true
	}
	_, ok := c.FindVPNZone(id)
	return ok
}

// ---- ports ----------------------------------------------------------------------------------------------------

// PortRange is a port, or a range of ports (Lo == Hi for one).
type PortRange struct{ Lo, Hi int }

func (r PortRange) String() string {
	if r.Lo == r.Hi {
		return strconv.Itoa(r.Lo)
	}
	return fmt.Sprintf("%d-%d", r.Lo, r.Hi)
}

// Count is how many ports the range holds.
func (r PortRange) Count() int { return r.Hi - r.Lo + 1 }

// ParsePorts reads "80", "80,443" or "8000-8100,22" into ranges, at most max of them.
func ParsePorts(s string, max int) ([]PortRange, error) {
	var out []PortRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("an empty entry; write ports like 80, 443 or 8000-8100")
		}
		lo, hi := part, part
		if i := strings.Index(part, "-"); i >= 0 {
			lo, hi = strings.TrimSpace(part[:i]), strings.TrimSpace(part[i+1:])
		}
		l, err1 := strconv.Atoi(lo)
		h, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || lo != strconv.Itoa(l) || hi != strconv.Itoa(h) {
			return nil, fmt.Errorf("%q is not a port or a range like 8000-8100", part)
		}
		if l < 1 || h > 65535 {
			return nil, fmt.Errorf("ports are between 1 and 65535")
		}
		if l > h {
			return nil, fmt.Errorf("the range %q goes downward", part)
		}
		out = append(out, PortRange{l, h})
	}
	if len(out) > max {
		return nil, fmt.Errorf("at most %d entries", max)
	}
	return out, nil
}

// NormalizePorts writes a port list in its canonical form (no spaces).
func NormalizePorts(s string) string {
	rs, err := ParsePorts(s, 1000)
	if err != nil {
		return s
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.String()
	}
	return strings.Join(parts, ",")
}

// ---- addresses ------------------------------------------------------------------------------------------------

// ParseAddrRange reads an IPv4 address or CIDR range; a bare address is a /32.
func ParseAddrRange(s string) (netip.Prefix, bool) {
	if s == "" || strings.TrimSpace(s) != s {
		return netip.Prefix{}, false
	}
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil || !p.Addr().Is4() || p.Addr().Zone() != "" {
			return netip.Prefix{}, false
		}
		if p.Masked() != p {
			return netip.Prefix{}, false // 192.168.1.5/24: the host bits must be zero
		}
		return p, true
	}
	a, ok := parseIPv4(s)
	if !ok {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, 32), true
}

var ruleIDPattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

func validName(s string) bool {
	if strings.TrimSpace(s) != s || s == "" || utf8.RuneCountInString(s) > maxRuleName {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

func validProtocol(p string, forward bool) bool {
	switch p {
	case ProtoTCP, ProtoUDP, ProtoTCPUDP:
		return true
	case ProtoAny, ProtoICMP:
		return !forward
	}
	return false
}

func hasPorts(protocol string) bool {
	return protocol == ProtoTCP || protocol == ProtoUDP || protocol == ProtoTCPUDP
}

// ---- validation -----------------------------------------------------------------------------------------------

// validateFirewall checks the rules and forwards. Errors are reported on firewall.rules[i].field,
// firewall.forwards[i].field, or firewall.rules / firewall.forwards for the list as a whole.
func (c NetworkConfig) validateFirewall(add func(field, format string, args ...any)) {
	if c.Firewall == nil {
		return
	}
	f := *c.Firewall

	if len(f.Rules) > MaxFirewallRules {
		add("firewall.rules", "at most %d rules are supported", MaxFirewallRules)
	}
	seen := map[string]int{}
	for i, r := range f.Rules {
		field := func(name string) string { return fmt.Sprintf("firewall.rules[%d].%s", i, name) }
		switch {
		case !ruleIDPattern.MatchString(r.ID):
			add(field("id"), "must be a short lower-case id such as fw1")
		case seen[r.ID] != 0:
			add(field("id"), "another rule already has this id")
		}
		seen[r.ID] = i + 1

		if !validName(r.Name) {
			add(field("name"), "give the rule a name of up to %d characters", maxRuleName)
		}
		switch r.Action {
		case ActionAllow, ActionBlock, ActionReject:
		default:
			add(field("action"), "must be allow, block or reject")
		}
		if !validProtocol(r.Protocol, false) {
			add(field("protocol"), "must be any, tcp, udp, tcp+udp or icmp")
		}

		fromOK := r.From == FromWAN || c.isNetwork(r.From)
		if !fromOK {
			add(field("from"), "must be the internet, one of your Local Networks or a VPN")
		}
		toOK := r.To == ToAny || r.To == ToRouter || r.To == ToInternet || c.isNetwork(r.To)
		if !toOK {
			add(field("to"), "must be anywhere, this router, the internet, one of your Local Networks or a VPN")
		}
		if fromOK && toOK {
			switch {
			case r.From == FromWAN && r.To == ToInternet:
				add(field("to"), "traffic from the internet cannot go back out to the internet")
			case r.From == r.To:
				add(field("to"), "a network's own traffic does not pass through the router, so a rule between a network and itself does nothing")
			}
		}

		if r.Source != "" {
			p, ok := ParseAddrRange(r.Source)
			switch {
			case !ok:
				add(field("source"), "must be an IPv4 address or range such as 192.168.1.20 or 192.168.1.0/28")
			case r.From != FromWAN && fromOK:
				if nps := c.ZonePrefixes(r.From); len(nps) > 0 && !prefixesContain(nps, p.Addr()) {
					add(field("source"), "is not inside the addresses of %s (%s)", c.networkName(r.From), prefixesText(nps))
				}
			}
		}
		if r.Destination != "" {
			p, ok := ParseAddrRange(r.Destination)
			switch {
			case !ok:
				add(field("destination"), "must be an IPv4 address or range such as 192.168.1.20 or 192.168.1.0/28")
			case c.isNetwork(r.To):
				if nps := c.ZonePrefixes(r.To); len(nps) > 0 && !prefixesContain(nps, p.Addr()) {
					add(field("destination"), "is not inside the addresses of %s (%s)", c.networkName(r.To), prefixesText(nps))
				}
			}
		}

		switch {
		case r.Ports != "" && !hasPorts(r.Protocol):
			add(field("ports"), "ports can only be used with tcp, udp or tcp+udp")
		case r.Ports != "":
			if _, err := ParsePorts(r.Ports, maxPortEntries); err != nil {
				add(field("ports"), "%v", err)
			} else if NormalizePorts(r.Ports) != r.Ports {
				add(field("ports"), "write the ports without spaces, like 80,443,8000-8100")
			}
		}
	}

	// port forwards
	if len(f.Forwards) > MaxPortForwards {
		add("firewall.forwards", "at most %d port forwards are supported", MaxPortForwards)
	}
	seenPF := map[string]int{}
	type claim struct {
		id    string
		proto string
		rng   PortRange
	}
	var claims []claim
	for i, p := range f.Forwards {
		field := func(name string) string { return fmt.Sprintf("firewall.forwards[%d].%s", i, name) }
		switch {
		case !ruleIDPattern.MatchString(p.ID):
			add(field("id"), "must be a short lower-case id such as pf1")
		case seenPF[p.ID] != 0:
			add(field("id"), "another port forward already has this id")
		}
		seenPF[p.ID] = i + 1

		if !validName(p.Name) {
			add(field("name"), "give the port forward a name of up to %d characters", maxRuleName)
		}
		if !validProtocol(p.Protocol, true) {
			add(field("protocol"), "must be tcp, udp or tcp+udp")
		}

		var ext, internal []PortRange
		if p.ExternalPorts == "" {
			add(field("external_ports"), "required")
		} else if rs, err := ParsePorts(p.ExternalPorts, maxPortForwardPorts); err != nil {
			add(field("external_ports"), "%v (a single port or one range)", err)
		} else if NormalizePorts(p.ExternalPorts) != p.ExternalPorts {
			add(field("external_ports"), "write the ports without spaces")
		} else {
			ext = rs
		}
		if p.InternalPorts != "" {
			if rs, err := ParsePorts(p.InternalPorts, maxPortForwardPorts); err != nil {
				add(field("internal_ports"), "%v (a single port or one range)", err)
			} else if NormalizePorts(p.InternalPorts) != p.InternalPorts {
				add(field("internal_ports"), "write the ports without spaces")
			} else {
				internal = rs
			}
		}
		if len(ext) == 1 && len(internal) == 1 {
			switch {
			case ext[0].Count() != internal[0].Count():
				add(field("internal_ports"), "must cover as many ports as the external ones (%d)", ext[0].Count())
			case ext[0].Count() > 1 && ext[0] != internal[0]:
				add(field("internal_ports"), "a range keeps its port numbers: leave this empty, or enter the same range")
			}
		}

		if ip, ok := parseIPv4(p.ToIP); !ok {
			add(field("to_ip"), "must be an IPv4 address such as 192.168.1.50")
		} else if !c.addressOnAnyNetwork(ip) {
			add(field("to_ip"), "is not on any of your Local Networks")
		}

		if len(p.Sources) > MaxForwardSources {
			add(field("sources"), "at most %d addresses or ranges", MaxForwardSources)
		}
		for _, s := range p.Sources {
			if _, ok := ParseAddrRange(s); !ok {
				add(field("sources"), "%q is not an IPv4 address or range", s)
			}
		}

		// two enabled forwards may not claim the same external port for the same protocol
		if !p.Disabled && len(ext) == 1 && validProtocol(p.Protocol, true) {
			for _, c2 := range claims {
				if overlapProto(c2.proto, p.Protocol) && ext[0].Lo <= c2.rng.Hi && c2.rng.Lo <= ext[0].Hi {
					add(field("external_ports"), "overlaps the port forward %s", c2.id)
					break
				}
			}
			claims = append(claims, claim{p.ID, p.Protocol, ext[0]})
		}
	}
}

func overlapProto(a, b string) bool {
	if a == ProtoTCPUDP || b == ProtoTCPUDP {
		return true
	}
	return a == b
}

// addressOnAnyNetwork reports whether an address is a usable host on one of the Local Networks (and not the router's own).
func (c NetworkConfig) addressOnAnyNetwork(ip netip.Addr) bool {
	ids := []string{DefaultNetworkID}
	for _, n := range c.Networks {
		ids = append(ids, n.ID)
	}
	for _, id := range ids {
		p, ok := c.NetworkPrefix(id)
		if !ok || !p.Contains(ip) {
			continue
		}
		if ip == networkAddr(p) || ip == broadcastAddr(p) {
			continue
		}
		own := c.LanIP
		if id != DefaultNetworkID {
			n, _ := c.FindNetwork(id)
			own = n.IP
		}
		if ip.String() == own {
			continue
		}
		return true
	}
	return false
}

// ---- words ----------------------------------------------------------------------------------------------------

func protocolText(p string) string {
	switch p {
	case ProtoTCP:
		return "TCP"
	case ProtoUDP:
		return "UDP"
	case ProtoTCPUDP:
		return "TCP and UDP"
	case ProtoICMP:
		return "ICMP"
	}
	return "any protocol"
}

func (c NetworkConfig) endpointText(id string) string {
	switch id {
	case FromWAN, ToInternet:
		return "the internet"
	case ToRouter:
		return "this router"
	case ToAny:
		return "anywhere"
	}
	return c.networkName(id)
}

// RuleText describes a rule in a sentence for the review screen and the lists.
func (c NetworkConfig) RuleText(r FirewallRule) string {
	var b strings.Builder
	b.WriteString(map[string]string{ActionAllow: "Allow", ActionBlock: "Block", ActionReject: "Reject"}[r.Action])
	b.WriteString(" ")
	b.WriteString(protocolText(r.Protocol))
	if r.Ports != "" {
		b.WriteString(" " + r.Ports)
	}
	b.WriteString(", from " + c.endpointText(r.From))
	if r.Source != "" {
		b.WriteString(" (" + r.Source + ")")
	}
	b.WriteString(" to " + c.endpointText(r.To))
	if r.Destination != "" {
		b.WriteString(" (" + r.Destination + ")")
	}
	if r.Log {
		b.WriteString(", logged")
	}
	if r.Disabled {
		b.WriteString(", switched off")
	}
	return b.String()
}

// ForwardText describes a port forward in a sentence.
func (c NetworkConfig) ForwardText(p PortForward) string {
	internal := p.InternalPorts
	if internal == "" {
		internal = p.ExternalPorts
	}
	text := fmt.Sprintf("%s %s from the internet to %s:%s", protocolText(p.Protocol), p.ExternalPorts, p.ToIP, internal)
	if len(p.Sources) > 0 {
		text += ", only from " + strings.Join(p.Sources, ", ")
	}
	if p.Disabled {
		text += ", switched off"
	}
	return text
}

// ---- rules that can never match -------------------------------------------------------------------------------

// Shadow says that a rule can never be reached, because an earlier rule on the same tab already decides
// everything it would match.
type Shadow struct {
	Rule     string // the rule that is never reached
	ShadowBy string // the earlier rule that decides first
}

func portsCover(a, b string) bool {
	if a == "" {
		return true // every port
	}
	if b == "" {
		return false
	}
	ar, err1 := ParsePorts(a, 1000)
	br, err2 := ParsePorts(b, 1000)
	if err1 != nil || err2 != nil {
		return false
	}
	for _, y := range br {
		covered := false
		for _, x := range ar {
			if x.Lo <= y.Lo && y.Hi <= x.Hi {
				covered = true
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func rangeCovers(a, b string) bool {
	if a == "" {
		return true
	}
	if b == "" {
		return false
	}
	pa, ok1 := ParseAddrRange(a)
	pb, ok2 := ParseAddrRange(b)
	return ok1 && ok2 && pa.Bits() <= pb.Bits() && pa.Contains(pb.Addr())
}

func protocolCovers(a, b string) bool {
	switch {
	case a == ProtoAny, a == b:
		return true
	case a == ProtoTCPUDP:
		return b == ProtoTCP || b == ProtoUDP
	}
	return false
}

func toCovers(a, b string) bool { return a == ToAny || a == b }

func covers(a, b FirewallRule) bool {
	return a.From == b.From && protocolCovers(a.Protocol, b.Protocol) && toCovers(a.To, b.To) &&
		rangeCovers(a.Source, b.Source) && rangeCovers(a.Destination, b.Destination) && portsCover(a.Ports, b.Ports)
}

// Shadows lists the enabled rules that no packet can reach. Switched-off rules are never reported, and a switched-off
// rule shadows nothing.
func (c NetworkConfig) Shadows() []Shadow {
	if c.Firewall == nil {
		return nil
	}
	var out []Shadow
	rules := c.Firewall.Rules
	for i, b := range rules {
		if b.Disabled {
			continue
		}
		for _, a := range rules[:i] {
			if !a.Disabled && covers(a, b) {
				out = append(out, Shadow{Rule: b.ID, ShadowBy: a.ID})
				break
			}
		}
	}
	return out
}

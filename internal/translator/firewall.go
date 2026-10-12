package translator

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"peakroute/internal/config"
)

// How the firewall's rules and port forwards are written (docs/firewall.md has the model and the reasons).
//
// Everything here is *added to* the ruleset the router has always had, at fixed places in its chains, so that with no
// rules and no forwards the file is exactly what it was before. The order within a chain is the order the document
// describes:
//
//	input:    basics, then (when there are rules) DHCP and the anti-lockout lines, then the rules that go to the
//	          router, then the network policy (the default network may use the router, other networks DNS and ping)
//	forward:  basics, MSS clamp, port-forward accepts, then the rules that go through, then the network policy
//
// Every rule carries a counter and a comment "fw:<id>" so the page can read the hit counts back; the id is the only
// thing from the configuration that goes into a comment tag, the rest is a readable "#" comment line above it.

// antiLockoutPorts are what the default network can always reach on the router: SSH and the web interface (443 when
// the daemon runs as the appliance, 8443 in development).
const antiLockoutPorts = "{ 22, 443, 8443 }"

// fwInterface is the interface a Local Network's traffic uses.
func fwInterface(cfg config.NetworkConfig, id string) string {
	switch id {
	case config.FromWAN, config.ToInternet:
		return cfg.WanDevice()
	case config.DefaultNetworkID:
		return LanTarget(cfg)
	}
	if n, ok := cfg.FindNetwork(id); ok {
		return networkIface(n)
	}
	if z, ok := cfg.FindVPNZone(id); ok {
		return z.Iface
	}
	return ""
}

// commentSafe makes text fit on a "#" comment line: plain printable ASCII.
func commentSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ensureSafeFirewall is the render-time twin of the firewall validation: everything written into the ruleset must be
// plain, whatever the configuration says.
func ensureSafeFirewall(cfg config.NetworkConfig) error {
	f := cfg.FirewallSettings()
	checkAddr := func(s string) error {
		if s == "" {
			return nil
		}
		if _, ok := config.ParseAddrRange(s); !ok {
			return fmt.Errorf("refusing to render the unsafe address %q", s)
		}
		return nil
	}
	for _, r := range f.Rules {
		if err := ensureSafe(r.ID); err != nil {
			return err
		}
		for _, a := range []string{r.Source, r.Destination} {
			if err := checkAddr(a); err != nil {
				return err
			}
		}
		if r.Ports != "" {
			if _, err := config.ParsePorts(r.Ports, 15); err != nil {
				return fmt.Errorf("refusing to render the unsafe ports %q", r.Ports)
			}
		}
		for _, id := range []string{r.From, r.To} {
			if id != "" && fwInterfaceToken(cfg, id) == "" {
				return fmt.Errorf("the firewall rule %s names %q, which does not exist", r.ID, id)
			}
		}
	}
	for _, p := range f.Forwards {
		if err := ensureSafe(p.ID); err != nil {
			return err
		}
		if _, ok := netip.ParseAddr(p.ToIP); ok != nil {
			return fmt.Errorf("refusing to render the unsafe forward address %q", p.ToIP)
		}
		for _, ports := range []string{p.ExternalPorts, p.InternalPorts} {
			if ports == "" {
				continue
			}
			if _, err := config.ParsePorts(ports, 1); err != nil {
				return fmt.Errorf("refusing to render the unsafe ports %q", ports)
			}
		}
		for _, s := range p.Sources {
			if err := checkAddr(s); err != nil {
				return err
			}
		}
	}
	return nil
}

// fwInterfaceToken returns a non-empty string when the id names something a rule may use.
func fwInterfaceToken(cfg config.NetworkConfig, id string) string {
	switch id {
	case config.ToAny, config.ToRouter, config.ToInternet, config.FromWAN:
		return id
	}
	return fwInterface(cfg, id)
}

// nftPorts writes a destination port list for nft: one value bare, several in braces.
func nftPorts(ports string) string {
	rs, _ := config.ParsePorts(ports, 1000)
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = r.String()
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// ruleMatch writes what a rule matches on, after the interface names: addresses, protocol and ports.
func ruleMatch(r config.FirewallRule) string {
	var m []string
	if r.Source != "" {
		m = append(m, "ip saddr "+r.Source)
	}
	if r.Destination != "" {
		m = append(m, "ip daddr "+r.Destination)
	}
	switch r.Protocol {
	case config.ProtoTCP, config.ProtoUDP:
		if r.Ports != "" {
			m = append(m, r.Protocol+" dport "+nftPorts(r.Ports))
		} else {
			m = append(m, "meta l4proto "+r.Protocol)
		}
	case config.ProtoTCPUDP:
		if r.Ports != "" {
			m = append(m, "meta l4proto { tcp, udp } th dport "+nftPorts(r.Ports))
		} else {
			m = append(m, "meta l4proto { tcp, udp }")
		}
	case config.ProtoICMP:
		m = append(m, "meta l4proto icmp")
	}
	return strings.Join(m, " ")
}

// ruleVerdict writes the tail of a rule: counter, optional log, and what to do.
func ruleVerdict(r config.FirewallRule) string {
	var b strings.Builder
	b.WriteString("counter")
	if r.Log {
		fmt.Fprintf(&b, " log prefix \"fw:%s \" limit rate 5/second", r.ID)
	}
	switch r.Action {
	case config.ActionAllow:
		b.WriteString(" accept")
	case config.ActionBlock:
		b.WriteString(" drop")
	default:
		if r.Protocol == config.ProtoTCP {
			b.WriteString(" reject with tcp reset")
		} else {
			b.WriteString(" reject")
		}
	}
	fmt.Fprintf(&b, " comment \"fw:%s\"", r.ID)
	return b.String()
}

// firewallParts are the pieces the main ruleset template places.
type firewallParts struct {
	input    string // after the basics, before the network policy
	forward  string // after the MSS clamp, before the network policy
	preroute string // a whole "prerouting" chain for the nat table, or ""
}

// renderFirewall writes the rules and port forwards. With none, every part is empty.
func renderFirewall(cfg config.NetworkConfig) firewallParts {
	f := cfg.FirewallSettings()
	var in, fw, pre strings.Builder

	// port forwards: the translation, and the line that lets the translated traffic through
	var forwards []config.PortForward
	for _, p := range f.Forwards {
		if !p.Disabled {
			forwards = append(forwards, p)
		}
	}
	if len(forwards) > 0 {
		wan := cfg.WanDevice()
		pre.WriteString("    chain prerouting {\n        type nat hook prerouting priority dstnat; policy accept;\n")
		fw.WriteString("        # port forwards: the traffic they translate is let through\n")
		for _, p := range forwards {
			internal := p.InternalPorts
			if internal == "" {
				internal = p.ExternalPorts
			}
			fmt.Fprintf(&pre, "        # %s: %s\n", p.ID, commentSafe(cfg.ForwardText(p)))
			match := fmt.Sprintf("iifname \"%s\"", wan)
			if len(p.Sources) > 0 {
				match += " ip saddr " + nftAddrs(p.Sources)
			}
			proto := forwardProto(p.Protocol)
			// a single port is rewritten to the internal one; a range keeps its numbers (the validation insists on that)
			to := p.ToIP
			if ext, _ := config.ParsePorts(p.ExternalPorts, 1); len(ext) == 1 && ext[0].Count() == 1 {
				to += ":" + internal
			}
			fmt.Fprintf(&pre, "        %s %s %s dport %s counter dnat to %s comment \"fw:%s\"\n", match, proto.match, proto.th, p.ExternalPorts, to, p.ID)
			fmt.Fprintf(&fw, "        iifname \"%s\" ip daddr %s %s %s dport %s ct status dnat counter accept comment \"fw:%s\"\n", wan, p.ToIP, proto.match, proto.th, internal, p.ID)
		}
		pre.WriteString("    }\n")
	}

	// the rules
	var active []config.FirewallRule
	for _, r := range f.Rules {
		if !r.Disabled {
			active = append(active, r)
		}
	}
	if len(active) > 0 {
		// what no rule may break: DHCP, and reaching the router's web interface and SSH from the default network
		in.WriteString("        # always allowed, whatever the rules say: DHCP, and the default network's way into this router\n")
		var internal []string
		internal = append(internal, fwInterface(cfg, config.DefaultNetworkID))
		for _, n := range cfg.Networks {
			internal = append(internal, networkIface(n))
		}
		fmt.Fprintf(&in, "        iifname { %s } udp dport 67 accept\n", quoteList(internal))
		fmt.Fprintf(&in, "        iifname \"%s\" tcp dport %s accept\n", fwInterface(cfg, config.DefaultNetworkID), antiLockoutPorts)
		in.WriteString("        # firewall rules, in order\n")
		fw.WriteString("        # firewall rules, in order\n")
	}
	for _, r := range active {
		src := fwInterface(cfg, r.From)
		line := func(oif string) string {
			parts := []string{fmt.Sprintf("iifname \"%s\"", src)}
			if oif != "" {
				parts = append(parts, fmt.Sprintf("oifname \"%s\"", oif))
			}
			if m := ruleMatch(r); m != "" {
				parts = append(parts, m)
			}
			parts = append(parts, ruleVerdict(r))
			return "        " + strings.Join(parts, " ") + "\n"
		}
		note := fmt.Sprintf("        # %s: %s\n", r.ID, commentSafe(cfg.RuleText(r)))
		toRouter := r.To == config.ToRouter || r.To == config.ToAny
		through := r.To != config.ToRouter
		if toRouter {
			in.WriteString(note)
			in.WriteString(line(""))
		}
		if through {
			fw.WriteString(note)
			oif := ""
			if r.To != config.ToAny {
				oif = fwInterface(cfg, r.To)
			}
			fw.WriteString(line(oif))
		}
	}
	return firewallParts{input: in.String(), forward: fw.String(), preroute: pre.String()}
}

type protoMatch struct{ match, th string }

func forwardProto(p string) protoMatch {
	switch p {
	case config.ProtoTCP:
		return protoMatch{"meta l4proto tcp", "th"}
	case config.ProtoUDP:
		return protoMatch{"meta l4proto udp", "th"}
	}
	return protoMatch{"meta l4proto { tcp, udp }", "th"}
}

func nftAddrs(addrs []string) string {
	if len(addrs) == 1 {
		return addrs[0]
	}
	sorted := append([]string(nil), addrs...)
	sort.Strings(sorted)
	return "{ " + strings.Join(sorted, ", ") + " }"
}

func quoteList(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = `"` + n + `"`
	}
	return strings.Join(q, ", ")
}

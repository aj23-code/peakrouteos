package syslogfwd

import (
	"strconv"
	"strings"
	"time"

	"peakroute/internal/network"
)

// FirewallMessage turns a logged packet into a message that a log server can read without knowing the router: the rule's
// name and action, the interfaces, the addresses and ports. name and action come from the rules in effect; a rule that is no
// longer there is named by its id and has no action. The kernel's own line is not sent (the same facts, less readable).
func FirewallMessage(e network.FirewallLogEntry, name, action, host string) Message {
	var kv []string
	add := func(k, v string) {
		if v != "" {
			kv = append(kv, k+"="+quoteValue(v))
		}
	}
	if name == "" {
		name = e.Rule
	}
	add("rule", name)
	add("rule_id", e.Rule)
	add("action", action)
	add("in", e.In)
	add("out", e.Out)
	add("src", e.Src)
	if e.SrcPort != 0 {
		add("sport", strconv.Itoa(e.SrcPort))
	}
	add("dst", e.Dst)
	if e.DstPort != 0 {
		add("dport", strconv.Itoa(e.DstPort))
	}
	add("proto", e.Proto)
	sev := 6 // informational: traffic that was allowed
	if action == "block" || action == "reject" {
		sev = 4 // warning: traffic that was stopped
	}
	t := time.Now()
	if e.Time > 0 {
		t = time.UnixMilli(e.Time)
	}
	return Message{Time: t, Facility: 4, Severity: sev, Host: host, App: "peakroute-firewall", MsgID: "FIREWALL", Text: strings.Join(kv, " ")}
}

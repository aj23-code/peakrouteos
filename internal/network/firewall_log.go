package network

import (
	"strconv"
	"strings"
)

// FirewallLogEntry is one packet a rule with "log" switched on has matched, read back from the kernel log (internal/fwlog follows the journal and keeps them).
type FirewallLogEntry struct {
	Time    int64 // Unix milliseconds
	Rule    string
	In, Out string // the interfaces ("" when not applicable: Out is empty for traffic to the router)
	Src     string
	Dst     string
	Proto   string // TCP, UDP, ICMP...
	SrcPort int
	DstPort int
}

// ParseFirewallLog reads the text of a kernel log line written by a rule's log statement: the rule's tag
// ("fw:fw2 ") followed by the fields the kernel prints (IN=br30 OUT=eth0 ... SRC=10.30.0.40 DST=1.2.3.4 ... PROTO=TCP
// SPT=40000 DPT=443 ...). ok is false for any other line.
func ParseFirewallLog(msg string) (FirewallLogEntry, bool) {
	f := strings.Fields(msg)
	if len(f) == 0 || !strings.HasPrefix(f[0], "fw:") || len(f[0]) == len("fw:") {
		return FirewallLogEntry{}, false
	}
	e := FirewallLogEntry{Rule: strings.TrimPrefix(f[0], "fw:")}
	for _, tok := range f[1:] {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		switch k {
		case "IN":
			e.In = v
		case "OUT":
			e.Out = v
		case "SRC":
			e.Src = v
		case "DST":
			e.Dst = v
		case "PROTO":
			e.Proto = v
		case "SPT":
			e.SrcPort, _ = strconv.Atoi(v)
		case "DPT":
			e.DstPort, _ = strconv.Atoi(v)
		}
	}
	if e.Src == "" || e.Dst == "" {
		return FirewallLogEntry{}, false
	}
	return e, true
}

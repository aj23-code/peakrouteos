package network

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
)

// FirewallLogEntry is one packet a rule with "log" switched on has matched, read back from the kernel log.
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

// ParseFirewallLogJournal reads `journalctl -o json` output (one JSON object per line) and returns the firewall
// entries it holds, oldest first.
func ParseFirewallLogJournal(data []byte) []FirewallLogEntry {
	var out []FirewallLogEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw struct {
			Message   string `json:"MESSAGE"`
			Timestamp string `json:"__REALTIME_TIMESTAMP"` // microseconds
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		e, ok := ParseFirewallLog(raw.Message)
		if !ok {
			continue
		}
		if us, err := strconv.ParseInt(raw.Timestamp, 10, 64); err == nil {
			e.Time = us / 1000
		}
		out = append(out, e)
	}
	return out
}

// ReadFirewallLog reads the most recent firewall log entries from the kernel messages in the journal (newest
// last). ok is false when the journal cannot be read.
func ReadFirewallLog(limit int) ([]FirewallLogEntry, bool) {
	// the journal is asked for the lines that carry the tag, so that a busy kernel log does not push them out
	out, err := exec.Command("journalctl", "-k", "--output=json", "--no-pager", "--grep=fw:", "-n", strconv.Itoa(limit)).Output()
	if err != nil {
		// no match is an error for some versions of journalctl: that is an empty log, not an unreadable one
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && len(out) == 0 {
			return nil, true
		}
		return nil, false
	}
	return ParseFirewallLogJournal(out), true
}

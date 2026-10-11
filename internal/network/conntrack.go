package network

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Connection is one entry of the kernel's connection tracking table: a connection that is passing through (or to) this
// router, as the firewall sees it. The first address pair is the original direction (who started it and what they
// asked for), the second the reply direction; where they differ the router translated an address (NAT).
type Connection struct {
	Proto    string // tcp, udp, icmp...
	State    string // the TCP state (ESTABLISHED, TIME_WAIT...); empty for other protocols
	Timeout  int    // seconds until the entry is forgotten if it stays quiet
	Src, Dst string
	SrcPort  int
	DstPort  int
	// the reply direction's addresses and ports
	ReplySrc, ReplyDst         string
	ReplySrcPort, ReplyDstPort int
	Packets, Bytes             uint64 // from the initiator
	ReplyPackets, ReplyBytes   uint64 // from the other side
	HasCounters                bool   // the kernel is counting (nf_conntrack_acct)
	Unreplied                  bool
	Assured                    bool
}

// SourceNAT is the address the other side sees when the router rewrote the initiator's address (masquerade); "" if not.
func (c Connection) SourceNAT() string {
	if c.ReplyDst != "" && c.ReplyDst != c.Src {
		return c.ReplyDst
	}
	return ""
}

// DestinationNAT is where a connection was really sent when the router rewrote its destination (a port forward):
// the address and port, "" if not.
func (c Connection) DestinationNAT() (string, int) {
	if c.ReplySrc != "" && (c.ReplySrc != c.Dst || c.ReplySrcPort != c.DstPort) {
		return c.ReplySrc, c.ReplySrcPort
	}
	return "", 0
}

// ParseConntrack reads the connection table as /proc/net/nf_conntrack prints it ("ipv4 2 tcp 6 431999 ESTABLISHED
// src=...") or as `conntrack -L` does (without the family columns). Lines it cannot make sense of are skipped.
func ParseConntrack(text string) []Connection {
	var out []Connection
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) > 2 && (f[0] == "ipv4" || f[0] == "ipv6") {
			f = f[2:] // the address family and its number
		}
		if len(f) < 3 {
			continue
		}
		timeout, err := strconv.Atoi(f[2])
		if err != nil {
			continue
		}
		c := Connection{Proto: f[0], Timeout: timeout}
		rest := f[3:]
		if len(rest) > 0 && !strings.Contains(rest[0], "=") && !strings.HasPrefix(rest[0], "[") {
			c.State, rest = rest[0], rest[1:]
		}
		var seen = map[string]int{}
		for _, tok := range rest {
			switch {
			case tok == "[UNREPLIED]":
				c.Unreplied = true
			case tok == "[ASSURED]":
				c.Assured = true
			case strings.Contains(tok, "="):
				k, v, _ := strings.Cut(tok, "=")
				seen[k]++
				reply := seen[k] > 1
				n, _ := strconv.Atoi(v)
				u, _ := strconv.ParseUint(v, 10, 64)
				switch k {
				case "src":
					if reply {
						c.ReplySrc = v
					} else {
						c.Src = v
					}
				case "dst":
					if reply {
						c.ReplyDst = v
					} else {
						c.Dst = v
					}
				case "sport":
					if reply {
						c.ReplySrcPort = n
					} else {
						c.SrcPort = n
					}
				case "dport":
					if reply {
						c.ReplyDstPort = n
					} else {
						c.DstPort = n
					}
				case "packets":
					c.HasCounters = true
					if reply {
						c.ReplyPackets = u
					} else {
						c.Packets = u
					}
				case "bytes":
					if reply {
						c.ReplyBytes = u
					} else {
						c.Bytes = u
					}
				}
			}
		}
		if c.Src == "" || c.Dst == "" {
			continue // not an IP connection (no addresses to show)
		}
		out = append(out, c)
	}
	return out
}

// ReadConntrack reads the kernel's connection table: from /proc when the kernel offers it there, else through the
// `conntrack` tool. ok is false when neither works (no permission, the module or the tool is missing).
func ReadConntrack() ([]Connection, bool) {
	if data, err := os.ReadFile("/proc/net/nf_conntrack"); err == nil {
		return ParseConntrack(string(data)), true
	}
	// stdout only: the tool prints "N flow entries have been shown" on stderr
	out, err := exec.Command("conntrack", "-L").Output()
	if err != nil {
		return nil, false
	}
	return ParseConntrack(string(out)), true
}

func readInt(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// ConntrackLimits returns how many connections are tracked now and how many the table can hold (0 when unknown).
func ConntrackLimits() (count, max int) {
	return readInt("/proc/sys/net/netfilter/nf_conntrack_count"), readInt("/proc/sys/net/netfilter/nf_conntrack_max")
}

// EnableConntrackAccounting asks the kernel to count the bytes and packets of every connection. It only counts
// connections created afterwards, and does nothing when it is not allowed to (not root) or the module is not loaded yet.
func EnableConntrackAccounting() {
	const path = "/proc/sys/net/netfilter/nf_conntrack_acct"
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == "0" {
		_ = os.WriteFile(path, []byte("1"), 0o644)
	}
}

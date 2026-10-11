package network

import (
	"encoding/json"
	"os/exec"
	"strings"
)

// RuleCounter is how often a firewall rule or port forward has matched since the rules were loaded.
type RuleCounter struct {
	Packets uint64
	Bytes   uint64
}

// ParseFirewallCounters reads `nft -j list ruleset` and returns the counters of the rules the firewall page wrote, by
// id (fw1, pf2...): each carries the comment "fw:<id>". A rule that goes to anywhere is in two chains with the same
// comment; its counts are added up.
func ParseFirewallCounters(data []byte) (map[string]RuleCounter, error) {
	var doc struct {
		Nftables []struct {
			Rule *struct {
				Comment string `json:"comment"`
				Expr    []struct {
					Counter *struct {
						Packets uint64 `json:"packets"`
						Bytes   uint64 `json:"bytes"`
					} `json:"counter"`
				} `json:"expr"`
			} `json:"rule"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := map[string]RuleCounter{}
	for _, item := range doc.Nftables {
		r := item.Rule
		if r == nil || !strings.HasPrefix(r.Comment, "fw:") {
			continue
		}
		id := strings.TrimPrefix(r.Comment, "fw:")
		for _, e := range r.Expr {
			if e.Counter != nil {
				c := out[id]
				c.Packets += e.Counter.Packets
				c.Bytes += e.Counter.Bytes
				out[id] = c
				break
			}
		}
	}
	return out, nil
}

// ReadFirewallCounters asks the kernel. ok is false when it cannot (no nft, or not allowed to ask).
func ReadFirewallCounters() (map[string]RuleCounter, bool) {
	data, err := exec.Command("nft", "-j", "list", "ruleset").Output()
	if err != nil {
		return nil, false
	}
	c, err := ParseFirewallCounters(data)
	return c, err == nil
}

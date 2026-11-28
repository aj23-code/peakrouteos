package network

import (
	"context"
	"encoding/json"
	"strings"
)

// Attachment is where the router's own bridge has learned a hardware address: the port the frames of that device arrive on.
type Attachment struct {
	Port   string `json:"port"`             // the router's port (or Wi-Fi interface) it is behind
	Bridge string `json:"bridge,omitempty"` // the bridge that learned it
	VLAN   int    `json:"vlan,omitempty"`   // 0: not stated
}

// ParseBridgeFDB reads `bridge -j fdb show`: the learned entries (not the ports' own addresses, which are "permanent", and not the
// multicast ones) by hardware address. A device behind a switch is reported on the port the switch is plugged into.
func ParseBridgeFDB(out string) map[string]Attachment {
	var entries []struct {
		MAC    string   `json:"mac"`
		Dev    string   `json:"ifname"`
		Dev2   string   `json:"dev"`
		Master string   `json:"master"`
		VLAN   int      `json:"vlan"`
		State  string   `json:"state"`
		Flags  []string `json:"flags"`
	}
	if json.Unmarshal([]byte(out), &entries) != nil {
		return nil
	}
	m := map[string]Attachment{}
	for _, e := range entries {
		mac := strings.ToLower(e.MAC)
		if mac == "" || e.State == "permanent" || e.Master == "" {
			continue
		}
		skip := false
		for _, f := range e.Flags {
			if f == "self" && e.Master == "" {
				skip = true
			}
		}
		if skip || strings.HasPrefix(mac, "01:") || strings.HasPrefix(mac, "33:33:") || mac == "ff:ff:ff:ff:ff:ff" {
			continue
		}
		dev := e.Dev
		if dev == "" {
			dev = e.Dev2
		}
		if dev == "" || dev == e.Master {
			continue // the bridge's own
		}
		if _, have := m[mac]; have && e.VLAN == 0 {
			continue // keep the first (a device on several VLANs is on one port)
		}
		m[mac] = Attachment{Port: dev, Bridge: e.Master, VLAN: e.VLAN}
	}
	return m
}

// ReadBridgeFDB asks the kernel. A machine with no bridge, or no `bridge` program, answers an empty map.
func ReadBridgeFDB(ctx context.Context, run Runner) map[string]Attachment {
	out, err := run.Run(ctx, "bridge", "-j", "fdb", "show")
	if err != nil {
		return map[string]Attachment{}
	}
	if m := ParseBridgeFDB(out); m != nil {
		return m
	}
	return map[string]Attachment{}
}

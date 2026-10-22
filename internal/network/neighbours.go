package network

import (
	"context"
	"encoding/json"
	"net/netip"
	"sort"
	"strings"

	"peakroute/internal/config"
)

// Neighbour is a device the router has exchanged ARP with: its address, hardware address and the interface it was seen on.
type Neighbour struct {
	IP  string
	MAC string
	Dev string
}

// ReadNeighbours reads the router's IPv4 neighbour table (`ip -j neigh show`): the devices it has seen on its networks, whether or not
// they got their address from its DHCP server. Entries that never completed are left out.
func ReadNeighbours(ctx context.Context, run Runner) []Neighbour {
	if run == nil {
		return nil
	}
	out, err := run.Run(ctx, "ip", "-j", "neigh", "show")
	if err != nil {
		return nil
	}
	var entries []struct {
		Dst    string   `json:"dst"`
		Dev    string   `json:"dev"`
		LLAddr string   `json:"lladdr"`
		State  []string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &entries) != nil {
		return nil
	}
	var res []Neighbour
	for _, e := range entries {
		a, err := netip.ParseAddr(e.Dst)
		if err != nil || !a.Is4() || e.LLAddr == "" {
			continue
		}
		bad := false
		for _, s := range e.State {
			if s == "FAILED" || s == "INCOMPLETE" {
				bad = true
			}
		}
		if !bad {
			res = append(res, Neighbour{IP: e.Dst, MAC: strings.ToLower(e.LLAddr), Dev: e.Dev})
		}
	}
	return res
}

// StaticClients are the neighbours that have no lease: devices with an address of their own. A neighbour counts when it is on one of the
// router's networks (not the upstream router or anything on the internet side), is not the router itself, and is neither a leased
// device's hardware address nor a leased address. devNetwork names the network an interface belongs to ("" when it is none of them):
// a device seen on one network with an address from another is marked, which is how a device on the wrong VLAN shows up.
func StaticClients(leases []ClientLease, neighbours []Neighbour, cfg config.NetworkConfig, devNetwork func(dev string) string) []ClientLease {
	subnets := localSubnets(cfg)
	have, haveIP := map[string]bool{}, map[string]bool{}
	for _, l := range leases {
		have[strings.ToLower(l.MACAddress)] = true
		haveIP[l.IPAddress] = true
	}
	router := map[string]bool{}
	for _, s := range subnets {
		router[s.router.String()] = true
	}
	var out []ClientLease
	seen := map[string]bool{}
	for _, n := range neighbours {
		a, err := netip.ParseAddr(n.IP)
		if err != nil || router[n.IP] || have[n.MAC] || haveIP[n.IP] || seen[n.MAC+"|"+n.IP] {
			continue
		}
		name := ""
		for _, s := range subnets {
			if s.prefix.Contains(a) {
				name = s.name
				break
			}
		}
		if name == "" {
			continue // not in any of the router's networks: the upstream router, a neighbour on the internet side
		}
		seen[n.MAC+"|"+n.IP] = true
		c := ClientLease{MACAddress: n.MAC, IPAddress: n.IP, Hostname: "", ExpiryText: "Static", Static: true, Network: name}
		if devNetwork != nil {
			if on := devNetwork(n.Dev); on != "" && on != name {
				c.SeenOn = on
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IPAddress < out[j].IPAddress })
	return out
}

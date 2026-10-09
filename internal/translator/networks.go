package translator

import (
	"fmt"
	"sort"
	"strings"

	"peakroute/internal/config"
)

// How several networks are built (standard Debian ifupdown, with the bridge-utils and vlan packages):
//
//   - The default network is untagged, exactly as before: the LAN port itself, or the bridge br0 when
//     several ports carry it (and an empty br0 when none does).
//   - Every other network has its own bridge, br<vlan> (br20), which holds the ports that carry it
//     untagged and a VLAN interface (eth1.20) for each port that carries it tagged. The router's
//     address on the network sits on that bridge.
//
// Separate bridges keep the networks apart at layer 2; the firewall decides what crosses between them.

func vlanOf(cfg config.NetworkConfig, id string) int {
	if n, ok := cfg.FindNetwork(id); ok {
		return n.VLAN
	}
	return 0
}

func networkIface(n config.Network) string { return fmt.Sprintf("br%d", n.VLAN) }

// defaultPorts are the LAN ports whose untagged network is the default one.
func defaultPorts(cfg config.NetworkConfig) []string {
	var out []string
	for _, port := range cfg.LanInterfaces {
		if cfg.PortNetworks[port].NativeID() == config.DefaultNetworkID {
			out = append(out, port)
		}
	}
	return out
}

// networkMembers lists what is bridged into a network: its untagged ports, then its VLAN interfaces.
func networkMembers(cfg config.NetworkConfig, n config.Network) []string {
	var members []string
	for _, port := range cfg.LanInterfaces {
		if cfg.PortNetworks[port].NativeID() == n.ID {
			members = append(members, port)
		}
	}
	for _, port := range cfg.LanInterfaces {
		for _, t := range cfg.PortNetworks[port].Tagged {
			if t == n.ID {
				members = append(members, fmt.Sprintf("%s.%d", port, n.VLAN))
			}
		}
	}
	return members
}

func anyNetworkDHCP(cfg config.NetworkConfig) bool {
	for _, n := range cfg.Networks {
		if n.DHCP != nil && n.DHCP.Enabled {
			return true
		}
	}
	return false
}

// carriedBy reports whether any LAN port carries a network, natively or tagged.
func carriedBy(cfg config.NetworkConfig, id string) bool {
	for _, port := range cfg.LanInterfaces {
		pn := cfg.PortNetworks[port]
		if pn.NativeID() == id {
			return true
		}
		for _, t := range pn.Tagged {
			if t == id {
				return true
			}
		}
	}
	return false
}

// networkNotes tells the person about networks that cannot work yet, shown with the plan.
func networkNotes(cfg config.NetworkConfig) []string {
	if !cfg.Bridged() {
		return nil
	}
	var notes []string
	if !carriedBy(cfg, config.DefaultNetworkID) {
		notes = append(notes, "No LAN port carries the default network, so nothing can reach this router's web interface from it.")
	}
	for _, n := range cfg.Networks {
		if !carriedBy(cfg, n.ID) {
			notes = append(notes, fmt.Sprintf("Network %s is not carried by any port yet, so nothing can connect to it.", strings.TrimSpace(n.Name)))
		}
	}
	return notes
}

// renderInterfacesNetworks writes /etc/network/interfaces when there are several networks.
func renderInterfacesNetworks(cfg config.NetworkConfig, lan string) string {
	var b strings.Builder
	b.WriteString("auto lo\niface lo inet loopback\n\n")
	writeWanStanza(&b, cfg)

	// a VLAN interface for every port that carries a network tagged
	for _, port := range cfg.LanInterfaces {
		tagged := append([]string(nil), cfg.PortNetworks[port].Tagged...)
		sort.Slice(tagged, func(i, j int) bool { return vlanOf(cfg, tagged[i]) < vlanOf(cfg, tagged[j]) })
		for _, id := range tagged {
			name := fmt.Sprintf("%s.%d", port, vlanOf(cfg, id))
			fmt.Fprintf(&b, "auto %s\niface %s inet manual\n    vlan-raw-device %s\n\n", name, name, port)
		}
	}

	// the default network
	fmt.Fprintf(&b, "auto %s\n", lan)
	fmt.Fprintf(&b, "iface %s inet static\n", lan)
	fmt.Fprintf(&b, "    address %s\n", cfg.LanIP)
	fmt.Fprintf(&b, "    netmask %s\n", cfg.LanSubnet)
	if lan == "br0" {
		fmt.Fprintf(&b, "    bridge_ports %s\n", portList(defaultPorts(cfg)))
	}

	for _, n := range cfg.Networks {
		iface := networkIface(n)
		fmt.Fprintf(&b, "\nauto %s\n", iface)
		fmt.Fprintf(&b, "iface %s inet static\n", iface)
		fmt.Fprintf(&b, "    address %s\n", n.IP)
		fmt.Fprintf(&b, "    netmask %s\n", n.Netmask)
		fmt.Fprintf(&b, "    bridge_ports %s\n", portList(networkMembers(cfg, n)))
	}
	return b.String()
}

// portList is a bridge's member list; an empty bridge says so.
func portList(ports []string) string {
	if len(ports) == 0 {
		return "none"
	}
	return strings.Join(ports, " ")
}

// renderDnsmasqDHCPNetworks writes the DHCP server's file for several networks. Every network's
// interface is listed (a network without DHCP still gets DNS), and each range and its options are
// tagged with the network so that a client never receives another network's router or DNS server.
func renderDnsmasqDHCPNetworks(cfg config.NetworkConfig, lan string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "interface=%s\n", lan)
	for _, n := range cfg.Networks {
		fmt.Fprintf(&b, "interface=%s\n", networkIface(n))
	}
	scope := func(tag, routerIP, mask string, d *config.DHCPConfig) {
		if d == nil || !d.Enabled {
			return
		}
		fmt.Fprintf(&b, "dhcp-range=set:%s,%s,%s,%s,%s\n", tag, d.RangeStart, d.RangeEnd, mask, d.LeaseTime)
		fmt.Fprintf(&b, "dhcp-option=tag:%s,option:router,%s\n", tag, routerIP)
		fmt.Fprintf(&b, "dhcp-option=tag:%s,option:dns-server,%s\n", tag, routerIP)
	}
	scope(config.DefaultNetworkID, cfg.LanIP, cfg.LanSubnet, cfg.DHCP)
	for _, n := range cfg.Networks {
		scope(n.ID, n.IP, n.Netmask, n.DHCP)
	}
	return b.String()
}

// nftablesNetworkRules returns the extra lines for the input and forward chains. Without further
// networks it returns nothing, which leaves the rules exactly as they always were.
//
//   - The default network keeps full access to the router, as before. Another network may only use
//     the router's DHCP, DNS and ping: no web interface, no ssh.
//   - A network reaches the internet only if its Internet switch is on. There is no other rule for it.
//   - Two networks may talk to each other only if neither is isolated, in both directions.
func nftablesNetworkRules(cfg config.NetworkConfig, lan string) (input, forward string) {
	if !cfg.Bridged() {
		return "", ""
	}
	var in, fw strings.Builder
	for _, n := range cfg.Networks {
		iface := networkIface(n)
		fmt.Fprintf(&in, "        iifname \"%s\" udp dport { 53, 67 } accept\n", iface)
		fmt.Fprintf(&in, "        iifname \"%s\" tcp dport 53 accept\n", iface)
		fmt.Fprintf(&in, "        iifname \"%s\" icmp type echo-request accept\n", iface)
		if n.Internet {
			fmt.Fprintf(&fw, "        iifname \"%s\" oifname \"%s\" accept\n", iface, cfg.WanInterface)
		}
	}

	type member struct {
		iface string
		open  bool // not isolated
	}
	members := []member{{lan, !cfg.LanIsolated}}
	for _, n := range cfg.Networks {
		members = append(members, member{networkIface(n), !n.Isolated})
	}
	for i := 0; i < len(members); i++ {
		for j := i + 1; j < len(members); j++ {
			if members[i].open && members[j].open {
				fmt.Fprintf(&fw, "        iifname \"%s\" oifname \"%s\" accept\n", members[i].iface, members[j].iface)
				fmt.Fprintf(&fw, "        iifname \"%s\" oifname \"%s\" accept\n", members[j].iface, members[i].iface)
			}
		}
	}
	return in.String(), fw.String()
}

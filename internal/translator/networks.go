package translator

import (
	"fmt"
	"sort"
	"strings"

	"peakroute/internal/config"
)

// The default network is VLAN 1 of the bridge (br0 itself); every other network is a VLAN of br0
// with its own interface, br0.<vlan>.

const defaultVLAN = 1

func vlanOf(cfg config.NetworkConfig, id string) int {
	if id == config.DefaultNetworkID {
		return defaultVLAN
	}
	if n, ok := cfg.FindNetwork(id); ok {
		return n.VLAN
	}
	return defaultVLAN
}

func networkIface(n config.Network) string { return fmt.Sprintf("br0.%d", n.VLAN) }

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

// renderInterfacesBridged writes /etc/network/interfaces when there are several networks: one
// VLAN-aware bridge holds every LAN port, the default network is the bridge's own (untagged)
// address and each other network is a VLAN interface on it.
//
// The image has no bridge-utils or vlan package, so ifupdown cannot build these on its own: the
// pre-up and post-up lines do it with ip and bridge from iproute2, which is installed.
func renderInterfacesBridged(cfg config.NetworkConfig) string {
	var b strings.Builder
	b.WriteString("auto lo\niface lo inet loopback\n\n")
	writeWanStanza(&b, cfg)

	fmt.Fprintf(&b, "auto br0\n")
	fmt.Fprintf(&b, "iface br0 inet static\n")
	fmt.Fprintf(&b, "    address %s\n", cfg.LanIP)
	fmt.Fprintf(&b, "    netmask %s\n", cfg.LanSubnet)
	b.WriteString("    pre-up ip link add name br0 type bridge || true\n")
	b.WriteString("    pre-up ip link set dev br0 type bridge vlan_filtering 1\n")
	for _, port := range cfg.LanInterfaces {
		fmt.Fprintf(&b, "    pre-up ip link set dev %s master br0\n", port)
		fmt.Fprintf(&b, "    pre-up ip link set dev %s up\n", port)
	}
	// the router itself takes part in each VLAN, so the VLAN interfaces can send and receive
	for _, n := range cfg.Networks {
		fmt.Fprintf(&b, "    post-up bridge vlan add dev br0 vid %d self\n", n.VLAN)
	}
	for _, port := range cfg.LanInterfaces {
		pn := cfg.PortNetworks[port]
		if native := vlanOf(cfg, pn.NativeID()); native != defaultVLAN {
			// the new untagged VLAN first, then the bridge's default one goes
			fmt.Fprintf(&b, "    post-up bridge vlan add dev %s vid %d pvid untagged\n", port, native)
			fmt.Fprintf(&b, "    post-up bridge vlan del dev %s vid %d\n", port, defaultVLAN)
		}
		var tagged []int
		for _, t := range pn.Tagged {
			tagged = append(tagged, vlanOf(cfg, t))
		}
		sort.Ints(tagged)
		for _, v := range tagged {
			fmt.Fprintf(&b, "    post-up bridge vlan add dev %s vid %d\n", port, v)
		}
	}
	b.WriteString("    post-down ip link del dev br0 || true\n")

	for _, n := range cfg.Networks {
		iface := networkIface(n)
		fmt.Fprintf(&b, "\nauto %s\n", iface)
		fmt.Fprintf(&b, "iface %s inet static\n", iface)
		fmt.Fprintf(&b, "    address %s\n", n.IP)
		fmt.Fprintf(&b, "    netmask %s\n", n.Netmask)
		fmt.Fprintf(&b, "    pre-up ip link add link br0 name %s type vlan id %d || true\n", iface, n.VLAN)
		fmt.Fprintf(&b, "    post-down ip link del dev %s || true\n", iface)
	}
	return b.String()
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

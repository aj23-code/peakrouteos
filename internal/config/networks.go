package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultNetworkID names the network that lives in the lan_* and dhcp fields: the one every
// router has, which the setup wizard creates. Further local networks (guest, IoT, a lab...) are
// entries of NetworkConfig.Networks.
const DefaultNetworkID = "lan"

const (
	MaxNetworks = 16
	maxNetName  = 32
)

// Network is an additional local network. A port carries it untagged (as the port's native network)
// and/or tagged as a VLAN.
//
// Isolated and Internet are both written to the file on purpose, with no "omitempty": a network
// the file does not say anything about is the least trusting one the format can express.
type Network struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	VLAN     int         `json:"vlan"` // 2 to 4094; the default network is VLAN 1
	IP       string      `json:"ip"`   // the router's address on this network
	Netmask  string      `json:"netmask"`
	DHCP     *DHCPConfig `json:"dhcp,omitempty"`
	Internet bool        `json:"internet"` // may reach the internet through the WAN
	Isolated bool        `json:"isolated"` // cannot talk to other networks, and they cannot talk to it
	// DNSServers are what the network's devices are told to use for DNS instead of this router (empty: the router)
	DNSServers []string `json:"dns_servers,omitempty"`
}

// PortNetworks says which networks a LAN port carries. Native is the network whose traffic is
// untagged on the port ("" or "lan" is the default network); Tagged lists the others it carries
// as VLANs. A LAN port with no entry carries just the default network.
type PortNetworks struct {
	Native string   `json:"native,omitempty"`
	Tagged []string `json:"tagged,omitempty"`
}

// NativeID returns the port's native network, never empty.
func (p PortNetworks) NativeID() string {
	if p.Native == "" {
		return DefaultNetworkID
	}
	return p.Native
}

// IsDefault reports whether the entry says nothing beyond the default.
func (p PortNetworks) IsDefault() bool { return p.NativeID() == DefaultNetworkID && len(p.Tagged) == 0 }

// Bridged reports whether there are further networks, which changes how the LAN is built: each
// network gets its own bridge and the firewall and DHCP files name every network.
func (c NetworkConfig) Bridged() bool { return len(c.Networks) > 0 }

// FindNetwork returns an additional network by id.
func (c NetworkConfig) FindNetwork(id string) (Network, bool) {
	for _, n := range c.Networks {
		if n.ID == id {
			return n, true
		}
	}
	return Network{}, false
}

var (
	networkIDPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)
	networkNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()+-]*$`)
)

// Equal reports whether two networks are configured identically.
func (n Network) Equal(o Network) bool {
	if n.ID != o.ID || n.Name != o.Name || n.VLAN != o.VLAN || n.IP != o.IP || n.Netmask != o.Netmask ||
		n.Internet != o.Internet || n.Isolated != o.Isolated || strings.Join(n.DNSServers, ",") != strings.Join(o.DNSServers, ",") {
		return false
	}
	switch {
	case n.DHCP == nil && o.DHCP == nil:
		return true
	case n.DHCP == nil || o.DHCP == nil:
		return false
	}
	return *n.DHCP == *o.DHCP
}

func (n Network) clone() Network {
	if n.DHCP != nil {
		d := *n.DHCP
		n.DHCP = &d
	}
	n.DNSServers = append([]string(nil), n.DNSServers...)
	if len(n.DNSServers) == 0 {
		n.DNSServers = nil
	}
	return n
}

func clonePortNetworks(in map[string]PortNetworks) map[string]PortNetworks {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]PortNetworks, len(in))
	for k, v := range in {
		v.Tagged = append([]string(nil), v.Tagged...)
		out[k] = v
	}
	return out
}

// validateDHCP checks a DHCP server's settings against the subnet it serves. field turns a short
// name ("range_start") into the name errors are reported on; what describes the subnet in messages.
func validateDHCP(d *DHCPConfig, subnet netip.Prefix, routerIP netip.Addr, subnetOK bool, what string,
	field func(string) string, add func(field, format string, args ...any)) {

	if d == nil || !d.Enabled {
		return
	}
	start, startOK := parseIPv4(d.RangeStart)
	end, endOK := parseIPv4(d.RangeEnd)
	if !startOK {
		add(field("range_start"), "must be an IPv4 address")
	}
	if !endOK {
		add(field("range_end"), "must be an IPv4 address")
	}
	if startOK && endOK && subnetOK {
		switch {
		case !isHostAddr(subnet, start):
			add(field("range_start"), "must be a host address inside %s (%s)", what, subnet)
		case !isHostAddr(subnet, end):
			add(field("range_end"), "must be a host address inside %s (%s)", what, subnet)
		case start.Compare(end) > 0:
			add(field("range_end"), "must not be lower than the range start")
		case routerIP.Compare(start) >= 0 && routerIP.Compare(end) <= 0:
			add(field("range_start"), "the range must not include the router's own address (%s)", routerIP)
		}
	}
	if _, ok := ParseLeaseTime(d.LeaseTime); !ok {
		add(field("lease_time"), "must look like 30m, 12h or 1d (at least 2 minutes), or \"infinite\"")
	}
}

// validateNetworks checks the additional networks and which ports carry them.
func (c NetworkConfig) validateNetworks(add func(field, format string, args ...any),
	lan netip.Prefix, lanOK bool, wan netip.Prefix, haveWan bool) {

	if len(c.Networks) > MaxNetworks {
		add("networks", "at most %d additional networks are supported", MaxNetworks)
	}
	if len(c.Networks) > 0 && c.DHCP == nil {
		// Writing the DHCP file for the extra networks would overwrite whatever serves the default one
		add("dhcp_enabled", "set up the DHCP server of the default network (even if only to turn it off) before adding networks")
	}

	// A problem shared by two networks (the same name, the same VLAN, overlapping subnets) is
	// reported on both, so editing either one shows it.
	type taken struct {
		what   string
		prefix netip.Prefix
		index  int // of the network, or -1 for the default network and the WAN
	}
	var prefixes []taken
	if lanOK {
		prefixes = append(prefixes, taken{"the default network", lan, -1})
	}
	if haveWan {
		prefixes = append(prefixes, taken{"the WAN", wan, -1})
	}

	ids, names, vlans := map[string]int{}, map[string]int{}, map[int]int{}
	for i, n := range c.Networks {
		field := func(name string) string { return fmt.Sprintf("networks[%d].%s", i, name) }

		switch {
		case !networkIDPattern.MatchString(n.ID):
			add(field("id"), "must be lower-case letters, digits and '-', starting with a letter (up to 16 characters)")
		case n.ID == DefaultNetworkID:
			add(field("id"), "%q is the default network", DefaultNetworkID)
		case hasKey(ids, n.ID):
			add(field("id"), "is used by another network")
			add(fmt.Sprintf("networks[%d].id", ids[n.ID]), "is used by another network")
		}
		if !hasKey(ids, n.ID) {
			ids[n.ID] = i
		}

		name := strings.TrimSpace(n.Name)
		switch {
		case name == "" || len(name) > maxNetName || !networkNamePattern.MatchString(name):
			add(field("name"), "use 1 to %d letters, digits, spaces or . _ ( ) + - characters", maxNetName)
		case hasKey(names, strings.ToLower(name)):
			add(field("name"), "is used by another network")
			add(fmt.Sprintf("networks[%d].name", names[strings.ToLower(name)]), "is used by another network")
		case strings.EqualFold(name, "lan") || strings.EqualFold(name, "default"):
			add(field("name"), "is used by another network")
		}
		if !hasKey(names, strings.ToLower(name)) {
			names[strings.ToLower(name)] = i
		}

		if n.VLAN < 2 || n.VLAN > 4094 {
			add(field("vlan"), "must be between 2 and 4094 (VLAN 1 is the default network)")
		} else if first, dup := vlans[n.VLAN]; dup {
			add(field("vlan"), "is used by another network")
			add(fmt.Sprintf("networks[%d].vlan", first), "is used by another network")
		} else {
			vlans[n.VLAN] = i
		}

		ip, ipOK := parseIPv4(n.IP)
		if !ipOK {
			add(field("ip"), "must be an IPv4 address such as 10.20.0.1")
		}
		ones, maskOK := checkNetmask(n.Netmask, field("netmask"), add)
		prefixOK := ipOK && maskOK
		var prefix netip.Prefix
		if prefixOK {
			prefix = netip.PrefixFrom(ip, ones).Masked()
			if !isHostAddr(prefix, ip) {
				add(field("ip"), "cannot be the network or broadcast address of its subnet")
			}
			for _, t := range prefixes {
				if prefix.Overlaps(t.prefix) {
					add(field("netmask"), "this subnet (%s) overlaps %s (%s); use a different private range", prefix, t.what, t.prefix)
					if t.index >= 0 {
						add(fmt.Sprintf("networks[%d].netmask", t.index), "this subnet (%s) overlaps network %q (%s)", t.prefix, strings.TrimSpace(n.Name), prefix)
					}
					break
				}
			}
			prefixes = append(prefixes, taken{"another network", prefix, i})
		}

		validateDHCP(n.DHCP, prefix, ip, prefixOK, "this network's subnet",
			func(s string) string { return field("dhcp." + s) }, add)
	}

	// ports
	ports := map[string]bool{}
	for _, p := range c.LanInterfaces {
		ports[p] = true
	}
	// Sorted, so the errors come in a stable order
	names2 := make([]string, 0, len(c.PortNetworks))
	for p := range c.PortNetworks {
		names2 = append(names2, p)
	}
	sort.Strings(names2)

	for _, port := range names2 {
		pn := c.PortNetworks[port]
		field := func(name string) string { return "port_networks." + port + "." + name }
		if !ports[port] {
			add("port_networks."+port, "is not a LAN port")
			continue
		}
		if native := pn.NativeID(); native != DefaultNetworkID && !hasKey(ids, native) {
			add(field("native"), "is not a network that exists")
		}
		seen := map[string]bool{}
		for _, t := range pn.Tagged {
			switch {
			case t == DefaultNetworkID:
				add(field("tagged"), "the default network is not tagged; it is the native network of the ports that carry it")
			case !hasKey(ids, t):
				add(field("tagged"), "%q is not a network that exists", t)
			case t == pn.NativeID():
				add(field("tagged"), "%q is already this port's native network", t)
			case seen[t]:
				add(field("tagged"), "%q is listed more than once", t)
			default:
				// the VLAN interface is named port.vlan, and the kernel allows 15 characters
				if n, ok := c.FindNetwork(t); ok && len(port)+1+len(strconv.Itoa(n.VLAN)) > 15 {
					add(field("tagged"), "the port name %q is too long to carry VLAN %d (interface names are at most 15 characters)", port, n.VLAN)
				}
			}
			seen[t] = true
		}
	}
}

func hasKey[K comparable](m map[K]int, k K) bool { _, ok := m[k]; return ok }

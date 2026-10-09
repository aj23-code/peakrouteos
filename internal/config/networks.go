package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"
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

// Network is an additional local network. Its traffic is a VLAN of the LAN bridge: a port carries
// it untagged (as the port's native network) and/or tagged.
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

// Bridged reports whether the LAN is built on a VLAN-aware bridge (br0) because there are several
// networks. Without extra networks the LAN keeps its simple form: the port itself, or a plain bridge.
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

func (n Network) clone() Network {
	if n.DHCP != nil {
		d := *n.DHCP
		n.DHCP = &d
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

	type taken struct {
		what   string
		prefix netip.Prefix
	}
	var prefixes []taken
	if lanOK {
		prefixes = append(prefixes, taken{"the default network", lan})
	}
	if haveWan {
		prefixes = append(prefixes, taken{"the WAN", wan})
	}

	ids, names, vlans := map[string]bool{}, map[string]bool{}, map[int]bool{}
	for i, n := range c.Networks {
		field := func(name string) string { return fmt.Sprintf("networks[%d].%s", i, name) }

		switch {
		case !networkIDPattern.MatchString(n.ID):
			add(field("id"), "must be lower-case letters, digits and '-', starting with a letter (up to 16 characters)")
		case n.ID == DefaultNetworkID:
			add(field("id"), "%q is the default network", DefaultNetworkID)
		case ids[n.ID]:
			add(field("id"), "is used by another network")
		}
		ids[n.ID] = true

		name := strings.TrimSpace(n.Name)
		switch {
		case name == "" || len(name) > maxNetName || !networkNamePattern.MatchString(name):
			add(field("name"), "use 1 to %d letters, digits, spaces or . _ ( ) + - characters", maxNetName)
		case names[strings.ToLower(name)] || strings.EqualFold(name, "lan") || strings.EqualFold(name, "default"):
			add(field("name"), "is used by another network")
		}
		names[strings.ToLower(name)] = true

		if n.VLAN < 2 || n.VLAN > 4094 {
			add(field("vlan"), "must be between 2 and 4094 (VLAN 1 is the default network)")
		} else if vlans[n.VLAN] {
			add(field("vlan"), "is used by another network")
		}
		vlans[n.VLAN] = true

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
					break
				}
			}
			prefixes = append(prefixes, taken{"another network", prefix})
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
		if native := pn.NativeID(); native != DefaultNetworkID && !ids[native] {
			add(field("native"), "is not a network that exists")
		}
		seen := map[string]bool{}
		for _, t := range pn.Tagged {
			switch {
			case t == DefaultNetworkID:
				add(field("tagged"), "the default network is not tagged; it is the native network of the ports that carry it")
			case !ids[t]:
				add(field("tagged"), "%q is not a network that exists", t)
			case t == pn.NativeID():
				add(field("tagged"), "%q is already this port's native network", t)
			case seen[t]:
				add(field("tagged"), "%q is listed more than once", t)
			}
			seen[t] = true
		}
	}
}

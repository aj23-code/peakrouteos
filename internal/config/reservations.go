package config

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// Reservation keeps an address for one device: whenever the device (known by its hardware address) asks the DHCP server of the network
// the address is in, it gets this address. It is the router-side way to give a device a fixed address, better than setting one on the
// device itself: the address is managed in one place, cannot clash with the dynamic range, and a device reset to its defaults gets it back.
type Reservation struct {
	MAC  string `json:"mac"`            // lower case, colon separated
	IP   string `json:"ip"`             // inside one of the local networks, which has its DHCP server on
	Name string `json:"name,omitempty"` // the host name the device is known by in the router's DNS (optional)
}

const maxReservations = 256

// A host name as DNS has it: letters, digits and hyphens, not starting or ending with a hyphen, up to 63 characters.
var reservationNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// reservationNetwork finds which local network an address is in: its name, subnet and router address, and whether its DHCP server is
// on. ok is false when the address is in none of them.
type reservationNetwork struct {
	name   string
	prefix netip.Prefix
	router netip.Addr
	dhcpOn bool
}

func (c NetworkConfig) reservationNetworks(lanPrefix netip.Prefix, lanOK bool, lanIP netip.Addr) []reservationNetwork {
	var out []reservationNetwork
	if lanOK {
		out = append(out, reservationNetwork{"LAN", lanPrefix, lanIP, c.DHCP != nil && c.DHCP.Enabled})
	}
	for _, n := range c.Networks {
		ip, ipOK := parseIPv4(n.IP)
		ones, maskOK := parseNetmask(n.Netmask)
		if !ipOK || !maskOK {
			continue
		}
		out = append(out, reservationNetwork{n.Name, netip.PrefixFrom(ip, ones).Masked(), ip, n.DHCP != nil && n.DHCP.Enabled})
	}
	return out
}

// validateReservations checks every reservation. Errors are on "reservations[i].field", or "reservations" for the list as a whole; the
// messages name the address or device, since the order of the list is not what a person sees.
func (c NetworkConfig) validateReservations(add func(field, format string, args ...any), lanPrefix netip.Prefix, lanOK bool, lanIP netip.Addr) {
	if len(c.Reservations) > maxReservations {
		add("reservations", "at most %d reserved addresses are supported", maxReservations)
	}
	nets := c.reservationNetworks(lanPrefix, lanOK, lanIP)
	macs, ips, names := map[string]bool{}, map[string]bool{}, map[string]bool{}

	for i, r := range c.Reservations {
		field := func(name string) string { return fmt.Sprintf("reservations[%d].%s", i, name) }

		mac, macOK := NormalizeMAC(r.MAC)
		switch {
		case !macOK:
			add(field("mac"), "%q is not a device hardware address such as 02:11:22:33:44:55", r.MAC)
		case mac != r.MAC:
			add(field("mac"), "must be written as %s", mac)
		case macs[mac]:
			add(field("mac"), "%s is reserved more than once", mac)
		}
		if macOK {
			macs[mac] = true
		}

		ip, ipOK := parseIPv4(r.IP)
		if !ipOK {
			add(field("ip"), "%q is not an IPv4 address such as 192.168.1.50", r.IP)
		} else {
			var home *reservationNetwork
			for j := range nets {
				if nets[j].prefix.Contains(ip) {
					home = &nets[j]
					break
				}
			}
			switch {
			case home == nil:
				add(field("ip"), "%s is not inside any of your local networks", r.IP)
			case !isHostAddr(home.prefix, ip):
				add(field("ip"), "%s is the network or broadcast address of %s", r.IP, home.name)
			case ip == home.router:
				add(field("ip"), "%s is this router's own address on %s", r.IP, home.name)
			case !home.dhcpOn:
				add(field("ip"), "the DHCP server of %s is off, so it cannot keep %s for a device", home.name, r.IP)
			}
			if ips[r.IP] {
				add(field("ip"), "%s is reserved for more than one device", r.IP)
			}
			ips[r.IP] = true
		}

		if r.Name != "" {
			low := strings.ToLower(r.Name)
			switch {
			case !reservationNamePattern.MatchString(r.Name):
				add(field("name"), "%q is not a host name: use letters, digits and hyphens, up to 63 characters", r.Name)
			case names[low]:
				add(field("name"), "the name %s is used for more than one device", r.Name)
			}
			names[low] = true
		}
	}
}

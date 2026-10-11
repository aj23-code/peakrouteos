package config

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Network settings end up inside root-owned config files (interfaces, nftables, dnsmasq), so
// every value is validated strictly before it can be committed: a name or address containing a
// newline or an unexpected character must never reach a template.

// FieldError describes one invalid setting.
type FieldError struct {
	Field   string `json:"field"` // matches the JSON key, e.g. "lan_ip"
	Message string `json:"message"`
}

// ValidationError is the full list of problems found in a configuration.
type ValidationError []FieldError

func (v ValidationError) Error() string {
	parts := make([]string, len(v))
	for i, e := range v {
		parts[i] = e.Field + ": " + e.Message
	}
	return strings.Join(parts, "; ")
}

// Has reports whether the field has a problem.
func (v ValidationError) Has(field string) bool {
	for _, e := range v {
		if e.Field == field {
			return true
		}
	}
	return false
}

// Starts with a letter or digit; kernel interface names are at most 15 characters
var ifaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,14}$`)

var leaseTimePattern = regexp.MustCompile(`^([0-9]{1,6})([smhd])$`)

const minLeaseSeconds = 120 // dnsmasq silently raises anything shorter to 2 minutes

// IsValidInterfaceName reports whether name is a safe, well-formed interface name.
func IsValidInterfaceName(name string) bool { return ifaceNamePattern.MatchString(name) }

// ParseLeaseTime returns the lease length in seconds, or -1 for "infinite".
func ParseLeaseTime(s string) (int64, bool) {
	if s == "infinite" {
		return -1, true
	}
	m := leaseTimePattern.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.ParseInt(m[1], 10, 64)
	unit := map[string]int64{"s": 1, "m": 60, "h": 3600, "d": 86400}[m[2]]
	secs := n * unit
	return secs, secs >= minLeaseSeconds
}

// parseIPv4 accepts plain dotted-quad IPv4 only (no CIDR, zone, IPv6 or whitespace).
func parseIPv4(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a, true
}

// parseNetmask turns a dotted netmask such as 255.255.255.0 into a prefix length.
func parseNetmask(s string) (int, bool) {
	a, ok := parseIPv4(s)
	if !ok {
		return 0, false
	}
	b := a.As4()
	ones, bits := net.IPMask(b[:]).Size()
	if bits == 0 { // not a contiguous mask
		return 0, false
	}
	return ones, true
}

// checkNetmask validates a dotted netmask (a contiguous /8 to /30), reporting problems through
// add, and returns its prefix length.
func checkNetmask(mask, field string, add func(field, format string, args ...any)) (int, bool) {
	ones, ok := parseNetmask(mask)
	if !ok {
		add(field, "must be a netmask such as 255.255.255.0")
		return 0, false
	}
	if ones < 8 || ones > 30 {
		add(field, "must be between 255.0.0.0 and 255.255.255.252")
		return 0, false
	}
	return ones, true
}

func networkAddr(p netip.Prefix) netip.Addr { return p.Masked().Addr() }

func broadcastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	hostBits := 32 - p.Bits()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= (1 << hostBits) - 1
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// isHostAddr reports whether a is a usable host address inside p (not the network or
// broadcast address).
func isHostAddr(p netip.Prefix, a netip.Addr) bool {
	return p.Contains(a) && a != networkAddr(p) && a != broadcastAddr(p)
}

// Validate checks a router-mode network configuration and returns a ValidationError listing
// every problem, or nil if it is safe to render and apply. It checks structure only; whether
// the named interfaces exist on this machine is checked where the hardware is known.
func (c NetworkConfig) Validate() error {
	var errs ValidationError
	add := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}

	// --- WAN ---
	if !IsValidInterfaceName(c.WanInterface) {
		add("wan_interface", "must be an interface name (letters, digits, '.', '_' or '-', up to 15 characters)")
	}

	var wanPrefix netip.Prefix
	haveWanPrefix := false
	switch strings.ToLower(c.WanMode) {
	case "", "dhcp":
		// addressing comes from the upstream router
	case "pppoe":
		// the provider assigns the address over the PPP session
	case "static":
		ip, ipOK := parseIPv4(c.WanIP)
		if !ipOK {
			add("wan_ip", "must be an IPv4 address such as 203.0.113.10")
		}
		ones, maskOK := checkNetmask(c.WanSubnet, "wan_subnet", add)
		if ipOK && maskOK {
			wanPrefix, haveWanPrefix = netip.PrefixFrom(ip, ones).Masked(), true
			if !isHostAddr(wanPrefix, ip) {
				add("wan_ip", "cannot be the network or broadcast address of its subnet")
			}
		}
		if c.WanGateway != "" {
			gw, gwOK := parseIPv4(c.WanGateway)
			switch {
			case !gwOK:
				add("wan_gateway", "must be an IPv4 address")
			case haveWanPrefix && !isHostAddr(wanPrefix, gw):
				add("wan_gateway", "must be a host address inside the WAN subnet")
			case c.WanGateway == c.WanIP:
				add("wan_gateway", "cannot be the same as the WAN IP")
			}
		}
	default:
		add("wan_mode", "must be \"dhcp\", \"static\" or \"pppoe\"")
	}
	// DNS servers: required with a static address; with DHCP or PPPoE they replace the provider's
	if len(c.WanDNS) > 3 {
		add("wan_dns", "at most 3 DNS servers are supported")
	}
	for _, d := range c.WanDNS {
		if _, ok := parseIPv4(d); !ok {
			add("wan_dns", "%q is not an IPv4 address", d)
		}
	}
	c.validateWan(add)

	// --- LAN ---
	if len(c.LanInterfaces) == 0 {
		add("lan_interfaces", "at least one LAN interface is required")
	}
	seen := map[string]bool{}
	for _, name := range c.LanInterfaces {
		switch {
		case !IsValidInterfaceName(name):
			add("lan_interfaces", "%q is not a valid interface name", name)
		case name == c.WanInterface:
			add("lan_interfaces", "%q is the WAN interface and cannot also be a LAN port", name)
		case seen[name]:
			add("lan_interfaces", "%q is listed more than once", name)
		}
		seen[name] = true
	}

	lanIP, lanIPOK := parseIPv4(c.LanIP)
	if !lanIPOK {
		add("lan_ip", "must be an IPv4 address such as 192.168.1.1")
	}
	lanOnes, lanMaskOK := checkNetmask(c.LanSubnet, "lan_subnet", add)
	lanOK := lanIPOK && lanMaskOK
	lanPrefix := netip.PrefixFrom(lanIP, lanOnes).Masked()
	if lanOK {
		if !isHostAddr(lanPrefix, lanIP) {
			add("lan_ip", "cannot be the network or broadcast address of its subnet")
		}
		if haveWanPrefix && lanPrefix.Overlaps(wanPrefix) {
			add("lan_subnet", "overlaps the WAN subnet (%s); use a different private range", wanPrefix)
			// Reported on the WAN side too, so the internet page can show it next to its own field
			add("wan_subnet", "overlaps the LAN subnet (%s); one of the two must change", lanPrefix)
		}
	}

	// --- DHCP (nil = not managed by the JSON yet, which is valid) ---
	validateDHCP(c.DHCP, lanPrefix, lanIP, lanOK, "the LAN subnet",
		func(s string) string { return "dhcp_" + s }, add)

	// --- Further local networks ---
	c.validateNetworks(add, lanPrefix, lanOK, wanPrefix, haveWanPrefix)

	// --- Static routes ---
	c.validateRoutes(add, lanPrefix, lanOK, lanIP, wanPrefix, haveWanPrefix)

	// --- Country ---
	if c.Country != "" && !IsCountry(c.Country) {
		add("country", "must be a two-letter country code such as US or DE")
	}

	// --- Wi-Fi ---
	c.validateWifi(add)

	if len(errs) == 0 {
		return nil
	}
	return errs
}

const (
	maxRoutes      = 64
	maxRouteMetric = 9999
	maxRouteName   = 40
)

// Letters, digits, spaces and a little punctuation. Names are only shown back to people, but
// keeping them plain means they can never matter anywhere else.
var routeNamePattern = regexp.MustCompile(`^[A-Za-z0-9 ._,:()/+-]*$`)

// routeKey identifies a route: the same network may be listed once per metric.
func routeKey(p netip.Prefix, metric int) string { return fmt.Sprintf("%s|%d", p, metric) }

// validateRoutes checks every route (disabled ones too, so switching one on never surprises).
// Errors are reported on "routes[i].field", or "routes" for the list as a whole.
func (c NetworkConfig) validateRoutes(add func(field, format string, args ...any),
	lan netip.Prefix, lanOK bool, lanIP netip.Addr, wan netip.Prefix, haveWan bool) {

	if len(c.Routes) > maxRoutes {
		add("routes", "at most %d static routes are supported", maxRoutes)
	}

	seen := map[string]bool{}
	for i, r := range c.Routes {
		field := func(name string) string { return fmt.Sprintf("routes[%d].%s", i, name) }

		if len(r.Name) > maxRouteName || !routeNamePattern.MatchString(r.Name) {
			add(field("name"), "use up to %d letters, digits or . , : ( ) / + - characters", maxRouteName)
		}
		if r.Metric < 0 || r.Metric > maxRouteMetric {
			add(field("metric"), "must be between 0 and %d (0 uses the default)", maxRouteMetric)
		}

		// destination
		dest, destErr := netip.ParsePrefix(strings.TrimSpace(r.Destination))
		destOK := destErr == nil && dest.Addr().Is4() && dest.Addr().Zone() == ""
		switch {
		case !destOK:
			add(field("destination"), "must be a network such as 10.20.0.0/16")
		case dest.Bits() == 0:
			add(field("destination"), "the default route (0.0.0.0/0) comes from the internet connection, not from a static route")
		case dest != dest.Masked():
			add(field("destination"), "has host bits set; the network is %s", dest.Masked())
			destOK = false
		default:
			switch {
			case lanOK && lan.Contains(dest.Addr()) && dest.Bits() >= lan.Bits():
				add(field("destination"), "is already directly connected to the LAN (%s)", lan)
			case haveWan && wan.Contains(dest.Addr()) && dest.Bits() >= wan.Bits():
				add(field("destination"), "is already directly connected to the WAN (%s)", wan)
			}
		}

		// gateway
		gw, gwOK := parseIPv4(r.Gateway)
		switch {
		case !gwOK:
			add(field("gateway"), "must be an IPv4 address such as 192.168.1.2")
		case lanOK && gw == lanIP, c.WanIP != "" && r.Gateway == c.WanIP:
			add(field("gateway"), "is this router's own address")
		case lanOK && lan.Contains(gw):
			if !isHostAddr(lan, gw) {
				add(field("gateway"), "cannot be the network or broadcast address of the LAN")
			}
		case haveWan && wan.Contains(gw):
			if !isHostAddr(wan, gw) {
				add(field("gateway"), "cannot be the network or broadcast address of the WAN")
			}
		case c.WanKind() != WanStatic:
			// A DHCP or PPPoE uplink's subnet is only known once it is up; a wrong gateway is caught when applied
		case lanOK || haveWan:
			var where []string
			if lanOK {
				where = append(where, "the LAN subnet ("+lan.String()+")")
			}
			if haveWan {
				where = append(where, "the WAN subnet ("+wan.String()+")")
			}
			add(field("gateway"), "must be directly reachable: inside %s", strings.Join(where, " or "))
		}

		if destOK {
			key := routeKey(dest, r.Metric)
			if seen[key] {
				add(field("destination"), "is listed more than once with the same metric")
			}
			seen[key] = true
		}
	}
}

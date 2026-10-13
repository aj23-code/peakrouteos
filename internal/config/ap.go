package config

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Access point mode: the device is a bridge, not a router. Every cable port and every Wi-Fi network joins one network (br0); the device
// gets its own address on it (from the main router's DHCP server, or fixed) to be managed from. There is no NAT, DHCP server, firewall
// forwarding, extra networks or VPN: those settings stay in the file, unused, so that going back to a router finds them again.
//
// The address of the setup network (192.168.77.1/24) is kept on the bridge as a recovery address: the computer that set the device up
// keeps reaching it there even when the address from DHCP is not yet known.

const (
	APDHCP   = "dhcp"
	APStatic = "static"

	// The VLAN IDs a Wi-Fi network of an access point can be on (1 is the untagged network).
	MinAPVLAN = 2
	MaxVLAN   = 4094

	// APRecoveryAddress is kept on the bridge beside the real address.
	APRecoveryAddress = "192.168.77.1/24"
)

// APConfig is how the device is set up when it is an access point.
type APConfig struct {
	Enabled bool `json:"enabled"` // the device is an access point (otherwise a router)
	// Ports are the cable ports joined into the bridge.
	Ports   []string `json:"ports,omitempty"`
	Mode    string   `json:"mode,omitempty"` // APDHCP (the default) or APStatic
	IP      string   `json:"ip,omitempty"`
	Netmask string   `json:"netmask,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

func (a APConfig) clone() APConfig {
	a.Ports = append([]string(nil), a.Ports...)
	a.DNS = append([]string(nil), a.DNS...)
	if len(a.Ports) == 0 {
		a.Ports = nil
	}
	if len(a.DNS) == 0 {
		a.DNS = nil
	}
	return a
}

// APSettings returns the access point setup, empty when there is none.
func (c NetworkConfig) APSettings() APConfig {
	if c.AP == nil {
		return APConfig{}
	}
	return *c.AP
}

// IsAP reports whether the device is an access point rather than a router.
func (c NetworkConfig) IsAP() bool { return c.AP != nil && c.AP.Enabled }

// OperationMode is "AP" or "ROUTER".
func (c NetworkConfig) OperationMode() string {
	if c.IsAP() {
		return "AP"
	}
	return "ROUTER"
}

// APVLANs lists the VLAN IDs the Wi-Fi networks that are switched on use, smallest first (not the untagged network).
func (c NetworkConfig) APVLANs() []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range c.WifiSettings().Networks {
		if !n.Disabled && n.VLAN != 0 && !seen[n.VLAN] {
			seen[n.VLAN] = true
			out = append(out, n.VLAN)
		}
	}
	sort.Ints(out)
	return out
}

// ShownMode is what the pages show the device as, "AP" or "ROUTER": what it is now (the settings in effect), except right after the
// first-run setup, when nothing has been applied yet: then what it is being set up as (the draft).
func ShownMode(active, staged NetworkConfig, hasDraft bool) string {
	if active.IsAP() {
		return "AP"
	}
	if active.WanInterface == "" && len(active.LanInterfaces) == 0 && hasDraft && staged.IsAP() {
		return "AP"
	}
	return "ROUTER"
}

// APBridgeMembers are what the access point bridge holds: the cable ports, then the Wi-Fi links.
func (c NetworkConfig) APBridgeMembers() []string {
	out := append([]string(nil), c.APSettings().Ports...)
	for _, st := range c.WifiSettings().ActiveStations() {
		if st.Role == StationBridge {
			out = append(out, st.Name)
		}
	}
	return out
}

// validateAP checks a configuration for an access point. It checks what an access point uses: the ports and address, Wi-Fi, and the
// settings every device has. Errors are reported on ap.field.
func (c NetworkConfig) validateAP() error {
	var errs ValidationError
	add := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}
	ap := c.APSettings()

	seen := map[string]bool{}
	for _, p := range ap.Ports {
		switch {
		case !IsValidInterfaceName(p):
			add("ap.ports", "%q is not a valid port name", p)
		case seen[p]:
			add("ap.ports", "%q is listed more than once", p)
		}
		seen[p] = true
	}
	for _, st := range c.WifiSettings().Stations {
		if st.Role == StationBridge && !st.Disabled && seen[st.Name] {
			add("ap.ports", "%q is also a Wi-Fi link", st.Name)
		}
	}
	if len(c.APBridgeMembers()) == 0 {
		add("ap.ports", "choose at least one port (or link to another access point by Wi-Fi)")
	}

	switch ap.Mode {
	case "", APDHCP:
		// the address comes from the network's DHCP server
	case APStatic:
		ip, ipOK := parseIPv4(ap.IP)
		if !ipOK {
			add("ap.ip", "must be an IPv4 address such as 192.168.1.2")
		}
		ones, maskOK := checkNetmask(ap.Netmask, "ap.netmask", add)
		if ipOK && maskOK {
			p := netip.PrefixFrom(ip, ones).Masked()
			if !isHostAddr(p, ip) {
				add("ap.ip", "cannot be the network or broadcast address of its subnet")
			}
			if rec := netip.MustParsePrefix(APRecoveryAddress); rec.Overlaps(p) {
				add("ap.ip", "overlaps the recovery address range %s that the device keeps for setup", rec)
			}
			if ap.Gateway != "" {
				gw, gwOK := parseIPv4(ap.Gateway)
				switch {
				case !gwOK:
					add("ap.gateway", "must be an IPv4 address")
				case !isHostAddr(p, gw):
					add("ap.gateway", "must be a host address inside the subnet")
				case ap.Gateway == ap.IP:
					add("ap.gateway", "cannot be the same as the device's address")
				}
			}
		}
		validateServers(ap.DNS, "ap.dns", add)
	default:
		add("ap.mode", "must be \"dhcp\" or \"static\"")
	}

	if c.Country != "" && !IsCountry(c.Country) {
		add("country", "must be a two-letter country code such as US or DE")
	}
	c.validateWifi(add)
	c.validateSettings(add)
	c.validateSyslog(add)

	if len(errs) == 0 {
		return nil
	}
	return errs
}

func apText(c NetworkConfig) string {
	ap := c.APSettings()
	var addr string
	if ap.Mode == APStatic {
		addr = "fixed address " + text(ap.IP) + " / " + text(ap.Netmask)
		if ap.Gateway != "" {
			addr += ", gateway " + ap.Gateway
		}
	} else {
		addr = "address from DHCP"
	}
	return fmt.Sprintf("ports %s, %s", list(ap.Ports), addr)
}

func diffAP(active, candidate NetworkConfig) []Change {
	var changes []Change
	if active.IsAP() != candidate.IsAP() {
		changes = append(changes, Change{Field: "ap_enabled", Label: "Operation mode",
			Old: strings.ToLower(modeName(active)), New: strings.ToLower(modeName(candidate))})
	}
	if candidate.IsAP() || active.IsAP() {
		if before, after := apText(active), apText(candidate); before != after {
			changes = append(changes, Change{Field: "ap", Label: "Access point network", Old: before, New: after})
		}
	}
	return changes
}

func modeName(c NetworkConfig) string {
	if c.IsAP() {
		return "Access point"
	}
	return "Router"
}

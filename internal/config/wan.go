package config

import (
	"fmt"
	"regexp"
	"strings"
)

// How the internet connection gets its address.
const (
	WanDHCP   = "dhcp"
	WanStatic = "static"
	WanPPPoE  = "pppoe"
)

// PPPoEConfig is a PPP over Ethernet login (DSL and some fibre providers).
type PPPoEConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Service names the service to ask the provider's concentrator for; most providers want it left empty.
	Service string `json:"service,omitempty"`
}

// Limits.
const (
	MinWanMTU      = 576
	MaxWanMTU      = 9000
	MaxPPPoEMTU    = 1492 // 1500 less the 8 bytes PPPoE uses
	DefaultWanMTU  = 1500
	DefaultPPPoE   = 1492
	maxPPPoEString = 64
)

// WanKind is the connection type in its canonical lower-case form (empty means DHCP).
func (c NetworkConfig) WanKind() string {
	switch strings.ToLower(strings.TrimSpace(c.WanMode)) {
	case WanStatic:
		return WanStatic
	case WanPPPoE:
		return WanPPPoE
	}
	return WanDHCP
}

// WanLink is the interface the internet connection runs over: the port itself, or its VLAN interface
// (eth0.100) when the provider wants a tagged VLAN.
func (c NetworkConfig) WanLink() string {
	if c.WanVlan > 0 && c.WanInterface != "" {
		return fmt.Sprintf("%s.%d", c.WanInterface, c.WanVlan)
	}
	return c.WanInterface
}

// WanPPPInterface is the interface a PPPoE session creates.
const WanPPPInterface = "ppp0"

// WanDevice is the interface that carries the router's internet address and the traffic to and from the
// internet: the link itself, or the PPP interface on top of it. The firewall, the routes and the health
// checks all follow this one.
func (c NetworkConfig) WanDevice() string {
	if c.WanKind() == WanPPPoE {
		return WanPPPInterface
	}
	return c.WanLink()
}

// UsualWanMTU is the MTU a connection of this type runs with when none is set.
func (c NetworkConfig) UsualWanMTU() int {
	if c.WanKind() == WanPPPoE {
		return DefaultPPPoE
	}
	return DefaultWanMTU
}

// EffectiveWanMTU is the MTU the connection runs with: the one set, or the usual one for its type.
func (c NetworkConfig) EffectiveWanMTU() int {
	if c.WanMTU > 0 {
		return c.WanMTU
	}
	return c.UsualWanMTU()
}

var (
	macPattern      = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)
	hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// NormalizeMAC lower-cases a MAC address and accepts '-' as the separator; it reports whether the result
// is a usable unicast address (a cloned address cannot be a group address, or all zeros).
func NormalizeMAC(s string) (string, bool) {
	m := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ":"))
	if !macPattern.MatchString(m) {
		return m, false
	}
	var first int
	fmt.Sscanf(m[:2], "%x", &first)
	if first&1 == 1 || m == "00:00:00:00:00:00" {
		return m, false
	}
	return m, true
}

// SafeCredential reports whether a PPPoE user name, password or service name can be written into the
// PPP options file as a quoted string: printable, no quotes and no backslashes.
func SafeCredential(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r > 0x7e {
			return false
		}
	}
	return true
}

// validateWan checks the internet connection's settings beyond the addressing done in Validate.
func (c NetworkConfig) validateWan(add func(field, format string, args ...any)) {
	kind := c.WanKind()

	if c.WanVlan != 0 {
		switch {
		case c.WanVlan < 1 || c.WanVlan > 4094:
			add("wan_vlan", "must be between 1 and 4094, or empty for none")
		case c.WanInterface != "" && !IsValidInterfaceName(c.WanLink()):
			add("wan_vlan", "the port name is too long to add a VLAN to (%q is not a valid interface name)", c.WanLink())
		}
	}

	if c.WanMTU != 0 {
		max := MaxWanMTU
		if kind == WanPPPoE {
			max = MaxPPPoEMTU
		}
		if c.WanMTU < MinWanMTU || c.WanMTU > max {
			add("wan_mtu", "must be between %d and %d, or empty for the usual %d", MinWanMTU, max, c.UsualWanMTU())
		}
	}

	if c.WanMAC != "" {
		if m, ok := NormalizeMAC(c.WanMAC); !ok || m != c.WanMAC {
			add("wan_mac", "must be a hardware address such as 00:11:22:33:44:55 (lower case, not a group address)")
		}
	}

	if c.Hostname != "" && !hostnamePattern.MatchString(c.Hostname) {
		add("hostname", "must be a host name: letters, digits and '-', up to 63 characters")
	}
	if c.SendHostname && c.Hostname == "" {
		add("send_hostname", "needs a host name to send")
	}

	if kind == WanPPPoE {
		p := c.WanPPPoE
		switch {
		case p == nil || p.Username == "":
			add("pppoe_username", "required for a PPPoE connection")
		case len(p.Username) > maxPPPoEString || !SafeCredential(p.Username):
			add("pppoe_username", "must be plain printable characters, without quotes or backslashes, up to %d", maxPPPoEString)
		}
		if p == nil || p.Password == "" {
			add("pppoe_password", "required for a PPPoE connection")
		} else if len(p.Password) > maxPPPoEString || !SafeCredential(p.Password) {
			add("pppoe_password", "must be plain printable characters, without quotes or backslashes, up to %d", maxPPPoEString)
		}
		if p != nil && (len(p.Service) > maxPPPoEString || !SafeCredential(p.Service)) {
			add("pppoe_service", "must be plain printable characters, without quotes or backslashes, up to %d", maxPPPoEString)
		}
	} else if c.WanPPPoE != nil {
		add("wan_pppoe", "PPPoE settings are only used with a PPPoE connection")
	}
}

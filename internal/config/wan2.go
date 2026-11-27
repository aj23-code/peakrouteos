package config

import (
	"net/netip"
	"strings"
)

// A second internet connection (docs/wan-failover.md). The first connection keeps its flat fields (wan_interface, wan_mode, ...), so
// nothing about a router with one connection changes; the second is the object "wan2" with the same settings. Everything that renders
// or checks a connection takes it as a NetworkConfig whose wan_* fields hold that connection's (WanView), so one piece of code serves both.

// The ids of the two connections.
const (
	WanFirst  = "wan"
	WanSecond = "wan2"
)

// What the second connection runs on that the first does not: its PPP interface (the first is ppp0, from "unit 0" in the provider
// file) and its provider file.
const (
	WanPPPInterface2 = "ppp1"
	PPPProvider2     = "peakroute-wan2"
)

// Default route metrics when there are two connections: the preferred one is the lower. A router with one leaves the metric alone.
const (
	WanMetricPreferred = 100
	WanMetricOther     = 200
)

// WanConfig is the second internet connection: the same settings as the first.
type WanConfig struct {
	Interface string       `json:"interface,omitempty"`
	Mode      string       `json:"mode,omitempty"` // "dhcp", "static" or "pppoe"
	IP        string       `json:"ip,omitempty"`
	Subnet    string       `json:"subnet,omitempty"`
	Gateway   string       `json:"gateway,omitempty"`
	DNS       []string     `json:"dns,omitempty"`
	Vlan      int          `json:"vlan,omitempty"`
	MTU       int          `json:"mtu,omitempty"`
	MAC       string       `json:"mac,omitempty"`
	PPPoE     *PPPoEConfig `json:"pppoe,omitempty"`
	Probe     *ProbeConfig `json:"probe,omitempty"` // how this connection is watched
}

func (w WanConfig) clone() WanConfig {
	w.DNS = append([]string(nil), w.DNS...)
	if w.PPPoE != nil {
		p := *w.PPPoE
		w.PPPoE = &p
	}
	if w.Probe != nil {
		p := w.Probe.clone()
		w.Probe = &p
	}
	return w
}

// HasWan2 says whether a second internet connection is set up (it needs a port).
func (c NetworkConfig) HasWan2() bool { return c.Wan2 != nil && c.Wan2.Interface != "" }

// WanView returns the configuration as seen by one connection: for the first, the configuration itself; for the second, a copy whose
// wan_* fields are the second's (and which has no second of its own). ok is false for the second when there is none.
func (c NetworkConfig) WanView(id string) (NetworkConfig, bool) {
	switch id {
	case WanFirst:
		v := c.clone() // keeps the second connection and the preference: the metric of a default route needs to know there are two
		v.uplink = 0
		return v, true
	case WanSecond:
		if !c.HasWan2() {
			return NetworkConfig{}, false
		}
		w := c.Wan2.clone()
		v := c.clone()
		v.WanInterface, v.WanMode, v.WanIP, v.WanSubnet, v.WanGateway = w.Interface, w.Mode, w.IP, w.Subnet, w.Gateway
		v.WanDNS, v.WanVlan, v.WanMTU, v.WanMAC, v.WanPPPoE, v.WanProbe = w.DNS, w.Vlan, w.MTU, w.MAC, w.PPPoE, w.Probe
		v.uplink = 2 // and, like the first's view, the second connection and the preference stay (WanMetric)
		return v, true
	}
	return NetworkConfig{}, false
}

// WanViews are the views of the connections that exist, the first one first.
func (c NetworkConfig) WanViews() []NetworkConfig {
	first, _ := c.WanView(WanFirst)
	out := []NetworkConfig{first}
	if second, ok := c.WanView(WanSecond); ok {
		out = append(out, second)
	}
	return out
}

// WanDevices are the interfaces that carry the connections (one, or two).
func (c NetworkConfig) WanDevices() []string {
	var out []string
	for _, v := range c.WanViews() {
		out = append(out, v.WanDevice())
	}
	return out
}

// IsSecondWan reports that this is the view of the second connection.
func (c NetworkConfig) IsSecondWan() bool { return c.uplink == 2 }

// WanID is the id of the connection a view belongs to.
func (c NetworkConfig) WanID() string {
	if c.uplink == 2 {
		return WanSecond
	}
	return WanFirst
}

// WanPPPDevice is the interface a PPPoE session of this connection creates.
func (c NetworkConfig) WanPPPDevice() string {
	if c.uplink == 2 {
		return WanPPPInterface2
	}
	return WanPPPInterface
}

// WanPPPProvider is the name of the PPP provider file of this connection.
func (c NetworkConfig) WanPPPProvider() string {
	if c.uplink == 2 {
		return PPPProvider2
	}
	return "peakroute-wan"
}

// WanMetric is the metric of this connection's default route: none (0) with a single connection, else the preferred connection's is lower.
func (c NetworkConfig) WanMetric() int {
	if !c.HasWan2() {
		return 0
	}
	preferSecond := c.WanPreferred == WanSecond
	if (c.uplink == 2) == preferSecond {
		return WanMetricPreferred
	}
	return WanMetricOther
}

// wan2Fields maps the name of a field of the first connection to the name of the same field of the second.
var wan2Fields = map[string]string{
	"wan_interface": "wan2.interface", "wan_mode": "wan2.mode", "wan_ip": "wan2.ip", "wan_subnet": "wan2.subnet",
	"wan_gateway": "wan2.gateway", "wan_dns": "wan2.dns", "wan_vlan": "wan2.vlan", "wan_mtu": "wan2.mtu", "wan_mac": "wan2.mac",
	"wan_pppoe": "wan2.pppoe", "pppoe_username": "wan2.pppoe_username", "pppoe_password": "wan2.pppoe_password",
	"pppoe_service": "wan2.pppoe_service",
}

// validateWan2 checks the second connection, using the checks of the first on its view, and that it does not collide with anything.
func (c NetworkConfig) validateWan2(add func(field, format string, args ...any), lan netip.Prefix, lanOK bool, first netip.Prefix, haveFirst bool) {
	switch c.WanPreferred {
	case "", WanFirst, WanSecond:
	default:
		add("wan_preferred", "must be %q or %q", WanFirst, WanSecond)
	}
	if c.Wan2 == nil {
		if c.WanPreferred == WanSecond {
			add("wan_preferred", "there is no second internet connection to prefer")
		}
		return
	}
	v, ok := c.WanView(WanSecond)
	if !ok {
		// a second connection with no port yet is not a connection: nothing to check but that nothing else is set
		if c.Wan2.Interface == "" && (c.Wan2.Mode != "" || c.Wan2.IP != "" || c.Wan2.PPPoE != nil) {
			add("wan2.interface", "choose a port for the second internet connection")
		}
		return
	}
	// the checks of the first connection's settings, reported under the second's names; the host name is the router's, not a connection's
	prefix, havePrefix := v.validateWanLink(func(field, format string, args ...any) {
		if name, ok := wan2Fields[field]; ok {
			add(name, format, args...)
		} else if strings.HasPrefix(field, "wan_probe") {
			add("wan2.probe"+strings.TrimPrefix(field, "wan_probe"), format, args...)
		}
	})
	if v.WanInterface == c.WanInterface {
		add("wan2.interface", "%q is the port of the first internet connection", v.WanInterface)
	}
	for _, name := range c.LanInterfaces {
		if name == v.WanInterface {
			add("wan2.interface", "%q is a LAN port and cannot also be the second internet connection", name)
		}
	}
	if havePrefix {
		if lanOK && lan.Overlaps(prefix) {
			add("wan2.subnet", "overlaps the LAN subnet (%s); one of the two must change", lan)
		}
		if haveFirst && first.Overlaps(prefix) {
			add("wan2.subnet", "overlaps the first connection's subnet (%s): the router could not tell them apart", first)
		}
		for _, n := range c.Networks {
			ip, ipOK := parseIPv4(n.IP)
			ones, maskOK := parseNetmask(n.Netmask)
			if !ipOK || !maskOK {
				continue
			}
			if np := netip.PrefixFrom(ip, ones).Masked(); np.Overlaps(prefix) {
				add("wan2.subnet", "overlaps the subnet of the network %q (%s)", n.Name, np)
			}
		}
	}
}

// Wan2Text is the second connection in one line, for the review of changes.
func (c NetworkConfig) Wan2Text() string {
	if !c.HasWan2() {
		return valueNone
	}
	v, _ := c.WanView(WanSecond)
	parts := []string{v.WanLink()}
	switch v.WanKind() {
	case WanStatic:
		parts = append(parts, "static "+v.WanIP)
	case WanPPPoE:
		parts = append(parts, "PPPoE")
	default:
		parts = append(parts, "DHCP")
	}
	return strings.Join(parts, ", ")
}

// WanPreferredText says which connection the traffic prefers.
func (c NetworkConfig) WanPreferredText() string {
	if c.HasWan2() && c.WanPreferred == WanSecond {
		return "the second connection"
	}
	return "the first connection"
}

// WanSettings returns the settings of one connection (the first's from the flat fields) without its port.
func (c NetworkConfig) WanSettings(id string) WanConfig {
	if id == WanSecond {
		if c.Wan2 == nil {
			return WanConfig{}
		}
		return c.Wan2.clone()
	}
	w := WanConfig{Interface: c.WanInterface, Mode: c.WanMode, IP: c.WanIP, Subnet: c.WanSubnet, Gateway: c.WanGateway,
		DNS: append([]string(nil), c.WanDNS...), Vlan: c.WanVlan, MTU: c.WanMTU, MAC: c.WanMAC}
	if c.WanProbe != nil {
		p := c.WanProbe.clone()
		w.Probe = &p
	}
	if c.WanPPPoE != nil {
		p := *c.WanPPPoE
		w.PPPoE = &p
	}
	return w
}

// SetWanSettings stores the settings of one connection; the port (Interface) is left as it is, as the Ports page owns it.
func (c *NetworkConfig) SetWanSettings(id string, w WanConfig) {
	w = w.clone()
	if id == WanSecond {
		if c.Wan2 != nil {
			w.Interface = c.Wan2.Interface
		}
		c.Wan2 = &w
		return
	}
	c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway, c.WanDNS = w.Mode, w.IP, w.Subnet, w.Gateway, w.DNS
	c.WanVlan, c.WanMTU, c.WanMAC, c.WanPPPoE, c.WanProbe = w.Vlan, w.MTU, w.MAC, w.PPPoE, w.Probe
}

// WanIsPreferred says whether the connection is the one the traffic prefers.
func (c NetworkConfig) WanIsPreferred(id string) bool {
	if id == WanSecond {
		return c.HasWan2() && c.WanPreferred == WanSecond
	}
	return !(c.HasWan2() && c.WanPreferred == WanSecond)
}

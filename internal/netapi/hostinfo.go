package netapi

import (
	"net"
	"net/netip"
	"os"
	"strings"
)

// What the machine itself says about its network, for the access point's home page (read from the system, not from the settings).

// ParseRouteGateway reads /proc/net/route and returns the gateway of the default route that leaves by the device ("" for any device).
// The kernel writes addresses as little-endian hexadecimal.
func ParseRouteGateway(table, dev string) string {
	for _, line := range strings.Split(table, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" || (dev != "" && f[0] != dev) {
			continue
		}
		if len(f[2]) != 8 {
			continue
		}
		var b [4]byte
		ok := true
		for i := 0; i < 4; i++ {
			v := 0
			for _, c := range f[2][i*2 : i*2+2] {
				d := strings.IndexRune("0123456789ABCDEF", c)
				if d < 0 {
					d = strings.IndexRune("0123456789abcdef", c)
				}
				if d < 0 {
					ok = false
				}
				v = v*16 + d
			}
			b[3-i] = byte(v)
		}
		if ok && b != [4]byte{} {
			return netip.AddrFrom4(b).String()
		}
	}
	return ""
}

// ParseResolvConf lists the name servers of a resolv.conf.
func ParseResolvConf(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "nameserver" {
			out = append(out, f[1])
		}
	}
	return out
}

// DefaultGateway is the gateway of the default route that leaves by the device.
func DefaultGateway(dev string) string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	return ParseRouteGateway(string(b), dev)
}

// Resolvers are the name servers the system uses.
func Resolvers() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	return ParseResolvConf(string(b))
}

// InterfaceAddrs lists the IPv4 addresses of an interface (with their prefix length) and whether it is up.
func InterfaceAddrs(name string) (addrs []string, up bool) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return nil, false
	}
	list, _ := ifc.Addrs()
	for _, a := range list {
		if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
			addrs = append(addrs, p.String())
		}
	}
	return addrs, ifc.Flags&net.FlagUp != 0
}

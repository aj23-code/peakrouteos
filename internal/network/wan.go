package network

import (
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"peakroute/internal/config"
)

// The upstream resolvers a router with a DHCP uplink uses are the ones its provider handed out,
// which end up in resolv.conf. systemd-resolved machines (a developer's laptop) list a local stub
// there and keep the real servers in a second file.
const (
	resolvConf         = "/etc/resolv.conf"
	resolvedResolvConf = "/run/systemd/resolve/resolv.conf"
)

// ParseResolvConf returns the IPv4 nameservers listed in resolv.conf text, in order.
func ParseResolvConf(data []byte) []netip.Addr {
	var out []netip.Addr
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() {
			out = append(out, a)
		}
	}
	return out
}

// nonLocal drops loopback addresses (a local stub resolver is not "upstream").
func nonLocal(in []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, a := range in {
		if !a.IsLoopback() && !a.IsUnspecified() {
			out = append(out, a)
		}
	}
	return out
}

// SystemResolvers returns the resolvers the machine itself uses, skipping local stubs.
func SystemResolvers(read func(string) ([]byte, error)) []netip.Addr {
	for _, path := range []string{resolvConf, resolvedResolvConf} {
		if data, err := read(path); err == nil {
			if r := nonLocal(ParseResolvConf(data)); len(r) > 0 {
				return r
			}
		}
	}
	return nil
}

// UpstreamResolvers returns the DNS servers the internet connection uses, and where they came
// from: the configured ones for a static address, the provider's otherwise.
func UpstreamResolvers(cfg config.NetworkConfig, read func(string) ([]byte, error)) ([]netip.Addr, string) {
	var configured []netip.Addr
	for _, s := range cfg.WanDNS {
		if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
			configured = append(configured, a)
		}
	}
	// servers set by hand win with every connection type; a static address never falls back to the
	// machine's own resolvers
	if len(configured) > 0 || strings.EqualFold(cfg.WanMode, "static") {
		return configured, "DNS server you configured"
	}
	return SystemResolvers(read), "DNS server from your provider"
}

// DefaultGateway finds the default route's gateway for a port in /proc/net/route text.
func DefaultGateway(route []byte, port string) string {
	for i, line := range strings.Split(string(route), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 || f[0] != port || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(v)) // the kernel prints the address in host order
		return netip.AddrFrom4(b).String()
	}
	return ""
}

// WANConnection describes how a port is connected right now: whether it has a link, and its first
// IPv4 address (as CIDR) and default gateway.
func WANConnection(port string) (up bool, address, gateway string) {
	if data, err := os.ReadFile("/sys/class/net/" + port + "/operstate"); err == nil {
		up = strings.TrimSpace(string(data)) == "up"
	}
	if iface, err := net.InterfaceByName(port); err == nil {
		if addrs, err := iface.Addrs(); err == nil {
			for _, a := range addrs {
				if p, err := netip.ParsePrefix(a.String()); err == nil && p.Addr().Is4() {
					address = p.String()
					break
				}
			}
		}
		// a PPP link reports its state as "unknown": it is up when the kernel says so and it has an address
		if !up && address != "" && iface.Flags&net.FlagUp != 0 {
			if data, err := os.ReadFile("/sys/class/net/" + port + "/operstate"); err == nil && strings.TrimSpace(string(data)) == "unknown" {
				up = true
			}
		}
	}
	if route, err := os.ReadFile("/proc/net/route"); err == nil {
		gateway = DefaultGateway(route, port)
	}
	return up, address, gateway
}

// UpstreamGateway returns the router in front of this one: the configured gateway for a static
// address, otherwise the default route the kernel learned over DHCP.
func UpstreamGateway(cfg config.NetworkConfig, read func(string) ([]byte, error)) (netip.Addr, bool) {
	gw := cfg.WanGateway
	if !strings.EqualFold(cfg.WanMode, "static") {
		gw = ""
		dev := cfg.WanDevice()
		if route, err := read("/proc/net/route"); err == nil {
			gw = DefaultGateway(route, dev)
		}
		// a PPPoE session has no gateway address: the provider's end of the link is what is in front of us
		if gw == "" && cfg.WanKind() == config.WanPPPoE {
			gw = PPPPeer(dev)
		}
	}
	a, err := netip.ParseAddr(gw)
	if err != nil || !a.Is4() {
		return netip.Addr{}, false
	}
	return a, true
}

// KernelRoute is one entry of the kernel's IPv4 routing table.
type KernelRoute struct {
	Destination string // CIDR, "0.0.0.0/0" for the default route
	Gateway     string // empty for a network the router is directly on
	Interface   string
	Metric      int
}

// ParseRoutingTable reads /proc/net/route text. The kernel prints addresses as host-order hex,
// so on a little-endian machine 0100A8C0 is 192.168.0.1.
func ParseRoutingTable(data []byte) []KernelRoute {
	hexAddr := func(s string) (netip.Addr, bool) {
		v, err := strconv.ParseUint(s, 16, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(v))
		return netip.AddrFrom4(b), true
	}

	var out []KernelRoute
	for i, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 8 {
			continue
		}
		dest, ok1 := hexAddr(f[1])
		gw, ok2 := hexAddr(f[2])
		mask, ok3 := hexAddr(f[7])
		metric, err := strconv.Atoi(f[6])
		if !ok1 || !ok2 || !ok3 || err != nil {
			continue
		}
		m := mask.As4()
		ones, bits := net.IPMask(m[:]).Size()
		if bits == 0 { // not a contiguous mask
			continue
		}
		r := KernelRoute{Destination: netip.PrefixFrom(dest, ones).String(), Interface: f[0], Metric: metric}
		if !gw.IsUnspecified() {
			r.Gateway = gw.String()
		}
		out = append(out, r)
	}
	return out
}

// ParsePeer returns the far end of a point-to-point link from `ip -4 -o addr show dev <ppp>` text, such
// as "5: ppp0    inet 10.0.0.2 peer 10.64.64.64/32 scope global ppp0".
func ParsePeer(text string) string {
	f := strings.Fields(text)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "peer" {
			if p, err := netip.ParsePrefix(f[i+1]); err == nil && p.Addr().Is4() {
				return p.Addr().String()
			}
			if a, err := netip.ParseAddr(f[i+1]); err == nil && a.Is4() {
				return a.String()
			}
		}
	}
	return ""
}

// PPPPeer finds the provider's end of a PPP link ("" when there is none). A variable so tests can replace it.
var PPPPeer = func(device string) string {
	out, err := exec.Command("ip", "-4", "-o", "addr", "show", "dev", device).Output()
	if err != nil {
		return ""
	}
	return ParsePeer(string(out))
}

// LinkDetails reads an interface's hardware address and MTU from the kernel (zero values when it is not there).
func LinkDetails(name string) (mac string, mtu int) {
	if name == "" {
		return "", 0
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", 0
	}
	return strings.ToLower(iface.HardwareAddr.String()), iface.MTU
}

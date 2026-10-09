package network

import (
	"encoding/binary"
	"net"
	"net/netip"
	"os"
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
	if strings.EqualFold(cfg.WanMode, "static") {
		var out []netip.Addr
		for _, s := range cfg.WanDNS {
			if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
				out = append(out, a)
			}
		}
		return out, "DNS server you configured"
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
	}
	if route, err := os.ReadFile("/proc/net/route"); err == nil {
		gateway = DefaultGateway(route, port)
	}
	return up, address, gateway
}

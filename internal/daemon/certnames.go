package daemon

import (
	"net"
	"strings"

	"peakroute/internal/bootstrap"
	"peakroute/internal/config"
)

// certAddresses are the addresses the web interface's certificate should be good for. A router's are its own settings (the LAN's
// address and every local network's). An access point has none of those (a router's leftover settings say 192.168.1.1, which is the
// main router's): its own address is the one its bridge has now, usually from the main router's DHCP server, or the fixed one, beside
// the recovery address. The access point build is an access point before its first settings are applied too (the setup network).
func certAddresses(cfg config.NetworkConfig, apOnly bool, ifaceAddrs func(name string) []net.IP) []net.IP {
	var out []net.IP
	seen := map[string]bool{}
	add := func(ip net.IP) {
		if ip != nil && !seen[ip.String()] {
			seen[ip.String()] = true
			out = append(out, ip)
		}
	}
	if !cfg.IsAP() && !apOnly {
		add(net.ParseIP(cfg.LanIP))
		for _, nw := range cfg.Networks {
			add(net.ParseIP(nw.IP))
		}
		return out
	}
	for _, name := range []string{"br0", bootstrap.Bridge} {
		for _, ip := range ifaceAddrs(name) {
			add(ip)
		}
	}
	if ap := cfg.APSettings(); ap.Mode == config.APStatic {
		add(net.ParseIP(ap.IP))
	}
	recovery, _, _ := strings.Cut(config.APRecoveryAddress, "/")
	add(net.ParseIP(recovery))
	return out
}

// interfaceAddrs lists the addresses an interface has now that a browser could use (no link-local ones); none if it is not there.
func interfaceAddrs(name string) []net.IP {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLinkLocalUnicast() && !n.IP.IsLoopback() {
			out = append(out, n.IP)
		}
	}
	return out
}

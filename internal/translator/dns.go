package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// How the router's DNS is written (docs/dns.md has the model and the reasons):
//
//   - What each network is told to use: the DHCP file's dns-server option names the router, or, when the network has
//     servers of its own, those servers. With none set the file is exactly what it was.
//   - The router's own answers (the local domain, the local names and the forwarded domains) go into one more file for
//     dnsmasq, /etc/dnsmasq.d/peakroute-dns.conf. It exists only while there is something to say; without a domain,
//     names or forwards it is not written at all.
//   - The upstream servers (the provider's, or the ones set by hand) are the existing upstream file's business.

// PathDnsmasqLocal is the file with the local domain, the local names and the forwarded domains.
const PathDnsmasqLocal = "/etc/dnsmasq.d/peakroute-dns.conf"

// dnsServerOption writes the dns-server value of a network's DHCP answer: its own servers, or the router.
func dnsServerOption(servers []string, routerIP string) string {
	if len(servers) == 0 {
		return routerIP
	}
	return strings.Join(servers, ",")
}

// ensureSafeDNS is the render-time twin of the DNS validation: everything written into the dnsmasq files must be plain.
func ensureSafeDNS(cfg config.NetworkConfig) error {
	if cfg.Domain != "" && !config.ValidDomain(cfg.Domain) {
		return fmt.Errorf("refusing to render the unsafe domain %q", cfg.Domain)
	}
	servers := append([]string(nil), cfg.LanDNSServers...)
	for _, n := range cfg.Networks {
		servers = append(servers, n.DNSServers...)
	}
	d := cfg.DNSSettings()
	for _, h := range d.Hosts {
		if !config.ValidDNSName(h.Name) {
			return fmt.Errorf("refusing to render the unsafe local name %q", h.Name)
		}
		servers = append(servers, h.Address)
	}
	for _, f := range d.Forwards {
		if !config.ValidDNSName(f.Domain) {
			return fmt.Errorf("refusing to render the unsafe forwarded domain %q", f.Domain)
		}
		servers = append(servers, f.Servers...)
	}
	return ensureSafeAddresses(servers)
}

// ensureSafeAddresses refuses anything that is not a plain IPv4 address.
func ensureSafeAddresses(addrs []string) error {
	for _, a := range addrs {
		if !config.IsIPv4(a) {
			return fmt.Errorf("refusing to render the unsafe address %q", a)
		}
	}
	return nil
}

// renderDnsmasqLocal writes the router's own DNS answers. ok is false when there is nothing to write.
func renderDnsmasqLocal(cfg config.NetworkConfig) (string, bool) {
	d := cfg.DNSSettings()
	var lines []string

	if cfg.Domain != "" {
		// the domain goes to the devices, is answered by the router alone (never asked of the upstream servers), and
		// short names get it added
		lines = append(lines, "domain="+cfg.Domain, "local=/"+cfg.Domain+"/", "expand-hosts")
	}
	for _, h := range d.Hosts {
		if h.Disabled {
			continue
		}
		names := []string{h.Name}
		// a short name also answers with the domain added
		if cfg.Domain != "" && !strings.Contains(h.Name, ".") {
			names = append(names, h.Name+"."+cfg.Domain)
		}
		lines = append(lines, "host-record="+strings.Join(names, ",")+","+h.Address)
	}
	for _, f := range d.Forwards {
		if f.Disabled {
			continue
		}
		for _, s := range f.Servers {
			lines = append(lines, fmt.Sprintf("server=/%s/%s", f.Domain, s))
		}
	}
	if len(lines) == 0 {
		return "", false
	}
	return "# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n" + strings.Join(lines, "\n") + "\n", true
}

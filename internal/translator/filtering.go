package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// DNS filtering (docs/dns-filtering.md). A network that filters has its DNS questions (port 53, to the router or to any server it
// has picked for itself) redirected by the firewall to AdGuard Home, which listens on a port of its own on every address and asks
// this router's own DNS server (dnsmasq, over the loopback) for the answers it does not block. So dnsmasq keeps doing DHCP, the local
// names, the forwarded domains and the upstream choice, AdGuard Home sees each device's real address, and nothing about DHCP changes.
const (
	// DNSFilterPort is where AdGuard Home listens for DNS. It is not 5353 (mDNS).
	DNSFilterPort = 5335
	// PathFiltering is present while any network filters; the AdGuard Home service runs only then (its unit has a condition on it).
	PathFiltering = "/etc/adguardhome/peakroute-enabled"
	// PathAdGuardHome is the program the image carries.
	PathAdGuardHome = "/usr/lib/peakroute/AdGuardHome"
	SvcFiltering    = "filtering"
	unitAdGuard     = "peakroute-adguard"
)

// filterInterfaces lists the interfaces of the networks that filter, the default network first.
func filterInterfaces(cfg config.NetworkConfig, lan string) []string {
	var out []string
	if cfg.LanFilter {
		out = append(out, lan)
	}
	for _, n := range cfg.Networks {
		if n.Filter {
			out = append(out, networkIface(n))
		}
	}
	return out
}

// renderFilteringMarker is the file that says the service should run, and for whom (for a person reading the file).
func renderFilteringMarker(cfg config.NetworkConfig, lan string) (string, bool) {
	ifaces := filterInterfaces(cfg, lan)
	if len(ifaces) == 0 {
		return "", false
	}
	return "# Written by PeakRoute from the networks that filter ads and trackers (Local Networks). While this file exists, AdGuard Home runs.\n" +
		"# Filtering: " + strings.Join(ifaces, " ") + "\n", true
}

// nftablesFilterChain redirects the DNS questions of the networks that filter to AdGuard Home. The packet keeps its sender, so
// AdGuard Home knows which device asked. It is a chain of its own in the nat table (beside the one the port forwards use).
func nftablesFilterChain(cfg config.NetworkConfig, lan string) string {
	ifaces := filterInterfaces(cfg, lan)
	if len(ifaces) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("    chain dns_filter {\n        type nat hook prerouting priority dstnat; policy accept;\n")
	for _, i := range ifaces {
		fmt.Fprintf(&b, "        iifname \"%s\" udp dport 53 redirect to :%d\n", i, DNSFilterPort)
		fmt.Fprintf(&b, "        iifname \"%s\" tcp dport 53 redirect to :%d\n", i, DNSFilterPort)
	}
	b.WriteString("    }\n")
	return b.String()
}

// filterCommand starts AdGuard Home with the marker and stops it without. Without the program (an image from before this feature)
// there is nothing to stop; wanting it and not having it fails the commit, which says why.
var filterCommand = command{"sh", []string{"-c", `if [ -f ` + PathFiltering + ` ]; then ` +
	`[ -x ` + PathAdGuardHome + ` ] || { echo "AdGuard Home is not in this system image: update to a version that has it" >&2; exit 1; }; ` +
	`systemctl enable ` + unitAdGuard + ` && systemctl start ` + unitAdGuard + `; ` +
	`else [ -x ` + PathAdGuardHome + ` ] || exit 0; systemctl disable --now ` + unitAdGuard + `; fi`}}

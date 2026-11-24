package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// mDNS (Bonjour, multicast DNS) is link-local: a device asks "who is printer.local?" on its own network and nobody on another
// network ever hears it. avahi-daemon, in its reflector mode, repeats what it hears on one of the networks that have mDNS switched on
// onto the others, so a phone on one network finds the speaker or printer on another. It also answers for the router's own name there.
// It runs on the router, so it needs the networks' mDNS packets (port 5353) to reach the router and nothing more: what the devices do
// after finding each other is still decided by the isolation switches.
const (
	PathAvahi = "/etc/avahi/avahi-daemon.conf"
	SvcMDNS   = "mdns"
)

// mdnsInterfaces lists the interfaces of the networks that share mDNS: the default network first, then the added ones.
func mdnsInterfaces(cfg config.NetworkConfig, lan string) []string {
	var out []string
	if cfg.LanMDNS {
		out = append(out, lan)
	}
	for _, n := range cfg.Networks {
		if n.MDNS {
			out = append(out, networkIface(n))
		}
	}
	return out
}

// renderAvahi writes avahi's configuration; ok is false when no network shares mDNS (there is then no file, and the service is stopped).
// The image ships without avahi's own file, so "no file" and "not running" mean the same thing.
func renderAvahi(cfg config.NetworkConfig, lan string) (string, bool) {
	ifaces := mdnsInterfaces(cfg, lan)
	if len(ifaces) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString("# Written by PeakRoute from the networks that share mDNS (Local Networks). Changes made here are lost.\n")
	b.WriteString("[server]\n")
	b.WriteString("use-ipv4=yes\n")
	b.WriteString("use-ipv6=yes\n")
	fmt.Fprintf(&b, "allow-interfaces=%s\n", strings.Join(ifaces, ","))
	b.WriteString("\n[wide-area]\nenable-wide-area=no\n")
	b.WriteString("\n[publish]\n")
	b.WriteString("publish-addresses=yes\n") // the router's own name (name.local) on these networks, and nothing else about it
	b.WriteString("publish-hinfo=no\npublish-workstation=no\npublish-domain=no\n")
	b.WriteString("\n[reflector]\nenable-reflector=yes\n")
	return b.String(), true
}

// mdnsCommand brings the service in line with the file: running with it, stopped and not started at boot without it. A machine
// without avahi (an image from before mDNS) has nothing to stop; with the file it needs the package.
var mdnsCommand = command{"sh", []string{"-c", `if [ -f ` + PathAvahi + ` ]; then ` +
	`command -v avahi-daemon >/dev/null 2>&1 || { echo "avahi-daemon is not installed on this system" >&2; exit 1; }; ` +
	`systemctl enable avahi-daemon.socket avahi-daemon.service && systemctl restart avahi-daemon.socket avahi-daemon.service; ` +
	`else command -v avahi-daemon >/dev/null 2>&1 || exit 0; systemctl disable --now avahi-daemon.service avahi-daemon.socket; fi`}}

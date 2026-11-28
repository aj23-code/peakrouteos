package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// LLDP (docs/device-pages.md, phase 2): lldpd listens on the router's LAN ports to hear what is plugged into them (a switch, an access point:
// its name, address and the port it is) and announces this router to them, so that a switch lists it by name. It is off by default and
// never on the internet port. A file only while it is switched on, as for mDNS; the image has the program but not running.
const (
	PathLLDP = "/etc/lldpd.d/peakroute.conf"
	SvcLLDP  = "lldp"
)

// lldpPorts are the physical ports lldpd listens on: the LAN ports.
func lldpPorts(cfg config.NetworkConfig) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range cfg.LanInterfaces {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// renderLLDP writes lldpd's configuration; ok is false while LLDP is off (or there is no LAN port to listen on).
func renderLLDP(cfg config.NetworkConfig) (string, bool) {
	ports := lldpPorts(cfg)
	if !cfg.LLDP || len(ports) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString("# Written by PeakRoute OS from Ports, Neighbours. Do not edit: this file is rewritten on every commit.\n")
	fmt.Fprintf(&b, "configure system interface pattern %s\n", strings.Join(ports, ","))
	b.WriteString("configure system description \"PeakRoute OS\"\n")
	b.WriteString("configure lldp portidsubtype ifname\n")
	return b.String(), true
}

func ensureSafeLLDP(cfg config.NetworkConfig) error {
	return ensureSafe(lldpPorts(cfg)...)
}

// lldpd runs as the system user _lldpd (privilege separation, it refuses to start without), which the lldpd package makes when it is installed.
// /etc is an overlay whose upper layer holds the passwd and group files from the day an account was first made on the machine, so a user a
// newer image adds never shows: the command makes the user and group when they are missing (it found this on a router updated from an
// image without lldpd: "no _lldpd user for privilege separation").
//
// lldpCommand brings the service in line with the file: running with it, stopped and not started at boot without it. A machine without lldpd
// (an image from before this) has nothing to stop; with the file it needs the package.
var lldpCommand = command{"sh", []string{"-c", `if [ -f ` + PathLLDP + ` ]; then ` +
	`command -v lldpd >/dev/null 2>&1 || { echo "lldpd is not installed on this system" >&2; exit 1; }; ` +
	`getent group _lldpd >/dev/null || groupadd --system _lldpd; ` +
	`getent passwd _lldpd >/dev/null || useradd --system --gid _lldpd --home-dir /run/lldpd --no-create-home --shell /usr/sbin/nologin _lldpd; ` +
	`systemctl enable lldpd.service && systemctl restart lldpd.service; ` +
	`else command -v lldpd >/dev/null 2>&1 || exit 0; systemctl disable --now lldpd.service; fi`}}

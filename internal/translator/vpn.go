package translator

import (
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"peakroute/internal/config"
	"peakroute/internal/wg"
)

// How the VPN is written (docs/vpn.md has the model and the reasons).
//
// Every tunnel is a WireGuard interface made by wg-quick from /etc/wireguard/<interface>.conf: wgra for the remote access server
// and wg<id> (wgs1, wgs2...) for the site tunnels. A script, written while there is any tunnel, brings them in line with the
// files on every commit: it takes down what is no longer wanted, and brings up (or, when its file changed, restarts) what is.
// The directory belongs to PeakRoute OS. The tunnels are zones of the firewall, like Local Networks.

const (
	SvcWireGuard = "wireguard"

	PathWireGuardDir = "/etc/wireguard"
	PathWGSync       = "/usr/local/sbin/peakroute-wg-sync"
	PathDnsmasqVPN   = "/etc/dnsmasq.d/peakroute-vpn.conf"

	// the largest segment of a TCP connection through a WireGuard tunnel (a tunnel's MTU is 1420)
	wgMSS = 1380
)

func wgConfPath(iface string) string { return PathWireGuardDir + "/" + iface + ".conf" }

// vpnZone is what the translator needs of a tunnel.
type vpnZone = config.Zone

// ensureSafeVPN is the render-time twin of the VPN validation: everything written to a WireGuard file or a ruleset must be exactly
// what the validation accepts.
func ensureSafeVPN(cfg config.NetworkConfig) error {
	v := cfg.VPNSettings()
	key := func(what, k string, optional bool) error {
		if k == "" && optional {
			return nil
		}
		if !wg.ValidKey(k) {
			return fmt.Errorf("refusing to render %s that is not a WireGuard key", what)
		}
		return nil
	}
	prefix := func(what, p string, v4 bool) error {
		pp, err := netip.ParsePrefix(p)
		if err != nil || !pp.Addr().Is4() {
			return fmt.Errorf("refusing to render the unsafe %s %q", what, p)
		}
		return nil
	}
	endpoint := func(e string) error {
		if e == "" {
			return nil
		}
		i := strings.LastIndex(e, ":")
		host, port := e, ""
		if i >= 0 {
			host, port = e[:i], e[i+1:]
		}
		if !config.ValidServerHost(host) {
			return fmt.Errorf("refusing to render the unsafe endpoint %q", e)
		}
		if port != "" {
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("refusing to render the unsafe endpoint %q", e)
			}
		}
		return nil
	}
	if r := v.WGRemote; r != nil {
		if err := key("the remote access key", r.PrivateKey, false); err != nil {
			return err
		}
		if r.ListenPort < 1 || r.ListenPort > 65535 {
			return fmt.Errorf("refusing to render the unsafe remote access port %d", r.ListenPort)
		}
		for _, p := range append([]string{r.Network}, r.Routes...) {
			if err := prefix("network", p, true); err != nil {
				return err
			}
		}
		if err := endpoint(r.Endpoint); err != nil {
			return err
		}
		for _, cl := range r.Clients {
			if err := ensureSafe(cl.ID); err != nil {
				return err
			}
			if err := key("a client's key", cl.PublicKey, false); err != nil {
				return err
			}
			if err := key("a client's preshared key", cl.PresharedKey, true); err != nil {
				return err
			}
			if err := prefix("client address", cl.Address, true); err != nil {
				return err
			}
		}
	}
	for _, s := range v.Sites {
		if s.Engine == config.EngineOpenVPN {
			continue // ensureSafeOVPN
		}
		if err := ensureSafe(s.ID); err != nil {
			return err
		}
		for what, k := range map[string]string{"a tunnel's key": s.PrivateKey, "a peer's key": s.PeerPublicKey} {
			if err := key(what, k, false); err != nil {
				return err
			}
		}
		if err := key("a preshared key", s.PresharedKey, true); err != nil {
			return err
		}
		if s.ListenPort < 1 || s.ListenPort > 65535 || s.Keepalive < 0 || s.Keepalive > 600 {
			return fmt.Errorf("refusing to render the unsafe tunnel port or keepalive of %s", s.ID)
		}
		for _, p := range append([]string{s.Address}, s.RemoteNetworks...) {
			if err := prefix("network", p, true); err != nil {
				return err
			}
		}
		if err := endpoint(s.Endpoint); err != nil {
			return err
		}
	}
	return nil
}

func renderWGSite(s config.VPNSite) string {
	var b strings.Builder
	b.WriteString("# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n")
	fmt.Fprintf(&b, "# Site tunnel %s: %s\n", s.ID, commentSafe(strings.TrimSpace(s.Name)))
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", s.PrivateKey)
	fmt.Fprintf(&b, "ListenPort = %d\n", s.ListenPort)
	fmt.Fprintf(&b, "Address = %s\n", s.Address)
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", s.PeerPublicKey)
	if s.PresharedKey != "" {
		fmt.Fprintf(&b, "PresharedKey = %s\n", s.PresharedKey)
	}
	// what the peer may send: the tunnel's own small network (the other router's address in it) and the networks behind it
	tunnel, _ := netip.ParsePrefix(s.Address)
	allowed := append([]string{tunnel.Masked().String()}, s.RemoteNetworks...)
	fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(allowed, ", "))
	if s.Endpoint != "" {
		fmt.Fprintf(&b, "Endpoint = %s\n", s.Endpoint)
	}
	if s.Keepalive > 0 {
		fmt.Fprintf(&b, "PersistentKeepalive = %d\n", s.Keepalive)
	}
	return b.String()
}

func renderWGRemote(r config.WGRemote) string {
	server, _ := r.ServerAddress()
	pool, _ := netip.ParsePrefix(r.Network)
	var b strings.Builder
	b.WriteString("# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n")
	b.WriteString("# Remote access server\n")
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", r.PrivateKey)
	fmt.Fprintf(&b, "ListenPort = %d\n", r.ListenPort)
	fmt.Fprintf(&b, "Address = %s/%d\n", server, pool.Bits())
	for _, cl := range r.Clients {
		if cl.Disabled {
			continue
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "# Client %s: %s\n[Peer]\n", cl.ID, commentSafe(strings.TrimSpace(cl.Name)))
		fmt.Fprintf(&b, "PublicKey = %s\n", cl.PublicKey)
		if cl.PresharedKey != "" {
			fmt.Fprintf(&b, "PresharedKey = %s\n", cl.PresharedKey)
		}
		fmt.Fprintf(&b, "AllowedIPs = %s\n", cl.Address)
	}
	return b.String()
}

// wgSyncScript brings the tunnels in line with the files. It only touches interfaces named wgra or wgs<number>, so an interface
// somebody made by hand is left alone; what it brought up is remembered (with a fingerprint of the file) in /run, so that a tunnel is
// restarted only when its file changed or it is not up.
const wgSyncScript = `#!/bin/sh
# Brings the WireGuard tunnels in line with /etc/wireguard (PeakRoute OS; rewritten on every commit that has tunnels).
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
DIR=/etc/wireguard
STATE=/run/peakroute-wg
mkdir -p "$STATE"
rc=0
mine() { case "$1" in wgra|wgs[0-9]|wgs[0-9][0-9]) return 0 ;; esac; return 1; }
# tunnels that are no longer wanted
for f in "$STATE"/*.sha; do
	[ -e "$f" ] || continue
	i=$(basename "$f" .sha)
	mine "$i" || continue
	if [ ! -f "$DIR/$i.conf" ]; then
		wg-quick down "$i" >/dev/null 2>&1 || ip link del "$i" 2>/dev/null
		rm -f "$f"
	fi
done
# tunnels that are wanted
for conf in "$DIR"/*.conf; do
	[ -e "$conf" ] || continue
	i=$(basename "$conf" .conf)
	mine "$i" || continue
	new=$(sha256sum "$conf" | cut -d' ' -f1)
	old=$(cat "$STATE/$i.sha" 2>/dev/null)
	if [ "$new" = "$old" ] && ip link show "$i" >/dev/null 2>&1; then
		continue
	fi
	if ip link show "$i" >/dev/null 2>&1; then
		wg-quick down "$i" >/dev/null 2>&1 || ip link del "$i" 2>/dev/null
	fi
	if wg-quick up "$i"; then
		echo "$new" > "$STATE/$i.sha"
	else
		echo "wg-quick up $i failed" >&2
		rc=1
	fi
done
exit $rc
`

// vpnArtifacts are the files of the tunnels that are switched on, and the script that applies them (written while there are any).
func vpnArtifacts(cfg config.NetworkConfig) []Artifact {
	v := cfg.VPNSettings()
	var out []Artifact
	if r := v.WGRemote; r != nil && !r.Disabled {
		out = append(out, Artifact{Path: wgConfPath(config.RemoteInterface()), Content: []byte(renderWGRemote(*r)), Mode: 0600, Service: SvcWireGuard})
	}
	for _, s := range v.Sites {
		if !s.Disabled && s.Engine != config.EngineOpenVPN {
			out = append(out, Artifact{Path: wgConfPath(config.SiteInterface(s.ID)), Content: []byte(renderWGSite(s)), Mode: 0600, Service: SvcWireGuard})
		}
	}
	if len(out) > 0 {
		out = append(out, Artifact{Path: PathWGSync, Content: []byte(wgSyncScript), Mode: 0755, Service: SvcWireGuard})
	}
	return out
}

// renderDnsmasqVPN lets the DNS server answer on the remote access interfaces, when the clients are told to use it.
func renderDnsmasqVPN(cfg config.NetworkConfig) (string, bool) {
	v := cfg.VPNSettings()
	var ifaces []string
	if r := v.WGRemote; r != nil && !r.Disabled && r.DNS {
		ifaces = append(ifaces, config.RemoteInterface())
	}
	if r := v.OVRemote; r != nil && !r.Disabled && r.DNS {
		ifaces = append(ifaces, config.OVRemoteInterface())
	}
	if len(ifaces) == 0 {
		return "", false
	}
	out := "# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n"
	for _, i := range ifaces {
		out += "interface=" + i + "\n"
	}
	return out, true
}

// vpnListenPorts are the ports the tunnels listen on, by protocol, in order (a tunnel that only calls the other router listens on none).
func vpnListenPorts(cfg config.NetworkConfig) (udp, tcp []int) {
	v := cfg.VPNSettings()
	if r := v.WGRemote; r != nil && !r.Disabled {
		udp = append(udp, r.ListenPort)
	}
	if r := v.OVRemote; r != nil && !r.Disabled {
		if r.Proto == "tcp" {
			tcp = append(tcp, r.ListenPort)
		} else {
			udp = append(udp, r.ListenPort)
		}
	}
	for _, s := range v.Sites {
		if s.Disabled {
			continue
		}
		switch {
		case s.Engine != config.EngineOpenVPN:
			udp = append(udp, s.ListenPort)
		case s.OV != nil && s.OV.Role == config.OVRoleServer && s.OV.Proto == "tcp":
			tcp = append(tcp, s.ListenPort)
		case s.OV != nil && s.OV.Role == config.OVRoleServer:
			udp = append(udp, s.ListenPort)
		}
	}
	sort.Ints(udp)
	sort.Ints(tcp)
	return udp, tcp
}

func portSet(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// nftVPNRules are the lines the VPN adds to the input and the forward chain, after what the Local Networks add:
//
//   - the tunnels' UDP ports are open on the internet connection;
//   - a tunnel may use this router's DNS and ping, nothing else of it (like a Local Network that is not the default one);
//   - a tunnel reaches the internet only if its switch is on;
//   - a tunnel and a Local Network (or another tunnel) may talk only if neither is isolated, in both directions (the Local
//     Networks' own pairs are written with the networks);
//   - TCP connections through a tunnel are asked to use segments that fit its MTU.
func nftVPNRules(cfg config.NetworkConfig, lan string) (input, forward string, members []policyMember) {
	zones := cfg.VPNZones()
	if len(zones) == 0 {
		return "", "", nil
	}
	var in, fw strings.Builder
	udp, tcp := vpnListenPorts(cfg)
	in.WriteString("        # VPN: the tunnels' ports\n")
	if len(udp) > 0 {
		fmt.Fprintf(&in, "        iifname \"%s\" udp dport %s accept\n", cfg.WanDevice(), portSet(udp))
	}
	if len(tcp) > 0 {
		fmt.Fprintf(&in, "        iifname \"%s\" tcp dport %s accept\n", cfg.WanDevice(), portSet(tcp))
	}
	var ifaces []string
	for _, z := range zones {
		ifaces = append(ifaces, z.Iface)
		fmt.Fprintf(&in, "        iifname \"%s\" udp dport 53 accept\n", z.Iface)
		fmt.Fprintf(&in, "        iifname \"%s\" tcp dport 53 accept\n", z.Iface)
		fmt.Fprintf(&in, "        iifname \"%s\" icmp type echo-request accept\n", z.Iface)
	}
	fmt.Fprintf(&fw, "        # VPN: segments that fit a tunnel\n")
	fmt.Fprintf(&fw, "        oifname { %[1]s } tcp flags syn tcp option maxseg size > %[2]d tcp option maxseg size set %[2]d\n", quoteList(ifaces), wgMSS)
	fmt.Fprintf(&fw, "        iifname { %[1]s } tcp flags syn tcp option maxseg size > %[2]d tcp option maxseg size set %[2]d\n", quoteList(ifaces), wgMSS)
	for _, z := range zones {
		if z.Internet {
			fmt.Fprintf(&fw, "        iifname \"%s\" oifname \"%s\" accept\n", z.Iface, cfg.WanDevice())
		}
		members = append(members, policyMember{z.Iface, !z.Isolated})
	}
	return in.String(), fw.String(), members
}

// policyMember is a Local Network or a tunnel in the pairs that may talk to each other.
type policyMember struct {
	iface string
	open  bool // not isolated
}

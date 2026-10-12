package translator

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"

	"peakroute/internal/config"
	"peakroute/internal/pki"
)

// How OpenVPN is written (docs/vpn.md, sections 9 to 11).
//
// Every OpenVPN tunnel is a systemd instance of the package's own units: openvpn-server@pr-ra (remote access), openvpn-server@pr-s<N>
// (a site tunnel where this router is the server) and openvpn-client@pr-s<N> (where it is the client), each with its file
// /etc/openvpn/{server,client}/<name>.conf. Everything the file needs (the authority's certificate, the certificate and its key, the
// control channel key, the revocation list) is inside it, so that one file is one tunnel and a change to anything restarts exactly
// that tunnel. A server has one more kind of file: /etc/openvpn/ccd/<common name>, one per device that may connect (its address) or
// per site tunnel (its address and the networks behind the other router). A device without a file is refused (ccd-exclusive), so
// removing a device cuts it off even before the revocation list reaches it.
//
// A script, written while there is any OpenVPN tunnel, brings the units in line with the files on every commit.

const (
	SvcOpenVPN = "openvpn"

	PathOVServerDir = "/etc/openvpn/server"
	PathOVClientDir = "/etc/openvpn/client"
	PathOVCCDDir    = "/etc/openvpn/ccd"
	PathOVSync      = "/usr/local/sbin/peakroute-ovpn-sync"

	ovPrefix  = "pr-"
	ovCiphers = "AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305"
)

func ovRemoteName() string            { return ovPrefix + "ra" }
func ovSiteName(id string) string     { return ovPrefix + id }
func ovServerPath(name string) string { return PathOVServerDir + "/" + name + ".conf" }
func ovClientPath(name string) string { return PathOVClientDir + "/" + name + ".conf" }

// ovAuthority is the authority's material as it is written: each block re-encoded, so that nothing but a block can get into a file.
type ovAuthority struct {
	CA, ServerCert, ServerKey, CRL string
}

func ovAuthorityOf(v config.VPNConfig) (ovAuthority, error) {
	ca := v.CA
	if ca == nil {
		return ovAuthority{}, fmt.Errorf("refusing to render OpenVPN without the certificate authority")
	}
	var a ovAuthority
	var err error
	if a.CA, err = pki.Normalize(ca.Cert, "CERTIFICATE"); err != nil {
		return a, fmt.Errorf("refusing to render the authority's certificate: %v", err)
	}
	if a.ServerCert, err = pki.Normalize(ca.ServerCert, "CERTIFICATE"); err != nil {
		return a, fmt.Errorf("refusing to render the server certificate: %v", err)
	}
	if a.ServerKey, err = pki.Normalize(ca.ServerKey, "EC PRIVATE KEY", "PRIVATE KEY"); err != nil {
		return a, fmt.Errorf("refusing to render the server key: %v", err)
	}
	if ca.CRL == "" {
		return a, fmt.Errorf("refusing to render OpenVPN without a revocation list")
	}
	if a.CRL, err = pki.Normalize(ca.CRL, "X509 CRL"); err != nil {
		return a, fmt.Errorf("refusing to render the revocation list: %v", err)
	}
	return a, nil
}

// inline writes a block of the file: <tag>, the text, </tag>.
func inline(b *strings.Builder, tag, text string) {
	fmt.Fprintf(b, "<%s>\n%s</%s>\n", tag, strings.TrimRight(text, "\n")+"\n", tag)
}

func netMask(p netip.Prefix) (string, string) {
	m := p.Masked()
	bits := p.Bits()
	var mask uint32
	if bits > 0 {
		mask = ^uint32(0) << (32 - bits)
	}
	return m.Addr().String(), fmt.Sprintf("%d.%d.%d.%d", byte(mask>>24), byte(mask>>16), byte(mask>>8), byte(mask))
}

func routeLines(prefix string, nets []string) string {
	var b strings.Builder
	for _, n := range nets {
		p, _ := netip.ParsePrefix(n)
		addr, mask := netMask(p)
		fmt.Fprintf(&b, "%s %s %s\n", prefix, addr, mask)
	}
	return b.String()
}

// ccdHash fingerprints the device files of a server, so that the server's own file changes when they do and the server is restarted
// (OpenVPN reads a device file when the device connects, so one that was removed would otherwise stay connected).
func ccdHash(files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%s\x00", n, files[n])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func ovServerCommon(b *strings.Builder, proto string, port int, iface, serverNet string, ccdSum string) {
	b.WriteString("mode server\ntls-server\n")
	fmt.Fprintf(b, "port %d\nproto %s\n", port, proto)
	fmt.Fprintf(b, "dev %s\ndev-type tun\ntopology subnet\n", iface)
	b.WriteString(serverNet)
	fmt.Fprintf(b, "client-config-dir %s\nccd-exclusive\n", PathOVCCDDir)
	b.WriteString("keepalive 10 60\npersist-key\npersist-tun\nuser nobody\ngroup nogroup\n")
	fmt.Fprintf(b, "data-ciphers %s\ntls-version-min 1.2\ndh none\nverify-client-cert require\nremote-cert-tls client\n", ovCiphers)
	if proto == "udp" {
		b.WriteString("explicit-exit-notify 1\n")
	}
	fmt.Fprintf(b, "# device files %s\n", ccdSum)
	b.WriteString("verb 3\n")
}

// renderOVRemote is the remote access server's file and its device files (by file name).
func renderOVRemote(cfg config.NetworkConfig, r config.OVRemote, auth ovAuthority) (conf string, ccd map[string]string, err error) {
	tc, err := pki.NormalizeTLSCrypt(r.TLSCrypt)
	if err != nil {
		return "", nil, fmt.Errorf("refusing to render the remote access control channel key: %v", err)
	}
	pool, _ := netip.ParsePrefix(r.Network)
	netAddr, mask := netMask(pool)
	server, _ := r.ServerAddress()
	ccd = map[string]string{}
	for _, cl := range r.Clients {
		if cl.Disabled {
			continue // no file: refused
		}
		p, _ := netip.ParsePrefix(cl.Address)
		ccd[config.OVClientCN(cl.ID)] = fmt.Sprintf("# Managed by PeakRoute OS: device %s\nifconfig-push %s %s\n", cl.ID, p.Addr(), mask)
	}
	var b strings.Builder
	b.WriteString("# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n# Remote access server\n")
	ovServerCommon(&b, r.Proto, r.ListenPort, config.OVRemoteInterface(), fmt.Sprintf("server %s %s\n", netAddr, mask), ccdHash(ccd))
	b.WriteString("verify-x509-name " + config.OVClientCN("") + " name-prefix\n")
	for _, n := range cfg.OVClientRoutes() {
		if n == "0.0.0.0/0" {
			continue
		}
		p, _ := netip.ParsePrefix(n)
		addr, m := netMask(p)
		fmt.Fprintf(&b, "push \"route %s %s\"\n", addr, m)
	}
	if r.FullTunnel {
		b.WriteString("push \"redirect-gateway def1\"\n")
	}
	if r.DNS {
		fmt.Fprintf(&b, "push \"dhcp-option DNS %s\"\n", server)
	}
	inline(&b, "ca", auth.CA)
	inline(&b, "cert", auth.ServerCert)
	inline(&b, "key", auth.ServerKey)
	inline(&b, "crl-verify", auth.CRL)
	inline(&b, "tls-crypt", tc)
	return b.String(), ccd, nil
}

// renderOVSiteServer is a site tunnel where this router is the server: its file and the file of the other router.
func renderOVSiteServer(s config.VPNSite, auth ovAuthority) (conf string, ccd map[string]string, err error) {
	o := s.OV
	tc, err := pki.NormalizeTLSCrypt(o.TLSCrypt)
	if err != nil {
		return "", nil, fmt.Errorf("refusing to render the control channel key of %s: %v", s.ID, err)
	}
	ap, _ := netip.ParsePrefix(s.Address)
	netAddr, mask := netMask(ap)
	peer := ap.Masked().Addr().Next().Next()
	cn := config.OVPeerCN(s.ID)
	var f strings.Builder
	fmt.Fprintf(&f, "# Managed by PeakRoute OS: tunnel %s\nifconfig-push %s %s\n", s.ID, peer, mask)
	f.WriteString(routeLines("iroute", s.RemoteNetworks))
	ccd = map[string]string{cn: f.String()}

	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n# Site tunnel %s: %s\n", s.ID, commentSafe(strings.TrimSpace(s.Name)))
	ovServerCommon(&b, o.Proto, s.ListenPort, s.Interface(), fmt.Sprintf("server %s %s\n", netAddr, mask), ccdHash(ccd))
	fmt.Fprintf(&b, "verify-x509-name %s name\n", cn)
	b.WriteString(routeLines("route", s.RemoteNetworks))
	inline(&b, "ca", auth.CA)
	inline(&b, "cert", auth.ServerCert)
	inline(&b, "key", auth.ServerKey)
	inline(&b, "crl-verify", auth.CRL)
	inline(&b, "tls-crypt", tc)
	return b.String(), ccd, nil
}

// renderOVSiteClient is a site tunnel where this router is the client, with what it imported from the other router.
func renderOVSiteClient(s config.VPNSite) (string, error) {
	o := s.OV
	ca, err := pki.Normalize(o.CACert, "CERTIFICATE")
	if err != nil {
		return "", fmt.Errorf("refusing to render the authority of %s: %v", s.ID, err)
	}
	cert, err := pki.Normalize(o.Cert, "CERTIFICATE")
	if err != nil {
		return "", fmt.Errorf("refusing to render the certificate of %s: %v", s.ID, err)
	}
	key, err := pki.Normalize(o.Key, "EC PRIVATE KEY", "PRIVATE KEY")
	if err != nil {
		return "", fmt.Errorf("refusing to render the key of %s: %v", s.ID, err)
	}
	tc, err := pki.NormalizeTLSCrypt(o.TLSCrypt)
	if err != nil {
		return "", fmt.Errorf("refusing to render the control channel key of %s: %v", s.ID, err)
	}
	host, port, _ := splitRemote(o.Remote)
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n# Site tunnel %s: %s\n", s.ID, commentSafe(strings.TrimSpace(s.Name)))
	fmt.Fprintf(&b, "client\ndev %s\ndev-type tun\nproto %s\nremote %s %d\nnobind\n", s.Interface(), o.Proto, host, port)
	b.WriteString("persist-key\npersist-tun\nuser nobody\ngroup nogroup\nresolv-retry infinite\n")
	// the other router tells this one its address; nothing else it says is taken (routes are the ones listed here)
	b.WriteString("route-nopull\n")
	fmt.Fprintf(&b, "data-ciphers %s\ntls-version-min 1.2\nremote-cert-tls server\nverb 3\n", ovCiphers)
	b.WriteString(routeLines("route", s.RemoteNetworks))
	inline(&b, "ca", ca)
	inline(&b, "cert", cert)
	inline(&b, "key", key)
	inline(&b, "tls-crypt", tc)
	return b.String(), nil
}

func splitRemote(s string) (host string, port int, ok bool) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, 0, false
	}
	p, err := strconv.Atoi(s[i+1:])
	return s[:i], p, err == nil
}

// ovSyncScript brings the OpenVPN units in line with the files. It only touches the units of the files it recognises (pr-ra, pr-s<N>),
// remembers what it started with a fingerprint of the file in /run, and restarts a unit only when its file changed or it is not
// running.
const ovSyncScript = `#!/bin/sh
# Brings the OpenVPN tunnels in line with /etc/openvpn (PeakRoute OS; rewritten on every commit that has OpenVPN tunnels).
PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
STATE=/run/peakroute-ovpn
mkdir -p "$STATE"
rc=0
mine() { case "$1" in pr-ra|pr-s[0-9]|pr-s[0-9][0-9]) return 0 ;; esac; return 1; }
# tunnels that are no longer wanted
for f in "$STATE"/*.sha; do
	[ -e "$f" ] || continue
	n=$(basename "$f" .sha)
	kind=${n%%:*}
	inst=${n#*:}
	mine "$inst" || continue
	if [ ! -f "/etc/openvpn/$kind/$inst.conf" ]; then
		systemctl stop "openvpn-$kind@$inst" >/dev/null 2>&1
		rm -f "$f"
	fi
done
# tunnels that are wanted
for kind in server client; do
	for conf in /etc/openvpn/$kind/pr-*.conf; do
		[ -e "$conf" ] || continue
		inst=$(basename "$conf" .conf)
		mine "$inst" || continue
		new=$(sha256sum "$conf" | cut -d' ' -f1)
		old=$(cat "$STATE/$kind:$inst.sha" 2>/dev/null)
		if [ "$new" = "$old" ] && systemctl is-active --quiet "openvpn-$kind@$inst"; then
			continue
		fi
		if systemctl restart "openvpn-$kind@$inst"; then
			echo "$new" > "$STATE/$kind:$inst.sha"
		else
			echo "openvpn-$kind@$inst failed to start" >&2
			rc=1
		fi
	done
done
exit $rc
`

// ovArtifacts are the files of the OpenVPN tunnels that are switched on, the device files, and the script that applies them
// (written while there are any).
func ovArtifacts(cfg config.NetworkConfig) ([]Artifact, error) {
	v := cfg.VPNSettings()
	var out []Artifact
	need := func() (ovAuthority, error) { return ovAuthorityOf(v) }
	add := func(path, content string, mode uint32) {
		out = append(out, Artifact{Path: path, Content: []byte(content), Mode: os.FileMode(mode), Service: SvcOpenVPN})
	}
	if r := v.OVRemote; r != nil && !r.Disabled {
		auth, err := need()
		if err != nil {
			return nil, err
		}
		conf, ccd, err := renderOVRemote(cfg, *r, auth)
		if err != nil {
			return nil, err
		}
		add(ovServerPath(ovRemoteName()), conf, 0600)
		for cn, c := range ccd {
			add(PathOVCCDDir+"/"+cn, c, 0644)
		}
	}
	for _, s := range v.Sites {
		if s.Disabled || s.Engine != config.EngineOpenVPN || s.OV == nil {
			continue
		}
		switch s.OV.Role {
		case config.OVRoleServer:
			auth, err := need()
			if err != nil {
				return nil, err
			}
			conf, ccd, err := renderOVSiteServer(s, auth)
			if err != nil {
				return nil, err
			}
			add(ovServerPath(ovSiteName(s.ID)), conf, 0600)
			for cn, c := range ccd {
				add(PathOVCCDDir+"/"+cn, c, 0644)
			}
		case config.OVRoleClient:
			conf, err := renderOVSiteClient(s)
			if err != nil {
				return nil, err
			}
			add(ovClientPath(ovSiteName(s.ID)), conf, 0600)
		}
	}
	if len(out) > 0 {
		add(PathOVSync, ovSyncScript, 0755)
	}
	return out, nil
}

// ensureSafeOVPN is the render-time twin of the OpenVPN validation: what is written to a file must be what the validation accepts.
func ensureSafeOVPN(cfg config.NetworkConfig) error {
	v := cfg.VPNSettings()
	prefix := func(what, p string) error {
		pp, err := netip.ParsePrefix(p)
		if err != nil || !pp.Addr().Is4() {
			return fmt.Errorf("refusing to render the unsafe %s %q", what, p)
		}
		return nil
	}
	remote := func(what, e string, needPort bool) error {
		if e == "" && !needPort {
			return nil
		}
		host, port, ok := splitRemote(e)
		if !ok {
			host, port = e, 0
		}
		if !config.ValidServerHost(host) || port < 0 || port > 65535 || (needPort && port < 1) {
			return fmt.Errorf("refusing to render the unsafe %s %q", what, e)
		}
		return nil
	}
	proto := func(p string) error {
		if p != "udp" && p != "tcp" {
			return fmt.Errorf("refusing to render the unsafe protocol %q", p)
		}
		return nil
	}
	if r := v.OVRemote; r != nil {
		if err := proto(r.Proto); err != nil {
			return err
		}
		if r.ListenPort < 1 || r.ListenPort > 65535 {
			return fmt.Errorf("refusing to render the unsafe OpenVPN remote access port %d", r.ListenPort)
		}
		for _, p := range append([]string{r.Network}, r.Routes...) {
			if err := prefix("network", p); err != nil {
				return err
			}
		}
		if err := remote("endpoint", r.Endpoint, false); err != nil {
			return err
		}
		for _, cl := range r.Clients {
			if err := ensureSafe(cl.ID); err != nil {
				return err
			}
			if err := prefix("device address", cl.Address); err != nil {
				return err
			}
		}
	}
	for _, s := range v.Sites {
		if s.Engine != config.EngineOpenVPN || s.OV == nil {
			continue
		}
		if err := ensureSafe(s.ID); err != nil {
			return err
		}
		if err := proto(s.OV.Proto); err != nil {
			return err
		}
		switch s.OV.Role {
		case config.OVRoleServer:
			if s.ListenPort < 1 || s.ListenPort > 65535 {
				return fmt.Errorf("refusing to render the unsafe tunnel port of %s", s.ID)
			}
			if err := prefix("tunnel address", s.Address); err != nil {
				return err
			}
		case config.OVRoleClient:
			if err := remote("remote", s.OV.Remote, true); err != nil {
				return err
			}
		default:
			return fmt.Errorf("refusing to render the unsafe role %q of %s", s.OV.Role, s.ID)
		}
		for _, n := range s.RemoteNetworks {
			if err := prefix("network", n); err != nil {
				return err
			}
		}
	}
	return nil
}

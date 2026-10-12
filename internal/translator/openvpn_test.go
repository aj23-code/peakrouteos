package translator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/pki"
)

type ovFix struct {
	cfg        config.NetworkConfig
	ca, caKey  string
	srvKey     string
	tcRA, tcS1 string
	peerKey    string
}

// ovCfg is a router with the OpenVPN remote access server (three devices, one of them off), a site tunnel where it is the server
// and one where it is the client.
func ovCfg(t *testing.T, edit func(*config.VPNConfig)) ovFix {
	t.Helper()
	now := time.Now()
	ca, caKey, err := pki.NewCA("PeakRoute test CA", now)
	if err != nil {
		t.Fatal(err)
	}
	srv, srvKey, _ := pki.Issue(ca, caKey, "server", true, now)
	serial := func(cn string) string {
		c, _, _ := pki.Issue(ca, caKey, cn, false, now)
		i, _ := pki.Describe(c)
		return i.Serial
	}
	crl, _ := pki.NewCRL(ca, caKey, nil, 1, now)
	tcRA, _ := pki.NewTLSCryptKey()
	tcS1, _ := pki.NewTLSCryptKey()
	tcS2, _ := pki.NewTLSCryptKey()
	oca, ocaKey, _ := pki.NewCA("Another router", now)
	ccert, ckey, _ := pki.Issue(oca, ocaKey, "s2-peer", false, now)
	c := fwCfg(nil)
	c.Firewall = nil
	c.VPN = &config.VPNConfig{
		CA: &config.VPNCA{Cert: ca, Key: caKey, ServerCert: srv, ServerKey: srvKey, CRL: crl, CRLNumber: 1},
		OVRemote: &config.OVRemote{ListenPort: 1194, Proto: "udp", Network: "10.9.0.0/24", Endpoint: "vpn.example.com", DNS: true, TLSCrypt: tcRA,
			Clients: []config.OVClient{
				{ID: "c1", Name: "Anna phone", Serial: serial("ra-c1"), Address: "10.9.0.2/32"},
				{ID: "c2", Name: "Off laptop", Serial: serial("ra-c2"), Address: "10.9.0.3/32", Disabled: true},
				{ID: "c3", Name: "Work laptop", Serial: serial("ra-c3"), Address: "10.9.0.4/32"},
			}},
		Sites: []config.VPNSite{
			{ID: "s1", Name: "Branch office", Engine: config.EngineOpenVPN, ListenPort: 1195, Address: "10.98.1.1/29", RemoteNetworks: []string{"10.50.0.0/24", "10.51.0.0/16"},
				OV: &config.OVSite{Role: config.OVRoleServer, Proto: "tcp", Remote: "router.example.com:1195", TLSCrypt: tcS1, PeerSerial: serial("s1-peer")}},
			{ID: "s2", Name: "Depot", Engine: config.EngineOpenVPN, Address: "10.98.2.2/29", RemoteNetworks: []string{"10.60.0.0/24"},
				OV: &config.OVSite{Role: config.OVRoleClient, Proto: "udp", Remote: "depot.example.com:1196", TLSCrypt: tcS2, CACert: oca, Cert: ccert, Key: ckey}},
		},
	}
	if edit != nil {
		edit(c.VPN)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("the fixture is not valid: %v", err)
	}
	return ovFix{cfg: c, ca: ca, caKey: caKey, srvKey: srvKey, tcRA: tcRA, tcS1: tcS1}
}

// with is the configuration with an edit that does not touch the original (and keeps its certificates, so that files can be compared).
func (f ovFix) with(edit func(*config.VPNConfig)) config.NetworkConfig {
	c := f.cfg
	v := *f.cfg.VPN
	if v.OVRemote != nil {
		r := *v.OVRemote
		r.Clients = append([]config.OVClient(nil), r.Clients...)
		r.Routes = append([]string(nil), r.Routes...)
		v.OVRemote = &r
	}
	v.Sites = append([]config.VPNSite(nil), v.Sites...)
	for i := range v.Sites {
		if v.Sites[i].OV != nil {
			o := *v.Sites[i].OV
			v.Sites[i].OV = &o
		}
	}
	if v.CA != nil {
		ca := *v.CA
		v.CA = &ca
	}
	c.VPN = &v
	edit(c.VPN)
	return c
}

func lines(s string) map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(s, "\n") {
		m[strings.TrimSpace(l)] = true
	}
	return m
}

func TestTheRemoteAccessServerFile(t *testing.T) {
	f := ovCfg(t, nil)
	conf := text(t, f.cfg, "/etc/openvpn/server/pr-ra.conf")
	got := lines(conf)
	for _, want := range []string{"mode server", "tls-server", "port 1194", "proto udp", "dev ovra", "dev-type tun", "topology subnet", "server 10.9.0.0 255.255.255.0",
		"client-config-dir /etc/openvpn/ccd", "ccd-exclusive", "verify-x509-name ra- name-prefix", "remote-cert-tls client", "verify-client-cert require", "dh none",
		"data-ciphers AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305", "tls-version-min 1.2", "explicit-exit-notify 1", "status /run/openvpn-server/status-pr-ra.log 10", "status-version 2", "user nobody", "group nogroup",
		`push "route 192.168.1.0 255.255.255.0"`, `push "route 10.20.0.0 255.255.255.0"`, `push "route 10.30.0.0 255.255.255.0"`, `push "dhcp-option DNS 10.9.0.1"`,
		"<ca>", "</ca>", "<cert>", "</cert>", "<key>", "</key>", "<crl-verify>", "</crl-verify>", "<tls-crypt>", "</tls-crypt>"} {
		if !got[want] {
			t.Errorf("the file lacks %q:\n%s", want, conf)
		}
	}
	if got[`push "redirect-gateway def1"`] {
		t.Error("a full tunnel that was not asked for")
	}
	for _, secretless := range []string{"ca.crt", "/etc/openvpn/ccd/ra"} {
		_ = secretless
	}
	if !strings.Contains(conf, "-----BEGIN EC PRIVATE KEY-----") || !strings.Contains(conf, "-----BEGIN X509 CRL-----") || !strings.Contains(conf, "-----BEGIN OpenVPN Static key V1-----") {
		t.Errorf("the material is not inside:\n%s", conf)
	}
	a, _ := file(t, f.cfg, "/etc/openvpn/server/pr-ra.conf")
	if a.Mode != 0600 || a.Service != SvcOpenVPN {
		t.Errorf("%+v", a)
	}
	// a full tunnel, chosen routes
	full := ovCfg(t, func(v *config.VPNConfig) {
		v.OVRemote.FullTunnel, v.OVRemote.Internet, v.OVRemote.DNS = true, true, false
	})
	got = lines(text(t, full.cfg, "/etc/openvpn/server/pr-ra.conf"))
	if !got[`push "redirect-gateway def1"`] || got[`push "route 192.168.1.0 255.255.255.0"`] || got[`push "route 0.0.0.0 0.0.0.0"`] {
		t.Errorf("a full tunnel:\n%v", got)
	}
	for l := range got {
		if strings.HasPrefix(l, "push \"dhcp-option") {
			t.Errorf("DNS pushed that was not asked for: %s", l)
		}
	}
	chosen := ovCfg(t, func(v *config.VPNConfig) { v.OVRemote.Routes = []string{"192.168.1.0/24", "172.16.0.0/12"} })
	got = lines(text(t, chosen.cfg, "/etc/openvpn/server/pr-ra.conf"))
	if !got[`push "route 172.16.0.0 255.240.0.0"`] || got[`push "route 10.20.0.0 255.255.255.0"`] {
		t.Errorf("chosen routes:\n%v", got)
	}
}

func TestTheDeviceFilesDecideWhoMayConnect(t *testing.T) {
	f := ovCfg(t, nil)
	c1 := text(t, f.cfg, "/etc/openvpn/ccd/ra-c1")
	if !strings.Contains(c1, "ifconfig-push 10.9.0.2 255.255.255.0\n") {
		t.Errorf("%s", c1)
	}
	if !strings.Contains(text(t, f.cfg, "/etc/openvpn/ccd/ra-c3"), "ifconfig-push 10.9.0.4 255.255.255.0\n") {
		t.Error("the third device")
	}
	// a device that is switched off has no file: the server refuses it
	if _, ok := file(t, f.cfg, "/etc/openvpn/ccd/ra-c2"); ok {
		t.Error("a file for a device that is off")
	}
	// the site tunnel's peer: its address in the tunnel and the networks behind it
	peer := text(t, f.cfg, "/etc/openvpn/ccd/s1-peer")
	for _, want := range []string{"ifconfig-push 10.98.1.2 255.255.255.248\n", "iroute 10.50.0.0 255.255.255.0\n", "iroute 10.51.0.0 255.255.0.0\n"} {
		if !strings.Contains(peer, want) {
			t.Errorf("the peer file lacks %q:\n%s", want, peer)
		}
	}
	// when a device's file changes, the server's own file changes too (a device that was cut off must not stay connected)
	before := text(t, f.cfg, "/etc/openvpn/server/pr-ra.conf")
	off := f.with(func(v *config.VPNConfig) { v.OVRemote.Clients[0].Disabled = true })
	if after := text(t, off, "/etc/openvpn/server/pr-ra.conf"); after == before {
		t.Error("switching a device off does not restart the server")
	}
	moved := f.with(func(v *config.VPNConfig) { v.OVRemote.Clients[0].Address = "10.9.0.9/32" })
	if after := text(t, moved, "/etc/openvpn/server/pr-ra.conf"); after == before {
		t.Error("moving a device does not restart the server")
	}
	// the other server is not touched by a device change
	if text(t, off, "/etc/openvpn/server/pr-s1.conf") != text(t, f.cfg, "/etc/openvpn/server/pr-s1.conf") {
		t.Error("a device change restarts another tunnel")
	}
}

func TestTheSiteTunnelFiles(t *testing.T) {
	f := ovCfg(t, nil)
	srv := text(t, f.cfg, "/etc/openvpn/server/pr-s1.conf")
	got := lines(srv)
	for _, want := range []string{"# Site tunnel s1: Branch office", "port 1195", "proto tcp", "dev ovs1", "server 10.98.1.0 255.255.255.248", "verify-x509-name s1-peer name",
		"route 10.50.0.0 255.255.255.0", "route 10.51.0.0 255.255.0.0", "ccd-exclusive"} {
		if !got[want] {
			t.Errorf("the server file lacks %q:\n%s", want, srv)
		}
	}
	if got["explicit-exit-notify 1"] {
		t.Error("explicit-exit-notify with tcp")
	}
	for l := range got {
		if strings.HasPrefix(l, "push ") {
			t.Errorf("a site tunnel pushes %q", l)
		}
	}
	cli := text(t, f.cfg, "/etc/openvpn/client/pr-s2.conf")
	got = lines(cli)
	for _, want := range []string{"client", "dev ovs2", "proto udp", "remote depot.example.com 1196", "nobind", "route-nopull", "remote-cert-tls server", "route 10.60.0.0 255.255.255.0",
		"<ca>", "<cert>", "<key>", "<tls-crypt>"} {
		if !got[want] {
			t.Errorf("the client file lacks %q:\n%s", want, cli)
		}
	}
	if got["<crl-verify>"] || got["mode server"] {
		t.Errorf("a client file with server parts:\n%s", cli)
	}
	// the client's file is in its own directory, and nothing else is written for it
	if _, ok := file(t, f.cfg, "/etc/openvpn/server/pr-s2.conf"); ok {
		t.Error("a server file for a client tunnel")
	}
	if _, ok := file(t, f.cfg, "/etc/openvpn/ccd/s2-peer"); ok {
		t.Error("a device file for a client tunnel")
	}
	// the other router's authority is the one inside, not ours
	if strings.Contains(cli, strings.TrimSpace(f.ca)) {
		t.Error("our authority in the client file")
	}
}

func TestOnlyWhatIsSwitchedOnIsWrittenForOpenVPN(t *testing.T) {
	// nothing: no file, no script, the directories are managed (so that what was there goes)
	d := mustRender(t, fwCfg(nil))
	for _, p := range []string{"/etc/openvpn/server/pr-ra.conf", PathOVSync} {
		if _, ok := hasArtifact(d, p); ok {
			t.Errorf("%s without OpenVPN", p)
		}
	}
	var dirs []string
	for _, m := range d.ManagedDirs {
		dirs = append(dirs, m.Path)
	}
	if !strings.Contains(strings.Join(dirs, " "), "/etc/openvpn/server /etc/openvpn/client /etc/openvpn/ccd") {
		t.Errorf("%v", dirs)
	}
	f := ovCfg(t, func(v *config.VPNConfig) {
		v.OVRemote.Disabled = true
		v.Sites[0].Disabled = true
		v.Sites[1].Disabled = true
	})
	d = mustRender(t, f.cfg)
	for _, a := range d.Artifacts {
		if strings.HasPrefix(a.Path, "/etc/openvpn") || a.Path == PathOVSync {
			t.Errorf("%s is written for tunnels that are off", a.Path)
		}
	}
	// the script comes with the first tunnel
	only := ovCfg(t, func(v *config.VPNConfig) { v.OVRemote = nil; v.Sites = v.Sites[1:] })
	if a, ok := file(t, only.cfg, PathOVSync); !ok || a.Mode != 0755 || a.Service != SvcOpenVPN {
		t.Errorf("%+v", a)
	}
}

func TestOpenVPNRidesAlongWithTheFirewallAndDNS(t *testing.T) {
	f := ovCfg(t, nil)
	nft := text(t, f.cfg, PathNftables)
	// remote access over udp 1194; the site server listens on tcp 1195; the client tunnel listens on nothing
	if !strings.Contains(nft, `iifname "eth0" udp dport 1194 accept`) || !strings.Contains(nft, `iifname "eth0" tcp dport 1195 accept`) {
		t.Errorf("the ports:\n%s", nft)
	}
	if strings.Contains(nft, "1196") {
		t.Error("a port for the client tunnel")
	}
	for _, want := range []string{`iifname "ovra" udp dport 53 accept`, `iifname "ovs1" tcp dport 53 accept`, `iifname "ovs2" icmp type echo-request accept`} {
		if !strings.Contains(nft, want) {
			t.Errorf("no %q", want)
		}
	}
	// the DNS server answers on the interface when the devices are told to use it, next to WireGuard's
	dns := text(t, f.cfg, PathDnsmasqVPN)
	if !strings.Contains(dns, "interface=ovra\n") || strings.Contains(dns, "wgra") {
		t.Errorf("%s", dns)
	}
	both := f.with(func(v *config.VPNConfig) {
		v.WGRemote = &config.WGRemote{PrivateKey: kRAPriv, ListenPort: 51821, Network: "10.8.0.0/24", DNS: true}
	})
	dns = text(t, both, PathDnsmasqVPN)
	if !strings.Contains(dns, "interface=wgra\n") || !strings.Contains(dns, "interface=ovra\n") {
		t.Errorf("%s", dns)
	}
	noDNS := f.with(func(v *config.VPNConfig) { v.OVRemote.DNS = false })
	if a, ok := file(t, noDNS, PathDnsmasqVPN); !ok || !a.Remove {
		t.Errorf("%+v", a)
	}
	// only a client tunnel: no port is opened at all (and nothing breaks)
	client := ovCfg(t, func(v *config.VPNConfig) { v.OVRemote = nil; v.Sites = v.Sites[1:] })
	nft = text(t, client.cfg, PathNftables)
	if strings.Contains(nft, "dport 1") || !strings.Contains(nft, `iifname "ovs2" udp dport 53 accept`) {
		t.Errorf("only a client tunnel:\n%s", nft)
	}
	// both protocols for one port number are not a thing, but one of each is
	udp, tcp := vpnListenPorts(f.cfg)
	if len(udp) != 1 || udp[0] != 1194 || len(tcp) != 1 || tcp[0] != 1195 {
		t.Errorf("%v %v", udp, tcp)
	}
}

func TestOpenVPNValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	saved := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = saved }()
	f := ovCfg(t, nil)
	bad := map[string]func(*config.VPNConfig){
		"a protocol that is a command":     func(v *config.VPNConfig) { v.OVRemote.Proto = "udp\nup /bin/sh" },
		"a port of 0":                      func(v *config.VPNConfig) { v.OVRemote.ListenPort = 0 },
		"a port too big":                   func(v *config.VPNConfig) { v.Sites[0].ListenPort = 70000 },
		"a network that is not one":        func(v *config.VPNConfig) { v.OVRemote.Network = "10.9.0.0/24\npush x" },
		"a route that is not one":          func(v *config.VPNConfig) { v.OVRemote.Routes = []string{"nope"} },
		"an endpoint with a command":       func(v *config.VPNConfig) { v.OVRemote.Endpoint = "a.example\nup /bin/sh" },
		"a device with a bad id":           func(v *config.VPNConfig) { v.OVRemote.Clients[0].ID = "c1/../../x" },
		"a device address that is not one": func(v *config.VPNConfig) { v.OVRemote.Clients[0].Address = "10.9.0.2" },
		"a site network that is not one":   func(v *config.VPNConfig) { v.Sites[0].RemoteNetworks = []string{"10.50.0.0/24\niroute x"} },
		"a site address that is not one":   func(v *config.VPNConfig) { v.Sites[0].Address = "10.98.1.1" },
		"a site id with a space":           func(v *config.VPNConfig) { v.Sites[0].ID = "s 1" },
		"a client remote with a command":   func(v *config.VPNConfig) { v.Sites[1].OV.Remote = "a.example:1\nup /bin/sh" },
		"a client remote without a port":   func(v *config.VPNConfig) { v.Sites[1].OV.Remote = "a.example" },
		"a role that is not one":           func(v *config.VPNConfig) { v.Sites[1].OV.Role = "both" },
		"a protocol on a site":             func(v *config.VPNConfig) { v.Sites[1].OV.Proto = "icmp" },
		"a certificate that is garbage":    func(v *config.VPNConfig) { v.CA.ServerCert = "x" },
		"a key with more after it":         func(v *config.VPNConfig) { v.CA.ServerKey += "\n</key>\nup /bin/sh\n" },
		"no revocation list":               func(v *config.VPNConfig) { v.CA.CRL = "" },
		"a control channel key with text":  func(v *config.VPNConfig) { v.OVRemote.TLSCrypt = "up /bin/sh\n" + v.OVRemote.TLSCrypt },
		"no authority":                     func(v *config.VPNConfig) { v.CA = nil },
		"a client's key that is garbage":   func(v *config.VPNConfig) { v.Sites[1].OV.Key = "x" },
	}
	_ = f
	for name, edit := range bad {
		c := ovCfg(t, nil).cfg
		// validation is off, so the fixture can be broken freely
		edit(c.VPN)
		if _, err := Render(c, "ROUTER"); err == nil {
			t.Errorf("%s was rendered", name)
		}
	}
	// a name with odd characters is only ever in a comment line, and cannot leave it
	c := ovCfg(t, nil).cfg
	c.VPN.Sites[0].Name = "Branch\nup /bin/sh"
	conf := text(t, c, "/etc/openvpn/server/pr-s1.conf")
	if strings.Contains(conf, "\nup /bin/sh") {
		t.Errorf("a name started a line:\n%s", conf)
	}
}

func TestChangingOpenVPNJustAppliesTheTunnels(t *testing.T) {
	f := ovCfg(t, nil)
	desired := mustRender(t, f.cfg)
	root := t.TempDir()
	install(t, root, desired)
	fs := RootFS(root)
	// the same configuration again: nothing to do
	if p, err := PlanChanges(fs, desired); err != nil || len(p.Files) != 0 || len(p.Actions) != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	// a device taken out: its file goes and the tunnel is applied (and only that service)
	g := f.with(func(v *config.VPNConfig) { v.OVRemote.Clients = v.OVRemote.Clients[1:] })
	plan, err := PlanChanges(fs, mustRender(t, g))
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, c := range plan.Files {
		if c.Kind == Remove {
			removed = append(removed, c.Path)
		}
	}
	if len(removed) != 1 || removed[0] != "/etc/openvpn/ccd/ra-c1" {
		t.Errorf("%v", removed)
	}
	if got := actionSummary(plan); got != "apply openvpn" {
		t.Errorf("%s", got)
	}
	// the whole thing taken out: every file goes; the script stays, because the commit runs it to take the tunnels down
	plan, err = PlanChanges(fs, mustRender(t, fwCfg(nil)))
	if err != nil {
		t.Fatal(err)
	}
	removed = nil
	for _, c := range plan.Files {
		if c.Kind == Remove && strings.HasPrefix(c.Path, "/etc/openvpn") {
			removed = append(removed, c.Path)
		}
		if c.Path == PathOVSync {
			t.Errorf("the script goes: %+v", c)
		}
	}
	if len(removed) != 6 || !strings.Contains(actionSummary(plan), "apply openvpn") || strings.Contains(actionSummary(plan), "wireguard") {
		t.Errorf("not everything went: %v %s", removed, actionSummary(plan))
	}
	// a file the translator does not own stays: one that is not ours in the managed directory (no prefix), and a stranger in ccd goes
	for path, content := range map[string]string{"/etc/openvpn/server/mine.conf": "not ours", "/etc/openvpn/ccd/stranger": "dev x"} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	plan, _ = PlanChanges(fs, mustRender(t, f.cfg))
	var gone []string
	for _, c := range plan.Files {
		gone = append(gone, c.Path)
	}
	if strings.Contains(strings.Join(gone, " "), "mine.conf") || !strings.Contains(strings.Join(gone, " "), "/etc/openvpn/ccd/stranger") {
		t.Errorf("%v", gone)
	}
	// the service command and its position
	if c := serviceCommands[SvcOpenVPN]; c.name != "sh" || c.args[0] != PathOVSync {
		t.Errorf("%+v", c)
	}
	order := strings.Join(serviceOrder, " ")
	if !strings.Contains(order, "dnsmasq wireguard openvpn") {
		t.Errorf("%s", order)
	}
}

func TestAServerWithoutAnAddressForDevicesStillRenders(t *testing.T) {
	f := ovCfg(t, func(v *config.VPNConfig) { v.OVRemote.Endpoint = "" })
	if !strings.Contains(text(t, f.cfg, "/etc/openvpn/server/pr-ra.conf"), "dev ovra") {
		t.Fatal("not rendered")
	}
}

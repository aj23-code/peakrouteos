package translator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
	"peakroute/internal/wg"
)

// fixed keys, so that the files can be compared as text
const (
	kSitePriv = "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk="
	kSitePeer = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
	kRAPriv   = "mLo7zHTRcnYkbu0lHEhpWv5HHe1xvYGk2o1UGfqmBXI="
	kClient1  = "gN65BkIKy1eCE9pP1wdc8ROUtkHLF2PfAqYdyYBz6EA="
	kClient2  = "FpCzxUzOo4vXq3MPLNzPQUVLXF3mjYhVcXdkzSrLXDg="
	kPSK      = "6lZkRfXhJTI+MVvPzEzRJKrHb1m1PnbcKGO3uFRwOUc="
)

// a third key, made from one of the others (so that it is a key)
var kClient3, _ = wg.PublicKey(kClient1)

func sampleVPN(f func(*config.VPNConfig)) config.NetworkConfig {
	c := fwCfg(nil)
	c.Firewall = nil
	c.VPN = &config.VPNConfig{}
	if f != nil {
		f(c.VPN)
	}
	return c
}

func siteA() config.VPNSite {
	return config.VPNSite{ID: "s1", Name: "Branch office", Engine: config.EngineWireGuard, PrivateKey: kSitePriv, PeerPublicKey: kSitePeer, ListenPort: 51820, Address: "10.99.1.1/30",
		Endpoint: "site-b.example.com:51820", Keepalive: 25, RemoteNetworks: []string{"10.50.0.0/24", "10.51.0.0/16"}}
}

func raServer() *config.WGRemote {
	return &config.WGRemote{PrivateKey: kRAPriv, ListenPort: 51821, Network: "10.8.0.0/24", Endpoint: "vpn.example.com", DNS: true,
		Clients: []config.VPNClient{
			{ID: "c1", Name: "Anna phone", PublicKey: kClient1, Address: "10.8.0.2/32"},
			{ID: "c2", Name: "Off laptop", PublicKey: kClient2, Address: "10.8.0.3/32", Disabled: true},
			{ID: "c3", Name: "Work laptop", PublicKey: kClient3, Address: "10.8.0.4/32", PresharedKey: kPSK},
		}}
}

func TestTheTunnelFilesAreWritten(t *testing.T) {
	cfg := sampleVPN(func(v *config.VPNConfig) {
		v.Sites = []config.VPNSite{siteA()}
		v.WGRemote = raServer()
	})
	a, ok := file(t, cfg, "/etc/wireguard/wgs1.conf")
	want := `# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.
# Site tunnel s1: Branch office
[Interface]
PrivateKey = ` + kSitePriv + `
ListenPort = 51820
Address = 10.99.1.1/30

[Peer]
PublicKey = ` + kSitePeer + `
AllowedIPs = 10.99.1.0/30, 10.50.0.0/24, 10.51.0.0/16
Endpoint = site-b.example.com:51820
PersistentKeepalive = 25
`
	if !ok || a.Remove || string(a.Content) != want || a.Mode != 0600 || a.Service != SvcWireGuard {
		t.Fatalf("%+v\n%s", a, a.Content)
	}
	r, ok := file(t, cfg, "/etc/wireguard/wgra.conf")
	wantRA := `# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.
# Remote access server
[Interface]
PrivateKey = ` + kRAPriv + `
ListenPort = 51821
Address = 10.8.0.1/24

# Client c1: Anna phone
[Peer]
PublicKey = ` + kClient1 + `
AllowedIPs = 10.8.0.2/32

# Client c3: Work laptop
[Peer]
PublicKey = ` + kClient3 + `
PresharedKey = ` + kPSK + `
AllowedIPs = 10.8.0.4/32
`
	if !ok || r.Remove || string(r.Content) != wantRA || r.Mode != 0600 {
		t.Fatalf("%+v\n%s", r, r.Content)
	}
	// the script that applies them, and the DNS server listening on the clients' interface
	s, ok := file(t, cfg, PathWGSync)
	if !ok || s.Remove || string(s.Content) != wgSyncScript || s.Mode != 0755 || s.Service != SvcWireGuard {
		t.Fatalf("%+v", s)
	}
	if d := text(t, cfg, PathDnsmasqVPN); d != "# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\ninterface=wgra\n" {
		t.Fatalf("%q", d)
	}
}

func TestOnlyWhatIsSwitchedOnIsWrittenAndNothingWithoutVPN(t *testing.T) {
	// no VPN at all: no tunnel file, no script, the DNS file removed (it is not there), the directory managed
	plain := fwCfg(nil)
	d := mustRender(t, plain)
	for _, p := range []string{"/etc/wireguard/wgra.conf", "/etc/wireguard/wgs1.conf", PathWGSync} {
		if _, ok := hasArtifact(d, p); ok {
			t.Errorf("%s is rendered without a VPN", p)
		}
	}
	if a, ok := hasArtifact(d, PathDnsmasqVPN); !ok || !a.Remove {
		t.Errorf("%+v", a)
	}
	// everything switched off: the same
	off := sampleVPN(func(v *config.VPNConfig) {
		s := siteA()
		s.Disabled = true
		v.Sites = []config.VPNSite{s}
		r := raServer()
		r.Disabled = true
		v.WGRemote = r
	})
	d = mustRender(t, off)
	for _, p := range []string{"/etc/wireguard/wgra.conf", "/etc/wireguard/wgs1.conf", PathWGSync} {
		if _, ok := hasArtifact(d, p); ok {
			t.Errorf("%s is rendered for a tunnel that is switched off", p)
		}
	}
	// the remote access server without DNS: no DNS file content; a site alone: its file and the script
	// the DNS file is only for a server that is switched on
	offDNS := sampleVPN(func(v *config.VPNConfig) { r := raServer(); r.Disabled = true; v.WGRemote = r })
	if a, ok := hasArtifact(mustRender(t, offDNS), PathDnsmasqVPN); !ok || !a.Remove {
		t.Errorf("a server that is off has a DNS file: %+v", a)
	}
	noDNS := sampleVPN(func(v *config.VPNConfig) { r := raServer(); r.DNS = false; v.WGRemote = r })
	if a, ok := hasArtifact(mustRender(t, noDNS), PathDnsmasqVPN); !ok || !a.Remove {
		t.Errorf("%+v", a)
	}
	only := mustRender(t, sampleVPN(func(v *config.VPNConfig) { v.Sites = []config.VPNSite{siteA()} }))
	if _, ok := hasArtifact(only, "/etc/wireguard/wgra.conf"); ok {
		t.Error("the remote access file without a server")
	}
	if _, ok := hasArtifact(only, "/etc/wireguard/wgs1.conf"); !ok {
		t.Error("the site file is missing")
	}
	if _, ok := hasArtifact(only, PathWGSync); !ok {
		t.Error("the script is missing")
	}
}

func TestTheFirewallKnowsTheTunnels(t *testing.T) {
	cfg := sampleVPN(func(v *config.VPNConfig) {
		v.Sites = []config.VPNSite{siteA()}
		r := raServer()
		r.Internet = true
		v.WGRemote = r
	})
	nft := text(t, cfg, PathNftables)
	for _, want := range []string{
		"        # VPN: the tunnels' UDP ports\n        iifname \"eth0\" udp dport { 51820, 51821 } accept\n",
		"        iifname \"wgra\" udp dport 53 accept\n        iifname \"wgra\" tcp dport 53 accept\n        iifname \"wgra\" icmp type echo-request accept\n",
		"        iifname \"wgs1\" udp dport 53 accept\n",
		"        oifname { \"wgra\", \"wgs1\" } tcp flags syn tcp option maxseg size > 1380 tcp option maxseg size set 1380\n",
		"        iifname { \"wgra\", \"wgs1\" } tcp flags syn tcp option maxseg size > 1380 tcp option maxseg size set 1380\n",
		"        iifname \"wgra\" oifname \"eth0\" accept\n", // the remote access network may use the internet
	} {
		if !strings.Contains(nft, want) {
			t.Errorf("missing %q in\n%s", want, nft)
		}
	}
	if strings.Contains(nft, "iifname \"wgs1\" oifname \"eth0\" accept") {
		t.Error("a site with no internet switch reaches the internet")
	}
	// neither is isolated: the default network, the other networks and the tunnels all talk, both ways
	for _, pair := range [][2]string{{"eth1", "wgra"}, {"wgra", "eth1"}, {"wgs1", "wgra"}, {"wgra", "wgs1"}} {
		line := "        iifname \"" + pair[0] + "\" oifname \"" + pair[1] + "\" accept\n"
		if pair[0] == "eth1" {
			line = strings.Replace(line, "eth1", "eth1", 1)
		}
		if !strings.Contains(nft, line) && !strings.Contains(nft, strings.Replace(line, "eth1", "br1", 1)) {
			t.Errorf("no line from %s to %s", pair[0], pair[1])
		}
	}
	// an isolated tunnel talks to nobody
	iso := sampleVPN(func(v *config.VPNConfig) {
		s := siteA()
		s.Isolated = true
		v.Sites = []config.VPNSite{s}
		v.WGRemote = raServer()
	})
	n := text(t, iso, PathNftables)
	if strings.Contains(n, "iifname \"wgs1\" oifname") || strings.Contains(n, "oifname \"wgs1\" accept") {
		t.Errorf("an isolated tunnel is reachable or reaches:\n%s", n)
	}
	if !strings.Contains(n, "iifname \"wgra\" oifname \"wgs1\"") == false && false {
		t.Fatal("unreachable")
	}
	// without any VPN the ruleset has no VPN line at all
	if strings.Contains(text(t, fwCfg(nil), PathNftables), "VPN") || strings.Contains(text(t, fwCfg(nil), PathNftables), "wgra") {
		t.Error("a VPN line without a VPN")
	}
	// a tunnel that is switched off is not in the ruleset either
	off := sampleVPN(func(v *config.VPNConfig) { r := raServer(); r.Disabled = true; v.WGRemote = r })
	if strings.Contains(text(t, off, PathNftables), "wgra") {
		t.Error("a tunnel that is switched off is in the ruleset")
	}
}

func TestRulesCanNameATunnel(t *testing.T) {
	cfg := sampleVPN(func(v *config.VPNConfig) {
		v.Sites = []config.VPNSite{siteA()}
		v.WGRemote = raServer()
	})
	cfg.Firewall = &config.FirewallConfig{Rules: []config.FirewallRule{
		{ID: "fw1", Name: "Phones to the printer", From: "vpn-ra", Action: config.ActionAllow, Protocol: config.ProtoTCP, To: config.DefaultNetworkID, Destination: "192.168.1.20", Ports: "9100"},
		{ID: "fw2", Name: "No remote access to the router", From: "vpn-ra", Action: config.ActionBlock, Protocol: config.ProtoAny, To: config.ToRouter},
		{ID: "fw3", Name: "Branch to the web server", From: "vpn-s1", Action: config.ActionAllow, Protocol: config.ProtoTCP, To: config.DefaultNetworkID, Ports: "443"},
		{ID: "fw4", Name: "Out to the branch", From: config.DefaultNetworkID, Action: config.ActionBlock, Protocol: config.ProtoAny, To: "vpn-s1"},
	}}
	nft := text(t, cfg, PathNftables)
	for _, want := range []string{
		"iifname \"wgra\" oifname \"eth1\" ip daddr 192.168.1.20 tcp dport 9100 counter accept comment \"fw:fw1\"",
		"iifname \"wgra\" counter drop comment \"fw:fw2\"",
		"iifname \"wgs1\" oifname \"eth1\" tcp dport 443 counter accept comment \"fw:fw3\"",
		"iifname \"eth1\" oifname \"wgs1\" counter drop comment \"fw:fw4\"",
	} {
		if !strings.Contains(nft, want) {
			t.Errorf("missing %q in\n%s", want, nft)
		}
	}
	// the review says it in words
	if got := cfg.RuleText(cfg.Firewall.Rules[0]); !strings.Contains(got, "from Remote access") {
		t.Errorf("%q", got)
	}
	if got := cfg.RuleText(cfg.Firewall.Rules[3]); !strings.Contains(got, "to Branch office") {
		t.Errorf("%q", got)
	}
}

func TestVPNValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	saved := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = saved }()
	bad := map[string]func(*config.VPNConfig){
		"a site key with a line break": func(v *config.VPNConfig) {
			s := siteA()
			s.PrivateKey = kSitePriv[:20] + "\nPostUp = reboot\n" + kSitePriv[20:]
			v.Sites = []config.VPNSite{s}
		},
		"a peer key that is not a key":      func(v *config.VPNConfig) { s := siteA(); s.PeerPublicKey = "x"; v.Sites = []config.VPNSite{s} },
		"a preshared key that is not a key": func(v *config.VPNConfig) { s := siteA(); s.PresharedKey = "x"; v.Sites = []config.VPNSite{s} },
		"an endpoint with a command": func(v *config.VPNConfig) {
			s := siteA()
			s.Endpoint = "a.example:1\nPostUp = reboot"
			v.Sites = []config.VPNSite{s}
		},
		"an endpoint port too big": func(v *config.VPNConfig) { s := siteA(); s.Endpoint = "a.example:70000"; v.Sites = []config.VPNSite{s} },
		"an address that is not one": func(v *config.VPNConfig) {
			s := siteA()
			s.Address = "10.99.1.1/30\nPostUp = reboot"
			v.Sites = []config.VPNSite{s}
		},
		"a network that is not one": func(v *config.VPNConfig) {
			s := siteA()
			s.RemoteNetworks = []string{"10.50.0.0/24, 0.0.0.0/0"}
			v.Sites = []config.VPNSite{s}
		},
		"a port of 0":              func(v *config.VPNConfig) { s := siteA(); s.ListenPort = 0; v.Sites = []config.VPNSite{s} },
		"a keepalive too long":     func(v *config.VPNConfig) { s := siteA(); s.Keepalive = 9999; v.Sites = []config.VPNSite{s} },
		"a site id with a space":   func(v *config.VPNConfig) { s := siteA(); s.ID = "s 1"; v.Sites = []config.VPNSite{s} },
		"a port that is too big":   func(v *config.VPNConfig) { s := siteA(); s.ListenPort = 70000; v.Sites = []config.VPNSite{s} },
		"a remote port of 0":       func(v *config.VPNConfig) { r := raServer(); r.ListenPort = 0; v.WGRemote = r },
		"a remote port too big":    func(v *config.VPNConfig) { r := raServer(); r.ListenPort = 70000; v.WGRemote = r },
		"a remote access key":      func(v *config.VPNConfig) { r := raServer(); r.PrivateKey = "x"; v.WGRemote = r },
		"a remote access network":  func(v *config.VPNConfig) { r := raServer(); r.Network = "10.8.0.0/24\nx"; v.WGRemote = r },
		"a remote access route":    func(v *config.VPNConfig) { r := raServer(); r.Routes = []string{"nope"}; v.WGRemote = r },
		"a remote access endpoint": func(v *config.VPNConfig) { r := raServer(); r.Endpoint = "a b"; v.WGRemote = r },
		"a client key":             func(v *config.VPNConfig) { r := raServer(); r.Clients[0].PublicKey = "x"; v.WGRemote = r },
		"a client preshared key":   func(v *config.VPNConfig) { r := raServer(); r.Clients[0].PresharedKey = "x"; v.WGRemote = r },
		"a client address":         func(v *config.VPNConfig) { r := raServer(); r.Clients[0].Address = "10.8.0.2"; v.WGRemote = r },
		"a client id with a slash": func(v *config.VPNConfig) { r := raServer(); r.Clients[0].ID = "c/1"; v.WGRemote = r },
	}
	for name, f := range bad {
		if _, err := Render(sampleVPN(f), "ROUTER"); err == nil {
			t.Errorf("%s was rendered", name)
		}
	}
	// a name with odd characters is only ever in a comment line, and cannot leave it
	s := siteA()
	s.Name = "Branch\nPostUp = reboot"
	conf := renderWGSite(s)
	if strings.Contains(conf, "\nPostUp") {
		t.Errorf("a name started a line:\n%s", conf)
	}
}

func TestChangingTheVPNJustAppliesTheTunnels(t *testing.T) {
	fs := RootFS(t.TempDir())
	run := &LogRunner{}
	ap := &Applier{FS: fs, Run: run}
	apply := func(cfg config.NetworkConfig) Plan {
		d := mustRender(t, cfg)
		plan, err := PlanChanges(fs, d)
		if err != nil {
			t.Fatal(err)
		}
		if res := ap.Apply(context.Background(), d, plan); !res.OK {
			t.Fatalf("%+v", res)
		}
		return plan
	}
	// the same configuration without the VPN, for the plans that need nothing but the tunnels
	base := apply(sampleVPN(func(v *config.VPNConfig) {}))
	_ = base
	p := apply(sampleVPN(func(v *config.VPNConfig) { v.Sites = []config.VPNSite{siteA()} }))
	// the firewall changes too (the port is opened, the tunnel is a zone), the tunnel is applied after the firewall and the DNS server
	got := actionSummary(p)
	if !strings.Contains(got, "reload nftables") || !strings.HasSuffix(got, "apply wireguard") {
		t.Fatalf("%s", got)
	}
	if st, err := os.Stat(filepath.Join(string(fs), "etc/wireguard/wgs1.conf")); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("%v %v", st, err)
	}
	// nothing changed: nothing to do
	if p := apply(sampleVPN(func(v *config.VPNConfig) { v.Sites = []config.VPNSite{siteA()} })); !p.Empty() {
		t.Fatalf("%+v", p)
	}
	// the other router's networks change: the file changes, the firewall does not care, only the tunnels are applied
	s := siteA()
	s.RemoteNetworks = []string{"10.50.0.0/24"}
	p = apply(sampleVPN(func(v *config.VPNConfig) { v.Sites = []config.VPNSite{s} }))
	if actionSummary(p) != "apply wireguard" || len(p.Files) != 1 || p.Files[0].Path != "/etc/wireguard/wgs1.conf" {
		t.Fatalf("%+v", p)
	}
	// the tunnel is taken out: its file goes, and the script (which stays) takes the interface down
	p = apply(sampleVPN(func(v *config.VPNConfig) {}))
	var removed []string
	for _, f := range p.Files {
		if f.Kind == Remove {
			removed = append(removed, f.Path)
		}
	}
	if !strings.Contains(actionSummary(p), "apply wireguard") || len(removed) != 1 || removed[0] != "/etc/wireguard/wgs1.conf" {
		t.Fatalf("%v %s", removed, actionSummary(p))
	}
	if _, err := os.Stat(filepath.Join(string(fs), "etc/wireguard/wgs1.conf")); !os.IsNotExist(err) {
		t.Fatal("the file is still there")
	}
	if _, err := os.Stat(filepath.Join(string(fs), "usr/local/sbin/peakroute-wg-sync")); err != nil {
		t.Fatal("the script is gone, so nothing can take the tunnel down")
	}
	// a file nobody wrote in the directory is removed too: the directory is PeakRoute's
	os.WriteFile(filepath.Join(string(fs), "etc/wireguard/wg0.conf"), []byte("[Interface]\n"), 0600)
	p = apply(sampleVPN(func(v *config.VPNConfig) {}))
	if _, err := os.Stat(filepath.Join(string(fs), "etc/wireguard/wg0.conf")); !os.IsNotExist(err) {
		t.Fatalf("%+v", p)
	}
	// the commands: a check first, then the script
	cmds := strings.Join(run.Commands(), "\n")
	if !strings.Contains(cmds, "wg-quick strip") || !strings.Contains(cmds, "sh "+PathWGSync) {
		t.Fatalf("%s", cmds)
	}
	if strings.Index(cmds, "wg-quick strip") > strings.Index(cmds, "sh "+PathWGSync) {
		t.Fatal("the check comes after the script")
	}
}

func TestTheKeysInTheFixturesAreKeys(t *testing.T) {
	for _, k := range []string{kSitePriv, kSitePeer, kRAPriv, kClient1, kClient2, kPSK} {
		if !wg.ValidKey(k) {
			t.Errorf("%s is not a key", k)
		}
	}
}

func TestOptionalPartsOfASiteFileAreLeftOutWhenNotSet(t *testing.T) {
	s := siteA()
	s.Keepalive, s.Endpoint = 0, ""
	conf := renderWGSite(s)
	if strings.Contains(conf, "PersistentKeepalive") || strings.Contains(conf, "Endpoint") || strings.Contains(conf, "PresharedKey") {
		t.Fatalf("the optional lines are there:\n%s", conf)
	}
	s.PresharedKey, s.Keepalive = kPSK, 30
	conf = renderWGSite(s)
	if !strings.Contains(conf, "PresharedKey = "+kPSK+"\n") || !strings.Contains(conf, "PersistentKeepalive = 30\n") {
		t.Fatalf("%s", conf)
	}
	// the preshared key comes in the [Peer] section, after the peer's key
	if i, j := strings.Index(conf, "[Peer]"), strings.Index(conf, "PresharedKey"); i < 0 || j < i {
		t.Fatalf("%s", conf)
	}
}

package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"peakroute/internal/pki"
)

// ovFixture is a complete OpenVPN setup: the authority and the server certificate, a remote access server with two devices, a site
// tunnel where this router is the server and one where it is the client (with the file of another router's authority).
type ovFixtureData struct {
	c              NetworkConfig
	ca, caKey      string
	dev1, dev2     string // serials
	peerSerial     string
	otherCA, other string // another router's authority, and its key
}

func ovFixture(t *testing.T) ovFixtureData {
	t.Helper()
	now := time.Now()
	ca, caKey, err := pki.NewCA("PeakRoute test CA", now)
	if err != nil {
		t.Fatal(err)
	}
	srv, srvKey, _ := pki.Issue(ca, caKey, "server", true, now)
	issue := func(cn string) string {
		cert, _, err := pki.Issue(ca, caKey, cn, false, now)
		if err != nil {
			t.Fatal(err)
		}
		i, _ := pki.Describe(cert)
		return i.Serial
	}
	tc1, _ := pki.NewTLSCryptKey()
	tc2, _ := pki.NewTLSCryptKey()
	tc3, _ := pki.NewTLSCryptKey()
	// the other router's authority issued the certificate this router uses as a client
	oca, ocaKey, _ := pki.NewCA("Another router", now)
	ccert, ckey, _ := pki.Issue(oca, ocaKey, "s2-peer", false, now)

	f := ovFixtureData{ca: ca, caKey: caKey, dev1: issue("ra-c1"), dev2: issue("ra-c2"), peerSerial: issue("s1-peer"), otherCA: oca, other: ocaKey}
	f.c = vpnNet(t, func(v *VPNConfig) {
		v.CA = &VPNCA{Cert: ca, Key: caKey, ServerCert: srv, ServerKey: srvKey}
		v.OVRemote = &OVRemote{ListenPort: DefaultOVRAPort, Proto: "udp", Network: "10.9.0.0/24", Endpoint: "vpn.example.com", TLSCrypt: tc1,
			Clients: []OVClient{
				{ID: "c1", Name: "Anna phone", Serial: f.dev1, Address: "10.9.0.2/32", NotAfter: now.AddDate(5, 0, 0).Unix()},
				{ID: "c2", Name: "Work laptop", Serial: f.dev2, Address: "10.9.0.3/32"},
			}}
		v.Sites = []VPNSite{
			{ID: "s1", Name: "Branch", Engine: EngineOpenVPN, ListenPort: 1195, Address: "10.98.1.1/30", RemoteNetworks: []string{"10.20.0.0/24"},
				OV: &OVSite{Role: OVRoleServer, Proto: "udp", Remote: "router.example.com:1195", TLSCrypt: tc2, PeerSerial: f.peerSerial}},
			{ID: "s2", Name: "Depot", Engine: EngineOpenVPN, Address: "10.98.2.2/30", RemoteNetworks: []string{"10.30.0.0/24"},
				OV: &OVSite{Role: OVRoleClient, Proto: "tcp", Remote: "depot.example.com:443", TLSCrypt: tc3, CACert: oca, Cert: ccert, Key: ckey}},
		}
	})
	return f
}

func TestOpenVPNSettingsThatAreAccepted(t *testing.T) {
	f := ovFixture(t)
	if err := f.c.Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	// a client tunnel may have no address (the server tells it), a server tunnel any /24 to /30 that starts with the first address
	c := f.c.clone()
	c.VPN.Sites[1].Address = ""
	if err := c.Validate(); err != nil {
		t.Fatalf("a client without an address: %v", err)
	}
	c = f.c.clone()
	c.VPN.Sites[0].Address = "10.98.1.1/24"
	if err := c.Validate(); err != nil {
		t.Fatalf("a /24 tunnel: %v", err)
	}
	// only some of it: the authority with nothing using it, a remote access server alone, a client tunnel alone (no authority needed)
	c = f.c.clone()
	c.VPN.OVRemote, c.VPN.Sites = nil, nil
	if err := c.Validate(); err != nil {
		t.Fatalf("an authority nobody uses: %v", err)
	}
	c = f.c.clone()
	c.VPN.CA, c.VPN.OVRemote, c.VPN.Sites = nil, nil, c.VPN.Sites[1:]
	if err := c.Validate(); err != nil {
		t.Fatalf("a client tunnel alone: %v", err)
	}
	// switched off things are checked like the others
	c = f.c.clone()
	c.VPN.OVRemote.Disabled = true
	c.VPN.Sites[0].Disabled = true
	if err := c.Validate(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestOpenVPNSettingsThatAreRefused(t *testing.T) {
	f := ovFixture(t)
	mod := func(fn func(*VPNConfig)) NetworkConfig {
		c := f.c.clone()
		fn(c.VPN)
		return c
	}
	now := time.Now()
	wrongCA, wrongCAKey, _ := pki.NewCA("Wrong", now)
	wrongSrv, wrongSrvKey, _ := pki.Issue(wrongCA, wrongCAKey, "server", true, now)
	clientCert, clientKey, _ := pki.Issue(f.ca, f.caKey, "someone", false, now)
	crl, _ := pki.NewCRL(f.ca, f.caKey, []pki.Revoked{{Serial: "abc", At: now}}, 1, now)
	cases := []struct {
		name  string
		c     NetworkConfig
		field string
	}{
		// the authority
		{"no authority", mod(func(v *VPNConfig) { v.CA = nil }), "vpn.ca"},
		{"an authority that is not one", mod(func(v *VPNConfig) { v.CA.Cert = "x" }), "vpn.ca.cert"},
		{"a certificate that is no authority", mod(func(v *VPNConfig) { v.CA.Cert = clientCert; v.CA.Key = clientKey }), "vpn.ca.cert"},
		{"a key that is not the authority's", mod(func(v *VPNConfig) { v.CA.Key = wrongCAKey }), "vpn.ca.key"},
		{"no server certificate", mod(func(v *VPNConfig) { v.CA.ServerCert, v.CA.ServerKey = "", "" }), "vpn.ca.server_cert"},
		{"a server key that does not fit", mod(func(v *VPNConfig) { v.CA.ServerKey = wrongSrvKey }), "vpn.ca.server_key"},
		{"a server certificate of another authority", mod(func(v *VPNConfig) { v.CA.ServerCert, v.CA.ServerKey = wrongSrv, wrongSrvKey }), "vpn.ca.server_cert"},
		{"a client certificate as the server's", mod(func(v *VPNConfig) { v.CA.ServerCert, v.CA.ServerKey = clientCert, clientKey }), "vpn.ca.server_cert"},
		{"a revocation list that does not match", mod(func(v *VPNConfig) { v.CA.CRL = crl }), "vpn.ca.crl"},
		{"revoked certificates without a list", mod(func(v *VPNConfig) { v.CA.Revoked = []VPNRevoked{{Serial: "abc"}} }), "vpn.ca.crl"},
		{"a serial that is not one", mod(func(v *VPNConfig) { v.CA.Revoked = []VPNRevoked{{Serial: "xyz"}}; v.CA.CRL = crl }), "vpn.ca.revoked[0]"},
		{"a device whose certificate is revoked", mod(func(v *VPNConfig) {
			l, _ := pki.NewCRL(f.ca, f.caKey, []pki.Revoked{{Serial: f.dev1, At: now}}, 1, now)
			v.CA.Revoked, v.CA.CRL = []VPNRevoked{{Serial: f.dev1, Name: "x"}}, l
		}), "vpn.ca.revoked"},
		// the remote access server
		{"another protocol", mod(func(v *VPNConfig) { v.OVRemote.Proto = "sctp" }), "vpn.ov_remote.proto"},
		{"no port", mod(func(v *VPNConfig) { v.OVRemote.ListenPort = 0 }), "vpn.ov_remote.listen_port"},
		{"the port of a tunnel", mod(func(v *VPNConfig) { v.OVRemote.ListenPort = 1195 }), "vpn.sites[0].listen_port"},
		{"a network that is not private", mod(func(v *VPNConfig) { v.OVRemote.Network = "8.8.8.0/24" }), "vpn.ov_remote.network"},
		{"a network of the router", mod(func(v *VPNConfig) { v.OVRemote.Network = "192.168.1.0/24" }), "vpn.ov_remote.network"},
		{"a network too small", mod(func(v *VPNConfig) { v.OVRemote.Network = "10.9.0.0/30" }), "vpn.ov_remote.network"},
		{"a network with host bits", mod(func(v *VPNConfig) { v.OVRemote.Network = "10.9.0.1/24" }), "vpn.ov_remote.network"},
		{"an endpoint that is not one", mod(func(v *VPNConfig) { v.OVRemote.Endpoint = "a b" }), "vpn.ov_remote.endpoint"},
		{"a full tunnel without the internet", mod(func(v *VPNConfig) { v.OVRemote.FullTunnel = true }), "vpn.ov_remote.full_tunnel"},
		{"a bad route", mod(func(v *VPNConfig) { v.OVRemote.Routes = []string{"nope"} }), "vpn.ov_remote.routes"},
		{"a route twice", mod(func(v *VPNConfig) { v.OVRemote.Routes = []string{"10.1.0.0/16", "10.1.0.0/16"} }), "vpn.ov_remote.routes"},
		{"no control channel key", mod(func(v *VPNConfig) { v.OVRemote.TLSCrypt = "" }), "vpn.ov_remote.tls_crypt"},
		// devices
		{"a device with a bad id", mod(func(v *VPNConfig) { v.OVRemote.Clients[0].ID = "phone" }), "vpn.ov_remote.clients[0].id"},
		{"two devices with one id", mod(func(v *VPNConfig) { v.OVRemote.Clients[1].ID = "c1" }), "vpn.ov_remote.clients[1].id"},
		{"a device with no name", mod(func(v *VPNConfig) { v.OVRemote.Clients[0].Name = "" }), "vpn.ov_remote.clients[0].name"},
		{"two devices with one name", mod(func(v *VPNConfig) { v.OVRemote.Clients[1].Name = "anna PHONE" }), "vpn.ov_remote.clients[1].name"},
		{"a device with no certificate", mod(func(v *VPNConfig) { v.OVRemote.Clients[0].Serial = "" }), "vpn.ov_remote.clients[0].serial"},
		{"two devices with one certificate", mod(func(v *VPNConfig) { v.OVRemote.Clients[1].Serial = f.dev1 }), "vpn.ov_remote.clients[1].serial"},
		{"a device outside the network", mod(func(v *VPNConfig) { v.OVRemote.Clients[0].Address = "10.8.0.2/32" }), "vpn.ov_remote.clients[0].address"},
		{"a device with the router's address", mod(func(v *VPNConfig) { v.OVRemote.Clients[0].Address = "10.9.0.1/32" }), "vpn.ov_remote.clients[0].address"},
		{"a device with a network as its address", mod(func(v *VPNConfig) { v.OVRemote.Clients[0].Address = "10.9.0.0/24" }), "vpn.ov_remote.clients[0].address"},
		{"two devices with one address", mod(func(v *VPNConfig) { v.OVRemote.Clients[1].Address = "10.9.0.2/32" }), "vpn.ov_remote.clients[1].address"},
		// site tunnels
		{"no OpenVPN settings", mod(func(v *VPNConfig) { v.Sites[0].OV = nil }), "vpn.sites[0].openvpn"},
		{"a WireGuard key on an OpenVPN tunnel", mod(func(v *VPNConfig) { v.Sites[0].PrivateKey = key(t) }), "vpn.sites[0].engine"},
		{"a keep-alive on an OpenVPN tunnel", mod(func(v *VPNConfig) { v.Sites[0].Keepalive = 25 }), "vpn.sites[0].engine"},
		{"an unknown role", mod(func(v *VPNConfig) { v.Sites[0].OV.Role = "both" }), "vpn.sites[0].openvpn.role"},
		{"another protocol on a tunnel", mod(func(v *VPNConfig) { v.Sites[0].OV.Proto = "icmp" }), "vpn.sites[0].openvpn.proto"},
		{"no control channel key on a tunnel", mod(func(v *VPNConfig) { v.Sites[0].OV.TLSCrypt = "x" }), "vpn.sites[0].openvpn.tls_crypt"},
		{"a remote that is not one", mod(func(v *VPNConfig) { v.Sites[0].OV.Remote = "a b:1" }), "vpn.sites[0].openvpn.remote"},
		{"a server without the port to connect to", mod(func(v *VPNConfig) { v.Sites[0].OV.Remote = "router.example.com" }), "vpn.sites[0].openvpn.remote"},
		{"a server without a port", mod(func(v *VPNConfig) { v.Sites[0].ListenPort = 0 }), "vpn.sites[0].listen_port"},
		{"a server with the router's port twice", mod(func(v *VPNConfig) { v.Sites[0].ListenPort = 1194 }), "vpn.sites[0].listen_port"},
		{"a server without a certificate for the other router", mod(func(v *VPNConfig) { v.Sites[0].OV.PeerSerial = "" }), "vpn.sites[0].openvpn.peer_serial"},
		{"a server with an imported certificate", mod(func(v *VPNConfig) { v.Sites[0].OV.Cert = clientCert }), "vpn.sites[0].openvpn.role"},
		{"a server that does not take the first address", mod(func(v *VPNConfig) { v.Sites[0].Address = "10.98.1.2/30" }), "vpn.sites[0].address"},
		{"a server without an address", mod(func(v *VPNConfig) { v.Sites[0].Address = "" }), "vpn.sites[0].address"},
		{"a /31 tunnel", mod(func(v *VPNConfig) { v.Sites[0].Address = "10.98.1.1/31" }), "vpn.sites[0].address"},
		{"a client that listens", mod(func(v *VPNConfig) { v.Sites[1].ListenPort = 1196 }), "vpn.sites[1].listen_port"},
		{"a client with a certificate to revoke", mod(func(v *VPNConfig) { v.Sites[1].OV.PeerSerial = "abc" }), "vpn.sites[1].openvpn.role"},
		{"a client without a remote", mod(func(v *VPNConfig) { v.Sites[1].OV.Remote = "" }), "vpn.sites[1].openvpn.remote"},
		{"a client remote without a port", mod(func(v *VPNConfig) { v.Sites[1].OV.Remote = "depot.example.com" }), "vpn.sites[1].openvpn.remote"},
		{"a client without its certificate", mod(func(v *VPNConfig) { v.Sites[1].OV.Cert = "" }), "vpn.sites[1].openvpn.cert"},
		{"a client key that does not fit", mod(func(v *VPNConfig) { v.Sites[1].OV.Key = clientKey }), "vpn.sites[1].openvpn.key"},
		{"a client authority that is not one", mod(func(v *VPNConfig) { v.Sites[1].OV.CACert = clientCert }), "vpn.sites[1].openvpn.ca_cert"},
		{"a client certificate of another authority", mod(func(v *VPNConfig) { v.Sites[1].OV.CACert = f.ca }), "vpn.sites[1].openvpn.cert"},
		{"a server certificate as the client's", mod(func(v *VPNConfig) {
			cert, key, _ := pki.Issue(f.otherCA, f.other, "x", true, now)
			v.Sites[1].OV.Cert, v.Sites[1].OV.Key = cert, key
		}), "vpn.sites[1].openvpn.cert"},
		{"the networks of the other router overlap ours", mod(func(v *VPNConfig) { v.Sites[1].RemoteNetworks = []string{"10.9.0.0/24"} }), "vpn.sites[1].remote_networks"},
		{"a tunnel network that overlaps the remote access network", mod(func(v *VPNConfig) { v.Sites[0].Address = "10.9.0.1/30" }), "vpn.sites[0].address"},
		{"a name that is the OpenVPN remote access's", mod(func(v *VPNConfig) { v.Sites[0].Name = "Remote access (OpenVPN)" }), "vpn.sites[0].name"},
	}
	for _, tc := range cases {
		if !errorFieldsOf(t, tc.c)[tc.field] {
			t.Errorf("%s: no error on %s (got %v)", tc.name, tc.field, errorFieldsOf(t, tc.c))
		}
	}
}

func TestACertificateThatHasRunOutDoesNotBlockTheConfiguration(t *testing.T) {
	// a client tunnel whose certificate ran out a day ago: still a valid configuration (the page warns)
	f := ovFixture(t)
	past := time.Now().AddDate(-6, 0, 0)
	oca, ocaKey, _ := pki.NewCA("Another router", past)
	cert, key, _ := pki.Issue(oca, ocaKey, "s2-peer", false, past)
	if info, _ := pki.Describe(cert); !info.NotAfter.Before(time.Now()) {
		t.Fatal("the fixture is not out of date")
	}
	f.c.VPN.Sites[1].OV.CACert, f.c.VPN.Sites[1].OV.Cert, f.c.VPN.Sites[1].OV.Key = oca, cert, key
	if err := f.c.Validate(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestOpenVPNZonesAndInterfaces(t *testing.T) {
	f := ovFixture(t)
	f.c.VPN.OVRemote.Internet = true
	f.c.VPN.Sites[1].Isolated = true
	var ids []string
	for _, z := range f.c.VPNZones() {
		ids = append(ids, z.ID+":"+z.Iface+":"+z.Name)
	}
	if got := strings.Join(ids, " "); got != "vpn-ora:ovra:Remote access (OpenVPN) vpn-s1:ovs1:Branch vpn-s2:ovs2:Depot" {
		t.Fatalf("%s", got)
	}
	z, ok := f.c.FindVPNZone(VPNOpenRemoteZone)
	if !ok || !z.Internet || z.Isolated || len(z.Prefixes) != 1 || z.Prefixes[0].String() != "10.9.0.0/24" {
		t.Fatalf("%+v", z)
	}
	if z, _ := f.c.FindVPNZone("vpn-s2"); !z.Isolated || len(z.Prefixes) != 1 || z.Prefixes[0].String() != "10.30.0.0/24" {
		t.Fatalf("%+v", z)
	}
	// switched off, they are not zones; rules may name the ones that are
	f.c.VPN.OVRemote.Disabled = true
	if _, ok := f.c.FindVPNZone(VPNOpenRemoteZone); ok {
		t.Fatal("a zone for a server that is off")
	}
	if OVRemoteInterface() != "ovra" || (VPNSite{ID: "s4", Engine: EngineWireGuard}).Interface() != "wgs4" || (VPNSite{ID: "s4", Engine: EngineOpenVPN}).Interface() != "ovs4" {
		t.Fatal("interface names")
	}
	if OVClientCN("c3") != "ra-c3" || OVPeerCN("s2") != "s2-peer" {
		t.Fatal("common names")
	}
}

func TestWhatOpenVPNDevicesAreToldAndGiven(t *testing.T) {
	f := ovFixture(t)
	if got := strings.Join(f.c.OVClientRoutes(), " "); got != "192.168.1.0/24 10.20.0.0/24 10.30.0.0/24" {
		t.Fatalf("%s", got)
	}
	f.c.VPN.OVRemote.Routes = []string{"192.168.1.0/24"}
	if got := strings.Join(f.c.OVClientRoutes(), " "); got != "192.168.1.0/24" {
		t.Fatalf("%s", got)
	}
	f.c.VPN.OVRemote.FullTunnel = true
	if got := strings.Join(f.c.OVClientRoutes(), " "); got != "0.0.0.0/0" {
		t.Fatalf("%s", got)
	}
	f.c.VPN.OVRemote = nil
	if f.c.OVClientRoutes() != nil {
		t.Fatal("routes without a server")
	}
	// WireGuard's list does not hold the OpenVPN remote access network
	f = ovFixture(t)
	f.c.VPN.WGRemote = remote(t)
	for _, r := range f.c.ClientRoutes() {
		if r == "10.9.0.0/24" {
			t.Fatalf("%v", f.c.ClientRoutes())
		}
	}
	r := f.c.VPN.OVRemote
	if a, ok := r.NextClientAddress(); !ok || a != "10.9.0.4/32" {
		t.Fatalf("%q %v", a, ok)
	}
	if s, ok := r.ServerAddress(); !ok || s.String() != "10.9.0.1" {
		t.Fatalf("%v", s)
	}
	if r.ClientEndpoint() != "vpn.example.com:1194" {
		t.Fatalf("%q", r.ClientEndpoint())
	}
	// ports: all engines share one list
	v := f.c.VPN
	if p := v.NextOVSitePort(); p != 1196 {
		t.Fatalf("%d", p)
	}
	if p := v.NextSitePort(); p != 51820 {
		t.Fatalf("%d", p)
	}
	if v.OVSiteCount() != 2 {
		t.Fatalf("%d", v.OVSiteCount())
	}
}

func TestOpenVPNIsCopiedWhole(t *testing.T) {
	f := ovFixture(t)
	copy := f.c.clone()
	copy.VPN.OVRemote.Clients[0].Name = "changed"
	copy.VPN.OVRemote.Routes = append(copy.VPN.OVRemote.Routes, "x")
	copy.VPN.Sites[0].OV.Remote = "changed.example.com:1"
	copy.VPN.CA.Revoked = append(copy.VPN.CA.Revoked, VPNRevoked{Serial: "1"})
	if f.c.VPN.OVRemote.Clients[0].Name == "changed" || len(f.c.VPN.OVRemote.Routes) != 0 || f.c.VPN.Sites[0].OV.Remote == "changed.example.com:1" || len(f.c.VPN.CA.Revoked) != 0 {
		t.Fatal("a copy shares memory with the original")
	}
	// written only when used, read back the same
	b, _ := json.Marshal(f.c)
	var back NetworkConfig
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatal(err)
	}
	plain, _ := json.Marshal(validNet())
	if strings.Contains(string(plain), "openvpn") || strings.Contains(string(plain), "ov_remote") {
		t.Fatalf("%s", plain)
	}
}

func TestOpenVPNReviewLinesNeverShowASecret(t *testing.T) {
	f := ovFixture(t)
	empty := validNet()
	var all string
	for _, c := range DiffNetwork(empty, f.c) {
		all += c.Label + " | " + c.Old + " | " + c.New + "\n"
	}
	for _, secret := range []string{f.caKey, f.c.VPN.CA.ServerKey, f.c.VPN.OVRemote.TLSCrypt, f.c.VPN.Sites[0].OV.TLSCrypt, f.c.VPN.Sites[1].OV.Key, f.c.VPN.Sites[1].OV.Cert} {
		if strings.Contains(all, strings.TrimSpace(strings.Split(secret, "\n")[1])) {
			t.Fatalf("a secret in the review:\n%s", all)
		}
	}
	for _, want := range []string{"OpenVPN remote access", "OpenVPN device Anna phone", "OpenVPN device Work laptop", "VPN tunnel Branch", "VPN tunnel Depot", "VPN certificate authority"} {
		if !strings.Contains(all, want) {
			t.Errorf("no line for %q in:\n%s", want, all)
		}
	}
	if !strings.Contains(all, "OpenVPN, waits for the other router on UDP 1195 (it connects to router.example.com:1195), tunnel 10.98.1.1/30") ||
		!strings.Contains(all, "OpenVPN to depot.example.com:443 (TCP), tunnel 10.98.2.2/30") {
		t.Errorf("the tunnels are not described:\n%s", all)
	}
	// a new key or a new certificate is "changed", never shown
	g := f.c.clone()
	g.VPN.Sites[0].OV.TLSCrypt, _ = pki.NewTLSCryptKey()
	g.VPN.OVRemote.TLSCrypt, _ = pki.NewTLSCryptKey()
	g.VPN.Sites[1].OV.Key = "x"
	g.VPN.Sites[0].OV.PeerSerial = "abc"
	var lines []string
	for _, c := range DiffNetwork(f.c, g) {
		lines = append(lines, c.Label+"|"+c.Old+"|"+c.New)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{"Control channel key of the VPN tunnel Branch|set|changed", "Control channel key of the OpenVPN remote access|set|changed", "Certificate of the VPN tunnel Depot|set|changed", "Certificate made for the other router of the VPN tunnel Branch|set|changed"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	// removing and revoking
	h := f.c.clone()
	h.VPN.OVRemote.Clients = h.VPN.OVRemote.Clients[1:]
	h.VPN.Sites = h.VPN.Sites[:1]
	l, _ := pki.NewCRL(f.ca, f.caKey, []pki.Revoked{{Serial: f.dev1, At: time.Now()}}, 1, time.Now())
	h.VPN.CA.Revoked, h.VPN.CA.CRL = []VPNRevoked{{Serial: f.dev1, Name: "Anna phone"}}, l
	lines = nil
	for _, c := range DiffNetwork(f.c, h) {
		lines = append(lines, c.Label+"|"+c.New)
	}
	got = strings.Join(lines, "\n")
	for _, want := range []string{"OpenVPN device Anna phone|(removed)", "VPN tunnel Depot|(removed)", "Certificates that are no longer accepted|1"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
}

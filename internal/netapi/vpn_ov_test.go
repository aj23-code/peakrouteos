package netapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/ovpn"
	"peakroute/internal/pki"
)

func (e *env) ovRemote(method, body string) *httptest.ResponseRecorder {
	return e.vpnCall(e.api.HandleVPNOVRemote, method, "", body)
}
func (e *env) addOVClient(body string) *httptest.ResponseRecorder {
	return e.vpnCall(e.api.HandleVPNOVClients, http.MethodPost, "", body)
}
func (e *env) ovClient(method, id, body string) *httptest.ResponseRecorder {
	return e.vpnCall(e.api.HandleVPNOVClient, method, id, body)
}

// secrets of the draft that must never leave the router in a list or a review
func ovSecrets(e *env) []string {
	v := e.draft().VPNSettings()
	var out []string
	add := func(s string) {
		for _, l := range strings.Split(s, "\n") {
			if l = strings.TrimSpace(l); len(l) > 20 && !strings.HasPrefix(l, "-----") && !strings.HasPrefix(l, "#") {
				out = append(out, l)
			}
		}
	}
	if v.CA != nil {
		add(v.CA.Key)
		add(v.CA.ServerKey)
	}
	if v.OVRemote != nil {
		add(v.OVRemote.TLSCrypt)
	}
	for _, s := range v.Sites {
		if s.OV != nil {
			add(s.OV.TLSCrypt)
			add(s.OV.Key)
		}
	}
	return out
}

func (e *env) noSecretsIn(what, text string) {
	e.t.Helper()
	secrets := ovSecrets(e)
	if len(secrets) == 0 {
		e.t.Fatal("no secrets to look for")
	}
	for _, s := range secrets {
		if strings.Contains(text, s) {
			e.t.Fatalf("%s holds a secret:\n%s", what, text)
		}
	}
}

func TestOpenVPNStartsEmpty(t *testing.T) {
	e := standard(t)
	v := e.vpn()
	if v.OVRemote != nil || v.CA != nil || v.Suggest.OVSitePort != 1195 || v.Suggest.OVSiteAddress != "10.98.1.1/29" || v.Suggest.OVRemotePort != 1194 || v.Suggest.OVRemoteNetwork != "10.9.0.0/24" {
		t.Fatalf("%+v", v)
	}
}

func TestSettingUpOpenVPNRemoteAccessMakesTheAuthority(t *testing.T) {
	e := standard(t)
	rec := e.ovRemote(http.MethodPut, `{"endpoint":"Vpn.Example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	v := decodeInto[VPNResponse](t, rec)
	r := v.OVRemote
	if r == nil || !r.Enabled || r.ListenPort != 1194 || r.Proto != "udp" || r.Network != "10.9.0.0/24" || r.ServerAddress != "10.9.0.1" || r.Endpoint != "vpn.example.com" || !r.Pending ||
		len(r.Clients) != 0 || r.Clients == nil || len(r.ClientRoutes) == 0 || r.Text == "" {
		t.Fatalf("%+v", r)
	}
	if v.CA == nil || !strings.HasPrefix(v.CA.Subject, "PeakRoute OS VPN ") || v.CA.Revoked != 0 || v.CA.NotAfter < time.Now().AddDate(19, 0, 0).UnixMilli() || v.CA.ServerNotAfter == 0 {
		t.Fatalf("%+v", v.CA)
	}
	e.noSecretsIn("the answer", rec.Body.String())
	e.noSecretsIn("the list", string(mustJSON(e.vpn())))
	// the draft is complete: authority, server certificate, an empty list, the control channel key
	d := e.draft().VPNSettings()
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	if d.CA.Key == "" || d.CA.ServerKey == "" || d.CA.CRL == "" || d.OVRemote.TLSCrypt == "" {
		t.Fatalf("%+v", d.CA)
	}
	if e.mgr.NetworkConfig().VPN != nil {
		t.Fatal("the active configuration changed")
	}
	// the review names the authority and the server, never a key
	var all string
	for _, c := range e.staged() {
		all += c.Label + "|" + c.Old + "|" + c.New + "\n"
	}
	e.noSecretsIn("the review", all)
	if !strings.Contains(all, "OpenVPN remote access") || !strings.Contains(all, "VPN certificate authority") {
		t.Fatalf("%s", all)
	}
	// a second call keeps the authority and the key, and changes what it names
	ca, tc := d.CA.Cert, d.OVRemote.TLSCrypt
	rec = e.ovRemote(http.MethodPut, `{"proto":"TCP","listen_port":443,"dns":true,"internet":true,"full_tunnel":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	n := e.draft().VPNSettings()
	if n.CA.Cert != ca || n.OVRemote.TLSCrypt != tc || n.OVRemote.Proto != "tcp" || n.OVRemote.ListenPort != 443 || !n.OVRemote.FullTunnel || n.OVRemote.Endpoint != "vpn.example.com" {
		t.Fatalf("%+v", n.OVRemote)
	}
	if got := decodeInto[VPNResponse](t, rec).OVRemote.ClientRoutes; len(got) != 1 || got[0] != "0.0.0.0/0" {
		t.Fatalf("%v", got)
	}
	// checked
	for name, body := range map[string]string{"proto": `{"proto":"sctp"}`, "port": `{"listen_port":0}`, "network": `{"network":"192.168.1.0/24"}`, "full tunnel": `{"internet":false}`, "routes": `{"routes":["x"]}`} {
		if rec := e.ovRemote(http.MethodPut, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := e.ovRemote(http.MethodPut, `{"bogus":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := e.ovRemote(http.MethodGet, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	e.editable = false
	if rec := e.ovRemote(http.MethodPut, `{}`); rec.Code != http.StatusConflict {
		t.Errorf("while applying: %d", rec.Code)
	}
}

func TestAnOpenVPNDeviceGetsAFileOnceAndItsCertificateCanBeRevoked(t *testing.T) {
	e := standard(t)
	if rec := e.addOVClient(`{"name":"Phone"}`); rec.Code != http.StatusConflict {
		t.Fatalf("before the server: %d %s", rec.Code, rec.Body)
	}
	e.ovRemote(http.MethodPut, `{}`)
	// no address to connect to: refused, and no certificate is kept
	rec := e.addOVClient(`{"name":"Phone"}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["endpoint"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.draft().VPNSettings().OVRemote.Clients) != 0 {
		t.Fatal("a device was kept whose file cannot be made")
	}
	e.ovRemote(http.MethodPut, `{"endpoint":"vpn.example.com"}`)
	rec = e.addOVClient(`{"name":"Alec Phone"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decodeInto[vpnOVClientResponse](t, rec)
	d := e.draft().VPNSettings()
	cl := d.OVRemote.Clients[0]
	if resp.File == nil || resp.File.Filename != "peakroute-alec-phone.ovpn" || resp.Client.ID != "c1" || resp.Client.Address != "10.9.0.2/32" || !resp.Client.Enabled || !resp.Client.Pending ||
		resp.Client.NotAfter < time.Now().AddDate(4, 11, 0).UnixMilli() || resp.Client.Expiring || cl.Serial == "" {
		t.Fatalf("%+v", resp)
	}
	// the file is a complete client file for the server: its certificate is signed by the authority, for a client, with the right name
	got, err := ovpn.Read(resp.File.Config)
	if err != nil {
		t.Fatalf("%v\n%s", err, resp.File.Config)
	}
	if got.Host != "vpn.example.com" || got.Port != 1194 || got.Proto != "udp" || got.CA != d.CA.Cert || got.TLSCrypt == "" {
		t.Fatalf("%+v", got)
	}
	if err := pki.Verify(d.CA.Cert, got.Cert, false, time.Now()); err != nil {
		t.Fatalf("the device's certificate: %v", err)
	}
	if info, _ := pki.Describe(got.Cert); info.CommonName != "ra-c1" || info.Serial != cl.Serial {
		t.Fatalf("%+v want %s", info, cl.Serial)
	}
	if !pki.MatchesKey(got.Cert, got.Key) {
		t.Fatal("the file's key is not the certificate's")
	}
	// the private key is in the file and nowhere the router keeps
	if strings.Contains(string(mustJSON(e.draft())), strings.Split(strings.TrimSpace(got.Key), "\n")[1]) {
		t.Fatal("the device's private key was kept")
	}
	e.noSecretsIn("the answer", rec.Body.String()[:strings.Index(rec.Body.String(), `"file"`)])
	e.noSecretsIn("the list", string(mustJSON(e.vpn())))
	var all string
	for _, c := range e.staged() {
		all += c.Label + "|" + c.Old + "|" + c.New + "\n"
	}
	e.noSecretsIn("the review", all)
	if !strings.Contains(all, "OpenVPN device Alec Phone") {
		t.Fatalf("%s", all)
	}
	if rec := e.addOVClient(`{"name":"Laptop"}`); decodeInto[vpnOVClientResponse](t, rec).Client.Address != "10.9.0.3/32" {
		t.Fatalf("%s", rec.Body)
	}
	if rec := e.addOVClient(`{"name":"alec PHONE"}`); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["name"] {
		t.Fatalf("a name used twice: %d %s", rec.Code, rec.Body)
	}
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}

	// a new file: the old certificate is revoked, the new one is another
	e.commit()
	old := cl.Serial
	rec = e.ovClient(http.MethodPut, "c1", `{"new_file":true}`)
	n := decodeInto[vpnOVClientResponse](t, rec)
	d = e.draft().VPNSettings()
	if rec.Code != http.StatusOK || n.File == nil || d.OVRemote.Clients[0].Serial == old || len(d.CA.Revoked) != 1 || d.CA.Revoked[0].Serial != old || !n.Client.Pending {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if list, _ := pki.CRLSerials(d.CA.CRL); len(list) != 1 || list[0] != old {
		t.Fatalf("the list: %v", list)
	}
	if v := e.vpn().CA; v.Revoked != 1 {
		t.Fatalf("%+v", v)
	}
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}

	// switching off keeps the certificate; renaming too
	was := d.OVRemote.Clients[0].Serial
	rec = e.ovClient(http.MethodPut, "c1", `{"enabled":false,"name":"Old phone"}`)
	n = decodeInto[vpnOVClientResponse](t, rec)
	if rec.Code != http.StatusOK || n.Client.Enabled || n.Client.Name != "Old phone" || n.File != nil || e.draft().VPNSettings().OVRemote.Clients[0].Serial != was || len(e.draft().VPNSettings().CA.Revoked) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	// removing revokes
	rec = e.ovClient(http.MethodDelete, "c1", "")
	d = e.draft().VPNSettings()
	if rec.Code != http.StatusOK || len(d.OVRemote.Clients) != 1 || d.OVRemote.Clients[0].ID != "c2" || len(d.CA.Revoked) != 2 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if list, _ := pki.CRLSerials(d.CA.CRL); len(list) != 2 {
		t.Fatalf("%v", list)
	}
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	for _, id := range []string{"c9"} {
		for _, m := range []string{http.MethodPut, http.MethodDelete} {
			if rec := e.ovClient(m, id, `{}`); rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d", m, id, rec.Code)
			}
		}
	}
	if rec := e.ovClient(http.MethodPost, "c2", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("%d", rec.Code)
	}
	e.editable = false
	if rec := e.ovClient(http.MethodDelete, "c2", ""); rec.Code != http.StatusConflict {
		t.Errorf("%d", rec.Code)
	}
}

func TestRemovingTheOpenVPNServerRevokesItsDevicesAndKeepsTheAuthority(t *testing.T) {
	e := standard(t)
	e.ovRemote(http.MethodPut, `{"endpoint":"vpn.example.com"}`)
	e.addOVClient(`{"name":"One"}`)
	e.addOVClient(`{"name":"Two"}`)
	rec := e.ovRemote(http.MethodDelete, "")
	if rec.Code != http.StatusOK || decodeInto[VPNResponse](t, rec).OVRemote != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := e.draft().VPNSettings()
	if d.OVRemote != nil || d.CA == nil || len(d.CA.Revoked) != 2 {
		t.Fatalf("%+v", d)
	}
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	if rec := e.ovRemote(http.MethodDelete, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	// the authority stays, so that the files of other tunnels keep working: setting the server up again does not make a new one
	ca := d.CA.Cert
	e.ovRemote(http.MethodPut, `{}`)
	if e.draft().VPNSettings().CA.Cert != ca {
		t.Fatal("a new authority was made")
	}
}

func TestAnOpenVPNSiteTunnelAsTheServerMakesTheFileForTheOtherRouter(t *testing.T) {
	e := standard(t)
	// without the address the other router connects to, nothing is kept (not even an authority)
	rec := e.addSite(`{"engine":"openvpn","name":"Branch","remote_networks":["10.50.0.0/24"]}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["remote"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.stageFileExists() {
		t.Fatal("a refused tunnel left a draft")
	}
	rec = e.addSite(`{"engine":"openvpn","role":"server","name":" Branch ","proto":"TCP","remote":"Router.Example.com:1195","remote_networks":["10.50.0.0/24"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decodeInto[vpnSiteWithFile](t, rec)
	d := e.draft().VPNSettings()
	s := d.Sites[0]
	if resp.ID != "s1" || resp.Engine != "openvpn" || resp.Role != "server" || resp.Proto != "tcp" || resp.Remote != "router.example.com:1195" || resp.Name != "Branch" || resp.ListenPort != 1195 ||
		resp.Address != "10.98.1.1/29" || resp.OtherAddress != "10.98.1.2/29" || !resp.Pending || resp.PublicKey != "" || resp.Status != nil || resp.File == nil || resp.File.Filename != "peakroute-tunnel-branch.ovpn" {
		t.Fatalf("%+v", resp)
	}
	got, err := ovpn.Read(resp.File.Config)
	if err != nil {
		t.Fatalf("%v\n%s", err, resp.File.Config)
	}
	if got.Host != "router.example.com" || got.Port != 1195 || got.Proto != "tcp" || got.Tunnel != "10.98.1.2/29" || len(got.Networks) != 1 || got.Networks[0] != "192.168.1.0/24" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(resp.File.Config, "route-nopull") {
		t.Fatal("the other router would take what this one pushes")
	}
	if err := pki.Verify(d.CA.Cert, got.Cert, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if info, _ := pki.Describe(got.Cert); info.CommonName != "s1-peer" || info.Serial != s.OV.PeerSerial {
		t.Fatalf("%+v %q", info, s.OV.PeerSerial)
	}
	if strings.Contains(string(mustJSON(e.draft())), strings.Split(strings.TrimSpace(got.Key), "\n")[1]) {
		t.Fatal("the other router's key was kept")
	}
	e.noSecretsIn("the list", string(mustJSON(e.vpn())))
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	// the switches and the networks can be changed later; a new file replaces the certificate of the other router
	e.commit()
	old := s.OV.PeerSerial
	rec = e.site(http.MethodPut, "s1", `{"name":"HQ link","internet":true,"remote_networks":["10.50.0.0/24","10.51.0.0/24"]}`)
	r2 := decodeInto[vpnSiteWithFile](t, rec)
	if rec.Code != http.StatusOK || r2.File != nil || r2.Name != "HQ link" || !r2.Internet || len(r2.RemoteNetworks) != 2 || e.draft().VPNSettings().Sites[0].OV.PeerSerial != old {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = e.site(http.MethodPut, "s1", `{"new_file":true}`)
	r3 := decodeInto[vpnSiteWithFile](t, rec)
	d = e.draft().VPNSettings()
	if rec.Code != http.StatusOK || r3.File == nil || d.Sites[0].OV.PeerSerial == old || len(d.CA.Revoked) != 1 || d.CA.Revoked[0].Serial != old {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	// a new file needs the address to connect to
	e.site(http.MethodPut, "s1", `{"remote":""}`)
	before := e.draft().VPNSettings().Sites[0].OV.PeerSerial
	if rec := e.site(http.MethodPut, "s1", `{"new_file":true}`); rec.Code != http.StatusBadRequest || e.draft().VPNSettings().Sites[0].OV.PeerSerial != before {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// removing revokes the certificate of the other router
	e.site(http.MethodPut, "s1", `{"remote":"router.example.com:1195"}`)
	cur := e.draft().VPNSettings().Sites[0].OV.PeerSerial
	rec = e.site(http.MethodDelete, "s1", "")
	d = e.draft().VPNSettings()
	if rec.Code != http.StatusOK || len(d.Sites) != 0 || d.CA == nil || d.CA.Revoked[len(d.CA.Revoked)-1].Serial != cur {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}
}

// aFileFromAnotherRouter is what another PeakRoute router makes for a tunnel: its authority, a certificate for this router, a key.
func aFileFromAnotherRouter(t *testing.T, port int, proto string) string {
	t.Helper()
	now := time.Now()
	ca, caKey, _ := pki.NewCA("The other router", now)
	cert, key, err := pki.Issue(ca, caKey, "s4-peer", false, now)
	if err != nil {
		t.Fatal(err)
	}
	tc, _ := pki.NewTLSCryptKey()
	text, err := ovpn.Write(ovpn.Params{Name: "Main office", Host: "main.example.com", Port: port, Proto: proto, CA: ca, Cert: cert, Key: key, TLSCrypt: tc,
		Tunnel: "10.77.0.2/29", Networks: []string{"10.80.0.0/24"}, RouteNoPull: true})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestAnOpenVPNSiteTunnelAsTheClientImportsTheFile(t *testing.T) {
	e := standard(t)
	file := aFileFromAnotherRouter(t, 1195, "udp")
	rec := e.addSite(`{"engine":"openvpn","role":"client","name":"Main office","file":` + jsonText(file) + `}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decodeInto[vpnSiteWithFile](t, rec)
	if resp.ID != "s1" || resp.Role != "client" || resp.Proto != "udp" || resp.Remote != "main.example.com:1195" || resp.Address != "10.77.0.2/29" || len(resp.RemoteNetworks) != 1 || resp.RemoteNetworks[0] != "10.80.0.0/24" ||
		resp.File != nil || resp.ListenPort != 0 || !resp.Pending || resp.CertExpires < time.Now().AddDate(4, 11, 0).UnixMilli() {
		t.Fatalf("%+v", resp)
	}
	// the key of the other router's file is kept (the tunnel needs it) and never sent back
	e.noSecretsIn("the answer", rec.Body.String())
	e.noSecretsIn("the list", string(mustJSON(e.vpn())))
	if e.draft().VPNSettings().CA != nil {
		t.Fatal("a client tunnel needs no authority of its own")
	}
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	var all string
	for _, c := range e.staged() {
		all += c.Label + "|" + c.Old + "|" + c.New + "\n"
	}
	e.noSecretsIn("the review", all)
	for name, body := range map[string]string{
		"no file":                `{"engine":"openvpn","role":"client","name":"X"}`,
		"a file that is not one": `{"engine":"openvpn","role":"client","name":"X","file":"hello"}`,
		"a bad role":             `{"engine":"openvpn","role":"both","name":"X"}`,
		"a bad engine":           `{"engine":"ipsec","name":"X"}`,
	} {
		if rec := e.addSite(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// the networks and switches can be set when the file has no hints, and a changed file replaces the material
	plain := strings.NewReplacer("# peakroute-tunnel 10.77.0.2/29\n", "", "# peakroute-networks 10.80.0.0/24\n", "").Replace(aFileFromAnotherRouter(t, 443, "tcp"))
	rec = e.addSite(`{"engine":"openvpn","role":"client","name":"Depot","file":` + jsonText(plain) + `}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["remote_networks"] {
		t.Fatalf("no networks: %d %s", rec.Code, rec.Body)
	}
	rec = e.addSite(`{"engine":"openvpn","role":"client","name":"Depot","file":` + jsonText(plain) + `,"remote_networks":["10.90.0.0/24"],"internet":true}`)
	r2 := decodeInto[vpnSiteWithFile](t, rec)
	if rec.Code != http.StatusCreated || r2.ID != "s2" || r2.Proto != "tcp" || r2.Remote != "main.example.com:443" || r2.Address != "" || !r2.Internet {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	oldKey := e.draft().VPNSettings().Sites[0].OV.Key
	rec = e.site(http.MethodPut, "s1", `{"file":`+jsonText(aFileFromAnotherRouter(t, 1200, "udp"))+`}`)
	r3 := decodeInto[vpnSiteWithFile](t, rec)
	if rec.Code != http.StatusOK || r3.Remote != "main.example.com:1200" || e.draft().VPNSettings().Sites[0].OV.Key == oldKey || r3.File != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.site(http.MethodPut, "s1", `{"file":"junk"}`); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["file"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.site(http.MethodPut, "s1", `{"new_file":true}`); rec.Code != http.StatusOK || decodeInto[vpnSiteWithFile](t, rec).File != nil {
		t.Fatalf("a client has no file to make: %d %s", rec.Code, rec.Body)
	}
	// removing a client tunnel revokes nothing (it has no certificate of its own here)
	rec = e.site(http.MethodDelete, "s1", "")
	if rec.Code != http.StatusOK || len(e.draft().VPNSettings().Sites) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestTheEnginesAreNotMixedUp(t *testing.T) {
	e := standard(t)
	peer := genPub(t)
	e.addSite(`{"name":"Wire","address":"10.99.1.1/30","peer_public_key":"` + peer + `","remote_networks":["10.50.0.0/24"]}`)
	e.addSite(`{"engine":"openvpn","role":"server","name":"Open","remote":"r.example.com:1195","remote_networks":["10.60.0.0/24"]}`)
	d := e.draft().VPNSettings()
	if len(d.Sites) != 2 || d.Sites[0].Engine != "wireguard" || d.Sites[1].Engine != "openvpn" || d.Sites[1].ID != "s2" || d.Sites[1].ListenPort != 1195 || d.Sites[0].ListenPort != 51820 {
		t.Fatalf("%+v", d.Sites)
	}
	// OpenVPN settings on a WireGuard tunnel, WireGuard settings that make no sense on an OpenVPN tunnel
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"role on a WireGuard tunnel":  e.site(http.MethodPut, "s1", `{"role":"server"}`),
		"remote on a WireGuard one":   e.site(http.MethodPut, "s1", `{"remote":"a.example:1"}`),
		"a file on a WireGuard one":   e.site(http.MethodPut, "s1", `{"file":"x"}`),
		"a new file on a WireGuard":   e.site(http.MethodPut, "s1", `{"new_file":true}`),
		"engine change":               e.site(http.MethodPut, "s2", `{"engine":"wireguard"}`),
		"engine change the other way": e.site(http.MethodPut, "s1", `{"engine":"openvpn"}`),
		"role at creation":            e.addSite(`{"engine":"wireguard","role":"server","name":"X"}`),
	} {
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	// an OpenVPN tunnel keeps no WireGuard keys: the request cannot carry them (strict decoding knows the fields, validation refuses them)
	for name, body := range map[string]string{"a peer key": `{"peer_public_key":"` + peer + `"}`, "a keepalive": `{"keepalive":25}`, "a new key": `{"new_key":true}`, "an endpoint": `{"endpoint":"a.example:1"}`,
		"a preshared key": `{"preshared_key":"generate"}`} {
		if rec := e.site(http.MethodPut, "s2", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s on an OpenVPN tunnel: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := e.addSite(`{"engine":"openvpn","name":"X","endpoint":"a.example:1"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an endpoint at creation: %d", rec.Code)
	}
	if k := e.draft().VPNSettings().Sites[1]; k.PeerPublicKey != "" {
		t.Fatalf("an OpenVPN tunnel took a WireGuard key: %+v", k)
	}
	// both are in the list, and the firewall has a tab for each zone
	f := e.firewall()
	var ids []string
	for _, tb := range f.Tabs {
		ids = append(ids, tb.ID)
	}
	if got := strings.Join(ids, " "); got != "lan vpn-s1 vpn-s2 wan" {
		t.Fatalf("%s", got)
	}
	v := e.vpn()
	if len(v.Sites) != 2 || v.Sites[0].Engine != "wireguard" || v.Sites[1].Engine != "openvpn" || v.Sites[1].PublicKey != "" || v.Suggest.OVSitePort != 1196 || v.Suggest.OVSiteAddress != "10.98.2.1/29" || v.Suggest.SiteAddress != "10.99.2.1/30" {
		t.Fatalf("%+v", v)
	}
}

func TestHowOpenVPNTunnelsAreGoing(t *testing.T) {
	e := standard(t)
	e.ovRemote(http.MethodPut, `{"endpoint":"vpn.example.com"}`)
	e.addOVClient(`{"name":"Phone"}`)
	e.addOVClient(`{"name":"Laptop"}`)
	e.addSite(`{"engine":"openvpn","role":"server","name":"Branch","remote":"r.example.com:1195","remote_networks":["10.60.0.0/24"]}`)
	e.addSite(`{"engine":"openvpn","role":"client","name":"Depot","file":` + jsonText(aFileFromAnotherRouter(t, 1195, "udp")) + `}`)
	since := time.Now().Add(-5 * time.Minute)
	e.api.OVConnections = func(name string) ([]ovpn.Connection, bool) {
		switch name {
		case "pr-ra":
			return []ovpn.Connection{{CommonName: "ra-c1", RealAddress: "203.0.113.2:5000", VirtualAddress: "10.9.0.2", BytesReceived: 1000, BytesSent: 2000, Since: since}}, true
		case "pr-s1":
			return nil, true // running, nobody connected
		}
		return nil, false
	}
	up := map[string]bool{"ovs2": true}
	e.api.LinkUp = func(iface string) bool { return up[iface] }
	// nothing runs as it is shown yet
	v := e.vpn()
	if v.OVRemote.Clients[0].Status != nil || v.Sites[0].Status != nil || v.Sites[1].Status != nil {
		t.Fatalf("%+v", v)
	}
	e.commit()
	v = e.vpn()
	c := v.OVRemote.Clients
	if c[0].Status == nil || c[0].Status.State != "connected" || c[0].Status.Endpoint != "203.0.113.2:5000" || c[0].Status.Received != 1000 || c[0].Status.Sent != 2000 || c[0].Status.Handshake != since.UnixMilli() {
		t.Fatalf("%+v", c[0])
	}
	if c[1].Status == nil || c[1].Status.State != "offline" {
		t.Fatalf("%+v", c[1])
	}
	if v.Sites[0].Status == nil || v.Sites[0].Status.State != "offline" {
		t.Fatalf("a server nobody has connected to: %+v", v.Sites[0].Status)
	}
	if v.Sites[1].Status == nil || v.Sites[1].Status.State != "connected" {
		t.Fatalf("a client tunnel that is up: %+v", v.Sites[1].Status)
	}
	up["ovs2"] = false
	if v := e.vpn(); v.Sites[1].Status.State != "offline" {
		t.Fatalf("%+v", v.Sites[1].Status)
	}
	// a server that is not running: nothing is claimed
	e.api.OVConnections = func(string) ([]ovpn.Connection, bool) { return nil, false }
	if v := e.vpn(); v.OVRemote.Clients[0].Status != nil || v.Sites[0].Status != nil {
		t.Fatalf("%+v", v)
	}
	// no providers: nothing is claimed either
	e.api.OVConnections, e.api.LinkUp = nil, nil
	if v := e.vpn(); v.OVRemote.Clients[0].Status != nil || v.Sites[1].Status != nil {
		t.Fatalf("%+v", v)
	}
	// a change makes it pending again, and a switched-off tunnel has no status
	e.api.OVConnections = func(string) ([]ovpn.Connection, bool) { return nil, true }
	e.ovClient(http.MethodPut, "c1", `{"enabled":false}`)
	if v := e.vpn(); !v.OVRemote.Clients[0].Pending || v.OVRemote.Clients[0].Status != nil {
		t.Fatalf("%+v", v.OVRemote.Clients[0])
	}
}

func TestOpenVPNZonesAreTabsOfTheFirewall(t *testing.T) {
	e := standard(t)
	e.ovRemote(http.MethodPut, `{"endpoint":"vpn.example.com","internet":true}`)
	f := e.firewall()
	var ids []string
	for _, tb := range f.Tabs {
		ids = append(ids, tb.ID+":"+tb.Kind+":"+tb.Name)
	}
	if got := strings.Join(ids, " "); got != "lan:network:LAN vpn-ora:vpn:Remote access (OpenVPN) wan:internet:Internet" {
		t.Fatalf("%s", got)
	}
	var lines []string
	for _, l := range f.Policy["vpn-ora"].After {
		lines = append(lines, l.Text)
	}
	if got := strings.Join(lines, "|"); !strings.Contains(got, "Allow to the internet (set on VPN)") || !strings.Contains(got, "LAN") {
		t.Fatalf("%s", got)
	}
	rec := e.addRule(`{"name":"Printer","from":"vpn-ora","action":"allow","protocol":"tcp","to":"lan","destination":"192.168.1.50","ports":"631"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func jsonText(s string) string { return string(mustJSON(s)) }

func TestOpenVPNSettingsCanBeChangedOneByOne(t *testing.T) {
	e := standard(t)
	e.ovRemote(http.MethodPut, `{"endpoint":"vpn.example.com"}`)
	// every field of the server is taken, and left alone when it is not named
	rec := e.ovRemote(http.MethodPut, `{"dns":true,"isolated":true,"enabled":false,"internet":true,"routes":["192.168.1.0/24"," "],"network":"10.77.0.0/24","endpoint":"B.Example.com:443"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := e.draft().VPNSettings().OVRemote
	if !r.DNS || !r.Isolated || !r.Disabled || !r.Internet || len(r.Routes) != 1 || r.Network != "10.77.0.0/24" || r.Endpoint != "b.example.com:443" {
		t.Fatalf("%+v", r)
	}
	e.ovRemote(http.MethodPut, `{"dns":false,"isolated":false,"enabled":true}`)
	r = e.draft().VPNSettings().OVRemote
	if r.DNS || r.Isolated || r.Disabled || !r.Internet || r.Network != "10.77.0.0/24" {
		t.Fatalf("%+v", r)
	}
	// a device: switched off and on again, named; a missing endpoint stops a new file but not a rename
	e.addOVClient(`{"name":"Phone","enabled":false}`)
	if c := e.draft().VPNSettings().OVRemote.Clients[0]; !c.Disabled {
		t.Fatalf("%+v", c)
	}
	e.ovClient(http.MethodPut, "c1", `{"enabled":true}`)
	if c := e.draft().VPNSettings().OVRemote.Clients[0]; c.Disabled || c.Name != "Phone" {
		t.Fatalf("%+v", c)
	}
	e.ovRemote(http.MethodPut, `{"endpoint":""}`)
	serial := e.draft().VPNSettings().OVRemote.Clients[0].Serial
	if rec := e.ovClient(http.MethodPut, "c1", `{"new_file":true}`); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["endpoint"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.draft().VPNSettings().OVRemote.Clients[0].Serial != serial || len(e.draft().VPNSettings().CA.Revoked) != 0 {
		t.Fatal("a refused new file changed the certificate")
	}
	if rec := e.ovClient(http.MethodPut, "c1", `{"name":"Renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestOpenVPNSiteTunnelSettingsCanBeChangedOneByOne(t *testing.T) {
	e := standard(t)
	e.addSite(`{"engine":"openvpn","role":"server","name":"Branch","remote":"r.example.com:1195","remote_networks":["10.60.0.0/24"]}`)
	rec := e.site(http.MethodPut, "s1", `{"listen_port":1300,"address":"10.98.5.1/29","isolated":true,"enabled":false,"proto":"TCP"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	s := e.draft().VPNSettings().Sites[0]
	if s.ListenPort != 1300 || s.Address != "10.98.5.1/29" || !s.Isolated || !s.Disabled || s.OV.Proto != "tcp" || s.OV.Remote != "r.example.com:1195" {
		t.Fatalf("%+v", s)
	}
	e.site(http.MethodPut, "s1", `{"isolated":false,"enabled":true}`)
	if s := e.draft().VPNSettings().Sites[0]; s.Isolated || s.Disabled || s.ListenPort != 1300 {
		t.Fatalf("%+v", s)
	}
	// a file can only go to a tunnel that connects
	rec = e.site(http.MethodPut, "s1", `{"file":`+jsonText(aFileFromAnotherRouter(t, 1195, "udp"))+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// a client tunnel keeps its address unless the request or the file names one
	file := aFileFromAnotherRouter(t, 1195, "udp")
	e.addSite(`{"engine":"openvpn","role":"client","name":"Main","file":` + jsonText(file) + `}`)
	e.site(http.MethodPut, "s2", `{"address":"10.77.0.3/29"}`)
	if s := e.draft().VPNSettings().Sites[1]; s.Address != "10.77.0.3/29" {
		t.Fatalf("%+v", s)
	}
	e.site(http.MethodPut, "s2", `{"file":`+jsonText(aFileFromAnotherRouter(t, 1196, "udp"))+`,"address":"10.77.0.4/29"}`)
	if s := e.draft().VPNSettings().Sites[1]; s.Address != "10.77.0.4/29" {
		t.Fatalf("the request's address lost to the file's: %+v", s)
	}
	e.site(http.MethodPut, "s2", `{"file":`+jsonText(aFileFromAnotherRouter(t, 1196, "udp"))+`}`)
	if s := e.draft().VPNSettings().Sites[1]; s.Address != "10.77.0.2/29" {
		t.Fatalf("the file's address was not taken: %+v", s)
	}
	// other tunnels are not touched
	if s := e.draft().VPNSettings().Sites[0]; s.ListenPort != 1300 {
		t.Fatalf("%+v", s)
	}
	// an unknown role, other engines' fields, a wrong method
	if rec := e.addSite(`{"engine":"openvpn","role":"server","name":"X","peer_public_key":"` + genPub(t) + `"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.site(http.MethodPut, "s9", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestRevokingTwiceListsACertificateOnce(t *testing.T) {
	v := &config.VPNConfig{}
	if err := ensureCA(v); err != nil {
		t.Fatal(err)
	}
	if err := revoke(v, "", "nobody"); err != nil || len(v.CA.Revoked) != 0 {
		t.Fatalf("%v %+v", err, v.CA.Revoked)
	}
	if err := revoke(&config.VPNConfig{}, "abc", "no authority"); err != nil {
		t.Fatal(err)
	}
	if err := revoke(v, "abc", "x"); err != nil {
		t.Fatal(err)
	}
	n := v.CA.CRLNumber
	if err := revoke(v, "abc", "x again"); err != nil || len(v.CA.Revoked) != 1 || v.CA.CRLNumber != n {
		t.Fatalf("%v %+v %d %d", err, v.CA.Revoked, v.CA.CRLNumber, n)
	}
	if list, _ := pki.CRLSerials(v.CA.CRL); len(list) != 1 || list[0] != "abc" {
		t.Fatalf("%v", list)
	}
	// the authority is made once, with its server certificate, and kept
	cert := v.CA.Cert
	if err := ensureCA(v); err != nil || v.CA.Cert != cert {
		t.Fatalf("%v", err)
	}
	v.CA.ServerCert, v.CA.ServerKey = "", ""
	if err := ensureCA(v); err != nil || v.CA.Cert != cert || v.CA.ServerCert == "" {
		t.Fatalf("%v", err)
	}
}

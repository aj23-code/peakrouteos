package netapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/wg"
)

func (e *env) vpn() VPNResponse {
	e.t.Helper()
	return decodeInto[VPNResponse](e.t, e.do(e.api.HandleVPN, http.MethodGet, "", ""))
}

func (e *env) vpnCall(h http.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func (e *env) addSite(body string) *httptest.ResponseRecorder {
	return e.vpnCall(e.api.HandleVPNSites, http.MethodPost, "", body)
}
func (e *env) site(method, id, body string) *httptest.ResponseRecorder {
	return e.vpnCall(e.api.HandleVPNSite, method, id, body)
}
func (e *env) remote(method, body string) *httptest.ResponseRecorder {
	return e.vpnCall(e.api.HandleVPNRemote, method, "", body)
}
func (e *env) addClient(body string) *httptest.ResponseRecorder {
	return e.vpnCall(e.api.HandleVPNClients, http.MethodPost, "", body)
}
func (e *env) client(method, id, body string) *httptest.ResponseRecorder {
	return e.vpnCall(e.api.HandleVPNClient, method, id, body)
}

func genPub(t *testing.T) string {
	t.Helper()
	_, pub, err := wg.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestVPNStartsEmpty(t *testing.T) {
	e := standard(t)
	v := e.vpn()
	if len(v.LocalNetworks) != 1 || v.LocalNetworks[0].Name != "LAN" || v.LocalNetworks[0].Prefix != "192.168.1.0/24" {
		t.Fatalf("%+v", v.LocalNetworks)
	}
	if len(v.Sites) != 0 || v.Sites == nil || v.Remote != nil || !v.Editable || v.Suggest.SitePort != 51820 || v.Suggest.SiteAddress != "10.99.1.1/30" || v.Suggest.RemotePort != 51821 || v.Suggest.RemoteNetwork != "10.8.0.0/24" {
		t.Fatalf("%+v", v)
	}
	if rec := e.do(e.api.HandleVPN, http.MethodPost, `{}`, "application/json"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: %d", rec.Code)
	}
}

func TestAddingASiteNeverSendsASecretBack(t *testing.T) {
	e := standard(t)
	peer := genPub(t)
	rec := e.addSite(`{"name":" Branch office ","address":"10.99.1.1/30","peer_public_key":"` + peer + `","preshared_key":"generate","endpoint":"Branch.Example.com:51820","remote_networks":["192.168.50.0/24"," "]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := e.draft().VPNSettings().Sites[0]
	if d.PrivateKey == "" || d.PresharedKey == "" {
		t.Fatalf("keys not made: %+v", d)
	}
	for _, secret := range []string{d.PrivateKey, d.PresharedKey} {
		if strings.Contains(rec.Body.String(), secret) || strings.Contains(string(mustJSON(e.vpn())), secret) {
			t.Fatal("a secret came back")
		}
	}
	for _, c := range e.staged() {
		if strings.Contains(c.Old+c.New+c.Label, d.PrivateKey) || strings.Contains(c.Old+c.New+c.Label, d.PresharedKey) {
			t.Fatalf("a secret in the review: %+v", c)
		}
	}
	v := decodeInto[VPNSiteView](t, rec)
	pub, _ := wg.PublicKey(d.PrivateKey)
	if v.ID != "s1" || v.Name != "Branch office" || v.Engine != "wireguard" || !v.Enabled || v.ListenPort != 51820 || v.Address != "10.99.1.1/30" || v.OtherAddress != "10.99.1.2/30" ||
		v.Endpoint != "branch.example.com:51820" || v.Keepalive != config.DefaultKeepalive || len(v.RemoteNetworks) != 1 || v.PublicKey != pub || v.PeerPublicKey != peer || !v.PresharedSet || !v.Pending || v.Status != nil || v.Text == "" {
		t.Fatalf("%+v", v)
	}
	if e.mgr.NetworkConfig().VPN != nil {
		t.Fatal("the active configuration changed")
	}
	if !e.changed("vpn.sites") && len(e.staged()) == 0 {
		t.Fatalf("no review line: %+v", e.staged())
	}
	// the next tunnel gets the next id, port and (suggested) address
	if s := e.vpn().Suggest; s.SitePort != 51822 || s.SiteAddress != "10.99.2.1/30" {
		t.Fatalf("%+v", s)
	}
	rec = e.addSite(`{"name":"Second","address":"10.99.2.1/30","peer_public_key":"` + genPub(t) + `","remote_networks":["192.168.60.0/24"]}`)
	if v := decodeInto[VPNSiteView](t, rec); rec.Code != http.StatusCreated || v.ID != "s2" || v.ListenPort == 51820 || v.Keepalive != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestASiteIsCheckedWhenAdded(t *testing.T) {
	e := standard(t)
	peer := genPub(t)
	for name, c := range map[string]struct{ body, field string }{
		"no peer key":    {`{"address":"10.99.1.1/30","remote_networks":["192.168.50.0/24"]}`, "peer_public_key"},
		"a bad peer key": {`{"address":"10.99.1.1/30","peer_public_key":"abc","remote_networks":["192.168.50.0/24"]}`, "peer_public_key"},
		"no address":     {`{"peer_public_key":"` + peer + `","remote_networks":["192.168.50.0/24"]}`, "address"},
		"overlap router": {`{"address":"10.99.1.1/30","peer_public_key":"` + peer + `","remote_networks":["192.168.1.0/24"]}`, "remote_networks"},
		"no networks":    {`{"address":"10.99.1.1/30","peer_public_key":"` + peer + `"}`, "remote_networks"},
	} {
		rec := e.addSite(c.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[c.field] {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if e.stageFileExists() {
		t.Fatal("a refused tunnel left a draft")
	}
	for name, body := range map[string]string{"an unknown field": `{"extra":1}`, "not an object": `[]`} {
		if rec := e.addSite(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	e.editable = false
	if rec := e.addSite(`{}`); rec.Code != http.StatusConflict {
		t.Errorf("while applying: %d", rec.Code)
	}
	if rec := e.vpnCall(e.api.HandleVPNSites, http.MethodGet, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}

const siteBody = `{"name":"Branch","address":"10.99.1.1/30","peer_public_key":"%s","endpoint":"b.example.com:51820","remote_networks":["192.168.50.0/24"]}`

func (e *env) oneSite() VPNSiteView {
	e.t.Helper()
	rec := e.addSite(strings.Replace(siteBody, "%s", genPub(e.t), 1))
	if rec.Code != http.StatusCreated {
		e.t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return decodeInto[VPNSiteView](e.t, rec)
}

func TestChangingASiteKeepsWhatWasLeftOut(t *testing.T) {
	e := standard(t)
	s := e.oneSite()
	e.commit()
	was := e.draft().VPNSettings().Sites[0]
	rec := e.site(http.MethodPut, "s1", `{"name":"HQ link"}`)
	v := decodeInto[VPNSiteView](t, rec)
	if rec.Code != http.StatusOK || v.Name != "HQ link" || v.Pending != true || v.Endpoint != s.Endpoint || v.PublicKey != s.PublicKey || v.PeerPublicKey != s.PeerPublicKey {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	now := e.draft().VPNSettings().Sites[0]
	if now.PrivateKey != was.PrivateKey || now.Keepalive != was.Keepalive || now.ListenPort != was.ListenPort {
		t.Fatalf("%+v", now)
	}
	// switching off, new key, new preshared key, a changed list
	rec = e.site(http.MethodPut, "s1", `{"enabled":false,"new_key":true,"preshared_key":"generate","remote_networks":["192.168.51.0/24","192.168.52.0/24"],"internet":true,"isolated":true}`)
	v = decodeInto[VPNSiteView](t, rec)
	now = e.draft().VPNSettings().Sites[0]
	if rec.Code != http.StatusOK || v.Enabled || v.PublicKey == s.PublicKey || now.PrivateKey == was.PrivateKey || now.PresharedKey == "" || len(v.RemoteNetworks) != 2 || !v.Internet || !v.Isolated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// an error leaves the draft as it was
	before := e.draft()
	if rec := e.site(http.MethodPut, "s1", `{"peer_public_key":"nope"}`); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["peer_public_key"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.draft().VPNSettings().Sites[0].PeerPublicKey != before.VPNSettings().Sites[0].PeerPublicKey {
		t.Fatal("a refused change was kept")
	}
	if rec := e.site(http.MethodPut, "s9", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.site(http.MethodPost, "s1", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
	e.editable = false
	if rec := e.site(http.MethodPut, "s1", `{}`); rec.Code != http.StatusConflict {
		t.Fatalf("%d", rec.Code)
	}
}

func TestRemovingASite(t *testing.T) {
	e := standard(t)
	e.oneSite()
	e.commit()
	rec := e.site(http.MethodDelete, "s1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if v := decodeInto[VPNResponse](t, rec); len(v.Sites) != 0 {
		t.Fatalf("%+v", v)
	}
	if e.draft().VPN != nil {
		t.Fatalf("an empty VPN setting stays: %+v", e.draft().VPN)
	}
	if rec := e.site(http.MethodDelete, "s1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestSetUpRemoteAccess(t *testing.T) {
	e := standard(t)
	rec := e.remote(http.MethodPut, `{"endpoint":"Vpn.Example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	v := decodeInto[VPNResponse](t, rec)
	r := v.Remote
	d := e.draft().VPNSettings().WGRemote
	pub, _ := wg.PublicKey(d.PrivateKey)
	if r == nil || !r.Enabled || r.ListenPort != 51821 || r.Network != "10.8.0.0/24" || r.ServerAddress != "10.8.0.1" || r.Endpoint != "vpn.example.com" || r.PublicKey != pub || !r.Pending || len(r.Clients) != 0 || r.Clients == nil || len(r.ClientRoutes) == 0 {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(rec.Body.String(), d.PrivateKey) {
		t.Fatal("the private key came back")
	}
	// a change keeps the key, "new_key" replaces it
	e.remote(http.MethodPut, `{"dns":true,"internet":true}`)
	if now := e.draft().VPNSettings().WGRemote; now.PrivateKey != d.PrivateKey || !now.DNS || !now.Internet || now.Endpoint != "vpn.example.com" {
		t.Fatalf("%+v", now)
	}
	e.remote(http.MethodPut, `{"new_key":true}`)
	if e.draft().VPNSettings().WGRemote.PrivateKey == d.PrivateKey {
		t.Fatal("the key stays")
	}
	// full tunnel changes what clients get
	rec = e.remote(http.MethodPut, `{"full_tunnel":true}`)
	if v := decodeInto[VPNResponse](t, rec); !v.Remote.FullTunnel || v.Remote.ClientRoutes[0] != "0.0.0.0/0" {
		t.Fatalf("%+v", v.Remote)
	}
	// checked
	for name, body := range map[string]string{"port": `{"listen_port":0}`, "network": `{"network":"192.168.1.0/24"}`, "routes": `{"routes":["x"]}`} {
		if rec := e.remote(http.MethodPut, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := e.remote(http.MethodPut, `{"bogus":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := e.remote(http.MethodGet, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
	e.editable = false
	if rec := e.remote(http.MethodPut, `{}`); rec.Code != http.StatusConflict {
		t.Errorf("while applying: %d", rec.Code)
	}
	e.editable = true
	if rec := e.remote(http.MethodDelete, ""); rec.Code != http.StatusOK || decodeInto[VPNResponse](t, rec).Remote != nil || e.draft().VPN != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.remote(http.MethodDelete, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestAClientNeedsTheServerFirst(t *testing.T) {
	e := standard(t)
	if rec := e.addClient(`{"name":"Phone"}`); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		if rec := e.client(m, "c1", `{}`); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", m, rec.Code)
		}
	}
}

func TestAddingAClientMakesTheFileOnce(t *testing.T) {
	e := standard(t)
	e.remote(http.MethodPut, `{"endpoint":"vpn.example.com","dns":true}`)
	rec := e.addClient(`{"name":"Alec Phone","preshared":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	resp := decodeInto[vpnClientResponse](t, rec)
	d := e.draft().VPNSettings().WGRemote
	cl := d.Clients[0]
	if resp.File == nil || resp.File.Filename != "peakroute-alec-phone.conf" || resp.Client.ID != "c1" || resp.Client.Address != "10.8.0.2/32" || resp.Client.PublicKey != cl.PublicKey || !resp.Client.PresharedSet || !resp.Client.Pending {
		t.Fatalf("%+v", resp)
	}
	f := resp.File.Config
	serverPub, _ := wg.PublicKey(d.PrivateKey)
	for _, want := range []string{"[Interface]", "Address = 10.8.0.2/32", "DNS = 10.8.0.1", "[Peer]", "PublicKey = " + serverPub, "PresharedKey = " + cl.PresharedKey, "Endpoint = vpn.example.com:51821"} {
		if !strings.Contains(f, want) {
			t.Errorf("the file lacks %q:\n%s", want, f)
		}
	}
	// the private key is in the file and nowhere the router keeps
	var priv string
	for _, l := range strings.Split(f, "\n") {
		if k, ok := strings.CutPrefix(l, "PrivateKey = "); ok {
			priv = k
		}
	}
	if p, _ := wg.PublicKey(priv); p != cl.PublicKey || priv == "" {
		t.Fatalf("the file's private key does not match: %q", priv)
	}
	if strings.Contains(string(mustJSON(e.vpn())), priv) || strings.Contains(string(mustJSON(e.draft())), priv) {
		t.Fatal("the client's private key was kept")
	}
	for _, c := range e.staged() {
		if strings.Contains(c.Old+c.New+c.Label, priv) || strings.Contains(c.Old+c.New+c.Label, cl.PresharedKey) {
			t.Fatalf("a secret in the review: %+v", c)
		}
	}
	// the list never has the preshared key
	if strings.Contains(string(mustJSON(e.vpn())), cl.PresharedKey) {
		t.Fatal("the preshared key is in the list")
	}
	// the second client gets the next address
	if r2 := decodeInto[vpnClientResponse](t, e.addClient(`{"name":"Laptop"}`)); r2.Client.ID != "c2" || r2.Client.Address != "10.8.0.3/32" || r2.Client.PresharedSet {
		t.Fatalf("%+v", r2)
	}
}

func TestAClientWithItsOwnKey(t *testing.T) {
	e := standard(t)
	e.remote(http.MethodPut, `{"endpoint":"vpn.example.com"}`)
	pub := genPub(t)
	rec := e.addClient(`{"name":"Tablet","public_key":"` + pub + `"}`)
	resp := decodeInto[vpnClientResponse](t, rec)
	if rec.Code != http.StatusCreated || resp.File != nil || resp.Client.PublicKey != pub {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// the same key again, a bad key
	if rec := e.addClient(`{"name":"Again","public_key":"` + pub + `"}`); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["public_key"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.addClient(`{"name":"Bad","public_key":"abc"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// a file with the place for the private key is available
	rec = e.vpnCall(e.api.HandleVPNClientTemplate, http.MethodGet, "c1", "")
	f := decodeInto[VPNClientFile](t, rec)
	if rec.Code != http.StatusOK || !strings.Contains(f.Config, "PrivateKey = <paste the private key of the device here>") || f.Filename != "peakroute-tablet.conf" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.vpnCall(e.api.HandleVPNClientTemplate, http.MethodGet, "c9", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.vpnCall(e.api.HandleVPNClientTemplate, http.MethodPost, "c1", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
}

func TestAClientIsRefusedWithoutAWayToConnect(t *testing.T) {
	e := standard(t)
	e.remote(http.MethodPut, `{}`)
	rec := e.addClient(`{"name":"Phone"}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["endpoint"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.draft().VPNSettings().WGRemote.Clients) != 0 {
		t.Fatal("a client was kept whose file cannot be made")
	}
	// the template says the same
	e.addClient(`{"name":"Own","public_key":"` + genPub(t) + `"}`)
	if rec := e.vpnCall(e.api.HandleVPNClientTemplate, http.MethodGet, "c1", ""); rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestChangingAndRemovingAClient(t *testing.T) {
	e := standard(t)
	e.remote(http.MethodPut, `{"endpoint":"vpn.example.com"}`)
	first := decodeInto[vpnClientResponse](t, e.addClient(`{"name":"Phone"}`))
	e.commit()
	rec := e.client(http.MethodPut, "c1", `{"name":"Old phone","enabled":false}`)
	resp := decodeInto[vpnClientResponse](t, rec)
	if rec.Code != http.StatusOK || resp.Client.Name != "Old phone" || resp.Client.Enabled || resp.File != nil || resp.Client.PublicKey != first.Client.PublicKey || !resp.Client.Pending {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// a new key pair answers with a new file
	rec = e.client(http.MethodPut, "c1", `{"new_key":true,"preshared":true}`)
	resp = decodeInto[vpnClientResponse](t, rec)
	if rec.Code != http.StatusOK || resp.File == nil || resp.Client.PublicKey == first.Client.PublicKey || !resp.Client.PresharedSet || !strings.Contains(resp.File.Config, "PresharedKey = ") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// "preshared": false drops it
	if resp := decodeInto[vpnClientResponse](t, e.client(http.MethodPut, "c1", `{"preshared":false}`)); resp.Client.PresharedSet {
		t.Fatalf("%+v", resp)
	}
	// the same key as another client is refused and nothing changes
	e.addClient(`{"name":"B","public_key":"` + genPub(t) + `"}`)
	other := e.draft().VPNSettings().WGRemote.Clients[1].PublicKey
	if rec := e.client(http.MethodPut, "c1", `{"public_key":"`+other+`"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.client(http.MethodPut, "c9", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.client(http.MethodPost, "c1", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
	e.editable = false
	if rec := e.client(http.MethodDelete, "c1", ""); rec.Code != http.StatusConflict {
		t.Fatalf("%d", rec.Code)
	}
	e.editable = true
	rec = e.client(http.MethodDelete, "c1", "")
	if rec.Code != http.StatusOK || len(e.draft().VPNSettings().WGRemote.Clients) != 1 || e.draft().VPNSettings().WGRemote.Clients[0].ID != "c2" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestHowTunnelsAreGoing(t *testing.T) {
	e := standard(t)
	peer := genPub(t)
	e.addSite(`{"name":"Branch","address":"10.99.1.1/30","peer_public_key":"` + peer + `","remote_networks":["192.168.50.0/24"]}`)
	e.remote(http.MethodPut, `{"endpoint":"vpn.example.com"}`)
	c1 := decodeInto[vpnClientResponse](t, e.addClient(`{"name":"Phone"}`))
	c2 := decodeInto[vpnClientResponse](t, e.addClient(`{"name":"Laptop"}`))
	c3 := decodeInto[vpnClientResponse](t, e.addClient(`{"name":"Never"}`))
	now := time.Now()
	e.api.WireGuard = func() []wg.Interface {
		return []wg.Interface{
			{Name: "wgs1", Peers: []wg.Peer{{PublicKey: peer, Endpoint: "198.51.100.7:51820", LatestHandshake: now.Add(-20 * time.Second), Received: 1000, Sent: 2000}}},
			{Name: "wgra", Peers: []wg.Peer{
				{PublicKey: c1.Client.PublicKey, LatestHandshake: now.Add(-10 * time.Second), Endpoint: "203.0.113.5:4444"},
				{PublicKey: c2.Client.PublicKey, LatestHandshake: now.Add(-time.Hour)},
			}},
		}
	}
	// nothing is running as it is shown yet: no status
	v := e.vpn()
	if v.Sites[0].Status != nil || v.Remote.Clients[0].Status != nil {
		t.Fatalf("%+v", v)
	}
	e.commit()
	v = e.vpn()
	s := v.Sites[0]
	if s.Pending || s.Status == nil || s.Status.State != "connected" || s.Status.Endpoint != "198.51.100.7:51820" || s.Status.Received != 1000 || s.Status.Sent != 2000 || s.Status.Handshake == 0 {
		t.Fatalf("%+v", s)
	}
	cl := v.Remote.Clients
	if cl[0].Status == nil || cl[0].Status.State != "connected" || cl[1].Status.State != "idle" || cl[2].Status == nil || cl[2].Status.State != "waiting" || cl[2].Status.Handshake != 0 {
		t.Fatalf("%+v", cl)
	}
	_ = c3
	// switched off: not running, so no status; a change makes it pending again
	e.site(http.MethodPut, "s1", `{"enabled":false}`)
	if s := e.vpn().Sites[0]; s.Status != nil || !s.Pending {
		t.Fatalf("%+v", s)
	}
	// no provider: nothing is claimed
	e.api.WireGuard = nil
	e.site(http.MethodPut, "s1", `{"enabled":true}`)
	if s := e.vpn().Sites[0]; s.Status != nil {
		t.Fatalf("%+v", s)
	}
}

func TestTheVPNPageShowsTheInternetAddress(t *testing.T) {
	e := standard(t)
	e.api.Live = func(config.NetworkConfig) *Live { return &Live{Up: true, Address: "203.0.113.10/24"} }
	if v := e.vpn(); v.WANAddress != "203.0.113.10" {
		t.Fatalf("%q", v.WANAddress)
	}
}

func TestVPNsAreTabsOfTheFirewall(t *testing.T) {
	e := standard(t)
	if f := e.firewall(); len(f.Tabs) != 2 {
		t.Fatalf("%+v", f.Tabs)
	}
	e.addSite(`{"name":"Branch","address":"10.99.1.1/30","peer_public_key":"` + genPub(t) + `","remote_networks":["192.168.50.0/24"],"isolated":true}`)
	e.remote(http.MethodPut, `{"endpoint":"vpn.example.com","internet":true}`)
	f := e.firewall()
	var ids []string
	for _, tb := range f.Tabs {
		ids = append(ids, tb.ID+":"+tb.Kind+":"+tb.Name)
	}
	if got := strings.Join(ids, " "); got != "lan:network:LAN vpn-ra:vpn:Remote access vpn-s1:vpn:Branch wan:internet:Internet" {
		t.Fatalf("%s", got)
	}
	text := func(ls []PolicyLine) string {
		var s []string
		for _, l := range ls {
			s = append(s, l.Kind+": "+l.Text)
		}
		return strings.Join(s, "\n")
	}
	ra := f.Policy["vpn-ra"]
	if strings.Contains(text(ra.Before), "DHCP") || !strings.Contains(text(ra.After), "allow: Allow this router's DNS and ping") || !strings.Contains(text(ra.After), "allow: Allow to the internet (set on VPN)") ||
		!strings.Contains(text(ra.After), "Allow to the other networks that are not isolated: LAN (set on Local Networks and VPN)") || strings.Contains(text(ra.After), "Branch") {
		t.Fatalf("remote access:\n%s", text(ra.After))
	}
	br := f.Policy["vpn-s1"]
	if !strings.Contains(text(br.After), "off: No access to the internet (set on VPN)") || !strings.Contains(text(br.After), "off: Isolated from the other networks (set on VPN)") {
		t.Fatalf("branch:\n%s", text(br.After))
	}
	// the Local Networks now say networks, and leave the isolated one out
	lan := f.Policy["lan"].After
	if got := text(lan); !strings.Contains(got, "Allow to the other networks that are not isolated: Remote access (set on Local Networks and VPN)") || strings.Contains(got, "Branch") {
		t.Fatalf("lan:\n%s", got)
	}
	// a rule can be made from a VPN to the Local Network
	rec := e.addRule(`{"name":"Printer","from":"vpn-ra","action":"allow","protocol":"tcp","to":"lan","destination":"192.168.1.50","ports":"631"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if r := decodeInto[FirewallRule](t, rec); r.Text == "" || !strings.Contains(r.Text, "Remote access") {
		t.Fatalf("%+v", r)
	}
	// switched off, the tunnel is not a tab
	e.site(http.MethodPut, "s1", `{"enabled":false}`)
	if f := e.firewall(); len(f.Tabs) != 3 {
		t.Fatalf("%+v", f.Tabs)
	}
}

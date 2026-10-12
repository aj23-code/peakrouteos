package netapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/wg"
)

// The VPN API: site-to-site tunnels, the remote access server and its clients, all with WireGuard. Everything is staged like the other
// editors. Keys: this router's private keys are made here and never sent back; a client's private key is made here too, shown once in
// the file the client imports, and not kept. docs/vpn.md has the model.

// VPNPeerStatus is how a peer of a running tunnel is doing (only for a tunnel as it is running).
type VPNPeerStatus struct {
	State     string `json:"state"`     // "connected" (a handshake in the last three minutes), "waiting" (none yet) or "idle" (an old one)
	Handshake int64  `json:"handshake"` // Unix milliseconds, 0: never
	Endpoint  string `json:"endpoint"`  // where it was last heard from
	Received  uint64 `json:"received"`
	Sent      uint64 `json:"sent"`
}

// VPNSiteView is a site tunnel as the page shows it. Keys: ours is public, the other's is public, the secret ones are never here.
type VPNSiteView struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	Engine         string         `json:"engine"`
	Enabled        bool           `json:"enabled"`
	ListenPort     int            `json:"listen_port"`
	Address        string         `json:"address"`
	OtherAddress   string         `json:"other_address"` // what the other router uses inside the tunnel
	Endpoint       string         `json:"endpoint"`
	Keepalive      int            `json:"keepalive"`
	RemoteNetworks []string       `json:"remote_networks"`
	Internet       bool           `json:"internet"`
	Isolated       bool           `json:"isolated"`
	PublicKey      string         `json:"public_key"`      // this router's, for the other router
	PeerPublicKey  string         `json:"peer_public_key"` // the other router's
	PresharedSet   bool           `json:"preshared_set"`
	Text           string         `json:"text"`
	Pending        bool           `json:"pending"`
	Status         *VPNPeerStatus `json:"status"`

	// OpenVPN
	Role        string `json:"role,omitempty"`         // "server" or "client"
	Proto       string `json:"proto,omitempty"`        // "udp" or "tcp"
	Remote      string `json:"remote,omitempty"`       // where the tunnel is made (see config.OVSite)
	CertExpires int64  `json:"cert_expires,omitempty"` // Unix milliseconds: when the certificate a client tunnel uses runs out
}

// VPNClientView is a remote access client.
type VPNClientView struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	PublicKey    string         `json:"public_key"`
	Address      string         `json:"address"`
	Enabled      bool           `json:"enabled"`
	PresharedSet bool           `json:"preshared_set"`
	Text         string         `json:"text"`
	Pending      bool           `json:"pending"`
	Status       *VPNPeerStatus `json:"status"`
}

// VPNRemoteView is the remote access server.
type VPNRemoteView struct {
	Enabled       bool            `json:"enabled"`
	ListenPort    int             `json:"listen_port"`
	Network       string          `json:"network"`
	ServerAddress string          `json:"server_address"`
	Endpoint      string          `json:"endpoint"`
	FullTunnel    bool            `json:"full_tunnel"`
	Routes        []string        `json:"routes"`        // the ones chosen
	ClientRoutes  []string        `json:"client_routes"` // what clients get
	DNS           bool            `json:"dns"`
	Internet      bool            `json:"internet"`
	Isolated      bool            `json:"isolated"`
	PublicKey     string          `json:"public_key"`
	Text          string          `json:"text"`
	Pending       bool            `json:"pending"`
	Clients       []VPNClientView `json:"clients"`
}

// VPNResponse is the VPN page's data.
type VPNResponse struct {
	Sites      []VPNSiteView    `json:"sites"`
	Remote     *VPNRemoteView   `json:"remote"`    // null: no WireGuard remote access server yet
	OVRemote   *VPNOVRemoteView `json:"ov_remote"` // null: no OpenVPN remote access server yet
	CA         *VPNCAView       `json:"ca"`        // null: no certificate authority yet (it is made with the first OpenVPN server)
	WANAddress string           `json:"wan_address"`
	Suggest    VPNSuggest       `json:"suggest"`
	// LocalNetworks are this router's own networks: what the other router of a tunnel must be told to send here.
	LocalNetworks []VPNLocal `json:"local_networks"`
	Editable      bool       `json:"editable"`
}

// VPNLocal is one of this router's Local Networks.
type VPNLocal struct {
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
}

// VPNSuggest are the values the page offers for a new tunnel.
type VPNSuggest struct {
	SitePort      int    `json:"site_port"`
	SiteAddress   string `json:"site_address"`
	RemotePort    int    `json:"remote_port"`
	RemoteNetwork string `json:"remote_network"`

	OVSitePort      int    `json:"ov_site_port"`
	OVSiteAddress   string `json:"ov_site_address"`
	OVRemotePort    int    `json:"ov_remote_port"`
	OVRemoteNetwork string `json:"ov_remote_network"`
}

const vpnHandshakeFresh = 3 * time.Minute

func peerStatus(p wg.Peer) *VPNPeerStatus {
	st := &VPNPeerStatus{State: "waiting", Endpoint: p.Endpoint, Received: p.Received, Sent: p.Sent}
	if !p.LatestHandshake.IsZero() {
		st.Handshake = p.LatestHandshake.UnixMilli()
		st.State = "idle"
		if time.Since(p.LatestHandshake) < vpnHandshakeFresh {
			st.State = "connected"
		}
	}
	return st
}

func (a *API) wgInterface(name string) (wg.Interface, bool) {
	if a.WireGuard == nil {
		return wg.Interface{}, false
	}
	for _, i := range a.WireGuard() {
		if i.Name == name {
			return i, true
		}
	}
	return wg.Interface{}, false
}

func publicOf(private string) string {
	p, _ := wg.PublicKey(private)
	return p
}

func (a *API) siteView(s config.VPNSite, active config.NetworkConfig) VPNSiteView {
	old, had := active.VPNSettings().FindSite(s.ID)
	pending := !had || !reflect.DeepEqual(old, s)
	other, _ := config.OtherSideAddress(s.Address)
	v := VPNSiteView{ID: s.ID, Name: strings.TrimSpace(s.Name), Engine: s.Engine, Enabled: !s.Disabled, ListenPort: s.ListenPort, Address: s.Address, OtherAddress: other,
		Endpoint: s.Endpoint, Keepalive: s.Keepalive, RemoteNetworks: append([]string{}, s.RemoteNetworks...), Internet: s.Internet, Isolated: s.Isolated,
		PublicKey: publicOf(s.PrivateKey), PeerPublicKey: s.PeerPublicKey, PresharedSet: s.PresharedKey != "", Text: config.SiteText(s), Pending: pending}
	if s.Engine == config.EngineOpenVPN {
		a.ovSiteExtras(&v, s, pending)
		return v
	}
	if !pending && !s.Disabled {
		if i, ok := a.wgInterface(config.SiteInterface(s.ID)); ok {
			if len(i.Peers) > 0 {
				v.Status = peerStatus(i.Peers[0])
			} else {
				v.Status = &VPNPeerStatus{State: "waiting"}
			}
		}
	}
	return v
}

// sameRemote compares the server itself, not its clients (they are compared one by one).
func sameRemote(a, b config.WGRemote) bool {
	a.Clients, b.Clients = nil, nil
	return reflect.DeepEqual(a, b)
}

func (a *API) remoteView(r config.WGRemote, cfg config.NetworkConfig, active config.NetworkConfig) VPNRemoteView {
	oldR := active.VPNSettings().WGRemote
	serverAddr := ""
	if s, ok := r.ServerAddress(); ok {
		serverAddr = s.String()
	}
	pending := oldR == nil || !sameRemote(*oldR, r)
	v := VPNRemoteView{Enabled: !r.Disabled, ListenPort: r.ListenPort, Network: r.Network, ServerAddress: serverAddr, Endpoint: r.Endpoint, FullTunnel: r.FullTunnel,
		Routes: append([]string{}, r.Routes...), ClientRoutes: cfg.ClientRoutes(), DNS: r.DNS, Internet: r.Internet, Isolated: r.Isolated, PublicKey: publicOf(r.PrivateKey),
		Text: config.RemoteText(r), Pending: pending, Clients: []VPNClientView{}}
	var live wg.Interface
	var haveLive bool
	if !pending && !r.Disabled {
		live, haveLive = a.wgInterface(config.RemoteInterface())
	}
	for _, cl := range r.Clients {
		var oldC config.VPNClient
		var had bool
		if oldR != nil {
			oldC, had = oldR.FindClient(cl.ID)
		}
		cv := VPNClientView{ID: cl.ID, Name: strings.TrimSpace(cl.Name), PublicKey: cl.PublicKey, Address: cl.Address, Enabled: !cl.Disabled, PresharedSet: cl.PresharedKey != "",
			Text: config.ClientText(cl), Pending: !had || oldC != cl}
		if haveLive && !cv.Pending && !cl.Disabled {
			cv.Status = &VPNPeerStatus{State: "waiting"}
			for _, p := range live.Peers {
				if p.PublicKey == cl.PublicKey {
					cv.Status = peerStatus(p)
				}
			}
		}
		v.Clients = append(v.Clients, cv)
	}
	return v
}

func (a *API) vpnView() VPNResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	d := draft.VPNSettings()
	resp := VPNResponse{Sites: []VPNSiteView{}, Editable: a.editable()}
	for _, s := range d.Sites {
		resp.Sites = append(resp.Sites, a.siteView(s, active))
	}
	if d.WGRemote != nil {
		v := a.remoteView(*d.WGRemote, draft, active)
		resp.Remote = &v
	}
	a.ovViews(&resp, d, draft, active)
	resp.LocalNetworks = []VPNLocal{}
	for _, t := range firewallTabs(draft) {
		if t.Kind != "network" {
			continue
		}
		if p, ok := draft.NetworkPrefix(t.ID); ok {
			resp.LocalNetworks = append(resp.LocalNetworks, VPNLocal{Name: t.Name, Prefix: p.String()})
		}
	}
	resp.Suggest = VPNSuggest{SitePort: d.NextSitePort(), RemotePort: config.DefaultWGRAPort, RemoteNetwork: "10.8.0.0/24",
		OVSitePort: d.NextOVSitePort(), OVRemotePort: config.DefaultOVRAPort, OVRemoteNetwork: "10.9.0.0/24"}
	resp.Suggest.SiteAddress = freeTunnelAddress(d, "10.99.%d.1/30")
	resp.Suggest.OVSiteAddress = freeTunnelAddress(d, "10.98.%d.1/29")
	if a.Live != nil {
		if live := a.Live(active); live != nil && live.Address != "" {
			resp.WANAddress, _, _ = strings.Cut(live.Address, "/")
		}
	}
	return resp
}

// freeTunnelAddress is the first address of the form that no tunnel network overlaps.
func freeTunnelAddress(d config.VPNConfig, format string) string {
	for n := 1; n < 250; n++ {
		addr := fmt.Sprintf(format, n)
		q, _ := netip.ParsePrefix(addr)
		taken := false
		for _, s := range d.Sites {
			if p, err := netip.ParsePrefix(s.Address); err == nil && p.Masked().Overlaps(q.Masked()) {
				taken = true
			}
		}
		if !taken {
			return addr
		}
	}
	return ""
}

// HandleVPN: GET /api/v1/vpn returns the VPN settings (the draft) with how each tunnel and client is going.
func (a *API) HandleVPN(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, a.vpnView())
	}
}

func tidyVPN(c *config.NetworkConfig) {
	if v := c.VPN; v != nil && len(v.Sites) == 0 && v.WGRemote == nil && v.OVRemote == nil && v.CA == nil {
		c.VPN = nil
	}
}

// vpnErrors keeps the validation problems of the VPN settings: the ones under the prefix (the item being changed) with the API's field
// names, and any others (a key or address that clashes is reported on the later of the two items) as a general problem that says where.
func vpnErrors(err error, prefix string) error {
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out config.ValidationError
	for _, e := range ve {
		switch {
		case strings.HasPrefix(e.Field, prefix):
			out = append(out, config.FieldError{Field: strings.TrimPrefix(e.Field, prefix), Message: e.Message})
		case e.Field == "vpn" || strings.HasPrefix(e.Field, "vpn.") || strings.HasPrefix(e.Field, "vpn["):
			out = append(out, config.FieldError{Field: "list", Message: strings.TrimPrefix(e.Field, "vpn.") + ": " + e.Message})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cleanList(in *[]string) []string {
	var out []string
	if in == nil {
		return out
	}
	for _, s := range *in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ---- site tunnels ---------------------------------------------------------------------------------------------

type vpnSiteRequest struct {
	Name           *string   `json:"name"`
	ListenPort     *int      `json:"listen_port"`
	Address        *string   `json:"address"`
	PeerPublicKey  *string   `json:"peer_public_key"`
	PresharedKey   *string   `json:"preshared_key"` // a key to use, or "generate"; left out: unchanged
	Endpoint       *string   `json:"endpoint"`
	Keepalive      *int      `json:"keepalive"`
	RemoteNetworks *[]string `json:"remote_networks"`
	Internet       *bool     `json:"internet"`
	Isolated       *bool     `json:"isolated"`
	Enabled        *bool     `json:"enabled"`
	NewKey         bool      `json:"new_key"` // make a new private key for this tunnel

	// OpenVPN tunnels
	Engine  *string `json:"engine"`   // "wireguard" (the default) or "openvpn"
	Role    *string `json:"role"`     // openvpn: "server" (this router makes the file for the other) or "client" (it imports one)
	Proto   *string `json:"proto"`    // openvpn: udp or tcp
	Remote  *string `json:"remote"`   // openvpn: where the tunnel is made (see docs/vpn.md)
	File    *string `json:"file"`     // openvpn client: the file from the other router
	NewFile bool    `json:"new_file"` // openvpn server: a new certificate for the other router (answers with the file)
}

// ovOnly says whether a request names settings that only OpenVPN tunnels have.
func (r vpnSiteRequest) ovOnly() bool {
	return r.Role != nil || r.Proto != nil || r.Remote != nil || r.File != nil || r.NewFile
}

// wgOnly says whether a request names settings that only WireGuard tunnels have.
func (r vpnSiteRequest) wgOnly() bool {
	return r.PeerPublicKey != nil || r.PresharedKey != nil || r.Endpoint != nil || r.Keepalive != nil || r.NewKey
}

func refuseWGSettings(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, errorBody{Error: "Those settings are for WireGuard tunnels, not for an OpenVPN tunnel."})
}

func refuseOVSettings(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, errorBody{Error: "Those settings are for OpenVPN tunnels, not for a WireGuard tunnel."})
}

func applySiteRequest(s *config.VPNSite, req vpnSiteRequest) error {
	if req.Name != nil {
		s.Name = trim(req.Name)
	}
	if req.ListenPort != nil {
		s.ListenPort = *req.ListenPort
	}
	if req.Address != nil {
		s.Address = trim(req.Address)
	}
	if req.PeerPublicKey != nil {
		s.PeerPublicKey = trim(req.PeerPublicKey)
	}
	if req.PresharedKey != nil {
		switch k := trim(req.PresharedKey); k {
		case "":
		case "generate":
			psk, err := wg.GeneratePresharedKey()
			if err != nil {
				return err
			}
			s.PresharedKey = psk
		default:
			s.PresharedKey = k
		}
	}
	if req.Endpoint != nil {
		s.Endpoint = strings.ToLower(trim(req.Endpoint))
	}
	if req.Keepalive != nil {
		s.Keepalive = *req.Keepalive
	}
	if req.RemoteNetworks != nil {
		s.RemoteNetworks = cleanList(req.RemoteNetworks)
	}
	if req.Internet != nil {
		s.Internet = *req.Internet
	}
	if req.Isolated != nil {
		s.Isolated = *req.Isolated
	}
	if req.Enabled != nil {
		s.Disabled = !*req.Enabled
	}
	if req.NewKey {
		k, err := wg.GeneratePrivateKey()
		if err != nil {
			return err
		}
		s.PrivateKey = k
	}
	return nil
}

// HandleVPNSites: POST /api/v1/vpn/sites adds a site tunnel. This router's key is made here; the answer shows the public key to give
// to the other router.
func (a *API) HandleVPNSites(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	var req vpnSiteRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Engine != nil {
		switch trim(req.Engine) {
		case config.EngineOpenVPN:
			a.addOVSite(w, req)
			return
		case config.EngineWireGuard, "":
		default:
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "The engine must be wireguard or openvpn.", Errors: []config.FieldError{{Field: "engine", Message: "must be wireguard or openvpn"}}})
			return
		}
	}
	if req.ovOnly() {
		refuseOVSettings(w)
		return
	}
	priv, err := wg.GeneratePrivateKey()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "A key could not be made."})
		return
	}
	var id string
	var applyErr error
	err = a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.VPN == nil {
			c.VPN = &config.VPNConfig{}
		}
		v := c.VPN
		id = nextID("s", func(id string) bool { _, ok := v.FindSite(id); return ok })
		s := config.VPNSite{ID: id, Engine: config.EngineWireGuard, PrivateKey: priv, ListenPort: v.NextSitePort()}
		if applyErr = applySiteRequest(&s, req); applyErr != nil {
			return nil
		}
		if s.Endpoint != "" && s.Keepalive == 0 && (req.Keepalive == nil) {
			s.Keepalive = config.DefaultKeepalive
		}
		v.Sites = append(v.Sites, s)
		return vpnErrors(c.Validate(), fmt.Sprintf("vpn.sites[%d].", len(v.Sites)-1))
	})
	if applyErr != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "A key could not be made."})
		return
	}
	var body any
	if err == nil {
		draft, _ := a.Manager.StagedNetworkConfig()
		s, _ := draft.VPNSettings().FindSite(id)
		body = a.siteView(s, a.Manager.NetworkConfig())
	}
	a.answerStaged(w, err, http.StatusCreated, "This tunnel cannot be added: ", body)
}

// HandleVPNSite: PUT /api/v1/vpn/sites/{id} changes a tunnel (a field left out stays; "new_key" makes a new key for this router);
// DELETE removes it.
func (a *API) HandleVPNSite(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	site, found := draft.VPNSettings().FindSite(id)
	if !found {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such tunnel"})
		return
	}
	if !a.requireEditable(w) {
		return
	}
	if site.Engine == config.EngineOpenVPN {
		a.changeOVSite(w, r, id, site)
		return
	}
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			kept := c.VPN.Sites[:0:0]
			for _, s := range c.VPN.Sites {
				if s.ID != id {
					kept = append(kept, s)
				}
			}
			c.VPN.Sites = kept
			tidyVPN(c)
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.vpnView())
		return
	}
	var req vpnSiteRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ovOnly() || (req.Engine != nil && trim(req.Engine) != config.EngineWireGuard) {
		refuseOVSettings(w)
		return
	}
	var applyErr error
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		for i := range c.VPN.Sites {
			if c.VPN.Sites[i].ID != id {
				continue
			}
			if applyErr = applySiteRequest(&c.VPN.Sites[i], req); applyErr != nil {
				return nil
			}
			return vpnErrors(c.Validate(), fmt.Sprintf("vpn.sites[%d].", i))
		}
		return nil
	})
	if applyErr != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "A key could not be made."})
		return
	}
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		s, _ := now.VPNSettings().FindSite(id)
		body = a.siteView(s, a.Manager.NetworkConfig())
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

// ---- remote access ---------------------------------------------------------------------------------------------

type vpnRemoteRequest struct {
	ListenPort *int      `json:"listen_port"`
	Network    *string   `json:"network"`
	Endpoint   *string   `json:"endpoint"`
	FullTunnel *bool     `json:"full_tunnel"`
	Routes     *[]string `json:"routes"`
	DNS        *bool     `json:"dns"`
	Internet   *bool     `json:"internet"`
	Isolated   *bool     `json:"isolated"`
	Enabled    *bool     `json:"enabled"`
	NewKey     bool      `json:"new_key"` // a new key for the server: every client must then import a new file
}

func applyRemoteRequest(r *config.WGRemote, req vpnRemoteRequest) error {
	if req.ListenPort != nil {
		r.ListenPort = *req.ListenPort
	}
	if req.Network != nil {
		r.Network = trim(req.Network)
	}
	if req.Endpoint != nil {
		r.Endpoint = strings.ToLower(trim(req.Endpoint))
	}
	if req.FullTunnel != nil {
		r.FullTunnel = *req.FullTunnel
	}
	if req.Routes != nil {
		r.Routes = cleanList(req.Routes)
	}
	if req.DNS != nil {
		r.DNS = *req.DNS
	}
	if req.Internet != nil {
		r.Internet = *req.Internet
	}
	if req.Isolated != nil {
		r.Isolated = *req.Isolated
	}
	if req.Enabled != nil {
		r.Disabled = !*req.Enabled
	}
	if req.NewKey || r.PrivateKey == "" {
		k, err := wg.GeneratePrivateKey()
		if err != nil {
			return err
		}
		r.PrivateKey = k
	}
	return nil
}

// HandleVPNRemote: PUT /api/v1/vpn/remote sets up or changes the remote access server (the first time it makes the server's key, and
// starts from the usual port and network; a field left out stays); DELETE removes it with all its clients.
func (a *API) HandleVPNRemote(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) || !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		draft, _ := a.Manager.StagedNetworkConfig()
		if draft.VPNSettings().WGRemote == nil {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no remote access server"})
			return
		}
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			c.VPN.WGRemote = nil
			tidyVPN(c)
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.vpnView())
		return
	}
	var req vpnRemoteRequest
	if !decode(w, r, &req) {
		return
	}
	var applyErr error
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.VPN == nil {
			c.VPN = &config.VPNConfig{}
		}
		if c.VPN.WGRemote == nil {
			c.VPN.WGRemote = &config.WGRemote{ListenPort: config.DefaultWGRAPort, Network: "10.8.0.0/24"}
		}
		if applyErr = applyRemoteRequest(c.VPN.WGRemote, req); applyErr != nil {
			return nil
		}
		return vpnErrors(c.Validate(), "vpn.wg_remote.")
	})
	if applyErr != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "A key could not be made."})
		return
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", a.vpnView())
}

// ---- clients ---------------------------------------------------------------------------------------------------

type vpnClientRequest struct {
	Name       *string `json:"name"`
	PublicKey  *string `json:"public_key"` // a key from the device itself; left out when the router makes one
	Preshared  *bool   `json:"preshared"`  // add (true) or drop (false) an extra shared secret
	Enabled    *bool   `json:"enabled"`
	NewKeyPair bool    `json:"new_key"` // make a new key pair for the client (it must import the new file)
}

// VPNClientFile is what the router gives for a client whose key it made: the file to import, once.
type VPNClientFile struct {
	Filename string `json:"filename"`
	Config   string `json:"config"`
}

type vpnClientResponse struct {
	Client VPNClientView  `json:"client"`
	File   *VPNClientFile `json:"file"` // null when the key is the device's own: the router does not know its private key
}

func fileName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "client"
	}
	return "peakroute-" + s + ".conf"
}

// clientFile writes the file a client imports. private is the client's private key ("" when it is the device's own and unknown here:
// the file then says where to put it).
func clientFile(cfg config.NetworkConfig, cl config.VPNClient, private string) (*VPNClientFile, string) {
	r := cfg.VPNSettings().WGRemote
	if r == nil {
		return nil, "There is no remote access server."
	}
	if r.ClientEndpoint() == "" {
		return nil, "Set the address clients connect to (a name or an address of this router that can be reached from the internet) first."
	}
	dns := ""
	if r.DNS {
		if s, ok := r.ServerAddress(); ok {
			dns = s.String()
		}
	}
	if private == "" {
		private = "<paste the private key of the device here>"
	}
	conf := wg.ClientConfig(wg.ClientParams{Name: strings.TrimSpace(cl.Name), PrivateKey: private, Address: cl.Address, DNS: dns, ServerPublicKey: publicOf(r.PrivateKey),
		PresharedKey: cl.PresharedKey, AllowedIPs: cfg.ClientRoutes(), Endpoint: r.ClientEndpoint(), Keepalive: config.DefaultKeepalive})
	return &VPNClientFile{Filename: fileName(cl.Name), Config: conf}, ""
}

// HandleVPNClients: POST /api/v1/vpn/remote/clients adds a client. Without a public_key the router makes the key pair, and the answer
// holds the file to import (with the private key, shown this once and not kept).
func (a *API) HandleVPNClients(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	var req vpnClientRequest
	if !decode(w, r, &req) {
		return
	}
	draft, _ := a.Manager.StagedNetworkConfig()
	rem := draft.VPNSettings().WGRemote
	if rem == nil {
		writeJSON(w, http.StatusConflict, errorBody{Error: "Set up the remote access server first."})
		return
	}
	private := ""
	pub := ""
	if req.PublicKey != nil && trim(req.PublicKey) != "" {
		pub = trim(req.PublicKey)
	} else {
		var err error
		if private, pub, err = wg.GenerateKeyPair(); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "A key could not be made."})
			return
		}
	}
	addr, ok := rem.NextClientAddress()
	if !ok {
		writeJSON(w, http.StatusConflict, errorBody{Error: "The remote access network has no free address left."})
		return
	}
	psk := ""
	if req.Preshared != nil && *req.Preshared {
		var err error
		if psk, err = wg.GeneratePresharedKey(); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "A key could not be made."})
			return
		}
	}
	if private != "" && rem.ClientEndpoint() == "" {
		// the key pair is made here and its file is the only place the private key will ever be: without an address to connect to the file is no use
		msg := "Set the address clients connect to (a name or an address of this router that can be reached from the internet) first."
		writeJSON(w, http.StatusBadRequest, errorBody{Error: msg, Errors: []config.FieldError{{Field: "endpoint", Message: msg}}})
		return
	}
	var id string
	var cl config.VPNClient
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		list := c.VPN.WGRemote
		id = nextID("c", func(id string) bool { _, ok := list.FindClient(id); return ok })
		cl = config.VPNClient{ID: id, PublicKey: pub, Address: addr, PresharedKey: psk}
		if req.Name != nil {
			cl.Name = trim(req.Name)
		}
		if req.Enabled != nil {
			cl.Disabled = !*req.Enabled
		}
		list.Clients = append(list.Clients, cl)
		return vpnErrors(c.Validate(), fmt.Sprintf("vpn.wg_remote.clients[%d].", len(list.Clients)-1))
	})
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		saved, _ := now.VPNSettings().WGRemote.FindClient(id)
		resp := vpnClientResponse{Client: a.remoteView(*now.VPNSettings().WGRemote, now, a.Manager.NetworkConfig()).clientByID(id)}
		if private != "" {
			resp.File, _ = clientFile(now, saved, private)
		}
		body = resp
	}
	a.answerStaged(w, err, http.StatusCreated, "This client cannot be added: ", body)
}

func (v VPNRemoteView) clientByID(id string) VPNClientView {
	for _, c := range v.Clients {
		if c.ID == id {
			return c
		}
	}
	return VPNClientView{}
}

// HandleVPNClient: PUT /api/v1/vpn/remote/clients/{id} renames or switches a client off or on, or (with new_key) makes it a new key pair
// and answers with the file to import; DELETE removes it.
func (a *API) HandleVPNClient(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	rem := draft.VPNSettings().WGRemote
	if rem == nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such client"})
		return
	}
	if _, ok := rem.FindClient(id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such client"})
		return
	}
	if !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			kept := c.VPN.WGRemote.Clients[:0:0]
			for _, cl := range c.VPN.WGRemote.Clients {
				if cl.ID != id {
					kept = append(kept, cl)
				}
			}
			c.VPN.WGRemote.Clients = kept
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.vpnView())
		return
	}
	var req vpnClientRequest
	if !decode(w, r, &req) {
		return
	}
	private := ""
	newPub := ""
	if req.NewKeyPair {
		var err error
		if private, newPub, err = wg.GenerateKeyPair(); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "A key could not be made."})
			return
		}
	} else if req.PublicKey != nil && trim(req.PublicKey) != "" {
		newPub = trim(req.PublicKey)
	}
	psk := ""
	if req.Preshared != nil && *req.Preshared {
		var err error
		if psk, err = wg.GeneratePresharedKey(); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody{Error: "A key could not be made."})
			return
		}
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		for i := range c.VPN.WGRemote.Clients {
			cl := &c.VPN.WGRemote.Clients[i]
			if cl.ID != id {
				continue
			}
			if req.Name != nil {
				cl.Name = trim(req.Name)
			}
			if req.Enabled != nil {
				cl.Disabled = !*req.Enabled
			}
			if newPub != "" {
				cl.PublicKey = newPub
			}
			if req.Preshared != nil {
				cl.PresharedKey = psk // true: a new one; false: none
			}
			return vpnErrors(c.Validate(), fmt.Sprintf("vpn.wg_remote.clients[%d].", i))
		}
		return nil
	})
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		saved, _ := now.VPNSettings().WGRemote.FindClient(id)
		resp := vpnClientResponse{Client: a.remoteView(*now.VPNSettings().WGRemote, now, a.Manager.NetworkConfig()).clientByID(id)}
		if private != "" {
			if f, _ := clientFile(now, saved, private); f != nil {
				resp.File = f
			}
		}
		body = resp
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

// HandleVPNClientTemplate: GET /api/v1/vpn/remote/clients/{id}/file answers with the file for a client whose key is its device's own: the
// private key is the one thing the router does not have, so the file says where to put it.
func (a *API) HandleVPNClientTemplate(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	draft, _ := a.Manager.StagedNetworkConfig()
	rem := draft.VPNSettings().WGRemote
	var cl config.VPNClient
	var ok bool
	if rem != nil {
		cl, ok = rem.FindClient(r.PathValue("id"))
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such client"})
		return
	}
	f, problem := clientFile(draft, cl, "")
	if f == nil {
		writeJSON(w, http.StatusConflict, errorBody{Error: problem})
		return
	}
	writeJSON(w, http.StatusOK, f)
}

package netapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/ovpn"
	"peakroute/internal/pki"
)

// The OpenVPN half of the VPN API: the certificate authority that is made with the first OpenVPN server, the remote access server
// with its devices, and OpenVPN site tunnels (server or client). Like WireGuard's, everything is staged. A device's (or a peer
// router's) private key is made here, shown once in the file that is imported, and not kept: only the serial number of its certificate
// is, so that the certificate can be revoked when the device is removed. docs/vpn.md, sections 9 to 11.

// ---- views ----------------------------------------------------------------------------------------------------

const certWarnDays = 60

// VPNOVClientView is an OpenVPN device.
type VPNOVClientView struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Address  string         `json:"address"`
	Enabled  bool           `json:"enabled"`
	NotAfter int64          `json:"not_after,omitempty"` // Unix milliseconds: when its certificate runs out
	Expiring bool           `json:"expiring"`            // within 60 days, or already
	Text     string         `json:"text"`
	Pending  bool           `json:"pending"`
	Status   *VPNPeerStatus `json:"status"`
}

// VPNOVRemoteView is the OpenVPN remote access server.
type VPNOVRemoteView struct {
	Enabled       bool              `json:"enabled"`
	ListenPort    int               `json:"listen_port"`
	Proto         string            `json:"proto"`
	Network       string            `json:"network"`
	ServerAddress string            `json:"server_address"`
	Endpoint      string            `json:"endpoint"`
	FullTunnel    bool              `json:"full_tunnel"`
	Routes        []string          `json:"routes"`
	ClientRoutes  []string          `json:"client_routes"`
	DNS           bool              `json:"dns"`
	Internet      bool              `json:"internet"`
	Isolated      bool              `json:"isolated"`
	Text          string            `json:"text"`
	Pending       bool              `json:"pending"`
	Clients       []VPNOVClientView `json:"clients"`
}

// VPNCAView is the certificate authority, as far as it is shown.
type VPNCAView struct {
	Subject        string `json:"subject"`
	NotAfter       int64  `json:"not_after"` // Unix milliseconds
	ServerNotAfter int64  `json:"server_not_after"`
	Revoked        int    `json:"revoked"` // certificates that are no longer accepted
}

func ms64(t time.Time) int64 { return t.UnixMilli() }

func expiring(t time.Time) bool { return time.Until(t) < certWarnDays*24*time.Hour }

// ovStatus is how a connection of a server is going: connected (with where from and how much), or not.
func (a *API) ovConnStatus(server, cn string) *VPNPeerStatus {
	if a.OVConnections == nil {
		return nil
	}
	list, running := a.OVConnections(server)
	if !running {
		return nil
	}
	for _, c := range list {
		if c.CommonName == cn {
			st := &VPNPeerStatus{State: "connected", Endpoint: c.RealAddress, Received: c.BytesReceived, Sent: c.BytesSent}
			if !c.Since.IsZero() {
				st.Handshake = c.Since.UnixMilli()
			}
			return st
		}
	}
	return &VPNPeerStatus{State: "offline"}
}

// ovSiteExtras fills in what an OpenVPN site tunnel has of its own.
func (a *API) ovSiteExtras(v *VPNSiteView, s config.VPNSite, pending bool) {
	o := s.OV
	if o == nil {
		return
	}
	v.Role, v.Proto, v.Remote = o.Role, o.Proto, o.Remote
	if o.Role == config.OVRoleClient && o.Cert != "" {
		if info, err := pki.Describe(o.Cert); err == nil {
			v.CertExpires = ms64(info.NotAfter)
		}
	}
	if pending || s.Disabled {
		return
	}
	switch o.Role {
	case config.OVRoleServer:
		v.Status = a.ovConnStatus(config.OVSiteUnit(s.ID), config.OVPeerCN(s.ID))
	case config.OVRoleClient:
		if a.LinkUp != nil {
			v.Status = &VPNPeerStatus{State: "offline"}
			if a.LinkUp(s.Interface()) {
				v.Status = &VPNPeerStatus{State: "connected"}
			}
		}
	}
}

func (a *API) ovRemoteView(r config.OVRemote, cfg, active config.NetworkConfig) VPNOVRemoteView {
	oldR := active.VPNSettings().OVRemote
	pending := oldR == nil || !sameOVRemote(*oldR, r)
	serverAddr := ""
	if s, ok := r.ServerAddress(); ok {
		serverAddr = s.String()
	}
	v := VPNOVRemoteView{Enabled: !r.Disabled, ListenPort: r.ListenPort, Proto: r.Proto, Network: r.Network, ServerAddress: serverAddr, Endpoint: r.Endpoint,
		FullTunnel: r.FullTunnel, Routes: append([]string{}, r.Routes...), ClientRoutes: cfg.OVClientRoutes(), DNS: r.DNS, Internet: r.Internet, Isolated: r.Isolated,
		Text: config.OVRemoteText(r), Pending: pending, Clients: []VPNOVClientView{}}
	for _, cl := range r.Clients {
		var oldC config.OVClient
		var had bool
		if oldR != nil {
			oldC, had = oldR.FindClient(cl.ID)
		}
		cv := VPNOVClientView{ID: cl.ID, Name: strings.TrimSpace(cl.Name), Address: cl.Address, Enabled: !cl.Disabled, Text: config.OVClientText(cl), Pending: !had || oldC != cl}
		if cl.NotAfter > 0 {
			na := time.Unix(cl.NotAfter, 0)
			cv.NotAfter, cv.Expiring = ms64(na), expiring(na)
		}
		if !pending && !cv.Pending && !r.Disabled && !cl.Disabled {
			cv.Status = a.ovConnStatus(config.OVRemoteUnit(), config.OVClientCN(cl.ID))
		}
		v.Clients = append(v.Clients, cv)
	}
	return v
}

func sameOVRemote(a, b config.OVRemote) bool {
	a.Clients, b.Clients = nil, nil
	return reflect.DeepEqual(a, b)
}

func (v VPNOVRemoteView) clientByID(id string) VPNOVClientView {
	for _, c := range v.Clients {
		if c.ID == id {
			return c
		}
	}
	return VPNOVClientView{}
}

// ovViews adds the OpenVPN parts to the page's data.
func (a *API) ovViews(resp *VPNResponse, d config.VPNConfig, draft, active config.NetworkConfig) {
	if d.OVRemote != nil {
		v := a.ovRemoteView(*d.OVRemote, draft, active)
		resp.OVRemote = &v
	}
	if ca := d.CA; ca != nil {
		if info, err := pki.Describe(ca.Cert); err == nil {
			v := VPNCAView{Subject: info.CommonName, NotAfter: ms64(info.NotAfter), Revoked: len(ca.Revoked)}
			if si, err := pki.Describe(ca.ServerCert); err == nil {
				v.ServerNotAfter = ms64(si.NotAfter)
			}
			resp.CA = &v
		}
	}
}

// ---- the authority --------------------------------------------------------------------------------------------

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ensureCA makes the authority, its server certificate and an empty revocation list when there are none yet.
func ensureCA(v *config.VPNConfig) error {
	now := time.Now()
	if v.CA == nil || v.CA.Cert == "" {
		cert, key, err := pki.NewCA("PeakRoute OS VPN "+randomHex(3), now)
		if err != nil {
			return err
		}
		crl, err := pki.NewCRL(cert, key, nil, 1, now)
		if err != nil {
			return err
		}
		v.CA = &config.VPNCA{Cert: cert, Key: key, CRL: crl, CRLNumber: 1}
	}
	if v.CA.ServerCert == "" || v.CA.ServerKey == "" {
		cert, key, err := pki.Issue(v.CA.Cert, v.CA.Key, "peakroute-server", true, now)
		if err != nil {
			return err
		}
		v.CA.ServerCert, v.CA.ServerKey = cert, key
	}
	return nil
}

// issue makes a certificate (and its key) for a device or a peer router: the serial and when it runs out are what is kept.
func issue(v *config.VPNConfig, cn string) (cert, key, serial string, notAfter time.Time, err error) {
	cert, key, err = pki.Issue(v.CA.Cert, v.CA.Key, cn, false, time.Now())
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	info, err := pki.Describe(cert)
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	return cert, key, info.Serial, info.NotAfter, nil
}

// revoke puts a certificate on the revocation list (and makes the list again). A serial that is empty or already there is left alone.
func revoke(v *config.VPNConfig, serial, name string) error {
	if serial == "" || v.CA == nil {
		return nil
	}
	for _, r := range v.CA.Revoked {
		if r.Serial == serial {
			return nil
		}
	}
	now := time.Now()
	v.CA.Revoked = append(v.CA.Revoked, config.VPNRevoked{Serial: serial, Name: name, At: now.Unix()})
	list := make([]pki.Revoked, len(v.CA.Revoked))
	for i, r := range v.CA.Revoked {
		list[i] = pki.Revoked{Serial: r.Serial, At: time.Unix(r.At, 0)}
	}
	v.CA.CRLNumber++
	crl, err := pki.NewCRL(v.CA.Cert, v.CA.Key, list, v.CA.CRLNumber, now)
	if err != nil {
		return err
	}
	v.CA.CRL = crl
	return nil
}

// ---- files ------------------------------------------------------------------------------------------------------

func ovFileName(name string) string { return strings.TrimSuffix(fileName(name), ".conf") + ".ovpn" }

func hostPort(endpoint string) (string, int, bool) {
	i := strings.LastIndex(endpoint, ":")
	if i < 0 {
		return "", 0, false
	}
	port, err := strconv.Atoi(endpoint[i+1:])
	return endpoint[:i], port, err == nil && port > 0
}

// deviceFile is the file a device imports. key is "" for... nothing: the router always makes a device's keys, so the file always
// has the private key (it is shown once and not kept).
func deviceFile(v config.VPNConfig, r config.OVRemote, name, cert, key string) (*VPNClientFile, error) {
	host, port, ok := hostPort(r.ClientEndpoint())
	if !ok {
		return nil, errors.New("Set the address devices connect to (a name or an address of this router that can be reached from the internet) first.")
	}
	text, err := ovpn.Write(ovpn.Params{Name: strings.TrimSpace(name), Host: host, Port: port, Proto: r.Proto, CA: v.CA.Cert, Cert: cert, Key: key, TLSCrypt: r.TLSCrypt})
	if err != nil {
		return nil, err
	}
	return &VPNClientFile{Filename: ovFileName(name), Config: text}, nil
}

// peerFile is the file the other router of a site tunnel imports.
func (a *API) peerFile(cfg config.NetworkConfig, s config.VPNSite, cert, key string) (*VPNClientFile, error) {
	v := cfg.VPNSettings()
	host, port, ok := hostPort(s.OV.Remote)
	if !ok {
		return nil, errors.New("Set the address the other router connects to (a name or an address of this router that can be reached from the internet, and the port) first.")
	}
	other, _ := config.OtherSideAddress(s.Address)
	var nets []string
	for _, t := range firewallTabs(cfg) {
		if t.Kind != "network" {
			continue
		}
		if p, ok := cfg.NetworkPrefix(t.ID); ok {
			nets = append(nets, p.String())
		}
	}
	text, err := ovpn.Write(ovpn.Params{Name: strings.TrimSpace(s.Name), Host: host, Port: port, Proto: s.OV.Proto, CA: v.CA.Cert, Cert: cert, Key: key, TLSCrypt: s.OV.TLSCrypt,
		Tunnel: other, Networks: nets, RouteNoPull: true})
	if err != nil {
		return nil, err
	}
	return &VPNClientFile{Filename: ovFileName("tunnel " + s.Name), Config: text}, nil
}

// ---- OpenVPN site tunnels -------------------------------------------------------------------------------------

type vpnSiteWithFile struct {
	VPNSiteView
	File *VPNClientFile `json:"file,omitempty"` // for a server tunnel: the file the other router imports, shown once
}

func (a *API) siteAnswer(id string, file *VPNClientFile) vpnSiteWithFile {
	draft, _ := a.Manager.StagedNetworkConfig()
	s, _ := draft.VPNSettings().FindSite(id)
	return vpnSiteWithFile{VPNSiteView: a.siteView(s, a.Manager.NetworkConfig()), File: file}
}

// addOVSite adds an OpenVPN tunnel: as the server (the router makes the file for the other router) or as the client (the router
// imports the file the other router made).
func (a *API) addOVSite(w http.ResponseWriter, req vpnSiteRequest) {
	if req.wgOnly() {
		refuseWGSettings(w)
		return
	}
	role := config.OVRoleServer
	if req.Role != nil {
		role = trim(req.Role)
	}
	if role != config.OVRoleServer && role != config.OVRoleClient {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "The role must be server or client.", Errors: []config.FieldError{{Field: "role", Message: "must be server or client"}}})
		return
	}
	var imported ovpn.Parsed
	if role == config.OVRoleClient {
		if req.File == nil || strings.TrimSpace(*req.File) == "" {
			msg := "Import the file the other router made for this tunnel."
			writeJSON(w, http.StatusBadRequest, errorBody{Error: msg, Errors: []config.FieldError{{Field: "file", Message: msg}}})
			return
		}
		var err error
		if imported, err = ovpn.Read(*req.File); err != nil {
			msg := "That file cannot be used: " + err.Error()
			writeJSON(w, http.StatusBadRequest, errorBody{Error: msg, Errors: []config.FieldError{{Field: "file", Message: msg}}})
			return
		}
	}
	var id string
	var file *VPNClientFile
	var fileErr error
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.VPN == nil {
			c.VPN = &config.VPNConfig{}
		}
		v := c.VPN
		id = nextID("s", func(id string) bool { _, ok := v.FindSite(id); return ok })
		s := config.VPNSite{ID: id, Engine: config.EngineOpenVPN, OV: &config.OVSite{Role: role, Proto: "udp"}}
		switch role {
		case config.OVRoleServer:
			s.ListenPort = v.NextOVSitePort()
			s.Address = freeTunnelAddress(*v, "10.98.%d.1/29")
			tc, err := pki.NewTLSCryptKey()
			if err != nil {
				return err
			}
			s.OV.TLSCrypt = tc
		case config.OVRoleClient:
			s.OV.Proto, s.OV.Remote = imported.Proto, fmt.Sprintf("%s:%d", imported.Host, imported.Port)
			s.OV.CACert, s.OV.Cert, s.OV.Key, s.OV.TLSCrypt = imported.CA, imported.Cert, imported.Key, imported.TLSCrypt
			s.Address = imported.Tunnel
			s.RemoteNetworks = imported.Networks
		}
		applyOVSiteRequest(&s, req)
		var cert, key string
		if role == config.OVRoleServer {
			if err := ensureCA(v); err != nil {
				return err
			}
			var err error
			if cert, key, s.OV.PeerSerial, _, err = issue(v, config.OVPeerCN(s.ID)); err != nil {
				return err
			}
		}
		v.Sites = append(v.Sites, s)
		if err := vpnErrors(c.Validate(), fmt.Sprintf("vpn.sites[%d].", len(v.Sites)-1)); err != nil {
			return err
		}
		if role == config.OVRoleServer {
			if file, fileErr = a.peerFile(*c, s, cert, key); fileErr != nil {
				return fileErr
			}
		}
		return nil
	})
	if fileErr != nil && err != nil {
		msg := fileErr.Error()
		writeJSON(w, http.StatusBadRequest, errorBody{Error: msg, Errors: []config.FieldError{{Field: "remote", Message: msg}}})
		return
	}
	var body any
	if err == nil {
		body = a.siteAnswer(id, file)
	}
	a.answerStaged(w, err, http.StatusCreated, "This tunnel cannot be added: ", body)
}

// applyOVSiteRequest sets what a request names: the fields an OpenVPN tunnel has (and the ones every tunnel has).
func applyOVSiteRequest(s *config.VPNSite, req vpnSiteRequest) {
	if req.Name != nil {
		s.Name = trim(req.Name)
	}
	if req.ListenPort != nil {
		s.ListenPort = *req.ListenPort
	}
	if req.Address != nil {
		s.Address = trim(req.Address)
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
	if req.Proto != nil && s.OV != nil {
		s.OV.Proto = strings.ToLower(trim(req.Proto))
	}
	if req.Remote != nil && s.OV != nil {
		s.OV.Remote = strings.ToLower(trim(req.Remote))
	}
}

// changeOVSite handles PUT and DELETE of an OpenVPN site tunnel.
func (a *API) changeOVSite(w http.ResponseWriter, r *http.Request, id string, site config.VPNSite) {
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			v := c.VPN
			kept := v.Sites[:0:0]
			for _, s := range v.Sites {
				if s.ID != id {
					kept = append(kept, s)
					continue
				}
				if s.OV != nil && s.OV.Role == config.OVRoleServer {
					if err := revoke(v, s.OV.PeerSerial, "tunnel "+s.Name); err != nil {
						return err
					}
				}
			}
			v.Sites = kept
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
	if req.Engine != nil && trim(req.Engine) != config.EngineOpenVPN {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "The engine of a tunnel cannot be changed."})
		return
	}
	if req.wgOnly() {
		refuseWGSettings(w)
		return
	}
	var imported *ovpn.Parsed
	if req.File != nil && strings.TrimSpace(*req.File) != "" {
		if site.OV == nil || site.OV.Role != config.OVRoleClient {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "Only a tunnel that connects to another router takes a file."})
			return
		}
		p, err := ovpn.Read(*req.File)
		if err != nil {
			msg := "That file cannot be used: " + err.Error()
			writeJSON(w, http.StatusBadRequest, errorBody{Error: msg, Errors: []config.FieldError{{Field: "file", Message: msg}}})
			return
		}
		imported = &p
	}
	var file *VPNClientFile
	var fileErr error
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		v := c.VPN
		for i := range v.Sites {
			if v.Sites[i].ID != id {
				continue
			}
			s := &v.Sites[i]
			applyOVSiteRequest(s, req)
			if imported != nil {
				s.OV.Proto, s.OV.Remote = imported.Proto, fmt.Sprintf("%s:%d", imported.Host, imported.Port)
				s.OV.CACert, s.OV.Cert, s.OV.Key, s.OV.TLSCrypt = imported.CA, imported.Cert, imported.Key, imported.TLSCrypt
				if req.Address == nil && imported.Tunnel != "" {
					s.Address = imported.Tunnel
				}
			}
			var cert, key string
			if req.NewFile && s.OV.Role == config.OVRoleServer {
				// a new certificate for the other router: the old one stops being accepted when this is committed
				if err := revoke(v, s.OV.PeerSerial, "tunnel "+s.Name); err != nil {
					return err
				}
				var err error
				if cert, key, s.OV.PeerSerial, _, err = issue(v, config.OVPeerCN(s.ID)); err != nil {
					return err
				}
			}
			if err := vpnErrors(c.Validate(), fmt.Sprintf("vpn.sites[%d].", i)); err != nil {
				return err
			}
			if cert != "" {
				if file, fileErr = a.peerFile(*c, *s, cert, key); fileErr != nil {
					return fileErr
				}
			}
			return nil
		}
		return nil
	})
	if fileErr != nil && err != nil {
		msg := fileErr.Error()
		writeJSON(w, http.StatusBadRequest, errorBody{Error: msg, Errors: []config.FieldError{{Field: "remote", Message: msg}}})
		return
	}
	var body any
	if err == nil {
		body = a.siteAnswer(id, file)
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

// ---- OpenVPN remote access ------------------------------------------------------------------------------------

type vpnOVRemoteRequest struct {
	ListenPort *int      `json:"listen_port"`
	Proto      *string   `json:"proto"`
	Network    *string   `json:"network"`
	Endpoint   *string   `json:"endpoint"`
	FullTunnel *bool     `json:"full_tunnel"`
	Routes     *[]string `json:"routes"`
	DNS        *bool     `json:"dns"`
	Internet   *bool     `json:"internet"`
	Isolated   *bool     `json:"isolated"`
	Enabled    *bool     `json:"enabled"`
}

// HandleVPNOVRemote: PUT /api/v1/vpn/ov-remote sets up or changes the OpenVPN remote access server (the first time it also makes the
// certificate authority, when there is none, and the server's control channel key); DELETE removes it and revokes its devices.
func (a *API) HandleVPNOVRemote(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) || !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		draft, _ := a.Manager.StagedNetworkConfig()
		if draft.VPNSettings().OVRemote == nil {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no OpenVPN remote access server"})
			return
		}
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			v := c.VPN
			for _, cl := range v.OVRemote.Clients {
				if err := revoke(v, cl.Serial, "device "+cl.Name); err != nil {
					return err
				}
			}
			v.OVRemote = nil
			tidyVPN(c)
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.vpnView())
		return
	}
	var req vpnOVRemoteRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.VPN == nil {
			c.VPN = &config.VPNConfig{}
		}
		v := c.VPN
		if v.OVRemote == nil {
			tc, err := pki.NewTLSCryptKey()
			if err != nil {
				return err
			}
			v.OVRemote = &config.OVRemote{ListenPort: config.DefaultOVRAPort, Proto: "udp", Network: "10.9.0.0/24", TLSCrypt: tc}
			if err := ensureCA(v); err != nil {
				return err
			}
		}
		rm := v.OVRemote
		if req.ListenPort != nil {
			rm.ListenPort = *req.ListenPort
		}
		if req.Proto != nil {
			rm.Proto = strings.ToLower(trim(req.Proto))
		}
		if req.Network != nil {
			rm.Network = trim(req.Network)
		}
		if req.Endpoint != nil {
			rm.Endpoint = strings.ToLower(trim(req.Endpoint))
		}
		if req.FullTunnel != nil {
			rm.FullTunnel = *req.FullTunnel
		}
		if req.Routes != nil {
			rm.Routes = cleanList(req.Routes)
		}
		if req.DNS != nil {
			rm.DNS = *req.DNS
		}
		if req.Internet != nil {
			rm.Internet = *req.Internet
		}
		if req.Isolated != nil {
			rm.Isolated = *req.Isolated
		}
		if req.Enabled != nil {
			rm.Disabled = !*req.Enabled
		}
		return vpnErrors(c.Validate(), "vpn.ov_remote.")
	})
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", a.vpnView())
}

// ---- OpenVPN devices ------------------------------------------------------------------------------------------

type vpnOVClientRequest struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
	NewFile bool    `json:"new_file"` // a new certificate and key for the device (the old ones stop being accepted)
}

type vpnOVClientResponse struct {
	Client VPNOVClientView `json:"client"`
	File   *VPNClientFile  `json:"file"` // shown once, not kept
}

func (a *API) ovClientAnswer(id string, file *VPNClientFile) vpnOVClientResponse {
	now, _ := a.Manager.StagedNetworkConfig()
	return vpnOVClientResponse{Client: a.ovRemoteView(*now.VPNSettings().OVRemote, now, a.Manager.NetworkConfig()).clientByID(id), File: file}
}

// HandleVPNOVClients: POST /api/v1/vpn/ov-remote/clients adds a device: the router makes its certificate and key, and the answer has
// the file to import (with the private key, shown this once and not kept).
func (a *API) HandleVPNOVClients(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	var req vpnOVClientRequest
	if !decode(w, r, &req) {
		return
	}
	draft, _ := a.Manager.StagedNetworkConfig()
	rem := draft.VPNSettings().OVRemote
	if rem == nil {
		writeJSON(w, http.StatusConflict, errorBody{Error: "Set up the OpenVPN remote access server first."})
		return
	}
	if _, _, ok := hostPort(rem.ClientEndpoint()); !ok {
		msg := "Set the address devices connect to (a name or an address of this router that can be reached from the internet) first."
		writeJSON(w, http.StatusBadRequest, errorBody{Error: msg, Errors: []config.FieldError{{Field: "endpoint", Message: msg}}})
		return
	}
	addr, ok := rem.NextClientAddress()
	if !ok {
		writeJSON(w, http.StatusConflict, errorBody{Error: "The remote access network has no free address left."})
		return
	}
	var id string
	var file *VPNClientFile
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		v := c.VPN
		list := v.OVRemote
		id = nextID("c", func(id string) bool { _, ok := list.FindClient(id); return ok })
		cert, key, serial, notAfter, err := issue(v, config.OVClientCN(id))
		if err != nil {
			return err
		}
		cl := config.OVClient{ID: id, Serial: serial, NotAfter: notAfter.Unix(), Address: addr}
		if req.Name != nil {
			cl.Name = trim(req.Name)
		}
		if req.Enabled != nil {
			cl.Disabled = !*req.Enabled
		}
		list.Clients = append(list.Clients, cl)
		if err := vpnErrors(c.Validate(), fmt.Sprintf("vpn.ov_remote.clients[%d].", len(list.Clients)-1)); err != nil {
			return err
		}
		file, err = deviceFile(*v, *list, cl.Name, cert, key)
		return err
	})
	var body any
	if err == nil {
		body = a.ovClientAnswer(id, file)
	}
	a.answerStaged(w, err, http.StatusCreated, "This device cannot be added: ", body)
}

// HandleVPNOVClient: PUT /api/v1/vpn/ov-remote/clients/{id} renames or switches a device off or on, or (new_file) makes it a new
// certificate and answers with the file to import; DELETE removes it and revokes its certificate.
func (a *API) HandleVPNOVClient(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	rem := draft.VPNSettings().OVRemote
	if rem == nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such device"})
		return
	}
	if _, ok := rem.FindClient(id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such device"})
		return
	}
	if !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			v := c.VPN
			kept := v.OVRemote.Clients[:0:0]
			for _, cl := range v.OVRemote.Clients {
				if cl.ID != id {
					kept = append(kept, cl)
					continue
				}
				if err := revoke(v, cl.Serial, "device "+cl.Name); err != nil {
					return err
				}
			}
			v.OVRemote.Clients = kept
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.vpnView())
		return
	}
	var req vpnOVClientRequest
	if !decode(w, r, &req) {
		return
	}
	if req.NewFile {
		if _, _, ok := hostPort(rem.ClientEndpoint()); !ok {
			msg := "Set the address devices connect to (a name or an address of this router that can be reached from the internet) first."
			writeJSON(w, http.StatusBadRequest, errorBody{Error: msg, Errors: []config.FieldError{{Field: "endpoint", Message: msg}}})
			return
		}
	}
	var file *VPNClientFile
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		v := c.VPN
		for i := range v.OVRemote.Clients {
			cl := &v.OVRemote.Clients[i]
			if cl.ID != id {
				continue
			}
			if req.Name != nil {
				cl.Name = trim(req.Name)
			}
			if req.Enabled != nil {
				cl.Disabled = !*req.Enabled
			}
			var cert, key string
			if req.NewFile {
				if err := revoke(v, cl.Serial, "device "+cl.Name); err != nil {
					return err
				}
				var notAfter time.Time
				var err error
				if cert, key, cl.Serial, notAfter, err = issue(v, config.OVClientCN(cl.ID)); err != nil {
					return err
				}
				cl.NotAfter = notAfter.Unix()
			}
			if err := vpnErrors(c.Validate(), fmt.Sprintf("vpn.ov_remote.clients[%d].", i)); err != nil {
				return err
			}
			if cert != "" {
				var err error
				file, err = deviceFile(*v, *v.OVRemote, cl.Name, cert, key)
				return err
			}
			return nil
		}
		return nil
	})
	var body any
	if err == nil {
		body = a.ovClientAnswer(id, file)
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

// HandleVPNCARenewServer: POST /api/v1/vpn/ca/renew-server makes the OpenVPN servers a new certificate (and key) from the authority.
// Devices are not touched (they trust the authority, not the server's certificate); the servers restart when it is committed.
func (a *API) HandleVPNCARenewServer(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	draft, _ := a.Manager.StagedNetworkConfig()
	if ca := draft.VPNSettings().CA; ca == nil || ca.Cert == "" || ca.Key == "" {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no OpenVPN certificate authority yet"})
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		cert, key, err := pki.Issue(c.VPN.CA.Cert, c.VPN.CA.Key, "peakroute-server", true, time.Now())
		if err != nil {
			return err
		}
		c.VPN.CA.ServerCert, c.VPN.CA.ServerKey = cert, key
		return vpnErrors(c.Validate(), "vpn.ca.")
	})
	a.answerStaged(w, err, http.StatusOK, "The certificate cannot be renewed: ", a.vpnView())
}

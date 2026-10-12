package config

import (
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"strings"
	"time"

	"peakroute/internal/pki"
)

// OpenVPN: the second engine of the VPN page (docs/vpn.md, sections 9 to 11). It uses the same two kinds of VPN as WireGuard, each one
// a zone of the firewall:
//
//   - the remote access server (zone vpn-ora, interface ovra) with its devices, each with a certificate made by the router's own
//     certificate authority, and a file to import;
//   - site tunnels with engine "openvpn": one router is the server (its authority makes a certificate for the other router and gives
//     a file), the other is the client (it imports that file).
//
// Certificates, the revocation list and the shared control-channel keys live in network.json like every other secret: never shown
// again, never in a review line.

const (
	EngineOpenVPN = "openvpn"

	OVRoleServer = "server"
	OVRoleClient = "client"

	DefaultOVRAPort   = 1194
	DefaultOVSitePort = 1195

	VPNOpenRemoteZone = "vpn-ora"
	ovRemoteIface     = "ovra"

	maxRevoked = 1000
)

var (
	serialPattern = regexp.MustCompile(`^[0-9a-f]{1,40}$`)
	hexPattern    = serialPattern
)

// VPNCA is the router's own certificate authority: it makes and signs everything OpenVPN needs, so that nothing has to be made
// by hand.
type VPNCA struct {
	Cert       string       `json:"cert"`
	Key        string       `json:"key,omitempty"` // never shown again
	ServerCert string       `json:"server_cert,omitempty"`
	ServerKey  string       `json:"server_key,omitempty"` // never shown again
	CRL        string       `json:"crl,omitempty"`        // the list of certificates that are no longer accepted
	CRLNumber  int64        `json:"crl_number,omitempty"`
	Revoked    []VPNRevoked `json:"revoked,omitempty"`
}

// VPNRevoked is a certificate that is no longer accepted.
type VPNRevoked struct {
	Serial string `json:"serial"` // hexadecimal
	Name   string `json:"name"`   // whose it was, for the page
	At     int64  `json:"at"`     // Unix seconds
}

// OVSite is what an OpenVPN site tunnel has besides what every site tunnel has (its address, networks and switches).
type OVSite struct {
	Role  string `json:"role"`  // "server" or "client"
	Proto string `json:"proto"` // "udp" or "tcp"
	// Remote is where the tunnel is made: for a client the other router's address (name or address, and a port), for a server the
	// address the other router connects to (it goes into the file made for it).
	Remote   string `json:"remote,omitempty"`
	TLSCrypt string `json:"tls_crypt,omitempty"` // the shared key of the control channel; never shown again

	// server: the certificate made for the other router (the file it imports), revoked when the tunnel goes
	PeerSerial string `json:"peer_serial,omitempty"`

	// client: what the file from the other router held
	CACert string `json:"ca_cert,omitempty"`
	Cert   string `json:"cert,omitempty"`
	Key    string `json:"key,omitempty"` // never shown again
}

// OVClient is a laptop or a phone that may connect to the OpenVPN remote access server.
type OVClient struct {
	ID       string `json:"id"` // c1, c2...
	Name     string `json:"name"`
	Serial   string `json:"serial"`              // of its certificate
	NotAfter int64  `json:"not_after,omitempty"` // Unix seconds, when the certificate runs out
	Address  string `json:"address"`             // its address inside the remote access network: 10.9.0.2/32
	Disabled bool   `json:"disabled,omitempty"`
}

// OVRemote is the OpenVPN remote access server.
type OVRemote struct {
	Disabled   bool       `json:"disabled,omitempty"`
	ListenPort int        `json:"listen_port,omitempty"`
	Proto      string     `json:"proto,omitempty"`
	Network    string     `json:"network,omitempty"`  // the clients' network: 10.9.0.0/24 (the router is the first address)
	Endpoint   string     `json:"endpoint,omitempty"` // what clients connect to: a name or an address, and a port when it is not the listen port
	FullTunnel bool       `json:"full_tunnel,omitempty"`
	Routes     []string   `json:"routes,omitempty"`
	DNS        bool       `json:"dns,omitempty"`
	Internet   bool       `json:"internet,omitempty"`
	Isolated   bool       `json:"isolated,omitempty"`
	TLSCrypt   string     `json:"tls_crypt,omitempty"` // never shown again
	Clients    []OVClient `json:"clients,omitempty"`
}

func (r OVRemote) clone() OVRemote {
	r.Routes = append([]string(nil), r.Routes...)
	r.Clients = append([]OVClient(nil), r.Clients...)
	return r
}

func (ca VPNCA) clone() VPNCA {
	ca.Revoked = append([]VPNRevoked(nil), ca.Revoked...)
	return ca
}

// FindClient returns a remote access device by id.
func (r OVRemote) FindClient(id string) (OVClient, bool) {
	for _, c := range r.Clients {
		if c.ID == id {
			return c, true
		}
	}
	return OVClient{}, false
}

// OVRemoteInterface is the interface of the OpenVPN remote access server.
func OVRemoteInterface() string { return ovRemoteIface }

// Interface is the interface of a site tunnel.
func (s VPNSite) Interface() string {
	if s.Engine == EngineOpenVPN {
		return "ov" + s.ID
	}
	return SiteInterface(s.ID)
}

// OVClientCN is the name in the certificate of a remote access device; OVPeerCN the one in the certificate made for the other router
// of a site tunnel. A server only accepts its own kind (verify-x509-name), so that one server's certificate cannot be used on another.
func OVClientCN(id string) string   { return "ra-" + id }
func OVPeerCN(siteID string) string { return siteID + "-peer" }

// ServerAddress is the router's own address in the remote access network (the first host address).
func (r OVRemote) ServerAddress() (netip.Addr, bool) {
	p, err := netip.ParsePrefix(r.Network)
	if err != nil || !p.Addr().Is4() {
		return netip.Addr{}, false
	}
	return p.Masked().Addr().Next(), true
}

// ClientEndpoint is what a device connects to: the endpoint set for the server, with the listen port added when it names none.
func (r OVRemote) ClientEndpoint() string {
	return WGRemote{Endpoint: r.Endpoint, ListenPort: r.ListenPort}.ClientEndpoint()
}

// NextClientAddress is the first free address of the remote access network for a new device.
func (r OVRemote) NextClientAddress() (string, bool) {
	return WGRemote{Network: r.Network, Clients: ovAddresses(r.Clients)}.NextClientAddress()
}

func ovAddresses(list []OVClient) []VPNClient {
	out := make([]VPNClient, len(list))
	for i, c := range list {
		out[i] = VPNClient{Address: c.Address}
	}
	return out
}

// OVClientRoutes are the networks a device is told to send through the tunnel: everything for a full tunnel; the chosen networks when
// some are chosen; otherwise the networks of this router (the default network, the other Local Networks, what is behind the site
// tunnels that are switched on). The remote access network itself is not in the list: OpenVPN adds it.
func (c NetworkConfig) OVClientRoutes() []string {
	r := c.VPNSettings().OVRemote
	if r == nil {
		return nil
	}
	return c.clientRoutes(r.FullTunnel, r.Routes)
}

// OVSiteCount is how many OpenVPN site tunnels there are.
func (v VPNConfig) OVSiteCount() int {
	n := 0
	for _, s := range v.Sites {
		if s.Engine == EngineOpenVPN {
			n++
		}
	}
	return n
}

// NextOVSitePort is the first listen port from 1195 that no tunnel uses.
func (v VPNConfig) NextOVSitePort() int {
	used := v.usedPorts()
	for p := DefaultOVSitePort; ; p++ {
		if !used[p] {
			return p
		}
	}
}

// OtherPeers are the certificates that stay valid: what is revoked is not among them.
func (v VPNConfig) activeSerials() map[string]bool {
	out := map[string]bool{}
	if r := v.OVRemote; r != nil {
		for _, c := range r.Clients {
			out[c.Serial] = true
		}
	}
	for _, s := range v.Sites {
		if s.OV != nil && s.OV.Role == OVRoleServer && s.OV.PeerSerial != "" {
			out[s.OV.PeerSerial] = true
		}
	}
	return out
}

// ---- words ------------------------------------------------------------------------------------------------------------------------

func ovProto(p string) string { return strings.ToUpper(p) }

// ovSiteText describes an OpenVPN site tunnel in a line, without any key.
func ovSiteText(s VPNSite) string {
	o := OVSite{}
	if s.OV != nil {
		o = *s.OV
	}
	var t string
	if o.Role == OVRoleClient {
		t = fmt.Sprintf("OpenVPN to %s (%s)", o.Remote, ovProto(o.Proto))
	} else {
		t = fmt.Sprintf("OpenVPN, waits for the other router on %s %d", ovProto(o.Proto), s.ListenPort)
		if o.Remote != "" {
			t += " (it connects to " + o.Remote + ")"
		}
	}
	if s.Address != "" {
		t += ", tunnel " + s.Address
	}
	t += ", networks " + strings.Join(s.RemoteNetworks, ", ")
	t += ", " + onOff(s.Internet, "internet allowed", "no internet") + ", " + onOff(s.Isolated, "isolated from the Local Networks", "can reach the Local Networks")
	if s.Disabled {
		t += ", switched off"
	}
	return t
}

// OVRemoteText describes the OpenVPN remote access server in a line, without any key.
func OVRemoteText(r OVRemote) string {
	t := fmt.Sprintf("OpenVPN, %s %d, network %s", ovProto(r.Proto), r.ListenPort, r.Network)
	if r.Endpoint != "" {
		t += ", clients connect to " + r.Endpoint
	}
	if r.FullTunnel {
		t += ", all the clients' traffic through the router"
	} else if len(r.Routes) > 0 {
		t += ", clients send " + strings.Join(r.Routes, ", ") + " through it"
	} else {
		t += ", clients send the router's networks through it"
	}
	if r.DNS {
		t += ", DNS from this router"
	}
	t += ", " + onOff(r.Internet, "internet allowed", "no internet") + ", " + onOff(r.Isolated, "isolated from the Local Networks", "can reach the Local Networks")
	if r.Disabled {
		t += ", switched off"
	}
	return t
}

// OVClientText describes a device in a line. The certificate's end is shown as a date.
func OVClientText(cl OVClient) string {
	t := "address " + strings.TrimSuffix(cl.Address, "/32")
	if cl.NotAfter > 0 {
		t += ", certificate valid until " + time.Unix(cl.NotAfter, 0).UTC().Format("2006-01-02")
	}
	if cl.Disabled {
		t += ", switched off"
	}
	return t
}

// ---- validation -------------------------------------------------------------------------------------------------------------------

// verifyNoDates checks who signed a certificate and what it is for, not whether it is in its dates: a certificate that has run out
// must not make the whole configuration unsavable (the page shows when they run out).
func verifyNoDates(ca, cert string, server bool) error {
	c, err := pki.ParseCert(cert)
	if err != nil {
		return err
	}
	return pki.Verify(ca, cert, server, c.NotBefore.Add(time.Minute))
}

func validProto(p string) bool { return p == "udp" || p == "tcp" }

// validateOVCA checks the authority and what it made, when anything uses it.
func (c NetworkConfig) validateOVCA(add func(field, format string, args ...any), v VPNConfig) {
	used := v.OVRemote != nil
	for _, s := range v.Sites {
		if s.OV != nil && s.OV.Role == OVRoleServer {
			used = true
		}
	}
	ca := v.CA
	if ca == nil {
		if used {
			add("vpn.ca", "the certificate authority is missing")
		}
		return
	}
	if _, err := pki.ParseCert(ca.Cert); err != nil {
		add("vpn.ca.cert", "is not a certificate")
		return
	}
	if info, err := pki.Describe(ca.Cert); err != nil || !info.IsCA {
		add("vpn.ca.cert", "is not the certificate of an authority")
		return
	}
	if !pki.MatchesKey(ca.Cert, ca.Key) {
		add("vpn.ca.key", "does not belong to the authority's certificate")
		return
	}
	if used {
		switch {
		case ca.ServerCert == "" || ca.ServerKey == "":
			add("vpn.ca.server_cert", "the server certificate is missing")
		case !pki.MatchesKey(ca.ServerCert, ca.ServerKey):
			add("vpn.ca.server_key", "does not belong to the server certificate")
		default:
			if err := verifyNoDates(ca.Cert, ca.ServerCert, true); err != nil {
				add("vpn.ca.server_cert", "is not a valid server certificate of this authority: %v", err)
			}
		}
	}
	if len(ca.Revoked) > maxRevoked {
		add("vpn.ca.revoked", "at most %d revoked certificates", maxRevoked)
	}
	seen := map[string]bool{}
	for i, r := range ca.Revoked {
		if !serialPattern.MatchString(r.Serial) {
			add(fmt.Sprintf("vpn.ca.revoked[%d]", i), "is not a serial number")
		}
		seen[r.Serial] = true
	}
	if ca.CRL != "" || len(ca.Revoked) > 0 {
		listed, err := pki.CRLSerials(ca.CRL)
		if err != nil {
			add("vpn.ca.crl", "is not a revocation list")
		} else {
			inList := map[string]bool{}
			for _, s := range listed {
				inList[s] = true
			}
			if !reflect.DeepEqual(inList, seen) {
				add("vpn.ca.crl", "does not list the revoked certificates")
			}
		}
	}
	for serial := range v.activeSerials() {
		if seen[serial] {
			add("vpn.ca.revoked", "a certificate that is in use is revoked")
		}
	}
}

// validateOVRemote checks the OpenVPN remote access server and its devices. overlap, claim and portField are the shared helpers of
// validateVPN.
func (c NetworkConfig) validateOVRemote(add func(field, format string, args ...any), r OVRemote, overlap func(field, what string, p netip.Prefix), claim func(what string, p netip.Prefix), portField func(field string, port int, who string)) {
	f := func(name string) string { return "vpn.ov_remote." + name }
	if !validProto(r.Proto) {
		add(f("proto"), "must be udp or tcp")
	}
	portField(f("listen_port"), r.ListenPort, "the OpenVPN remote access server")
	if p, ok := parseNetworkCIDR(r.Network); !ok || p.Bits() < 16 || p.Bits() > 29 {
		add(f("network"), "must be a private network in CIDR form such as 10.9.0.0/24 (from /16 to /29, no host bits)")
	} else {
		if !p.Addr().IsPrivate() {
			add(f("network"), "must be a private range (10.x.x.x, 172.16-31.x.x or 192.168.x.x)")
		}
		overlap(f("network"), "the OpenVPN remote access network", p)
		claim("the OpenVPN remote access network", p)
	}
	if r.Endpoint != "" {
		if _, _, ok := splitHostPort(r.Endpoint); !ok {
			add(f("endpoint"), "must be a name or an address devices can reach, such as vpn.example.com or vpn.example.com:1194")
		}
	}
	if r.FullTunnel && !r.Internet {
		add(f("full_tunnel"), "needs access to the internet: switch that on too, or the devices would have no internet at all")
	}
	if len(r.Routes) > MaxClientRoutes {
		add(f("routes"), "at most %d networks", MaxClientRoutes)
	}
	seenRoute := map[string]bool{}
	for _, n := range r.Routes {
		if p, ok := parseNetworkCIDR(n); !ok || p.Bits() < 1 {
			add(f("routes"), "%q is not a network in CIDR form such as 192.168.1.0/24", n)
		} else if seenRoute[n] {
			add(f("routes"), "%q is listed more than once", n)
		}
		seenRoute[n] = true
	}
	if !pki.ValidTLSCryptKey(r.TLSCrypt) {
		add(f("tls_crypt"), "is not a control channel key")
	}
	if len(r.Clients) > MaxVPNClients {
		add(f("clients"), "at most %d devices", MaxVPNClients)
	}
	pool, poolOK := parseNetworkCIDR(r.Network)
	serverAddr, _ := r.ServerAddress()
	ids, names, serials, addrs := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, cl := range r.Clients {
		cf := func(name string) string { return fmt.Sprintf("vpn.ov_remote.clients[%d].%s", i, name) }
		switch {
		case !clientIDPattern.MatchString(cl.ID):
			add(cf("id"), "must be a short lower-case id such as c1")
		case ids[cl.ID]:
			add(cf("id"), "another device already has this id")
		}
		ids[cl.ID] = true
		name := strings.TrimSpace(cl.Name)
		switch {
		case name == "" || len(name) > maxNetName || !networkNamePattern.MatchString(name):
			add(cf("name"), "use 1 to %d letters, digits, spaces or . _ ( ) + - characters", maxNetName)
		case names[strings.ToLower(name)]:
			add(cf("name"), "is used by another device")
		}
		names[strings.ToLower(name)] = true
		switch {
		case !serialPattern.MatchString(cl.Serial):
			add(cf("serial"), "is not the serial number of a certificate")
		case serials[cl.Serial]:
			add(cf("serial"), "belongs to another device")
		}
		serials[cl.Serial] = true
		p, err := netip.ParsePrefix(cl.Address)
		switch {
		case err != nil || !p.Addr().Is4() || p.Bits() != 32:
			add(cf("address"), "must be one address of the remote access network, such as 10.9.0.2/32")
		case !poolOK || !pool.Contains(p.Addr()) || p.Addr() == pool.Addr() || p.Addr() == serverAddr || p.Addr() == broadcastAddr(pool):
			add(cf("address"), "must be a host address inside the remote access network (not the router's own)")
		case addrs[p.Addr().String()]:
			add(cf("address"), "belongs to another device")
		}
		if err == nil {
			addrs[p.Addr().String()] = true
		}
	}
}

// validateOVSite checks what an OpenVPN site tunnel has of its own. The shared rules (name, address range, networks) are in
// validateVPN.
func (c NetworkConfig) validateOVSite(add func(field, format string, args ...any), f func(string) string, s VPNSite, portField func(field string, port int, who string), who string) {
	o := s.OV
	if o == nil {
		add(f("openvpn"), "the OpenVPN settings are missing")
		return
	}
	if s.PrivateKey != "" || s.PeerPublicKey != "" || s.PresharedKey != "" || s.Endpoint != "" || s.Keepalive != 0 {
		add(f("engine"), "an OpenVPN tunnel has no WireGuard settings")
	}
	if !validProto(o.Proto) {
		add(f("openvpn.proto"), "must be udp or tcp")
	}
	if !pki.ValidTLSCryptKey(o.TLSCrypt) {
		add(f("openvpn.tls_crypt"), "is not a control channel key")
	}
	if o.Remote != "" {
		if _, _, ok := splitHostPort(o.Remote); !ok {
			add(f("openvpn.remote"), "must be a name or an address and a port, such as site-b.example.com:1195")
		}
	}
	switch o.Role {
	case OVRoleServer:
		portField(f("listen_port"), s.ListenPort, who)
		if !serialPattern.MatchString(o.PeerSerial) {
			add(f("openvpn.peer_serial"), "is not the serial number of a certificate")
		}
		if o.CACert != "" || o.Cert != "" || o.Key != "" {
			add(f("openvpn.role"), "a server has no imported certificate")
		}
		if o.Remote != "" {
			if _, port, ok := splitHostPort(o.Remote); ok && port == 0 {
				add(f("openvpn.remote"), "needs the port the other router connects to, such as site-b.example.com:1195")
			}
		}
		if ap, err := netip.ParsePrefix(s.Address); err == nil && ap.Addr().Is4() && ap.Bits() >= 24 && ap.Bits() <= 29 {
			// the first address of the tunnel's network is this router's: the other router gets the next one
			if ap.Addr() != ap.Masked().Addr().Next() {
				add(f("address"), "this router takes the first address of the tunnel's network: %s/%d", ap.Masked().Addr().Next(), ap.Bits())
			}
		}
	case OVRoleClient:
		if s.ListenPort != 0 {
			add(f("listen_port"), "a client does not listen")
		}
		if o.PeerSerial != "" {
			add(f("openvpn.role"), "a client has no certificate of its own to revoke")
		}
		if _, port, ok := splitHostPort(o.Remote); !ok || port == 0 {
			add(f("openvpn.remote"), "must be the other router's name or address and its port, such as site-b.example.com:1195")
		}
		switch {
		case o.CACert == "" || o.Cert == "" || o.Key == "":
			add(f("openvpn.cert"), "the certificate, its key and the authority's certificate are needed (import the file from the other router)")
		case !pki.MatchesKey(o.Cert, o.Key):
			add(f("openvpn.key"), "does not belong to the certificate")
		default:
			if info, err := pki.Describe(o.CACert); err != nil || !info.IsCA {
				add(f("openvpn.ca_cert"), "is not the certificate of an authority")
			} else if err := verifyNoDates(o.CACert, o.Cert, false); err != nil {
				add(f("openvpn.cert"), "is not a valid client certificate of that authority: %v", err)
			}
		}
	default:
		add(f("openvpn.role"), "must be server or client")
	}
}

// ---- the review ---------------------------------------------------------------------------------------------------------------------

// diffOpenVPN describes the changes of the OpenVPN remote access server, its devices and the certificate authority. Keys and
// certificates are never shown: a change of one is "set" or "changed".
func diffOpenVPN(a, b VPNConfig) []Change {
	var changes []Change
	ra, rb := a.OVRemote, b.OVRemote
	switch {
	case ra == nil && rb == nil:
	case ra == nil:
		changes = append(changes, Change{Field: "vpn_ov_remote", Label: "OpenVPN remote access", Old: valueNone, New: OVRemoteText(*rb)})
	case rb == nil:
		changes = append(changes, Change{Field: "vpn_ov_remote", Label: "OpenVPN remote access", Old: OVRemoteText(*ra), New: "(removed)"})
	default:
		if OVRemoteText(*ra) != OVRemoteText(*rb) {
			changes = append(changes, Change{Field: "vpn_ov_remote", Label: "OpenVPN remote access", Old: OVRemoteText(*ra), New: OVRemoteText(*rb)})
		}
		changes = append(changes, secretChange("vpn_ov_keys", "Control channel key of the OpenVPN remote access", ra.TLSCrypt, rb.TLSCrypt)...)
	}
	var oldClients, newClients []OVClient
	if ra != nil {
		oldClients = ra.Clients
	}
	if rb != nil {
		newClients = rb.Clients
	}
	findClient := func(list []OVClient, id string) (OVClient, bool) {
		for _, cl := range list {
			if cl.ID == id {
				return cl, true
			}
		}
		return OVClient{}, false
	}
	have := map[string]bool{}
	for _, cl := range newClients {
		have[cl.ID] = true
		old, had := findClient(oldClients, cl.ID)
		label := "OpenVPN device " + strings.TrimSpace(cl.Name)
		switch {
		case !had:
			changes = append(changes, Change{Field: "vpn_ov_clients", Label: label, Old: valueNone, New: OVClientText(cl)})
		case old != cl:
			before := OVClientText(old)
			if old.Name != cl.Name {
				before = fmt.Sprintf("%q: %s", strings.TrimSpace(old.Name), before)
			}
			if old.Serial != cl.Serial {
				before += " (other certificate)"
			}
			changes = append(changes, Change{Field: "vpn_ov_clients", Label: label, Old: before, New: OVClientText(cl)})
		}
	}
	for _, cl := range oldClients {
		if !have[cl.ID] {
			changes = append(changes, Change{Field: "vpn_ov_clients", Label: "OpenVPN device " + strings.TrimSpace(cl.Name), Old: OVClientText(cl), New: "(removed)"})
		}
	}

	ca, cb := a.CA, b.CA
	switch {
	case ca == nil && cb == nil:
	case ca == nil:
		changes = append(changes, Change{Field: "vpn_ca", Label: "VPN certificate authority", Old: valueNone, New: caText(*cb)})
	case cb == nil:
		changes = append(changes, Change{Field: "vpn_ca", Label: "VPN certificate authority", Old: caText(*ca), New: "(removed)"})
	default:
		changes = append(changes, secretChange("vpn_ca", "Key of the VPN certificate authority", ca.Key, cb.Key)...)
		changes = append(changes, secretChange("vpn_ca", "Server certificate of the OpenVPN servers", ca.ServerKey, cb.ServerKey)...)
		if len(ca.Revoked) != len(cb.Revoked) {
			changes = append(changes, Change{Field: "vpn_ca_revoked", Label: "Certificates that are no longer accepted", Old: fmt.Sprint(len(ca.Revoked)), New: fmt.Sprint(len(cb.Revoked))})
		}
	}
	return changes
}

func caText(ca VPNCA) string {
	t := "made"
	if info, err := pki.Describe(ca.Cert); err == nil {
		t += ", valid until " + info.NotAfter.UTC().Format("2006-01-02")
	}
	return t
}

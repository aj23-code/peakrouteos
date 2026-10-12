package config

import (
	"fmt"
	"net/netip"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"peakroute/internal/wg"
)

// VPN: tunnels between sites (site-to-site) and remote access for laptops and phones (client VPN), with WireGuard. docs/vpn.md has
// the model and the reasons. Every tunnel is a zone the firewall talks about, like a Local Network.

const (
	EngineWireGuard = "wireguard"

	MaxVPNSites       = 8
	MaxVPNClients     = 100
	MaxSiteNetworks   = 16
	MaxClientRoutes   = 16
	DefaultWGSitePort = 51820
	DefaultWGRAPort   = 51821
	DefaultKeepalive  = 25

	// zone ids and interface names
	VPNRemoteZone = "vpn-ra"
	wgRemoteIface = "wgra"
)

// VPNSite is a tunnel to another site: the router there has a network (or several) that this router can reach, and the other way round.
type VPNSite struct {
	ID       string `json:"id"` // s1, s2...
	Name     string `json:"name"`
	Engine   string `json:"engine"`
	Disabled bool   `json:"disabled,omitempty"`

	// WireGuard
	PrivateKey     string   `json:"private_key,omitempty"`     // this router's key for the tunnel; never shown again
	PeerPublicKey  string   `json:"peer_public_key,omitempty"` // the other router's public key
	PresharedKey   string   `json:"preshared_key,omitempty"`   // an extra shared secret (optional); never shown again
	ListenPort     int      `json:"listen_port,omitempty"`
	Address        string   `json:"address,omitempty"`  // this side of the tunnel, in the tunnel's own small network: 10.99.1.1/30
	Endpoint       string   `json:"endpoint,omitempty"` // the other router's address and port, when this router is the one that calls
	Keepalive      int      `json:"keepalive,omitempty"`
	RemoteNetworks []string `json:"remote_networks,omitempty"` // the networks behind the other router

	// what the tunnel's traffic may reach, like a Local Network's switches
	Internet bool `json:"internet,omitempty"` // may use this router's internet connection
	Isolated bool `json:"isolated,omitempty"` // cannot talk to the Local Networks, and they cannot talk to it
}

// VPNClient is a laptop or a phone that may connect to the remote access server.
type VPNClient struct {
	ID           string `json:"id"` // c1, c2...
	Name         string `json:"name"`
	PublicKey    string `json:"public_key"`
	PresharedKey string `json:"preshared_key,omitempty"` // an extra shared secret (optional); never shown again
	Address      string `json:"address"`                 // its address inside the remote access network: 10.8.0.2/32
	Disabled     bool   `json:"disabled,omitempty"`
}

// WGRemote is the remote access server.
type WGRemote struct {
	Disabled   bool        `json:"disabled,omitempty"`
	PrivateKey string      `json:"private_key,omitempty"`
	ListenPort int         `json:"listen_port,omitempty"`
	Network    string      `json:"network,omitempty"`     // the clients' network: 10.8.0.0/24 (the router is the first address)
	Endpoint   string      `json:"endpoint,omitempty"`    // what clients connect to: a name or an address, and a port when it is not the listen port
	FullTunnel bool        `json:"full_tunnel,omitempty"` // clients send all their traffic through the router
	Routes     []string    `json:"routes,omitempty"`      // the networks clients send through the tunnel (empty: the router's networks)
	DNS        bool        `json:"dns,omitempty"`         // clients use this router for DNS
	Internet   bool        `json:"internet,omitempty"`
	Isolated   bool        `json:"isolated,omitempty"`
	Clients    []VPNClient `json:"clients,omitempty"`
}

// VPNConfig is the VPN page's settings.
type VPNConfig struct {
	Sites    []VPNSite `json:"sites,omitempty"`
	WGRemote *WGRemote `json:"wg_remote,omitempty"`
}

func (v VPNConfig) clone() VPNConfig {
	if len(v.Sites) > 0 {
		sites := make([]VPNSite, len(v.Sites))
		for i, s := range v.Sites {
			s.RemoteNetworks = append([]string(nil), s.RemoteNetworks...)
			sites[i] = s
		}
		v.Sites = sites
	} else {
		v.Sites = nil
	}
	if v.WGRemote != nil {
		r := *v.WGRemote
		r.Routes = append([]string(nil), r.Routes...)
		r.Clients = append([]VPNClient(nil), r.Clients...)
		v.WGRemote = &r
	}
	return v
}

// VPNSettings returns the VPN settings, empty when there are none.
func (c NetworkConfig) VPNSettings() VPNConfig {
	if c.VPN == nil {
		return VPNConfig{}
	}
	return c.VPN.clone()
}

// FindSite returns a site tunnel by id.
func (v VPNConfig) FindSite(id string) (VPNSite, bool) {
	for _, s := range v.Sites {
		if s.ID == id {
			return s, true
		}
	}
	return VPNSite{}, false
}

// FindClient returns a remote access client by id.
func (r WGRemote) FindClient(id string) (VPNClient, bool) {
	for _, c := range r.Clients {
		if c.ID == id {
			return c, true
		}
	}
	return VPNClient{}, false
}

// SiteZone is the firewall's id for a site tunnel; SiteInterface its interface.
func SiteZone(id string) string      { return "vpn-" + id }
func SiteInterface(id string) string { return "wg" + id }

// RemoteInterface is the interface of the remote access server.
func RemoteInterface() string { return wgRemoteIface }

// ServerAddress is the router's own address in the remote access network (the first host address).
func (r WGRemote) ServerAddress() (netip.Addr, bool) {
	p, err := netip.ParsePrefix(r.Network)
	if err != nil || !p.Addr().Is4() {
		return netip.Addr{}, false
	}
	return p.Masked().Addr().Next(), true
}

// Zone is a place traffic comes from or goes to, as the firewall talks about it, for the zones that are not Local Networks.
type Zone struct {
	ID       string
	Name     string
	Iface    string
	Internet bool
	Isolated bool
	Prefixes []netip.Prefix // the addresses behind it
}

// VPNZones lists the tunnels that are switched on, as the firewall's zones: the remote access server and each site.
func (c NetworkConfig) VPNZones() []Zone {
	v := c.VPNSettings()
	var zones []Zone
	if r := v.WGRemote; r != nil && !r.Disabled {
		z := Zone{ID: VPNRemoteZone, Name: "Remote access", Iface: wgRemoteIface, Internet: r.Internet, Isolated: r.Isolated}
		if p, err := netip.ParsePrefix(r.Network); err == nil {
			z.Prefixes = []netip.Prefix{p.Masked()}
		}
		zones = append(zones, z)
	}
	for _, s := range v.Sites {
		if s.Disabled {
			continue
		}
		z := Zone{ID: SiteZone(s.ID), Name: strings.TrimSpace(s.Name), Iface: SiteInterface(s.ID), Internet: s.Internet, Isolated: s.Isolated}
		for _, n := range s.RemoteNetworks {
			if p, err := netip.ParsePrefix(n); err == nil {
				z.Prefixes = append(z.Prefixes, p.Masked())
			}
		}
		zones = append(zones, z)
	}
	return zones
}

// FindVPNZone returns a VPN zone by its firewall id.
func (c NetworkConfig) FindVPNZone(id string) (Zone, bool) {
	for _, z := range c.VPNZones() {
		if z.ID == id {
			return z, true
		}
	}
	return Zone{}, false
}

var (
	siteIDPattern   = regexp.MustCompile(`^s[0-9]{1,2}$`)
	clientIDPattern = regexp.MustCompile(`^c[0-9]{1,3}$`)
)

// splitHostPort accepts "host" or "host:port" and returns the host and the port (0 when there is none).
func splitHostPort(s string) (host string, port int, ok bool) {
	host = s
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host = s[:i]
		p, err := strconv.Atoi(s[i+1:])
		if err != nil || p < 1 || p > 65535 || strconv.Itoa(p) != s[i+1:] {
			return "", 0, false
		}
		port = p
	}
	return host, port, ValidServerHost(host)
}

// parseCIDR reads an IPv4 network in CIDR form that has no host bits set.
func parseNetworkCIDR(s string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(s)
	if err != nil || !p.Addr().Is4() || p.Masked() != p || p.String() != s {
		return netip.Prefix{}, false
	}
	return p, true
}

// vpnLocalPrefixes are the addresses the router already uses: what a VPN's networks must not overlap.
func (c NetworkConfig) vpnLocalPrefixes() []struct {
	what   string
	prefix netip.Prefix
} {
	var out []struct {
		what   string
		prefix netip.Prefix
	}
	add := func(what string, p netip.Prefix, ok bool) {
		if ok {
			out = append(out, struct {
				what   string
				prefix netip.Prefix
			}{what, p})
		}
	}
	if ip, ok := parseIPv4(c.LanIP); ok {
		if ones, ok2 := parseNetmask(c.LanSubnet); ok2 {
			add("the default network", netip.PrefixFrom(ip, ones).Masked(), true)
		}
	}
	if strings.EqualFold(c.WanMode, "static") {
		if ip, ok := parseIPv4(c.WanIP); ok {
			if ones, ok2 := parseNetmask(c.WanSubnet); ok2 {
				add("the internet connection", netip.PrefixFrom(ip, ones).Masked(), true)
			}
		}
	}
	for _, n := range c.Networks {
		if ip, ok := parseIPv4(n.IP); ok {
			if ones, ok2 := parseNetmask(n.Netmask); ok2 {
				add("the network "+strings.TrimSpace(n.Name), netip.PrefixFrom(ip, ones).Masked(), true)
			}
		}
	}
	for _, r := range c.Routes {
		if !r.Disabled {
			if p, ok := parseNetworkCIDR(r.Destination); ok {
				add("the static route to "+r.Destination, p, true)
			}
		}
	}
	return out
}

// validateVPN checks the tunnels. Errors are reported on vpn.sites[i].field, vpn.sites (the list as a whole), vpn.wg_remote.field,
// vpn.wg_remote.clients[i].field and vpn.wg_remote.clients.
func (c NetworkConfig) validateVPN(add func(field, format string, args ...any)) {
	v := c.VPNSettings()
	local := c.vpnLocalPrefixes()

	type claimed struct {
		what   string
		prefix netip.Prefix
	}
	var claims []claimed // what the VPN already uses, for the next tunnel to keep clear of
	// overlap reports a network that overlaps what the router uses or what another tunnel uses, and remembers it
	overlap := func(field, what string, p netip.Prefix) {
		for _, l := range local {
			if p.Overlaps(l.prefix) {
				add(field, "%s (%s) overlaps %s (%s); use a different range", what, p, l.what, l.prefix)
				return
			}
		}
		for _, k := range claims {
			if p.Overlaps(k.prefix) {
				add(field, "%s (%s) overlaps %s (%s); use a different range", what, p, k.what, k.prefix)
				return
			}
		}
	}
	ports := map[int]string{}
	portField := func(field string, port int, who string) {
		if port < 1 || port > 65535 {
			add(field, "must be a port from 1 to 65535")
			return
		}
		if other, dup := ports[port]; dup {
			add(field, "UDP port %d is already used by %s", port, other)
		}
		ports[port] = who
	}

	// the remote access server
	if r := v.WGRemote; r != nil {
		f := func(name string) string { return "vpn.wg_remote." + name }
		if !wg.ValidKey(r.PrivateKey) {
			add(f("private_key"), "is not a WireGuard key")
		}
		portField(f("listen_port"), r.ListenPort, "the remote access server")
		if p, ok := parseNetworkCIDR(r.Network); !ok || p.Bits() < 16 || p.Bits() > 29 {
			add(f("network"), "must be a private network in CIDR form such as 10.8.0.0/24 (from /16 to /29, no host bits)")
		} else {
			if !p.Addr().IsPrivate() {
				add(f("network"), "must be a private range (10.x.x.x, 172.16-31.x.x or 192.168.x.x)")
			}
			overlap(f("network"), "the remote access network", p)
			claims = append(claims, claimed{"the remote access network", p})
		}
		if r.Endpoint != "" {
			if _, _, ok := splitHostPort(r.Endpoint); !ok {
				add(f("endpoint"), "must be a name or an address clients can reach, such as vpn.example.com or vpn.example.com:51821")
			}
		}
		if r.FullTunnel && !r.Internet {
			add(f("full_tunnel"), "needs access to the internet: switch that on too, or the clients would have no internet at all")
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
		if len(r.Clients) > MaxVPNClients {
			add(f("clients"), "at most %d clients", MaxVPNClients)
		}
		pool, poolOK := parseNetworkCIDR(r.Network)
		serverAddr, _ := r.ServerAddress()
		ids, names, keys, addrs := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
		for i, cl := range r.Clients {
			cf := func(name string) string { return fmt.Sprintf("vpn.wg_remote.clients[%d].%s", i, name) }
			switch {
			case !clientIDPattern.MatchString(cl.ID):
				add(cf("id"), "must be a short lower-case id such as c1")
			case ids[cl.ID]:
				add(cf("id"), "another client already has this id")
			}
			ids[cl.ID] = true
			name := strings.TrimSpace(cl.Name)
			switch {
			case name == "" || len(name) > maxNetName || !networkNamePattern.MatchString(name):
				add(cf("name"), "use 1 to %d letters, digits, spaces or . _ ( ) + - characters", maxNetName)
			case names[strings.ToLower(name)]:
				add(cf("name"), "is used by another client")
			}
			names[strings.ToLower(name)] = true
			switch {
			case !wg.ValidKey(cl.PublicKey):
				add(cf("public_key"), "is not a WireGuard key")
			case keys[cl.PublicKey]:
				add(cf("public_key"), "belongs to another client")
			}
			keys[cl.PublicKey] = true
			if cl.PresharedKey != "" && !wg.ValidKey(cl.PresharedKey) {
				add(cf("preshared_key"), "is not a WireGuard key")
			}
			p, err := netip.ParsePrefix(cl.Address)
			switch {
			case err != nil || !p.Addr().Is4() || p.Bits() != 32:
				add(cf("address"), "must be one address of the remote access network, such as 10.8.0.2/32")
			case !poolOK || !pool.Contains(p.Addr()) || p.Addr() == pool.Addr() || p.Addr() == serverAddr || p.Addr() == broadcastAddr(pool):
				add(cf("address"), "must be a host address inside the remote access network (not the router's own)")
			case addrs[p.Addr().String()]:
				add(cf("address"), "belongs to another client")
			}
			if err == nil {
				addrs[p.Addr().String()] = true
			}
		}
	}

	// the site tunnels
	if len(v.Sites) > MaxVPNSites {
		add("vpn.sites", "at most %d site tunnels", MaxVPNSites)
	}
	ids, names, keys := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, s := range v.Sites {
		f := func(name string) string { return fmt.Sprintf("vpn.sites[%d].%s", i, name) }
		switch {
		case !siteIDPattern.MatchString(s.ID):
			add(f("id"), "must be a short lower-case id such as s1")
		case ids[s.ID]:
			add(f("id"), "another tunnel already has this id")
		}
		ids[s.ID] = true
		name := strings.TrimSpace(s.Name)
		switch {
		case name == "" || len(name) > maxNetName || !networkNamePattern.MatchString(name):
			add(f("name"), "use 1 to %d letters, digits, spaces or . _ ( ) + - characters", maxNetName)
		case names[strings.ToLower(name)]:
			add(f("name"), "is used by another tunnel")
		case strings.EqualFold(name, "remote access") || strings.EqualFold(name, "lan"):
			add(f("name"), "is used by something else")
		}
		names[strings.ToLower(name)] = true
		if s.Engine != EngineWireGuard {
			add(f("engine"), "must be wireguard")
		}
		if !wg.ValidKey(s.PrivateKey) {
			add(f("private_key"), "is not a WireGuard key")
		} else if pub, _ := wg.PublicKey(s.PrivateKey); pub == s.PeerPublicKey {
			add(f("peer_public_key"), "is this router's own public key: the other router has its own")
		}
		switch {
		case !wg.ValidKey(s.PeerPublicKey):
			add(f("peer_public_key"), "is not a WireGuard key")
		case keys[s.PeerPublicKey]:
			add(f("peer_public_key"), "belongs to another tunnel")
		}
		keys[s.PeerPublicKey] = true
		if s.PresharedKey != "" && !wg.ValidKey(s.PresharedKey) {
			add(f("preshared_key"), "is not a WireGuard key")
		}
		portField(f("listen_port"), s.ListenPort, "the tunnel "+name)
		if s.Endpoint != "" {
			if _, port, ok := splitHostPort(s.Endpoint); !ok || port == 0 {
				add(f("endpoint"), "must be the other router's name or address and its port, such as site-b.example.com:51820")
			}
		}
		if s.Keepalive < 0 || s.Keepalive > 600 {
			add(f("keepalive"), "must be from 0 (off) to 600 seconds")
		}
		if ap, err := netip.ParsePrefix(s.Address); err != nil || !ap.Addr().Is4() || ap.Bits() < 24 || ap.Bits() > 31 || (ap.Bits() < 31 && (ap.Masked().Addr() == ap.Addr() || ap.Addr() == broadcastAddr(ap))) {
			add(f("address"), "must be this router's address inside the tunnel's own small network, such as 10.99.1.1/30 (from /24 to /31; the other router uses the next address)")
		} else {
			overlap(f("address"), "the tunnel's network", ap.Masked())
			claims = append(claims, claimed{"the tunnel " + name, ap.Masked()})
		}
		if len(s.RemoteNetworks) == 0 {
			add(f("remote_networks"), "at least one network behind the other router is needed")
		}
		if len(s.RemoteNetworks) > MaxSiteNetworks {
			add(f("remote_networks"), "at most %d networks", MaxSiteNetworks)
		}
		for _, n := range s.RemoteNetworks {
			p, ok := parseNetworkCIDR(n)
			if !ok || p.Bits() < 8 {
				add(f("remote_networks"), "%q is not a network in CIDR form such as 10.20.0.0/24 (from /8; no host bits)", n)
				continue
			}
			overlap(f("remote_networks"), "the network "+n, p)
			claims = append(claims, claimed{"a network behind the tunnel " + name, p})
		}
	}
}

// ---- words ----------------------------------------------------------------------------------------------------

// SiteText describes a site tunnel in a line, without any key.
func SiteText(s VPNSite) string {
	t := "WireGuard"
	if s.Endpoint != "" {
		t += " to " + s.Endpoint
	} else {
		t += ", waits for the other router"
	}
	t += fmt.Sprintf(", UDP %d, tunnel %s, networks %s", s.ListenPort, s.Address, strings.Join(s.RemoteNetworks, ", "))
	t += ", " + onOff(s.Internet, "internet allowed", "no internet") + ", " + onOff(s.Isolated, "isolated from the Local Networks", "can reach the Local Networks")
	if s.Disabled {
		t += ", switched off"
	}
	return t
}

// RemoteText describes the remote access server in a line, without any key.
func RemoteText(r WGRemote) string {
	t := fmt.Sprintf("WireGuard, UDP %d, network %s", r.ListenPort, r.Network)
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

// ClientText describes a remote access client in a line.
func ClientText(cl VPNClient) string {
	t := "address " + strings.TrimSuffix(cl.Address, "/32")
	if cl.Disabled {
		t += ", switched off"
	}
	return t
}

func secretChange(field, label, before, after string) []Change {
	if before == after {
		return nil
	}
	ch := Change{Field: field, Label: label, Old: "set", New: "changed"}
	if before == "" {
		ch.Old, ch.New = valueNone, "set"
	} else if after == "" {
		ch.New = valueNone
	}
	return []Change{ch}
}

// diffVPN describes the changes of the tunnels in words, one line per tunnel, remote access server or client added, removed or
// changed. A key is never shown: a change of it is "set" or "changed".
func diffVPN(active, candidate NetworkConfig) []Change {
	a, b := active.VPNSettings(), candidate.VPNSettings()
	var changes []Change

	sameSite := func(x, y VPNSite) bool {
		x.PrivateKey, y.PrivateKey, x.PresharedKey, y.PresharedKey = "", "", "", ""
		return reflect.DeepEqual(x, y)
	}
	newSites := map[string]bool{}
	for _, s := range b.Sites {
		newSites[s.ID] = true
		old, had := a.FindSite(s.ID)
		label := "VPN tunnel " + strings.TrimSpace(s.Name)
		switch {
		case !had:
			changes = append(changes, Change{Field: "vpn_sites", Label: label, Old: valueNone, New: SiteText(s)})
		case !sameSite(old, s):
			before := SiteText(old)
			if old.Name != s.Name {
				before = fmt.Sprintf("%q: %s", strings.TrimSpace(old.Name), before)
			}
			changes = append(changes, Change{Field: "vpn_sites", Label: label, Old: before, New: SiteText(s)})
		}
		if had {
			changes = append(changes, secretChange("vpn_site_keys", "Key of the VPN tunnel "+strings.TrimSpace(s.Name), old.PrivateKey, s.PrivateKey)...)
			changes = append(changes, secretChange("vpn_site_keys", "Preshared key of the VPN tunnel "+strings.TrimSpace(s.Name), old.PresharedKey, s.PresharedKey)...)
		}
	}
	for _, s := range a.Sites {
		if !newSites[s.ID] {
			changes = append(changes, Change{Field: "vpn_sites", Label: "VPN tunnel " + strings.TrimSpace(s.Name), Old: SiteText(s), New: "(removed)"})
		}
	}

	// the remote access server and its clients
	ra, rb := a.WGRemote, b.WGRemote
	switch {
	case ra == nil && rb == nil:
	case ra == nil:
		changes = append(changes, Change{Field: "vpn_remote", Label: "Remote access VPN", Old: valueNone, New: RemoteText(*rb)})
	case rb == nil:
		changes = append(changes, Change{Field: "vpn_remote", Label: "Remote access VPN", Old: RemoteText(*ra), New: "(removed)"})
	default:
		if RemoteText(*ra) != RemoteText(*rb) {
			changes = append(changes, Change{Field: "vpn_remote", Label: "Remote access VPN", Old: RemoteText(*ra), New: RemoteText(*rb)})
		}
		changes = append(changes, secretChange("vpn_remote_key", "Key of the remote access VPN", ra.PrivateKey, rb.PrivateKey)...)
	}
	var oldClients, newClients []VPNClient
	if ra != nil {
		oldClients = ra.Clients
	}
	if rb != nil {
		newClients = rb.Clients
	}
	findClient := func(list []VPNClient, id string) (VPNClient, bool) {
		for _, cl := range list {
			if cl.ID == id {
				return cl, true
			}
		}
		return VPNClient{}, false
	}
	have := map[string]bool{}
	for _, cl := range newClients {
		have[cl.ID] = true
		old, had := findClient(oldClients, cl.ID)
		label := "VPN client " + strings.TrimSpace(cl.Name)
		oldNoPSK, newNoPSK := old, cl
		oldNoPSK.PresharedKey, newNoPSK.PresharedKey = "", ""
		switch {
		case !had:
			changes = append(changes, Change{Field: "vpn_clients", Label: label, Old: valueNone, New: ClientText(cl)})
		case oldNoPSK != newNoPSK:
			before := ClientText(old)
			if old.Name != cl.Name {
				before = fmt.Sprintf("%q: %s", strings.TrimSpace(old.Name), before)
			}
			if old.PublicKey != cl.PublicKey {
				before += " (other key)"
			}
			changes = append(changes, Change{Field: "vpn_clients", Label: label, Old: before, New: ClientText(cl)})
		}
		if had {
			changes = append(changes, secretChange("vpn_client_keys", "Preshared key of the VPN client "+strings.TrimSpace(cl.Name), old.PresharedKey, cl.PresharedKey)...)
		}
	}
	for _, cl := range oldClients {
		if !have[cl.ID] {
			changes = append(changes, Change{Field: "vpn_clients", Label: "VPN client " + strings.TrimSpace(cl.Name), Old: ClientText(cl), New: "(removed)"})
		}
	}
	return changes
}

// ---- what a client is told ------------------------------------------------------------------------------------

// ClientRoutes are the networks a remote access client sends through the tunnel: everything for a full tunnel; the chosen networks
// when some are chosen; otherwise the networks of this router (the default network, the other Local Networks, what is behind the site
// tunnels that are switched on) and the remote access network itself.
func (c NetworkConfig) ClientRoutes() []string {
	r := c.VPNSettings().WGRemote
	if r == nil {
		return nil
	}
	if r.FullTunnel {
		return []string{"0.0.0.0/0"}
	}
	if len(r.Routes) > 0 {
		return append([]string(nil), r.Routes...)
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if p, ok := c.NetworkPrefix(DefaultNetworkID); ok {
		add(p.String())
	}
	for _, n := range c.Networks {
		if p, ok := c.NetworkPrefix(n.ID); ok {
			add(p.String())
		}
	}
	for _, z := range c.VPNZones() {
		if z.ID == VPNRemoteZone {
			continue
		}
		for _, p := range z.Prefixes {
			add(p.String())
		}
	}
	add(r.Network)
	return out
}

// ClientEndpoint is what a client connects to: the endpoint set for the server, with the listen port added when it names none.
func (r WGRemote) ClientEndpoint() string {
	if r.Endpoint == "" {
		return ""
	}
	if _, port, ok := splitHostPort(r.Endpoint); ok && port == 0 {
		return r.Endpoint + ":" + strconv.Itoa(r.ListenPort)
	}
	return r.Endpoint
}

// NextClientAddress is the first free address of the remote access network for a new client.
func (r WGRemote) NextClientAddress() (string, bool) {
	pool, err := netip.ParsePrefix(r.Network)
	if err != nil {
		return "", false
	}
	pool = pool.Masked()
	used := map[netip.Addr]bool{}
	if s, ok := r.ServerAddress(); ok {
		used[s] = true
	}
	for _, cl := range r.Clients {
		if p, err := netip.ParsePrefix(cl.Address); err == nil {
			used[p.Addr()] = true
		}
	}
	for a := pool.Addr().Next().Next(); pool.Contains(a); a = a.Next() {
		if !used[a] && a != broadcastAddr(pool) {
			return a.String() + "/32", true
		}
	}
	return "", false
}

// NextSitePort is the first listen port from 51820 that no tunnel uses.
func (v VPNConfig) NextSitePort() int {
	used := map[int]bool{}
	if v.WGRemote != nil {
		used[v.WGRemote.ListenPort] = true
	}
	for _, s := range v.Sites {
		used[s.ListenPort] = true
	}
	for p := DefaultWGSitePort; ; p++ {
		if !used[p] && p != DefaultWGRAPort {
			return p
		}
	}
}

// OtherSideAddress is the address the other router uses inside a site tunnel's small network: the next one after this router's, or
// the one before it when this router has the last.
func OtherSideAddress(address string) (string, bool) {
	p, err := netip.ParsePrefix(address)
	if err != nil || !p.Addr().Is4() {
		return "", false
	}
	m := p.Masked()
	if p.Bits() == 31 {
		if p.Addr() == m.Addr() {
			return fmt.Sprintf("%s/%d", p.Addr().Next(), p.Bits()), true
		}
		return fmt.Sprintf("%s/%d", p.Addr().Prev(), p.Bits()), true
	}
	if n := p.Addr().Next(); m.Contains(n) && n != broadcastAddr(m) {
		return fmt.Sprintf("%s/%d", n, p.Bits()), true
	}
	if q := p.Addr().Prev(); m.Contains(q) && q != m.Addr() {
		return fmt.Sprintf("%s/%d", q, p.Bits()), true
	}
	return "", false
}

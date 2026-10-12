package config

import (
	"encoding/json"
	"strings"
	"testing"

	"peakroute/internal/wg"
)

func key(t testing.TB) string {
	t.Helper()
	k, _, err := wg.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pub(t testing.TB) string {
	t.Helper()
	_, p, err := wg.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func site(t testing.TB, id, name string, port int, addr string, nets ...string) VPNSite {
	return VPNSite{ID: id, Name: name, Engine: EngineWireGuard, PrivateKey: key(t), PeerPublicKey: pub(t), ListenPort: port, Address: addr, Endpoint: "site-b.example.com:51820",
		Keepalive: 25, RemoteNetworks: nets}
}

func remote(t testing.TB) *WGRemote {
	return &WGRemote{PrivateKey: key(t), ListenPort: DefaultWGRAPort, Network: "10.8.0.0/24", Endpoint: "vpn.example.com", Internet: false}
}

func vpnNet(t testing.TB, f func(*VPNConfig)) NetworkConfig {
	c := validNet()
	c.VPN = &VPNConfig{}
	if f != nil {
		f(c.VPN)
	}
	return c
}

func TestVPNSettingsThatAreAccepted(t *testing.T) {
	cases := map[string]NetworkConfig{
		"none":           validNet(),
		"an empty block": vpnNet(t, nil),
		"one site": vpnNet(t, func(v *VPNConfig) {
			v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24")}
		}),
		"a site that waits": vpnNet(t, func(v *VPNConfig) {
			s := site(t, "s1", "Branch", 51820, "10.99.1.2/30", "10.20.0.0/24", "10.21.0.0/16")
			s.Endpoint, s.Keepalive = "", 0
			v.Sites = []VPNSite{s}
		}),
		"a site with a preshared key": vpnNet(t, func(v *VPNConfig) {
			s := site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24")
			s.PresharedKey = key(t)
			v.Sites = []VPNSite{s}
		}),
		"a site by address": vpnNet(t, func(v *VPNConfig) {
			s := site(t, "s1", "Branch", 51820, "10.99.1.1/31", "10.20.0.0/24")
			s.Endpoint = "203.0.113.4:51820"
			v.Sites = []VPNSite{s}
		}),
		"two sites": vpnNet(t, func(v *VPNConfig) {
			v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24"), site(t, "s2", "Depot", 51830, "10.99.2.1/30", "10.30.0.0/24")}
		}),
		"remote access": vpnNet(t, func(v *VPNConfig) { v.WGRemote = remote(t) }),
		"remote access with clients": vpnNet(t, func(v *VPNConfig) {
			r := remote(t)
			r.Clients = []VPNClient{{ID: "c1", Name: "Anna phone", PublicKey: pub(t), Address: "10.8.0.2/32"}, {ID: "c2", Name: "Laptop", PublicKey: pub(t), Address: "10.8.0.3/32", PresharedKey: key(t)}}
			v.WGRemote = r
		}),
		"a full tunnel": vpnNet(t, func(v *VPNConfig) { r := remote(t); r.FullTunnel, r.Internet = true, true; v.WGRemote = r }),
		"chosen routes": vpnNet(t, func(v *VPNConfig) {
			r := remote(t)
			r.Routes = []string{"192.168.1.0/24", "10.20.0.0/24"}
			v.WGRemote = r
		}),
		"remote access and a site": vpnNet(t, func(v *VPNConfig) {
			v.WGRemote = remote(t)
			v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24")}
		}),
		"switched off": vpnNet(t, func(v *VPNConfig) { r := remote(t); r.Disabled = true; v.WGRemote = r }),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestVPNSettingsThatAreRefused(t *testing.T) {
	withSite := func(f func(*VPNSite)) NetworkConfig {
		return vpnNet(t, func(v *VPNConfig) {
			s := site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24")
			f(&s)
			v.Sites = []VPNSite{s}
		})
	}
	withRemote := func(f func(*WGRemote)) NetworkConfig {
		return vpnNet(t, func(v *VPNConfig) { r := remote(t); f(r); v.WGRemote = r })
	}
	withClient := func(f func(*VPNClient)) NetworkConfig {
		return withRemote(func(r *WGRemote) {
			cl := VPNClient{ID: "c1", Name: "Phone", PublicKey: pub(t), Address: "10.8.0.2/32"}
			f(&cl)
			r.Clients = []VPNClient{cl}
		})
	}
	cases := []struct {
		name  string
		c     NetworkConfig
		field string
	}{
		// sites
		{"a bad id", withSite(func(s *VPNSite) { s.ID = "x1" }), "vpn.sites[0].id"},
		{"no name", withSite(func(s *VPNSite) { s.Name = "" }), "vpn.sites[0].name"},
		{"a name that is taken", vpnNet(t, func(v *VPNConfig) {
			v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24"), site(t, "s2", "branch", 51830, "10.99.2.1/30", "10.30.0.0/24")}
		}), "vpn.sites[1].name"},
		{"a name that is the remote access's", withSite(func(s *VPNSite) { s.Name = "Remote access" }), "vpn.sites[0].name"},
		{"another engine", withSite(func(s *VPNSite) { s.Engine = "ipsec" }), "vpn.sites[0].engine"},
		{"no private key", withSite(func(s *VPNSite) { s.PrivateKey = "" }), "vpn.sites[0].private_key"},
		{"a bad private key", withSite(func(s *VPNSite) { s.PrivateKey = "not a key" }), "vpn.sites[0].private_key"},
		{"the peer's key is ours", withSite(func(s *VPNSite) { s.PeerPublicKey, _ = wg.PublicKey(s.PrivateKey) }), "vpn.sites[0].peer_public_key"},
		{"a bad peer key", withSite(func(s *VPNSite) { s.PeerPublicKey = "x" }), "vpn.sites[0].peer_public_key"},
		{"the same peer twice", vpnNet(t, func(v *VPNConfig) {
			a, b := site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24"), site(t, "s2", "Depot", 51830, "10.99.2.1/30", "10.30.0.0/24")
			b.PeerPublicKey = a.PeerPublicKey
			v.Sites = []VPNSite{a, b}
		}), "vpn.sites[1].peer_public_key"},
		{"a bad preshared key", withSite(func(s *VPNSite) { s.PresharedKey = "x" }), "vpn.sites[0].preshared_key"},
		{"no port", withSite(func(s *VPNSite) { s.ListenPort = 0 }), "vpn.sites[0].listen_port"},
		{"port 70000", withSite(func(s *VPNSite) { s.ListenPort = 70000 }), "vpn.sites[0].listen_port"},
		{"a port the other tunnel has", vpnNet(t, func(v *VPNConfig) {
			v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24"), site(t, "s2", "Depot", 51820, "10.99.2.1/30", "10.30.0.0/24")}
		}), "vpn.sites[1].listen_port"},
		{"the remote access's port", vpnNet(t, func(v *VPNConfig) {
			v.WGRemote = remote(t)
			v.Sites = []VPNSite{site(t, "s1", "Branch", DefaultWGRAPort, "10.99.1.1/30", "10.20.0.0/24")}
		}), "vpn.sites[0].listen_port"},
		{"an endpoint without a port", withSite(func(s *VPNSite) { s.Endpoint = "site-b.example.com" }), "vpn.sites[0].endpoint"},
		{"an endpoint that is a url", withSite(func(s *VPNSite) { s.Endpoint = "https://site-b.example.com:1" }), "vpn.sites[0].endpoint"},
		{"an endpoint that is this router", withSite(func(s *VPNSite) { s.Endpoint = "127.0.0.1:51820" }), "vpn.sites[0].endpoint"},
		{"an endpoint with a line break", withSite(func(s *VPNSite) { s.Endpoint = "a.example:51820\\nPostUp = reboot" }), "vpn.sites[0].endpoint"},
		{"a keepalive too long", withSite(func(s *VPNSite) { s.Keepalive = 601 }), "vpn.sites[0].keepalive"},
		{"a negative keepalive", withSite(func(s *VPNSite) { s.Keepalive = -1 }), "vpn.sites[0].keepalive"},
		{"no address", withSite(func(s *VPNSite) { s.Address = "" }), "vpn.sites[0].address"},
		{"an address without a prefix", withSite(func(s *VPNSite) { s.Address = "10.99.1.1" }), "vpn.sites[0].address"},
		{"a /32 address", withSite(func(s *VPNSite) { s.Address = "10.99.1.1/32" }), "vpn.sites[0].address"},
		{"a /16 address", withSite(func(s *VPNSite) { s.Address = "10.99.0.1/16" }), "vpn.sites[0].address"},
		{"the network address itself", withSite(func(s *VPNSite) { s.Address = "10.99.1.0/30" }), "vpn.sites[0].address"},
		{"the broadcast address", withSite(func(s *VPNSite) { s.Address = "10.99.1.3/30" }), "vpn.sites[0].address"},
		{"an address on the LAN", withSite(func(s *VPNSite) { s.Address = "192.168.1.9/30" }), "vpn.sites[0].address"},
		{"no remote networks", withSite(func(s *VPNSite) { s.RemoteNetworks = nil }), "vpn.sites[0].remote_networks"},
		{"a remote network with host bits", withSite(func(s *VPNSite) { s.RemoteNetworks = []string{"10.20.0.5/24"} }), "vpn.sites[0].remote_networks"},
		{"everything", withSite(func(s *VPNSite) { s.RemoteNetworks = []string{"0.0.0.0/0"} }), "vpn.sites[0].remote_networks"},
		{"a too big network", withSite(func(s *VPNSite) { s.RemoteNetworks = []string{"10.0.0.0/7"} }), "vpn.sites[0].remote_networks"},
		{"not a network", withSite(func(s *VPNSite) { s.RemoteNetworks = []string{"office"} }), "vpn.sites[0].remote_networks"},
		{"a remote network on the LAN", withSite(func(s *VPNSite) { s.RemoteNetworks = []string{"192.168.1.0/24"} }), "vpn.sites[0].remote_networks"},
		{"a remote network inside the LAN", withSite(func(s *VPNSite) { s.RemoteNetworks = []string{"192.168.1.128/25"} }), "vpn.sites[0].remote_networks"},
		{"a remote network round the LAN", withSite(func(s *VPNSite) { s.RemoteNetworks = []string{"192.168.0.0/16"} }), "vpn.sites[0].remote_networks"},
		{"two sites with the same network", vpnNet(t, func(v *VPNConfig) {
			v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24"), site(t, "s2", "Depot", 51830, "10.99.2.1/30", "10.20.0.0/25")}
		}), "vpn.sites[1].remote_networks"},
		{"two tunnels in the same small network", vpnNet(t, func(v *VPNConfig) {
			v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24"), site(t, "s2", "Depot", 51830, "10.99.1.1/30", "10.30.0.0/24")}
		}), "vpn.sites[1].address"},
		{"a remote network that is the remote access network", vpnNet(t, func(v *VPNConfig) {
			v.WGRemote = remote(t)
			v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.8.0.0/24")}
		}), "vpn.sites[0].remote_networks"},
		{"too many networks", withSite(func(s *VPNSite) {
			s.RemoteNetworks = nil
			for i := 0; i <= MaxSiteNetworks; i++ {
				s.RemoteNetworks = append(s.RemoteNetworks, "10."+itoa(100+i)+".0.0/24")
			}
		}), "vpn.sites[0].remote_networks"},
		// remote access
		{"no key", withRemote(func(r *WGRemote) { r.PrivateKey = "" }), "vpn.wg_remote.private_key"},
		{"port 0", withRemote(func(r *WGRemote) { r.ListenPort = 0 }), "vpn.wg_remote.listen_port"},
		{"no network", withRemote(func(r *WGRemote) { r.Network = "" }), "vpn.wg_remote.network"},
		{"a public network", withRemote(func(r *WGRemote) { r.Network = "203.0.113.0/24" }), "vpn.wg_remote.network"},
		{"a network with host bits", withRemote(func(r *WGRemote) { r.Network = "10.8.0.1/24" }), "vpn.wg_remote.network"},
		{"a network that is too small", withRemote(func(r *WGRemote) { r.Network = "10.8.0.0/30" }), "vpn.wg_remote.network"},
		{"a network that is too big", withRemote(func(r *WGRemote) { r.Network = "10.0.0.0/8" }), "vpn.wg_remote.network"},
		{"a network on the LAN", withRemote(func(r *WGRemote) { r.Network = "192.168.1.0/24" }), "vpn.wg_remote.network"},
		{"a bad endpoint", withRemote(func(r *WGRemote) { r.Endpoint = "not a host" }), "vpn.wg_remote.endpoint"},
		{"an endpoint port of 0", withRemote(func(r *WGRemote) { r.Endpoint = "vpn.example.com:0" }), "vpn.wg_remote.endpoint"},
		{"a full tunnel without the internet", withRemote(func(r *WGRemote) { r.FullTunnel, r.Internet = true, false }), "vpn.wg_remote.full_tunnel"},
		{"a bad route", withRemote(func(r *WGRemote) { r.Routes = []string{"192.168.1.1/24"} }), "vpn.wg_remote.routes"},
		{"a route twice", withRemote(func(r *WGRemote) { r.Routes = []string{"192.168.1.0/24", "192.168.1.0/24"} }), "vpn.wg_remote.routes"},
		{"too many routes", withRemote(func(r *WGRemote) {
			for i := 0; i <= MaxClientRoutes; i++ {
				r.Routes = append(r.Routes, "10."+itoa(100+i)+".0.0/24")
			}
		}), "vpn.wg_remote.routes"},
		// clients
		{"a client with a bad id", withClient(func(c *VPNClient) { c.ID = "phone" }), "vpn.wg_remote.clients[0].id"},
		{"a client with no name", withClient(func(c *VPNClient) { c.Name = " " }), "vpn.wg_remote.clients[0].name"},
		{"two clients with one name", withRemote(func(r *WGRemote) {
			r.Clients = []VPNClient{{ID: "c1", Name: "Phone", PublicKey: pub(t), Address: "10.8.0.2/32"}, {ID: "c2", Name: "phone", PublicKey: pub(t), Address: "10.8.0.3/32"}}
		}), "vpn.wg_remote.clients[1].name"},
		{"two clients with one id", withRemote(func(r *WGRemote) {
			r.Clients = []VPNClient{{ID: "c1", Name: "A", PublicKey: pub(t), Address: "10.8.0.2/32"}, {ID: "c1", Name: "B", PublicKey: pub(t), Address: "10.8.0.3/32"}}
		}), "vpn.wg_remote.clients[1].id"},
		{"two clients with one key", withRemote(func(r *WGRemote) {
			k := pub(t)
			r.Clients = []VPNClient{{ID: "c1", Name: "A", PublicKey: k, Address: "10.8.0.2/32"}, {ID: "c2", Name: "B", PublicKey: k, Address: "10.8.0.3/32"}}
		}), "vpn.wg_remote.clients[1].public_key"},
		{"a client with no key", withClient(func(c *VPNClient) { c.PublicKey = "" }), "vpn.wg_remote.clients[0].public_key"},
		{"a client with a bad preshared key", withClient(func(c *VPNClient) { c.PresharedKey = "x" }), "vpn.wg_remote.clients[0].preshared_key"},
		{"a client with no address", withClient(func(c *VPNClient) { c.Address = "" }), "vpn.wg_remote.clients[0].address"},
		{"a client with a network as its address", withClient(func(c *VPNClient) { c.Address = "10.8.0.0/24" }), "vpn.wg_remote.clients[0].address"},
		{"a client outside the network", withClient(func(c *VPNClient) { c.Address = "10.9.0.2/32" }), "vpn.wg_remote.clients[0].address"},
		{"a client with the router's address", withClient(func(c *VPNClient) { c.Address = "10.8.0.1/32" }), "vpn.wg_remote.clients[0].address"},
		{"a client with the network address", withClient(func(c *VPNClient) { c.Address = "10.8.0.0/32" }), "vpn.wg_remote.clients[0].address"},
		{"a client with the broadcast address", withClient(func(c *VPNClient) { c.Address = "10.8.0.255/32" }), "vpn.wg_remote.clients[0].address"},
		{"two clients with one address", withRemote(func(r *WGRemote) {
			r.Clients = []VPNClient{{ID: "c1", Name: "A", PublicKey: pub(t), Address: "10.8.0.2/32"}, {ID: "c2", Name: "B", PublicKey: pub(t), Address: "10.8.0.2/32"}}
		}), "vpn.wg_remote.clients[1].address"},
	}
	for _, tc := range cases {
		if !errorFieldsOf(t, tc.c)[tc.field] {
			t.Errorf("%s: no error on %s (got %v)", tc.name, tc.field, errorFieldsOf(t, tc.c))
		}
	}
	tooMany := vpnNet(t, nil)
	for i := 0; i <= MaxVPNSites; i++ {
		tooMany.VPN.Sites = append(tooMany.VPN.Sites, site(t, "s"+itoa(i+1), "Site "+itoa(i+1), 51820+i, "10.99."+itoa(i+1)+".1/30", "10."+itoa(100+i)+".0.0/24"))
	}
	if !errorFieldsOf(t, tooMany)["vpn.sites"] {
		t.Error("nine tunnels were accepted")
	}
}

func TestVPNIsCopiedAndWrittenOnlyWhenUsed(t *testing.T) {
	plain, _ := json.Marshal(validNet())
	if strings.Contains(string(plain), "vpn") {
		t.Errorf("vpn written for a plain configuration: %s", plain)
	}
	c := vpnNet(t, func(v *VPNConfig) {
		r := remote(t)
		r.Routes = []string{"192.168.1.0/24"}
		r.Clients = []VPNClient{{ID: "c1", Name: "Phone", PublicKey: pub(t), Address: "10.8.0.2/32"}}
		v.WGRemote = r
		v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24")}
	})
	cp := c.clone()
	cp.VPN.Sites[0].RemoteNetworks[0] = "10.77.0.0/24"
	cp.VPN.Sites[0].Name = "Other"
	cp.VPN.WGRemote.Routes[0] = "10.66.0.0/24"
	cp.VPN.WGRemote.Clients[0].Name = "Tablet"
	cp.VPN.WGRemote.Network = "10.9.0.0/24"
	if c.VPN.Sites[0].RemoteNetworks[0] != "10.20.0.0/24" || c.VPN.Sites[0].Name != "Branch" || c.VPN.WGRemote.Routes[0] != "192.168.1.0/24" || c.VPN.WGRemote.Clients[0].Name != "Phone" || c.VPN.WGRemote.Network != "10.8.0.0/24" {
		t.Fatal("the copy shares the tunnels with the original")
	}
	s := c.VPNSettings()
	s.Sites[0].Name = "x"
	s.WGRemote.Clients[0].Name = "x"
	if c.VPN.Sites[0].Name != "Branch" || c.VPN.WGRemote.Clients[0].Name != "Phone" {
		t.Fatal("VPNSettings shares its tunnels")
	}
	if got, ok := c.VPNSettings().FindSite("s1"); !ok || got.Name != "Branch" {
		t.Fatal("FindSite")
	}
	if _, ok := c.VPNSettings().FindSite("s9"); ok {
		t.Fatal("FindSite found a tunnel that is not there")
	}
	if got, ok := c.VPNSettings().WGRemote.FindClient("c1"); !ok || got.Name != "Phone" {
		t.Fatal("FindClient")
	}
	if _, ok := c.VPNSettings().WGRemote.FindClient("c9"); ok {
		t.Fatal("FindClient found a client that is not there")
	}
	if got := validNet().VPNSettings(); len(got.Sites) != 0 || got.WGRemote != nil {
		t.Fatalf("%+v", got)
	}
}

func TestVPNZonesAreWhatTheFirewallSees(t *testing.T) {
	c := vpnNet(t, func(v *VPNConfig) {
		r := remote(t)
		r.Internet = true
		v.WGRemote = r
		off := site(t, "s2", "Depot", 51830, "10.99.2.1/30", "10.30.0.0/24")
		off.Disabled = true
		a := site(t, "s1", " Branch ", 51820, "10.99.1.1/30", "10.20.0.0/24", "10.21.0.0/24")
		a.Isolated = true
		v.Sites = []VPNSite{a, off}
	})
	zones := c.VPNZones()
	if len(zones) != 2 || zones[0].ID != "vpn-ra" || zones[0].Name != "Remote access" || zones[0].Iface != "wgra" || !zones[0].Internet || zones[0].Isolated || len(zones[0].Prefixes) != 1 || zones[0].Prefixes[0].String() != "10.8.0.0/24" {
		t.Fatalf("%+v", zones)
	}
	if zones[1].ID != "vpn-s1" || zones[1].Name != "Branch" || zones[1].Iface != "wgs1" || zones[1].Internet || !zones[1].Isolated || len(zones[1].Prefixes) != 2 || zones[1].Prefixes[1].String() != "10.21.0.0/24" {
		t.Fatalf("%+v", zones[1])
	}
	if _, ok := c.FindVPNZone("vpn-s2"); ok {
		t.Fatal("a tunnel that is switched off is a zone")
	}
	if z, ok := c.FindVPNZone("vpn-s1"); !ok || z.Iface != "wgs1" {
		t.Fatal("FindVPNZone")
	}
	// the remote access server is switched off: no zone
	c.VPN.WGRemote.Disabled = true
	if got := c.VPNZones(); len(got) != 1 || got[0].ID != "vpn-s1" {
		t.Fatalf("%+v", got)
	}
	if addr, ok := remote(t).ServerAddress(); !ok || addr.String() != "10.8.0.1" {
		t.Fatalf("%v %v", addr, ok)
	}
	if _, ok := (WGRemote{Network: "nope"}).ServerAddress(); ok {
		t.Fatal("an address from nothing")
	}
	if SiteZone("s1") != "vpn-s1" || SiteInterface("s1") != "wgs1" || RemoteInterface() != "wgra" {
		t.Fatal("names")
	}
}

func TestRulesMayNameVPNsAndStayInsideTheirAddresses(t *testing.T) {
	c := vpnNet(t, func(v *VPNConfig) {
		v.WGRemote = remote(t)
		v.Sites = []VPNSite{site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24")}
	})
	rule := func(f func(*FirewallRule)) NetworkConfig {
		r := FirewallRule{ID: "fw1", Name: "r", From: "vpn-ra", Action: ActionAllow, Protocol: ProtoAny, To: DefaultNetworkID}
		f(&r)
		cc := c.clone()
		cc.Firewall = &FirewallConfig{Rules: []FirewallRule{r}}
		return cc
	}
	for name, cc := range map[string]NetworkConfig{
		"from remote access":          rule(func(r *FirewallRule) {}),
		"from a site to the router":   rule(func(r *FirewallRule) { r.From, r.To = "vpn-s1", ToRouter }),
		"to a site":                   rule(func(r *FirewallRule) { r.From, r.To = DefaultNetworkID, "vpn-s1" }),
		"a source inside the clients": rule(func(r *FirewallRule) { r.Source = "10.8.0.5" }),
		"a destination behind a site": rule(func(r *FirewallRule) { r.From, r.To, r.Destination = DefaultNetworkID, "vpn-s1", "10.20.0.7" }),
	} {
		if err := cc.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		c     NetworkConfig
		field string
	}{
		"a source outside the clients": {rule(func(r *FirewallRule) { r.Source = "192.168.1.5" }), "firewall.rules[0].source"},
		"a destination outside a site": {rule(func(r *FirewallRule) { r.From, r.To, r.Destination = DefaultNetworkID, "vpn-s1", "10.8.0.5" }), "firewall.rules[0].destination"},
		"a VPN that does not exist":    {rule(func(r *FirewallRule) { r.From = "vpn-s9" }), "firewall.rules[0].from"},
		"a VPN that is switched off":   {func() NetworkConfig { cc := rule(func(r *FirewallRule) {}); cc.VPN.WGRemote.Disabled = true; return cc }(), "firewall.rules[0].from"},
		"a VPN and itself":             {rule(func(r *FirewallRule) { r.To = "vpn-ra" }), "firewall.rules[0].to"},
	} {
		if !errorFieldsOf(t, tc.c)[tc.field] {
			t.Errorf("%s: %v", name, errorFieldsOf(t, tc.c))
		}
	}
	if got := c.networkName("vpn-ra"); got != "Remote access" {
		t.Errorf("%q", got)
	}
	if got := c.networkName("vpn-s1"); got != "Branch" {
		t.Errorf("%q", got)
	}
}

func TestVPNReviewLinesNeverShowAKey(t *testing.T) {
	base := validNet()
	line := func(a, b NetworkConfig, field string) []Change {
		var out []Change
		for _, c := range DiffNetwork(a, b) {
			if c.Field == field {
				out = append(out, c)
			}
		}
		return out
	}
	s := site(t, "s1", "Branch", 51820, "10.99.1.1/30", "10.20.0.0/24")
	with := func(f func(*VPNSite)) NetworkConfig {
		x := s
		x.RemoteNetworks = append([]string(nil), s.RemoteNetworks...)
		f(&x)
		return vpnNet(t, func(v *VPNConfig) { v.Sites = []VPNSite{x} })
	}
	added := with(func(*VPNSite) {})
	l := line(base, added, "vpn_sites")
	if len(l) != 1 || l[0].Label != "VPN tunnel Branch" || l[0].Old != "(none)" ||
		l[0].New != "WireGuard to site-b.example.com:51820, UDP 51820, tunnel 10.99.1.1/30, networks 10.20.0.0/24, no internet, can reach the Local Networks" {
		t.Fatalf("%+v", l)
	}
	for _, c := range DiffNetwork(base, added) {
		if strings.Contains(c.Old+c.New+c.Label, s.PrivateKey) || strings.Contains(c.Old+c.New+c.Label, s.PeerPublicKey) {
			t.Fatalf("a key in the review: %+v", c)
		}
	}
	// changing the networks, the name, the switches
	l = line(added, with(func(x *VPNSite) {
		x.RemoteNetworks = []string{"10.20.0.0/24", "10.21.0.0/24"}
		x.Isolated = true
		x.Name = "Office"
	}), "vpn_sites")
	if len(l) != 1 || !strings.HasPrefix(l[0].Old, `"Branch": WireGuard`) || !strings.Contains(l[0].New, "networks 10.20.0.0/24, 10.21.0.0/24") || !strings.Contains(l[0].New, "isolated from the Local Networks") || l[0].Label != "VPN tunnel Office" {
		t.Fatalf("%+v", l)
	}
	if got := line(added, with(func(x *VPNSite) { x.Disabled = true }), "vpn_sites"); len(got) != 1 || !strings.HasSuffix(got[0].New, "switched off") {
		t.Fatalf("%+v", got)
	}
	// a key: "set" or "changed", never the key
	newKey := with(func(x *VPNSite) { x.PrivateKey = key(t) })
	if l := line(added, newKey, "vpn_site_keys"); len(l) != 1 || l[0].Old != "set" || l[0].New != "changed" || l[0].Label != "Key of the VPN tunnel Branch" {
		t.Fatalf("%+v", l)
	}
	if l := line(added, newKey, "vpn_sites"); len(l) != 0 {
		t.Fatalf("a key change also shows as a change of the tunnel: %+v", l)
	}
	if l := line(added, with(func(x *VPNSite) { x.PresharedKey = key(t) }), "vpn_site_keys"); len(l) != 1 || l[0].Old != "(none)" || l[0].New != "set" || !strings.HasPrefix(l[0].Label, "Preshared key") {
		t.Fatalf("%+v", l)
	}
	if got := line(added, base, "vpn_sites"); len(got) != 1 || got[0].New != "(removed)" {
		t.Fatalf("%+v", got)
	}
	// the remote access server and its clients
	rem := remote(t)
	c1 := VPNClient{ID: "c1", Name: "Phone", PublicKey: pub(t), Address: "10.8.0.2/32"}
	withR := func(f func(*WGRemote)) NetworkConfig {
		r := *rem
		r.Clients = append([]VPNClient(nil), rem.Clients...)
		f(&r)
		return vpnNet(t, func(v *VPNConfig) { v.WGRemote = &r })
	}
	on := withR(func(*WGRemote) {})
	l = line(base, on, "vpn_remote")
	if len(l) != 1 || l[0].Old != "(none)" || l[0].New != "WireGuard, UDP 51821, network 10.8.0.0/24, clients connect to vpn.example.com, clients send the router's networks through it, no internet, can reach the Local Networks" {
		t.Fatalf("%+v", l)
	}
	full := withR(func(r *WGRemote) { r.FullTunnel, r.Internet, r.DNS = true, true, true })
	if l := line(on, full, "vpn_remote"); len(l) != 1 || !strings.Contains(l[0].New, "all the clients' traffic through the router") || !strings.Contains(l[0].New, "DNS from this router") || !strings.Contains(l[0].New, "internet allowed") {
		t.Fatalf("%+v", l)
	}
	if l := line(on, withR(func(r *WGRemote) { r.Routes = []string{"192.168.1.0/24"} }), "vpn_remote"); len(l) != 1 || !strings.Contains(l[0].New, "clients send 192.168.1.0/24 through it") {
		t.Fatalf("%+v", l)
	}
	if l := line(on, withR(func(r *WGRemote) { r.PrivateKey = key(t) }), "vpn_remote_key"); len(l) != 1 || l[0].Old != "set" || l[0].New != "changed" {
		t.Fatalf("%+v", l)
	}
	if l := line(on, base, "vpn_remote"); len(l) != 1 || l[0].New != "(removed)" {
		t.Fatalf("%+v", l)
	}
	withC := withR(func(r *WGRemote) { r.Clients = []VPNClient{c1} })
	if l := line(on, withC, "vpn_clients"); len(l) != 1 || l[0].Label != "VPN client Phone" || l[0].Old != "(none)" || l[0].New != "address 10.8.0.2" {
		t.Fatalf("%+v", l)
	}
	for _, c := range DiffNetwork(on, withC) {
		if strings.Contains(c.Old+c.New+c.Label, c1.PublicKey) {
			t.Fatalf("a key in the review: %+v", c)
		}
	}
	renamed := withR(func(r *WGRemote) { cc := c1; cc.Name = "Tablet"; cc.PublicKey = pub(t); r.Clients = []VPNClient{cc} })
	if l := line(withC, renamed, "vpn_clients"); len(l) != 1 || l[0].Old != `"Phone": address 10.8.0.2 (other key)` || l[0].Label != "VPN client Tablet" {
		t.Fatalf("%+v", l)
	}
	if l := line(withC, on, "vpn_clients"); len(l) != 1 || l[0].New != "(removed)" {
		t.Fatalf("%+v", l)
	}
	off := withR(func(r *WGRemote) { cc := c1; cc.Disabled = true; r.Clients = []VPNClient{cc} })
	if l := line(withC, off, "vpn_clients"); len(l) != 1 || l[0].New != "address 10.8.0.2, switched off" {
		t.Fatalf("%+v", l)
	}
	if l := line(withC, withR(func(r *WGRemote) { cc := c1; cc.PresharedKey = key(t); r.Clients = []VPNClient{cc} }), "vpn_client_keys"); len(l) != 1 || l[0].Old != "(none)" || l[0].New != "set" {
		t.Fatalf("%+v", l)
	}
	if got := DiffNetwork(added, added); len(got) != 0 {
		t.Fatalf("no change, but %+v", got)
	}
}

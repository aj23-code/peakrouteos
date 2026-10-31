// The PeakRoute router: everything in internal/daemon that every build shares, and the parts only a router has (the internet
// connection, local networks, ports, routes, firewall, DNS and DHCP, VPN, Dynamic DNS and the clients). The access point is
// cmd/peakroute-ap.
package main

import (
	"context"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"peakroute/internal/clientnames"
	"peakroute/internal/config"
	"peakroute/internal/daemon"
	"peakroute/internal/ddns"
	"peakroute/internal/fwlog"
	"peakroute/internal/netapi"
	"peakroute/internal/network"
	"peakroute/internal/ovpn"
	"peakroute/internal/presence"
	"peakroute/internal/translator"
	"peakroute/internal/wanhealth"
	"peakroute/internal/wg"
)

// routerWorkEvery is how often the router's own background work looks whether the device is (still) a router.
const routerWorkEvery = 10 * time.Second

func main() {
	c := daemon.Start(daemon.ParseFlags())
	cfgMgr, runner, mux, netAPI, configDir, systemRoot := c.Manager, c.Runner, c.Mux, c.NetAPI, c.ConfigDir, c.SystemRoot
	requireAuth := c.RequireAuth
	pollerCtx := c.Ctx
	// The router's own background work rests while the device is an access point (it would count, probe and log nothing there),
	// and starts again when it is a router once more.
	isRouter := func() bool { return !cfgMgr.NetworkConfig().IsAP() }
	whileRouter := func(work func(context.Context)) { go daemon.RunWhile(pollerCtx, routerWorkEvery, isRouter, work) }

	// how much each device uses the internet: counted by the firewall's own table and read back here (network/usage.go)
	usage := network.NewUsageTracker(runner, filepath.Join(configDir, "client_usage.json"))
	usage.Load()
	// the counting table is part of the firewall file, but a file written by an older version has none and only a commit rewrites it:
	// load the table directly when it is missing. An access point and a router with no internet port yet count nothing.
	usage.Ensure = func(ctx context.Context) error {
		cfg := cfgMgr.NetworkConfig()
		if cfg.IsAP() || cfg.WanDevice() == "" {
			return nil
		}
		dir := filepath.Join(systemRoot, "/run/peakroute")
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		file := filepath.Join(dir, "usage.nft")
		if err := os.WriteFile(file, []byte(translator.AccountingRuleset(cfg)), 0600); err != nil {
			return err
		}
		_, err := runner.Run(ctx, "nft", "-f", file)
		return err
	}
	network.GlobalTracker = usage
	whileRouter(func(ctx context.Context) { usage.Run(ctx, 10*time.Second) })

	// Watch the internet connection: ping the upstream DNS resolver every few seconds
	healthStore, err := wanhealth.Open(filepath.Join(configDir, "wan_health.db"))
	if err != nil {
		log.Fatalf("[-] Failed to initialize WAN health history: %v", err)
	}
	c.OnClose(func() {
		if err := healthStore.Close(); err != nil {
			log.Printf("[-] Error closing WAN health history: %v", err)
		}
	})
	wanProbe := &wanhealth.Prober{
		Store:   healthStore,
		Pinger:  &wanhealth.AutoPinger{},
		Every:   5 * time.Second,
		Timeout: 2 * time.Second,
		Target: func() (wanhealth.Target, bool) {
			cfg := cfgMgr.NetworkConfig()
			if cfg.WanInterface == "" {
				return wanhealth.Target{}, false
			}
			resolvers, source := network.UpstreamResolvers(cfg, os.ReadFile)
			if len(resolvers) == 0 {
				return wanhealth.Target{}, false
			}
			return wanhealth.Target{Addr: resolvers[0], Source: source}, true
		},
		Gateway: func() (wanhealth.Target, bool) {
			cfg := cfgMgr.NetworkConfig()
			if cfg.WanInterface == "" {
				return wanhealth.Target{}, false
			}
			gw, ok := network.UpstreamGateway(cfg, os.ReadFile)
			return wanhealth.Target{Addr: gw, Source: "default gateway"}, ok
		},
	}
	whileRouter(wanProbe.Run)

	// Keep the names of the Dynamic DNS page pointed at the internet connection's address
	ddnsUpdater := &ddns.Updater{
		Entries: func() []config.DDNSEntry { return cfgMgr.NetworkConfig().DDNSSettings().Entries },
		WAN: func() (netip.Addr, bool) {
			up, addr, _ := network.WANConnection(cfgMgr.NetworkConfig().WanDevice())
			if !up || addr == "" {
				return netip.Addr{}, false
			}
			host, _, _ := strings.Cut(addr, "/")
			ip, err := netip.ParseAddr(host)
			return ip, err == nil
		},
	}
	whileRouter(ddnsUpdater.Run)

	// Keep the firewall log: the packets that rules with logging matched, read from the kernel log into a database
	// of their own (the journal is shared and size limited)
	fwLogStore, err := fwlog.Open(filepath.Join(configDir, "firewall_log.db"))
	if err != nil {
		log.Printf("[!] Firewall log unavailable: %v", err)
	} else {
		defer fwLogStore.Close()
		whileRouter((&fwlog.Collector{Store: fwLogStore, Source: fwlog.JournalSource, OnPacket: c.Syslog.Packet}).Run)
	}

	// An access point has no internet connection, networks, firewall, VPN or DHCP of its own: the interface of those pages says so
	routerOnly := func(h http.HandlerFunc) http.HandlerFunc {
		return netapi.RouterOnly(func() bool { return cfgMgr.NetworkConfig().IsAP() }, h)
	}
	netAPI.Live = func(active config.NetworkConfig) *netapi.Live {
		dev := active.WanDevice()
		up, addr, gw := network.WANConnection(dev)
		if gw == "" && active.WanKind() == config.WanPPPoE {
			gw = network.PPPPeer(dev) // the provider's end of the session
		}
		live := &netapi.Live{Up: up, Address: addr, Gateway: gw, DNS: []string{}, Device: dev}
		live.MAC, _ = network.LinkDetails(active.WanLink())
		_, live.MTU = network.LinkDetails(dev)
		resolvers, _ := network.UpstreamResolvers(active, os.ReadFile)
		for _, r := range resolvers {
			live.DNS = append(live.DNS, r.String())
		}
		return live
	}
	netAPI.RoutingTable = func() []netapi.LiveRoute {
		data, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return nil
		}
		return netapi.ClassifyRoutes(network.ParseRoutingTable(data))
	}
	mux.HandleFunc("/api/v1/routes", requireAuth(routerOnly(netAPI.HandleRoutes)))
	mux.HandleFunc("/api/v1/dhcp/reservations", requireAuth(routerOnly(netAPI.HandleReservations)))
	// Nicknames for the devices in the client list: only labels, saved at once, kept in a file of their own (internal/clientnames)
	clientNames := clientnames.New(filepath.Join(configDir, "client_names.json"))
	clientNames.Load()
	c.Backup.Env.Names = clientNames
	mux.HandleFunc("/api/v1/clients/{mac}/name", requireAuth(routerOnly(clientNames.HandleName)))

	// The devices to list: those with a DHCP lease, and those the router sees on its networks that have an address of their own (the
	// neighbour table). -dev has no real neighbours, so it shows two samples: one with an address from the wrong network.
	clientList := func(ctx context.Context) []network.ClientLease {
		leases := network.ReadLeases(network.LeasePaths)
		cfg := cfgMgr.NetworkConfig()
		var neighbours []network.Neighbour
		if c.Dev {
			neighbours = []network.Neighbour{
				{IP: "192.168.1.200", MAC: "02:aa:bb:cc:dd:01", Dev: "br0"},
				{IP: "10.20.0.77", MAC: "02:aa:bb:cc:dd:02", Dev: "br0"},
			}
		} else {
			neighbours = network.ReadNeighbours(ctx, runner)
		}
		return append(leases, network.StaticClients(leases, neighbours, cfg, func(dev string) string { return translator.NetworkOfInterface(cfg, dev) })...)
	}
	// Who is here: Wi-Fi devices from the access points' station lists, the others asked with ARP, all remembered
	presenceTracker := &presence.Tracker{
		Path: filepath.Join(configDir, "client_presence.json"),
		Wifi: func(ctx context.Context) (map[string]presence.Link, bool) {
			rep := c.Stations.Get(ctx)
			links := map[string]presence.Link{}
			for _, s := range rep.Stations {
				links[s.MAC] = presence.Link{SSID: s.SSID, Band: s.Band}
			}
			return links, rep.Known
		},
		Probe: presence.ARPProber{}.Probe,
		Targets: func() []presence.Target {
			var out []presence.Target
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, l := range clientList(ctx) {
				if ip, err := netip.ParseAddr(l.IPAddress); err == nil && ip.Is4() {
					out = append(out, presence.Target{MAC: l.MACAddress, IP: ip})
				}
			}
			return out
		},
	}
	if c.Dev {
		// a laptop cannot ask the devices on its network (a raw socket needs root, and the leases here are samples): pretend that all of
		// them but the last answer, so that the Clients page can be looked at with both states
		presenceTracker.Probe = func(_ context.Context, targets []presence.Target) (map[string]bool, error) {
			here := map[string]bool{}
			for i, t := range targets {
				if i != len(targets)-1 {
					here[t.MAC] = true
				}
			}
			return here, nil
		}
	}
	presenceTracker.Load()
	whileRouter(presenceTracker.Run)
	netAPI.FirewallCounters = func() (map[string]netapi.Counter, bool) {
		raw, ok := network.ReadFirewallCounters()
		if !ok {
			return nil, false
		}
		out := map[string]netapi.Counter{}
		for id, n := range raw {
			out[id] = netapi.Counter{Packets: n.Packets, Bytes: n.Bytes}
		}
		return out, true
	}
	netAPI.Connections = func() ([]network.Connection, int, int, bool) {
		network.EnableConntrackAccounting() // from now on connections carry their byte counts
		conns, ok := network.ReadConntrack()
		count, max := network.ConntrackLimits()
		return conns, count, max, ok
	}
	mux.HandleFunc("/api/v1/firewall/connections", requireAuth(routerOnly(netAPI.HandleConnections)))
	if fwLogStore != nil { // a nil *Store inside the interface would not be nil
		netAPI.FirewallLog = fwLogStore
	}
	mux.HandleFunc("/api/v1/firewall/log", requireAuth(routerOnly(netAPI.HandleFirewallLog)))
	mux.HandleFunc("/api/v1/firewall/log.csv", requireAuth(routerOnly(netAPI.HandleFirewallLogCSV)))
	mux.HandleFunc("/api/v1/firewall", requireAuth(routerOnly(netAPI.HandleFirewall)))
	mux.HandleFunc("/api/v1/firewall/counters", requireAuth(routerOnly(netAPI.HandleFirewallCounters)))
	mux.HandleFunc("/api/v1/firewall/order", requireAuth(routerOnly(netAPI.HandleFirewallOrder)))
	mux.HandleFunc("/api/v1/firewall/rules", requireAuth(routerOnly(netAPI.HandleFirewallRules)))
	mux.HandleFunc("/api/v1/firewall/rules/{id}", requireAuth(routerOnly(netAPI.HandleFirewallRule)))
	mux.HandleFunc("/api/v1/firewall/forwards", requireAuth(routerOnly(netAPI.HandleFirewallForwards)))
	mux.HandleFunc("/api/v1/firewall/forwards/{id}", requireAuth(routerOnly(netAPI.HandleFirewallForward)))
	netAPI.DDNSStatuses = ddnsUpdater.Statuses
	netAPI.DDNSUpdate = ddnsUpdater.UpdateNow
	mux.HandleFunc("/api/v1/ddns", requireAuth(routerOnly(netAPI.HandleDDNS)))
	mux.HandleFunc("/api/v1/ddns/{id}", requireAuth(routerOnly(netAPI.HandleDDNSEntry)))
	mux.HandleFunc("/api/v1/ddns/{id}/update", requireAuth(routerOnly(netAPI.HandleDDNSUpdate)))
	// VPN: site tunnels and remote access with WireGuard. The tunnels are in /etc/wireguard and survive a restart of the router only
	// because the daemon brings them up again here.
	wgReader := &wg.Reader{}
	netAPI.WireGuard = wgReader.Interfaces
	mux.HandleFunc("/api/v1/vpn", requireAuth(routerOnly(netAPI.HandleVPN)))
	mux.HandleFunc("/api/v1/vpn/sites", requireAuth(routerOnly(netAPI.HandleVPNSites)))
	mux.HandleFunc("/api/v1/vpn/sites/{id}", requireAuth(routerOnly(netAPI.HandleVPNSite)))
	mux.HandleFunc("/api/v1/vpn/remote", requireAuth(routerOnly(netAPI.HandleVPNRemote)))
	mux.HandleFunc("/api/v1/vpn/remote/clients", requireAuth(routerOnly(netAPI.HandleVPNClients)))
	mux.HandleFunc("/api/v1/vpn/remote/clients/{id}", requireAuth(routerOnly(netAPI.HandleVPNClient)))
	mux.HandleFunc("/api/v1/vpn/remote/clients/{id}/file", requireAuth(routerOnly(netAPI.HandleVPNClientTemplate)))
	ovReader := &ovpn.Reader{}
	netAPI.OVConnections = ovReader.Connections
	mux.HandleFunc("/api/v1/vpn/ca/renew-server", requireAuth(routerOnly(netAPI.HandleVPNCARenewServer)))
	mux.HandleFunc("/api/v1/vpn/ov-remote", requireAuth(routerOnly(netAPI.HandleVPNOVRemote)))
	mux.HandleFunc("/api/v1/vpn/ov-remote/clients", requireAuth(routerOnly(netAPI.HandleVPNOVClients)))
	mux.HandleFunc("/api/v1/vpn/ov-remote/clients/{id}", requireAuth(routerOnly(netAPI.HandleVPNOVClient)))
	if !c.Dev {
		for _, script := range []string{translator.PathWGSync, translator.PathOVSync} {
			if _, err := os.Stat(script); err == nil {
				go func(script string) {
					if out, err := runner.Run(context.Background(), "sh", script); err != nil {
						log.Printf("[-] Bringing the VPN tunnels up failed (%s): %v %s", script, err, out)
					}
				}(script)
			}
		}
	}
	mux.HandleFunc("/api/v1/dns", requireAuth(routerOnly(netAPI.HandleDNS)))
	mux.HandleFunc("/api/v1/dns/upstream", requireAuth(routerOnly(netAPI.HandleDNSUpstream)))
	mux.HandleFunc("/api/v1/dns/networks/{id}", requireAuth(routerOnly(netAPI.HandleDNSNetwork)))
	mux.HandleFunc("/api/v1/dns/hosts", requireAuth(routerOnly(netAPI.HandleDNSHosts)))
	mux.HandleFunc("/api/v1/dns/hosts/{id}", requireAuth(routerOnly(netAPI.HandleDNSHost)))
	mux.HandleFunc("/api/v1/dns/forwards", requireAuth(routerOnly(netAPI.HandleDNSForwards)))
	mux.HandleFunc("/api/v1/dns/forwards/{id}", requireAuth(routerOnly(netAPI.HandleDNSForward)))
	mux.HandleFunc("/api/v1/uplinks", requireAuth(routerOnly(netAPI.HandleUplinks)))
	mux.HandleFunc("/api/v1/uplinks/{id}", requireAuth(routerOnly(netAPI.HandleUplink)))
	mux.HandleFunc("/api/v1/uplinks/{id}/health", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "wan" {
			http.NotFound(w, r)
			return
		}
		wanProbe.HandleHistory(w, r)
	}))
	mux.HandleFunc("/api/v1/ports", requireAuth(routerOnly(netAPI.HandlePorts)))
	mux.HandleFunc("/api/v1/networks", requireAuth(routerOnly(netAPI.HandleNetworks)))
	mux.HandleFunc("/api/v1/networks/{id}", requireAuth(routerOnly(netAPI.HandleNetwork)))

	// System DHCP List Endpoint
	mux.HandleFunc("/api/v1/dhcp/leases", requireAuth(network.LeaseHandler(func(ctx context.Context) []network.ClientLease { return clientList(ctx) }, func(ctx context.Context, leases []network.ClientLease) {
		presenceTracker.Annotate(ctx, leases)
		active := cfgMgr.NetworkConfig()
		for i := range leases {
			leases[i].Nickname = clientNames.Get(leases[i].MACAddress)
		}
		network.LabelNetworks(leases, active)
		if draft, ok := cfgMgr.StagedNetworkConfig(); ok {
			network.MarkReservationChanges(leases, active, draft)
		}
	})))

	c.Serve()
}

// The PeakRoute router: everything in internal/daemon that every build shares, and the parts only a router has (the internet
// connection, local networks, ports, routes, firewall, DNS and DHCP, VPN, Dynamic DNS and the clients). The access point is
// cmd/peakroute-ap.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"peakroute/internal/clientnames"
	"peakroute/internal/config"
	"peakroute/internal/daemon"
	"peakroute/internal/ddns"
	"peakroute/internal/devhistory"
	"peakroute/internal/devices"
	"peakroute/internal/failover"
	"peakroute/internal/filtering"
	"peakroute/internal/forget"
	"peakroute/internal/fwlog"
	"peakroute/internal/lldp"
	"peakroute/internal/netapi"
	"peakroute/internal/netdevices"
	"peakroute/internal/network"
	"peakroute/internal/ovpn"
	"peakroute/internal/presence"
	"peakroute/internal/speedtest"
	"peakroute/internal/timeserver"
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
	// one prober for each internet connection. Each asks about its own connection (the view of the configuration that is its own: first or
	// second), and, when there are two, sends its probes out of that connection's interface only.
	newProber := func(id string) *wanhealth.Prober {
		view := func() (config.NetworkConfig, bool) { return cfgMgr.NetworkConfig().WanView(id) }
		return &wanhealth.Prober{
			Store: healthStore,
			ID:    id,
			Pinger: &wanhealth.AutoPinger{Device: func() string {
				if cfg := cfgMgr.NetworkConfig(); cfg.HasWan2() {
					if v, ok := cfg.WanView(id); ok {
						return v.WanDevice()
					}
				}
				return "" // a router with one connection probes as it always did
			}},
			Every:   5 * time.Second,
			Timeout: 2 * time.Second,
			EveryFn: func() time.Duration {
				v, _ := view()
				return v.ProbeSettings().Every
			},
			DownAfter: func() int {
				v, _ := view()
				return v.ProbeSettings().DownAfter
			},
			Target: func() (wanhealth.Target, bool) {
				cfg, ok := view()
				if !ok || cfg.WanInterface == "" {
					return wanhealth.Target{}, false
				}
				resolvers, source := network.UpstreamResolvers(cfg, os.ReadFile)
				if len(resolvers) == 0 {
					return wanhealth.Target{}, false
				}
				return wanhealth.Target{Addr: resolvers[0], Source: source}, true
			},
			Gateway: func() (wanhealth.Target, bool) {
				cfg, ok := view()
				if !ok || cfg.WanInterface == "" {
					return wanhealth.Target{}, false
				}
				gw, ok := network.UpstreamGateway(cfg, os.ReadFile)
				return wanhealth.Target{Addr: gw, Source: "default gateway"}, ok
			},
			// the addresses the administrator added, and the public ones unless they are turned off
			Extra: func() []wanhealth.Target {
				cfg, ok := view()
				if !ok || cfg.WanInterface == "" {
					return nil
				}
				set := cfg.ProbeSettings()
				var out []wanhealth.Target
				for _, a := range set.Targets {
					out = append(out, wanhealth.Target{Addr: a, Source: "added by you"})
				}
				if set.Fallbacks {
					for _, s := range config.PublicProbeTargets {
						if a, err := netip.ParseAddr(s); err == nil {
							out = append(out, wanhealth.Target{Addr: a, Source: "public address"})
						}
					}
				}
				return out
			},
		}
	}
	wanProbe := newProber(config.WanFirst)
	wanProbe2 := newProber(config.WanSecond)
	whileRouter(wanProbe.Run)
	go daemon.RunWhile(pollerCtx, routerWorkEvery, func() bool { return isRouter() && cfgMgr.NetworkConfig().HasWan2() }, wanProbe2.Run)

	// with two connections: the traffic goes through the preferred one, and through the other while that one is down
	failoverCtl := &failover.Controller{
		Config: cfgMgr.NetworkConfig,
		State: func(id string) string {
			if id == config.WanSecond {
				return wanProbe2.Status().State
			}
			return wanProbe.Status().State
		},
		Runner: runner,
	}
	// (it runs for every router: with no second connection it only takes away what the second left behind)
	whileRouter(failoverCtl.Run)
	failoverCtl.EventsPath = filepath.Join(configDir, "failover_events.json")
	netAPI.FailoverPin = failoverCtl.Pin
	netAPI.FailoverStatus = func(cfg config.NetworkConfig) (failover.Status, int) {
		return failoverCtl.Status(), failoverCtl.RecoveringIn(cfg)
	}

	// answer time questions on the networks that serve the time (the router's own clock follows its time servers; this repeats it)
	timeServer := &timeserver.Server{
		Addrs: func() []string { return cfgMgr.NetworkConfig().NTPAddrs() },
		Clock: func(ctx context.Context) timeserver.State {
			st, ok := network.ReadTimeStatus(ctx)
			if !ok || !st.Synchronized {
				return timeserver.State{}
			}
			stratum := st.Stratum + 1 // one further from the source than the server the router follows
			if st.Stratum == 0 {
				stratum = 3 // synchronised, but the stratum could not be read
			}
			return timeserver.State{Synchronized: true, Stratum: stratum, Upstream: st.ServerIP, LastSync: time.Now()}
		},
	}
	whileRouter(timeServer.Run)

	// AdGuard Home's own web interface (loopback only, no accounts of its own) is offered behind the router's sign-in, while a network filters
	filterProxy := &filtering.Server{
		Proxy: &filtering.Proxy{Sessions: c.Sessions, MainPort: c.Port},
		TLS:   &tls.Config{GetCertificate: c.Certs.GetCertificate, MinVersion: tls.VersionTLS12},
	}
	go daemon.RunWhile(pollerCtx, routerWorkEvery, func() bool { return isRouter() && cfgMgr.NetworkConfig().FilterOn() }, filterProxy.Run)
	netAPI.FilteringStatus = func(ctx context.Context) filtering.Status { return filtering.ReadStatus(ctx, filtering.Binary, "") }

	// Keep the names of the Dynamic DNS page pointed at the internet connection's address
	ddnsUpdater := &ddns.Updater{
		Entries: func() []config.DDNSEntry { return cfgMgr.NetworkConfig().DDNSSettings().Entries },
		WAN: func() (netip.Addr, bool) {
			// with two connections: the address of the one that carries the traffic (the name is kept pointed at what works)
			cfg := cfgMgr.NetworkConfig()
			id := config.WanFirst
			if cfg.HasWan2() {
				if a := failoverCtl.Status().Active; a != "" {
					id = a
				}
			}
			v, ok := cfg.WanView(id)
			if !ok {
				return netip.Addr{}, false
			}
			up, addr, _ := network.WANConnection(v.WanDevice())
			if !up || addr == "" {
				return netip.Addr{}, false
			}
			host, _, _ := strings.Cut(addr, "/")
			ip, err := netip.ParseAddr(host)
			return ip, err == nil
		},
	}
	whileRouter(ddnsUpdater.Run)
	// when the traffic moves to the other connection: the names and the tunnels follow it at once, a few seconds after the routes changed
	failoverCtl.OnChange = func(failover.Event) {
		time.Sleep(3 * time.Second)
		failover.MoveServices(pollerCtx, runner)
		ddnsUpdater.Moved(pollerCtx)
	}

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
	// The nicknames are given to AdGuard Home as named clients while it runs, so its log and statistics say "Alec's laptop"
	clientSync := &filtering.ClientSync{
		Names: clientNames.All,
		Devices: func(ctx context.Context) []filtering.Device {
			var out []filtering.Device
			for _, l := range clientList(ctx) {
				out = append(out, filtering.Device{MAC: l.MACAddress, IP: l.IPAddress})
			}
			return out
		},
		StatePath: filepath.Join(configDir, "adguard_clients.json"),
		Log:       log.Printf,
	}
	go daemon.RunWhile(pollerCtx, routerWorkEvery, func() bool { return isRouter() && cfgMgr.NetworkConfig().FilterOn() }, clientSync.Run)
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
	mux.HandleFunc("/api/v1/filtering", requireAuth(routerOnly(netAPI.HandleFiltering)))
	mux.HandleFunc("/api/v1/sqm", requireAuth(routerOnly(netAPI.HandleSQM)))
	mux.HandleFunc("/api/v1/sqm/speedtest", requireAuth(routerOnly(netAPI.HandleSQMSpeedTest)))
	// The speed test measures the line, not the shaping: while shaping runs it is taken off for the test (about twenty seconds) and put
	// back after, whatever the outcome (the helper reads the committed configuration, so "back" is what is committed)
	netAPI.SpeedTest = func(ctx context.Context, line string, progress func(string)) (speedtest.Result, error) {
		cfg := cfgMgr.NetworkConfig()
		if cfg.SQMAnyActive() && !c.Dev {
			runner.Run(ctx, translator.PathSQMGo, "stop")
			defer func() { runner.Run(context.WithoutCancel(ctx), translator.PathSQMGo, "reconcile") }()
		}
		// with two connections the test goes out of the one that is measured, whichever carries the traffic
		t := &speedtest.Tester{}
		if v, ok := cfg.WanView(line); ok && cfg.HasWan2() {
			t.Device = v.WanDevice()
		}
		return t.Run(ctx, progress)
	}
	netAPI.SQMStatus = func(ctx context.Context, line string) network.SQMStatus {
		cfg := cfgMgr.NetworkConfig()
		v, ok := cfg.WanView(line)
		if !ok {
			return network.SQMStatus{}
		}
		if line == config.WanSecond {
			return network.ReadSQMStatusOf(ctx, v.WanDevice(), network.SQMIfb2)
		}
		return network.ReadSQMStatus(ctx, v.WanDevice())
	}
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
	mux.HandleFunc("/api/v1/failover", requireAuth(routerOnly(netAPI.HandleFailover)))
	mux.HandleFunc("/api/v1/failover/switch", requireAuth(routerOnly(netAPI.HandleFailoverSwitch)))
	mux.HandleFunc("/api/v1/uplinks/{id}", requireAuth(routerOnly(netAPI.HandleUplink)))
	mux.HandleFunc("/api/v1/uplinks/{id}/health", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case config.WanFirst:
			wanProbe.HandleHistory(w, r)
		case config.WanSecond:
			if !cfgMgr.NetworkConfig().HasWan2() {
				http.NotFound(w, r)
				return
			}
			wanProbe2.HandleHistory(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	mux.HandleFunc("/api/v1/ports", requireAuth(routerOnly(netAPI.HandlePorts)))
	mux.HandleFunc("/api/v1/networks", requireAuth(routerOnly(netAPI.HandleNetworks)))
	mux.HandleFunc("/api/v1/networks/{id}", requireAuth(routerOnly(netAPI.HandleNetwork)))

	// System DHCP List Endpoint
	annotateClients := func(ctx context.Context, leases []network.ClientLease) {
		presenceTracker.Annotate(ctx, leases)
		active := cfgMgr.NetworkConfig()
		for i := range leases {
			leases[i].Nickname = clientNames.Get(leases[i].MACAddress)
		}
		network.LabelNetworks(leases, active)
		if draft, ok := cfgMgr.StagedNetworkConfig(); ok {
			network.MarkReservationChanges(leases, active, draft)
		}
	}
	mux.HandleFunc("/api/v1/dhcp/leases", requireAuth(network.LeaseHandler(func(ctx context.Context) []network.ClientLease { return clientList(ctx) }, annotateClients)))

	// What each device did over time (docs/device-pages.md, phase 1): bytes sent and received each minute, and when it came and went. A device's
	// page reads it. Without the database (it could not be opened) the page still works, without graphs.
	devHistory, err := devhistory.Open(filepath.Join(configDir, "device_history.db"))
	if err != nil {
		log.Printf("[!] Device history unavailable: %v", err)
		devHistory = nil
	} else {
		c.OnClose(func() { devHistory.Close() })
		sampler := &devhistory.Sampler{
			Store:      devHistory,
			Directions: usage.Directions,
			Clients: func(ctx context.Context) []devhistory.Client {
				leases := clientList(ctx)
				annotateClients(ctx, leases)
				out := make([]devhistory.Client, 0, len(leases))
				for _, l := range leases {
					out = append(out, devhistory.Client{MAC: l.MACAddress, IP: l.IPAddress, Status: l.Status, Connection: l.Connection, SSID: l.SSID, Band: l.Band, Network: l.Network})
				}
				return out
			},
		}
		whileRouter(sampler.Run)
	}
	// What lldpd has heard on the LAN ports (docs/device-pages.md, phase 2), and where the router's bridge has learned each device: asked at most
	// every few seconds, however many pages ask
	var nbMu sync.Mutex
	var nbAt, fdbAt time.Time
	var nbCache []lldp.Neighbour
	var nbErr error
	var fdbCache map[string]network.Attachment
	neighbours := func(ctx context.Context) ([]lldp.Neighbour, error) {
		nbMu.Lock()
		defer nbMu.Unlock()
		if time.Since(nbAt) > 5*time.Second {
			nbCache, nbErr = lldp.Read(ctx, runner)
			nbAt = time.Now()
		}
		return nbCache, nbErr
	}
	bridgeTable := func(ctx context.Context) map[string]network.Attachment {
		nbMu.Lock()
		defer nbMu.Unlock()
		if time.Since(fdbAt) > 5*time.Second || fdbCache == nil {
			fdbCache = network.ReadBridgeFDB(ctx, runner)
			fdbAt = time.Now()
		}
		return fdbCache
	}
	netAPI.LLDPNeighbours = neighbours
	mux.HandleFunc("/api/v1/lldp", requireAuth(routerOnly(netAPI.HandleLLDP)))
	// Switches and access points the router watches (docs/device-pages.md, phase 3): read over SNMP, and each says which port has which device
	var watched *netdevices.Service
	if wstore, err := netdevices.OpenStore(filepath.Join(configDir, "network_devices.json")); err != nil {
		log.Printf("[!] Watched devices unavailable: %v", err)
	} else {
		watched = &netdevices.Service{Store: wstore, Drivers: map[string]netdevices.Driver{netdevices.KindSNMP: &netdevices.SNMPDriver{}}}
		whileRouter(watched.Run)
		mux.HandleFunc("/api/v1/network-devices", requireAuth(routerOnly(watched.HandleCollection)))
		mux.HandleFunc("/api/v1/network-devices/{id}", requireAuth(routerOnly(watched.HandleItem)))
		mux.HandleFunc("/api/v1/network-devices/{id}/poll", requireAuth(routerOnly(watched.HandlePoll)))
	}
	deviceSvc := &devices.Service{
		Where: func(ctx context.Context, mac string) *devices.Where {
			att, ok := bridgeTable(ctx)[strings.ToLower(mac)]
			if !ok {
				return nil
			}
			w := &devices.Where{Port: att.Port, Bridge: att.Bridge, VLAN: att.VLAN}
			if cfgMgr.NetworkConfig().LLDP {
				if ns, err := neighbours(ctx); err == nil {
					for _, n := range ns {
						if n.LocalPort == att.Port {
							n := n
							w.Neighbour = &n
							break
						}
					}
				}
			}
			return w
		},
		List: func(ctx context.Context) []network.ClientLease {
			leases := clientList(ctx)
			annotateClients(ctx, leases)
			return leases
		},
		History:  devHistory,
		Stations: c.Stations.Get,
	}
	if watched != nil {
		deviceSvc.Edge = watched.Locate
	}
	mux.HandleFunc("GET /api/v1/clients/{mac}", requireAuth(routerOnly(deviceSvc.HandleClient)))
	mux.HandleFunc("GET /api/v1/clients/{mac}/history", requireAuth(routerOnly(deviceSvc.HandleHistory)))

	// Removing a device that is gone from the Clients list: its lease, its neighbour (ARP) entry, the presence memory and its nickname
	leaseFile := network.LeasePaths[len(network.LeasePaths)-1]
	for _, p := range network.LeasePaths {
		if _, err := os.Stat(p); err == nil {
			leaseFile = p
			break
		}
	}
	forgetSvc := &forget.Service{
		List: func(ctx context.Context) []network.ClientLease {
			leases := clientList(ctx)
			annotateClients(ctx, leases)
			return leases
		},
		Interface: func(l network.ClientLease) string {
			return translator.InterfaceOfNetwork(cfgMgr.NetworkConfig(), l.Network)
		},
		Run:       runner.Run,
		Leases:    func() []network.ClientLease { return network.ReadLeases(network.LeasePaths) },
		LeaseFile: leaseFile,
		Real:      !c.Dev,
		Unreserve: func(mac string) (bool, error) {
			draft, _ := cfgMgr.StagedNetworkConfig()
			has := false
			for _, r := range draft.Reservations {
				has = has || strings.EqualFold(r.MAC, mac)
			}
			if !has {
				return false, nil
			}
			if netAPI.Editable != nil && !netAPI.Editable() {
				return false, errors.New("a change is being applied or waits for confirmation, so remove it there once that is done")
			}
			err := cfgMgr.StageNetwork(func(c *config.NetworkConfig) error {
				var keep []config.Reservation
				for _, r := range c.Reservations {
					if !strings.EqualFold(r.MAC, mac) {
						keep = append(keep, r)
					}
				}
				c.Reservations = keep
				return nil
			})
			return err == nil, err
		},
		ForgetSeen: func(mac string) {
			presenceTracker.Forget(mac)
			if devHistory != nil {
				devHistory.Forget(mac) // and what its page kept
			}
		},
		DropNickname: func(mac string) error { return clientNames.Set(mac, "") },
	}
	mux.HandleFunc("/api/v1/clients/{mac}", requireAuth(routerOnly(forgetSvc.HandleDevice)))

	c.Serve()
}

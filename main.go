package main

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"peakroute/internal/auth"
	"peakroute/internal/backup"
	"peakroute/internal/bootstrap"
	"peakroute/internal/cert"
	"peakroute/internal/config"
	"peakroute/internal/console"
	"peakroute/internal/ddns"
	"peakroute/internal/fsutil"
	"peakroute/internal/fwlog"
	"peakroute/internal/netapi"
	"peakroute/internal/network"
	"peakroute/internal/ovpn"
	"peakroute/internal/pipeline"
	"peakroute/internal/presence"
	"peakroute/internal/setup"
	"peakroute/internal/syslogfwd"
	"peakroute/internal/system"
	"peakroute/internal/translator"
	"peakroute/internal/update"
	"peakroute/internal/wanhealth"
	"peakroute/internal/wg"
	"peakroute/internal/wireless"
)

//go:embed web/templates/* web/static/*
var embeddedFiles embed.FS

var (
	devMode      bool
	configDir    string
	systemRoot   string // "" on the appliance; a sandbox directory in -dev so dev never touches the host's /etc
	listenPort   string
	tmpl         *template.Template
	cfgMgr       *config.Manager
	configSvc    *pipeline.Service
	sessionMgr   *auth.SessionManager
	loginLimiter *auth.RateLimiter
)

func initAppConfig() {
	flag.BoolVar(&devMode, "dev", false, "Run in local development mode")
	flag.StringVar(&listenPort, "port", "", "Port to listen on (defaults: 8443 for dev, 443/8443 for prod)")
	flag.Parse()

	mime.AddExtensionType(".css", "text/css")
	mime.AddExtensionType(".js", "application/javascript")

	if listenPort != "" && listenPort[0] != ':' {
		listenPort = ":" + listenPort
	}

	if devMode {
		// Keep dev state out of the repo root (dev-data/ is gitignored)
		configDir = "./dev-data"
		if err := os.MkdirAll(configDir, 0700); err != nil {
			log.Fatalf("[-] Failed to create dev data directory: %v", err)
		}
		cert.EnableDevPaths(filepath.Join(configDir, "certs"))

		// Dev runs on a developer's own machine: system files are read from and (later) written to
		// a sandbox under dev-data, never the host's real /etc
		systemRoot = filepath.Join(configDir, "sysroot")

		if listenPort == "" {
			listenPort = ":8443"
		}
		fmt.Println("[+] Running in DEVELOPMENT mode (HTTPS)")

	} else {
		configDir = "/etc/peakroute"

		if listenPort == "" {
			listenPort = ":443"
		}
		fmt.Println("[+] Running in PRODUCTION mode (HTTPS)")
	}
}

func main() {
	initAppConfig()

	var err error

	// A restore that included the history left the databases in a directory of their own (they were in use): swap them in now,
	// before anything opens them.
	if done := backup.ApplyPending(configDir); len(done) > 0 {
		log.Printf("[+] Restored from a backup: %s", strings.Join(done, ", "))
	}

	// The config manager is the single owner of auth, network and system settings: it loads
	// them from configDir once and everything else reads them from memory
	cfgMgr, err = config.NewManager(configDir)
	if err != nil {
		log.Fatalf("[-] Failed to initialize config manager: %v", err)
	}

	// Record this build's version/arch/kernel in sys.json (only writes if it changed)
	if err := cfgMgr.SyncBuildMetadata(); err != nil {
		log.Printf("Warning: failed to save sys.json: %v", err)
	}

	// Router mode: fill in what network.json leaves blank (LAN defaults to 192.168.1.1/24) and
	// import an existing DHCP range from dnsmasq so a wizard-configured box keeps it. This changes
	// network.json only; nothing is applied to the system.
	if err := cfgMgr.AdoptNetwork(func(cfg *config.NetworkConfig) bool {
		return translator.AdoptExisting(cfg, translator.RootFS(systemRoot), cfg.OperationMode())
	}); err != nil {
		log.Printf("Warning: could not adopt existing network settings: %v", err)
	}

	// A crash between writing a file and putting it in place leaves a ".name.123.tmp" next to it: clear the old ones out of the
	// directories the daemon writes to (its own, the certificates', and those of the files the configuration makes).
	sweepDirs := map[string]bool{configDir: true, filepath.Dir(cert.CertPath): true}
	if d, err := translator.Render(cfgMgr.NetworkConfig(), ""); err == nil {
		for _, a := range d.Artifacts {
			sweepDirs[filepath.Join(systemRoot, filepath.Dir(a.Path))] = true
		}
		for _, m := range d.ManagedDirs {
			sweepDirs[filepath.Join(systemRoot, m.Path)] = true
		}
	}
	for dir := range sweepDirs {
		if n := fsutil.SweepStale(dir, time.Minute); n > 0 {
			log.Printf("[+] Removed %d temporary file(s) a crash left in %s", n, dir)
		}
	}

	// The commit/confirm/revert workflow for network changes. On the appliance it writes the real
	// system files and runs real commands; in -dev it works on the sandbox root and only logs the
	// commands, so a developer's own machine is never reconfigured.
	var runner translator.Runner = translator.ExecRunner{}
	if devMode {
		runner = &translator.LogRunner{}
	}
	// The setup network: until a local network is committed the wired ports are joined in a temporary bridge with an address, so
	// the welcome page can be reached (docs/setup.md). It lets go of the ports before a commit or a revert touches them and is
	// brought back afterwards if there is still no local network.
	setupNet := &bootstrap.Service{Run: runner, Manager: cfgMgr, Ports: network.GetPhysicalInterfaces, Dir: "/run/peakroute-setup"}
	if devMode {
		setupNet.Dir = filepath.Join(systemRoot, "run", "peakroute-setup")
	}
	configSvc = pipeline.New(pipeline.Options{
		Manager:  cfgMgr,
		FS:       translator.RootFS(systemRoot),
		Runner:   runner,
		StateDir: configDir,
		Before:   setupNet.Stop,
		Settled:  setupNet.Reconcile,
	})
	if err := configSvc.Resume(); err != nil {
		log.Printf("Warning: could not resume a pending configuration change: %v", err)
	}
	setupNet.Reconcile()

	// Initialize session tracking and rate limiting
	sessionMgr = auth.NewSessionManager(cfgMgr)
	loginLimiter = auth.NewRateLimiter(5, 2*time.Minute)

	// Initialize traffic tracking & time-series database
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
	dbPath := filepath.Join(configDir, "traffic_history.db")
	trafficDB, err := system.NewTrafficDB(dbPath)
	if err != nil {
		log.Fatalf("[-] Failed to initialize traffic database: %v", err)
	}
	system.SetTrafficDB(trafficDB)

	// Start 1-second background poller for traffic metrics
	pollerCtx, cancelPoller := context.WithCancel(context.Background())
	defer cancelPoller()
	go startBackgroundNetworkPoller(pollerCtx)
	go usage.Run(pollerCtx, 10*time.Second)

	// Watch the internet connection: ping the upstream DNS resolver every few seconds
	healthStore, err := wanhealth.Open(filepath.Join(configDir, "wan_health.db"))
	if err != nil {
		log.Fatalf("[-] Failed to initialize WAN health history: %v", err)
	}
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
	go wanProbe.Run(pollerCtx)

	// Send the logs to syslog servers, as the configuration in effect says
	syslogForwarder := &syslogfwd.Forwarder{Rule: func(id string) (string, string, bool) {
		rule, ok := cfgMgr.NetworkConfig().FirewallSettings().FindRule(id)
		return rule.Name, rule.Action, ok
	}}
	syslogForwarder.Start(pollerCtx)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			syslogForwarder.Update(syslogConfig())
			select {
			case <-pollerCtx.Done():
				return
			case <-tick.C:
			}
		}
	}()

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
	go ddnsUpdater.Run(pollerCtx)

	// Keep the firewall log: the packets that rules with logging matched, read from the kernel log into a database
	// of their own (the journal is shared and size limited)
	fwLogStore, err := fwlog.Open(filepath.Join(configDir, "firewall_log.db"))
	if err != nil {
		log.Printf("[!] Firewall log unavailable: %v", err)
	} else {
		defer fwLogStore.Close()
		go (&fwlog.Collector{Store: fwLogStore, Source: fwlog.JournalSource, OnPacket: syslogForwarder.Packet}).Run(pollerCtx)
	}

	// Drop timed-out web sessions in the background
	sessionMgr.StartJanitor(pollerCtx, time.Minute)

	// The web interface's certificate: made by this router's own web authority for the names and addresses the router has now (unless
	// a custom one was uploaded), checked again every twelve hours, and served without a restart when it changes.
	certMgr := &cert.Manager{Wanted: func() cert.Names {
		cfg := cfgMgr.NetworkConfig()
		var n cert.Names
		host := cfg.Hostname
		if host == "" {
			host, _ = os.Hostname()
		}
		if host != "" {
			n.DNS = append(n.DNS, host)
			if cfg.Domain != "" {
				n.DNS = append(n.DNS, host+"."+cfg.Domain)
			}
		}
		for _, a := range append([]string{cfg.LanIP}, func() []string {
			var ips []string
			for _, nw := range cfg.Networks {
				ips = append(ips, nw.IP)
			}
			return ips
		}()...) {
			if ip := net.ParseIP(a); ip != nil {
				n.IPs = append(n.IPs, ip)
			}
		}
		return n
	}}
	if err := certMgr.Ensure(); err != nil {
		log.Fatalf("[-] TLS initialization error: %v", err)
	}
	go func() {
		for {
			select {
			case <-pollerCtx.Done():
				return
			case <-time.After(12 * time.Hour):
				if err := certMgr.Ensure(); err != nil {
					log.Printf("[-] Checking the web interface's certificate failed: %v", err)
				}
			}
		}
	}()

	// Parse embedded HTML templates
	tmpl, err = template.ParseFS(embeddedFiles, "web/templates/*")
	if err != nil {
		log.Fatalf("[-] Failed to parse embedded templates: %v", err)
	}

	mux := http.NewServeMux()

	// Mount embedded static assets (CSS/JS) on /static/
	staticFS, err := fs.Sub(embeddedFiles, "web/static")
	if err != nil {
		log.Fatalf("[-] Failed to create static sub-FS: %v", err)
	}
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Register API endpoints. Every /api/v1/* route except login/setup goes through requireAuth.
	mux.HandleFunc("/api/v1/certificates", requireAuth(certMgr.HandleStatus))
	mux.HandleFunc("/api/v1/certificates/names", requireAuth(certMgr.HandleExtraNames))
	mux.HandleFunc("/api/v1/certificates/regenerate", requireAuth(certMgr.HandleRegenerate))
	mux.HandleFunc("/api/v1/certificates/upload", requireAuth(certMgr.HandleUpload))
	mux.HandleFunc("/api/v1/certificates/authority.crt", requireAuth(certMgr.HandleAuthority))

	// System Stats Telemetry Endpoint
	mux.HandleFunc("/api/v1/system/stats", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		system.HandleSystemStats(w, r, cfgMgr)
	}))

	// Historical Traffic Graph Data Endpoint
	mux.HandleFunc("/api/v1/system/traffic", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if trafficDB == nil {
			http.Error(w, "Traffic database not initialized", http.StatusServiceUnavailable)
			return
		}

		trafficDB.HandleTrafficHistory(w, r)
	}))

	// System Logs Endpoint
	mux.HandleFunc("/api/v1/system/logs", requireAuth(system.HandleGetLogs))

	// Editing the network: port roles and per-network settings. Saving stages a draft; edits are
	// refused while a change is being applied or waits for confirmation.
	// The administrator of the first-run wizard is also the login of the machine's console (a developer's machine only logs it)
	var consoleAccounts console.Accounts = console.System{}
	if devMode {
		consoleAccounts = console.Dev{}
	}
	// An access point has no internet connection, networks, firewall, VPN or DHCP of its own: the interface of those pages says so
	routerOnly := func(h http.HandlerFunc) http.HandlerFunc {
		return netapi.RouterOnly(func() bool { return cfgMgr.NetworkConfig().IsAP() }, h)
	}
	netAPI := &netapi.API{Manager: cfgMgr, Ports: network.GetPhysicalInterfaces, Editable: configSvc.Idle,
		Live: func(active config.NetworkConfig) *netapi.Live {
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
		}}
	netAPI.RoutingTable = func() []netapi.LiveRoute {
		data, err := os.ReadFile("/proc/net/route")
		if err != nil {
			return nil
		}
		return netapi.ClassifyRoutes(network.ParseRoutingTable(data))
	}
	mux.HandleFunc("/api/v1/routes", requireAuth(routerOnly(netAPI.HandleRoutes)))
	mux.HandleFunc("/api/v1/dhcp/reservations", requireAuth(routerOnly(netAPI.HandleReservations)))
	// Wi-Fi adapters and what they can do (read only)
	wifi := wireless.Detector{SysClassNet: "/sys/class/net", Run: func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		return string(out), err
	}}
	mux.HandleFunc("/api/v1/wireless", requireAuth(wifi.Handler))
	stations := &wireless.StationCache{Detector: wifi, TTL: 2 * time.Second}
	// The devices to list: those with a DHCP lease, and those the router sees on its networks that have an address of their own (the
	// neighbour table). -dev has no real neighbours, so it shows two samples: one with an address from the wrong network.
	clientList := func(ctx context.Context) []network.ClientLease {
		leases := network.ReadLeases(network.LeasePaths)
		cfg := cfgMgr.NetworkConfig()
		var neighbours []network.Neighbour
		if devMode {
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
			rep := stations.Get(ctx)
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
	if devMode {
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
	go presenceTracker.Run(pollerCtx)
	netAPI.Wireless = wifi.Detect
	netAPI.Scan = wifi.Scan
	netAPI.Bridge = func() ([]string, bool) { return netapi.InterfaceAddrs("br0") }
	netAPI.Gateway = func() string { return netapi.DefaultGateway("br0") }
	netAPI.Resolvers = netapi.Resolvers
	netAPI.Stations = stations.Get
	netAPI.WifiDev = wifi.Interfaces
	netAPI.Survey = wifi.Survey
	netAPI.Link = wifi.Link
	mux.HandleFunc("/api/v1/wifi", requireAuth(netAPI.HandleWifi))
	// Backup and restore: one encrypted file with the settings, the administrators, the certificates and (if asked) the history;
	// restoring shows what is in it first and puts the settings in the draft (docs/backup.md)
	backupSvc := &backup.Service{Env: backup.Env{
		ConfigDir: configDir, CertDir: filepath.Dir(cert.CertPath), Manager: cfgMgr, Certs: certMgr, Sessions: sessionMgr,
		Ports: network.GetPhysicalInterfaces,
		Radios: func() []string {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var names []string
			for _, a := range wifi.Detect(ctx).Adapters {
				names = append(names, a.Name)
			}
			return names
		},
		Editable: configSvc.Idle,
		Hostname: func() string { h, _ := os.Hostname(); return h },
		Software: cfgMgr.SysConfig().PeakRouteBuild,
	}}
	mux.HandleFunc("/api/v1/backup", requireAuth(backupSvc.HandleInfo))
	mux.HandleFunc("/api/v1/backup/create", requireAuth(backupSvc.HandleCreate))
	mux.HandleFunc("/api/v1/backup/open", requireAuth(backupSvc.HandleOpen))
	mux.HandleFunc("/api/v1/backup/preview", requireAuth(backupSvc.HandlePreview))
	mux.HandleFunc("/api/v1/backup/restore", requireAuth(backupSvc.HandleRestore))
	mux.HandleFunc("/api/v1/backup/copies/{id}", requireAuth(backupSvc.HandleCopy))
	mux.HandleFunc("/api/v1/backup/copies/{id}/restore", requireAuth(backupSvc.HandleCopyRestore))
	// Updates: a new version is written into the other of the two system slots, started on trial and kept (or not) from the web
	// interface (docs/updates.md)
	updateSvc := update.New(configDir, config.BuildVersion, config.DefaultUpdateRepo, configSvc.Idle, backupSvc.SaveCopy)
	if devMode {
		// no disk with two slots here: a simulated one in the development folder, with sample update files to try the page with
		if sim, err := update.NewSimulated(filepath.Join(configDir, "update-sim"), config.BuildVersion, configSvc.Idle, backupSvc.SaveCopy); err != nil {
			log.Printf("Warning: the simulated update slots could not be made: %v", err)
		} else {
			updateSvc = sim
			log.Printf("[+] Updates are simulated: sample update files are in %s", filepath.Join(configDir, "update-sim"))
		}
	}
	go updateSvc.Watch(pollerCtx)
	mux.HandleFunc("/api/v1/updates", requireAuth(updateSvc.HandleState))
	mux.HandleFunc("/api/v1/updates/check", requireAuth(updateSvc.HandleCheck))
	mux.HandleFunc("/api/v1/updates/install", requireAuth(updateSvc.HandleInstall))
	mux.HandleFunc("/api/v1/updates/cancel", requireAuth(updateSvc.HandleCancel))
	mux.HandleFunc("/api/v1/updates/upload", requireAuth(updateSvc.HandleUpload))
	mux.HandleFunc("/api/v1/updates/restart", requireAuth(updateSvc.HandleRestart))
	mux.HandleFunc("/api/v1/updates/keep", requireAuth(updateSvc.HandleKeep))
	mux.HandleFunc("/api/v1/updates/revert", requireAuth(updateSvc.HandleRevert))
	mux.HandleFunc("/api/v1/updates/settings", requireAuth(updateSvc.HandleSettings))
	netAPI.FirewallCounters = func() (map[string]netapi.Counter, bool) {
		raw, ok := network.ReadFirewallCounters()
		if !ok {
			return nil, false
		}
		out := map[string]netapi.Counter{}
		for id, c := range raw {
			out[id] = netapi.Counter{Packets: c.Packets, Bytes: c.Bytes}
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
	mux.HandleFunc("/api/v1/settings", requireAuth(netAPI.HandleSettings))
	mux.HandleFunc("/api/v1/settings/country", requireAuth(netAPI.HandleSettingsCountry))
	mux.HandleFunc("/api/v1/settings/domain", requireAuth(netAPI.HandleSettingsDomain))
	netAPI.TimeStatus = network.ReadTimeStatus
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
	netAPI.LinkUp = func(iface string) bool {
		i, err := net.InterfaceByName(iface)
		return err == nil && i.Flags&net.FlagUp != 0
	}
	mux.HandleFunc("/api/v1/vpn/ca/renew-server", requireAuth(routerOnly(netAPI.HandleVPNCARenewServer)))
	mux.HandleFunc("/api/v1/vpn/ov-remote", requireAuth(routerOnly(netAPI.HandleVPNOVRemote)))
	mux.HandleFunc("/api/v1/vpn/ov-remote/clients", requireAuth(routerOnly(netAPI.HandleVPNOVClients)))
	mux.HandleFunc("/api/v1/vpn/ov-remote/clients/{id}", requireAuth(routerOnly(netAPI.HandleVPNOVClient)))
	if !devMode {
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
	netAPI.SyslogStatuses = syslogForwarder.Statuses
	netAPI.SyslogTest = func(ctx context.Context) []syslogfwd.TestResult {
		return syslogForwarder.SendTest(ctx, "PeakRoute test message")
	}
	mux.HandleFunc("/api/v1/settings/syslog", requireAuth(netAPI.HandleSettingsSyslog))
	mux.HandleFunc("/api/v1/syslog/test", requireAuth(netAPI.HandleSyslogTest))
	mux.HandleFunc("/api/v1/settings/time", requireAuth(netAPI.HandleSettingsTime))
	mux.HandleFunc("/api/v1/settings/hostname", requireAuth(netAPI.HandleSettingsHostname))
	netAPI.ProviderDNS = func() []string {
		var out []string
		for _, a := range network.SystemResolvers(os.ReadFile) {
			out = append(out, a.String())
		}
		return out
	}
	// Administrators: who may sign in, with what role, their passwords and the session timeout. Changes count at once.
	adminAPI := &auth.AdminAPI{Cfg: cfgMgr, Sessions: sessionMgr, Attempts: auth.NewRateLimiter(5, 2*time.Minute), Console: consoleAccounts}
	mux.HandleFunc("/api/v1/admins", requireAuth(adminAPI.HandleAdmins))
	mux.HandleFunc("/api/v1/admins/{username}", requireAuth(adminAPI.HandleAdmin))
	mux.HandleFunc("/api/v1/admins/{username}/password", requireAuth(adminAPI.HandleAdminPassword))
	mux.HandleFunc("/api/v1/admins/{username}/sessions", requireAuth(adminAPI.HandleAdminSessions))
	mux.HandleFunc(auth.OwnPasswordPath, requireAuth(adminAPI.HandleOwnPassword))
	mux.HandleFunc("/api/v1/session/timeout", requireAuth(adminAPI.HandleSessionTimeout))
	adminAPI.Idle = configSvc.Idle
	adminAPI.Power = func(action string) error {
		// a moment later, so that the answer reaches the browser first (-dev only logs the command)
		go func() {
			time.Sleep(2 * time.Second)
			if out, err := runner.Run(context.Background(), "systemctl", action); err != nil {
				log.Printf("[-] systemctl %s failed: %v %s", action, err, out)
			}
		}()
		return nil
	}
	mux.HandleFunc("/api/v1/system/power", requireAuth(adminAPI.HandlePower))
	// A factory reset is done by the initramfs at the next start, before the saved layers are in use (image/overlay/.../peakroute-state):
	// the daemon only leaves a file on the data partition and restarts. -dev has no such partition, so it only says what it would do.
	adminAPI.FactoryReset = func() error {
		if devMode {
			log.Printf("[dry-run] would ask for a factory reset (a file on the data partition) and restart")
			return nil
		}
		if state, _ := os.ReadFile("/run/peakroute/state"); strings.TrimSpace(string(state)) != "persistent" {
			return errors.New("This router started without its data partition, so nothing it keeps is saved and a restart already starts it fresh.")
		}
		if err := fsutil.WriteFileAtomic("/data/factory-reset", []byte("erase everything the router saved\n"), 0600); err != nil {
			return errors.New("The reset could not be asked for: the data partition cannot be written.")
		}
		go func() {
			time.Sleep(2 * time.Second)
			if out, err := runner.Run(context.Background(), "systemctl", "reboot"); err != nil {
				log.Printf("[-] systemctl reboot failed: %v %s", err, out)
			}
		}()
		return nil
	}
	mux.HandleFunc("/api/v1/system/factory-reset", requireAuth(adminAPI.HandleFactoryReset))
	mux.HandleFunc("/api/v1/dns", requireAuth(routerOnly(netAPI.HandleDNS)))
	mux.HandleFunc("/api/v1/dns/lookup", requireAuth(netAPI.HandleDNSLookup))
	mux.HandleFunc("/api/v1/diagnostics", requireAuth(netAPI.HandleDiagnostics))
	mux.HandleFunc("/api/v1/diagnostics/jobs", requireAuth(netAPI.HandleDiagnosticsJobs))
	mux.HandleFunc("/api/v1/diagnostics/jobs/{id}", requireAuth(netAPI.HandleDiagnosticsJob))
	mux.HandleFunc("/api/v1/diagnostics/connect", requireAuth(netAPI.HandleDiagnosticsConnect))
	mux.HandleFunc("/api/v1/dns/upstream", requireAuth(routerOnly(netAPI.HandleDNSUpstream)))
	mux.HandleFunc("/api/v1/dns/networks/{id}", requireAuth(routerOnly(netAPI.HandleDNSNetwork)))
	mux.HandleFunc("/api/v1/dns/hosts", requireAuth(routerOnly(netAPI.HandleDNSHosts)))
	mux.HandleFunc("/api/v1/dns/hosts/{id}", requireAuth(routerOnly(netAPI.HandleDNSHost)))
	mux.HandleFunc("/api/v1/dns/forwards", requireAuth(routerOnly(netAPI.HandleDNSForwards)))
	mux.HandleFunc("/api/v1/dns/forwards/{id}", requireAuth(routerOnly(netAPI.HandleDNSForward)))
	mux.HandleFunc("/api/v1/wifi/radios/{name}", requireAuth(netAPI.HandleWifiRadio))
	mux.HandleFunc("/api/v1/wifi/stations/{name}", requireAuth(netAPI.HandleWifiStation))
	mux.HandleFunc("/api/v1/wifi/scan", requireAuth(netAPI.HandleWifiScan))
	mux.HandleFunc("/api/v1/ap/status", requireAuth(netAPI.HandleAPStatus))
	mux.HandleFunc("/api/v1/ap", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			netAPI.HandleAPSet(w, r)
			return
		}
		netAPI.HandleAP(w, r)
	}))
	mux.HandleFunc("/api/v1/wifi/networks", requireAuth(netAPI.HandleWifiNetworks))
	mux.HandleFunc("/api/v1/wifi/networks/{id}", requireAuth(netAPI.HandleWifiNetwork))
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

	// First-run wizard: open only while the router is not set up (docs/setup.md)
	setupSvc := &setup.Service{Manager: cfgMgr, Ports: network.GetPhysicalInterfaces, Here: setup.HerePorts, DefaultRoute: setup.DefaultRouteDevice,
		Restore: backupSvc, Console: consoleAccounts, Wireless: wifi.Detect, Scan: wifi.Scan, NewSession: sessionMgr.CreateSession, Hostname: func() string { h, _ := os.Hostname(); return h }}
	mux.HandleFunc("/api/v1/setup/info", setupSvc.Guard(setupSvc.HandleInfo))
	mux.HandleFunc("/api/v1/setup/check", setupSvc.Guard(setupSvc.HandleCheck))
	mux.HandleFunc("/api/v1/setup/apply", setupSvc.Guard(setupSvc.HandleApply))
	mux.HandleFunc("/api/v1/setup/wifi/scan", setupSvc.Guard(setupSvc.HandleScan))
	mux.HandleFunc("/api/v1/setup/restore/open", setupSvc.Guard(backupSvc.HandleOpen))
	mux.HandleFunc("/api/v1/setup/restore/preview", setupSvc.Guard(backupSvc.HandlePreviewChoice))
	mux.HandleFunc("/api/v1/setup/restore/apply", setupSvc.Guard(setupSvc.HandleRestoreApply))

	// Staged network changes: review, commit, confirm, revert, discard
	mux.HandleFunc("/api/v1/config/status", requireAuth(configSvc.HandleStatus))
	mux.HandleFunc("/api/v1/config/staged", requireAuth(configSvc.HandleStaged))
	mux.HandleFunc("/api/v1/config/commit", requireAuth(configSvc.HandleCommit))
	mux.HandleFunc("/api/v1/config/confirm", requireAuth(configSvc.HandleConfirm))
	mux.HandleFunc("/api/v1/config/revert", requireAuth(configSvc.HandleRevert))
	mux.HandleFunc("/api/v1/config/discard", requireAuth(configSvc.HandleDiscard))

	// System DHCP List Endpoint
	mux.HandleFunc("/api/v1/dhcp/leases", requireAuth(network.LeaseHandler(func(ctx context.Context) []network.ClientLease { return clientList(ctx) }, func(ctx context.Context, leases []network.ClientLease) {
		presenceTracker.Annotate(ctx, leases)
		active := cfgMgr.NetworkConfig()
		network.LabelNetworks(leases, active)
		if draft, ok := cfgMgr.StagedNetworkConfig(); ok {
			network.MarkReservationChanges(leases, active, draft)
		}
	})))

	// Main application routing logic
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Unconfigured state
		if !cfgMgr.IsConfigured() {
			if r.URL.Path == "/first_run" || r.URL.Path == "/first-run" {
				renderFirstRun(w)
				return
			}

			http.Redirect(w, r, "/first_run", http.StatusSeeOther)
			return
		}

		// Configured state - Login
		if r.URL.Path == "/login" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if err := tmpl.ExecuteTemplate(w, "login.html", nil); err != nil {
				log.Printf("[-] Template execution error (login.html): %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == "/api/v1/login" && r.Method == http.MethodPost {
			clientIP := r.RemoteAddr

			if !loginLimiter.Allow(clientIP) {
				tmpl.ExecuteTemplate(w, "login.html", map[string]string{
					"Error": "Too many login requests. Please wait 2 minutes and try again.",
				})
				return
			}

			handleLogin(w, r)
			return
		}

		// Configured Route - Logout
		if r.URL.Path == "/api/v1/logout" || r.URL.Path == "/logout" {
			handleLogout(w, r)
			return
		}

		// Protected Dashboard
		session, authenticated := sessionMgr.Lookup(r)
		if !authenticated {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		if r.URL.Path == "/" || r.URL.Path == "/dashboard" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")

			sysMode := dashboardMode()

			data := struct {
				SystemMode string
				CSRFToken  string
				Username   string
				ReadOnly   bool
			}{
				SystemMode: sysMode,
				CSRFToken:  session.CSRFToken,
				Username:   session.Username,
				ReadOnly:   !auth.CanWrite(session.Role),
			}

			if err := tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
				log.Printf("[-] Template execution error (dashboard.html): %v", err)
				return
			}
			return
		}

		http.NotFound(w, r)
	})

	secureMux := auth.SecurityHeadersMiddleware(mux)

	// Launch HTTPS server
	server := &http.Server{
		Addr:         listenPort,
		Handler:      secureMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		TLSConfig:    &tls.Config{GetCertificate: certMgr.GetCertificate, MinVersion: tls.VersionTLS12},
	}

	// Capture OS signals for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		fmt.Printf("[+] PeakRoute OS daemon active on https://0.0.0.0%s\n", listenPort)
		if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[-] HTTPS server crashed: %v", err)
		}
	}()

	<-stop
	log.Println("[*] Shutting down PeakRoute OS...")

	// Stop background polling loop
	cancelPoller()

	// An open confirm window stays on disk, so the next start resumes (or reverts) it
	configSvc.Close()

	// Shutdown server gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[-] Server shutdown error: %v", err)
	}

	// Flush RAM samples and close SQLite connection
	if err := healthStore.Close(); err != nil {
		log.Printf("[-] Error closing WAN health history: %v", err)
	}
	if err := trafficDB.Close(); err != nil {
		log.Printf("[-] Error closing TrafficDB: %v", err)
	} else {
		log.Println("[+] TrafficDB successfully flushed and closed.")
	}
}

// requireAuth wraps an API handler: a valid session is required (401) and, for requests that
// change state, that session's CSRF token (403).
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return auth.RequireAuth(sessionMgr, next)
}

func startBackgroundNetworkPoller(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Initial sample on startup
	_ = system.PollSystemSample()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = system.PollSystemSample()
		}
	}
}

// renderFirstRun serves the setup wizard (its answers go to /api/v1/setup/*).
func renderFirstRun(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := tmpl.ExecuteTemplate(w, "first_run.html", nil); err != nil {
		log.Printf("[-] Template execution error (first_run.html): %v", err)
	}
}

// dashboardMode is what the pages show the device as (config.ShownMode).
func dashboardMode() string {
	staged, has := cfgMgr.StagedNetworkConfig()
	return config.ShownMode(cfgMgr.NetworkConfig(), staged, has)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	auth.LimitPayloadSize(w, r, 1024*1024)

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if !auth.Authenticate(cfgMgr.Users(), username, password) {
		tmpl.ExecuteTemplate(w, "login.html", map[string]string{
			"Error": "Invalid username or password",
		})
		return
	}

	sessionMgr.CreateSession(username, w)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	// Logging out changes state, so it is POST-only and needs the session's CSRF token
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if info, ok := sessionMgr.Lookup(r); ok && !auth.CheckCSRFForm(w, r, info.CSRFToken) {
		http.Error(w, "Missing or invalid CSRF token", http.StatusForbidden)
		return
	}

	sessionMgr.DestroySession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// syslogConfig is the syslog forwarding the configuration in effect asks for.
func syslogConfig() syslogfwd.Config {
	cfg := cfgMgr.NetworkConfig()
	s := cfg.SyslogSettings()
	out := syslogfwd.Config{System: s.System, Firewall: s.Firewall, Hostname: cfg.Hostname}
	if out.Hostname == "" {
		out.Hostname = cfgMgr.SysConfig().Hostname
	}
	for _, srv := range s.Servers {
		out.Servers = append(out.Servers, syslogfwd.Server{Host: srv.Host, Port: srv.Port, Protocol: srv.Protocol})
	}
	return out
}

package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
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
	"peakroute/internal/cert"
	"peakroute/internal/config"
	"peakroute/internal/netapi"
	"peakroute/internal/network"
	"peakroute/internal/pipeline"
	"peakroute/internal/presence"
	"peakroute/internal/system"
	"peakroute/internal/translator"
	"peakroute/internal/wanhealth"
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
		return translator.AdoptExisting(cfg, translator.RootFS(systemRoot), cfgMgr.SysConfig().OperationMode)
	}); err != nil {
		log.Printf("Warning: could not adopt existing network settings: %v", err)
	}

	// The commit/confirm/revert workflow for network changes. On the appliance it writes the real
	// system files and runs real commands; in -dev it works on the sandbox root and only logs the
	// commands, so a developer's own machine is never reconfigured.
	var runner translator.Runner = translator.ExecRunner{}
	if devMode {
		runner = &translator.LogRunner{}
	}
	configSvc = pipeline.New(pipeline.Options{
		Manager:  cfgMgr,
		FS:       translator.RootFS(systemRoot),
		Runner:   runner,
		StateDir: configDir,
	})
	if err := configSvc.Resume(); err != nil {
		log.Printf("Warning: could not resume a pending configuration change: %v", err)
	}

	// Initialize session tracking and rate limiting
	sessionMgr = auth.NewSessionManager(cfgMgr)
	loginLimiter = auth.NewRateLimiter(5, 2*time.Minute)

	// Initialize traffic tracking & time-series database
	network.InitTrafficTracker(cfgMgr, "")
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

	// Drop timed-out web sessions in the background
	sessionMgr.StartJanitor(pollerCtx, time.Minute)

	// Bootstrap self-signed TLS cert if missing
	if err := cert.EnsureTLSCert(); err != nil {
		log.Fatalf("[-] TLS initialization error: %v", err)
	}

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
	mux.HandleFunc("/api/v1/cert/upload", requireAuth(cert.HandleUploadCert))

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
	mux.HandleFunc("/api/v1/routes", requireAuth(netAPI.HandleRoutes))
	// Wi-Fi adapters and what they can do (read only)
	wifi := wireless.Detector{SysClassNet: "/sys/class/net", Run: func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		return string(out), err
	}}
	mux.HandleFunc("/api/v1/wireless", requireAuth(wifi.Handler))
	stations := &wireless.StationCache{Detector: wifi, TTL: 2 * time.Second}
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
			for _, l := range network.ReadLeases(network.LeasePaths) {
				if ip, err := netip.ParseAddr(l.IPAddress); err == nil && ip.Is4() {
					out = append(out, presence.Target{MAC: l.MACAddress, IP: ip})
				}
			}
			return out
		},
	}
	presenceTracker.Load()
	go presenceTracker.Run(pollerCtx)
	netAPI.Wireless = wifi.Detect
	mux.HandleFunc("/api/v1/wifi", requireAuth(netAPI.HandleWifi))
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
	mux.HandleFunc("/api/v1/firewall/connections", requireAuth(netAPI.HandleConnections))
	netAPI.FirewallLog = network.ReadFirewallLog
	mux.HandleFunc("/api/v1/firewall/log", requireAuth(netAPI.HandleFirewallLog))
	mux.HandleFunc("/api/v1/firewall", requireAuth(netAPI.HandleFirewall))
	mux.HandleFunc("/api/v1/firewall/counters", requireAuth(netAPI.HandleFirewallCounters))
	mux.HandleFunc("/api/v1/firewall/order", requireAuth(netAPI.HandleFirewallOrder))
	mux.HandleFunc("/api/v1/firewall/rules", requireAuth(netAPI.HandleFirewallRules))
	mux.HandleFunc("/api/v1/firewall/rules/{id}", requireAuth(netAPI.HandleFirewallRule))
	mux.HandleFunc("/api/v1/firewall/forwards", requireAuth(netAPI.HandleFirewallForwards))
	mux.HandleFunc("/api/v1/firewall/forwards/{id}", requireAuth(netAPI.HandleFirewallForward))
	mux.HandleFunc("/api/v1/settings", requireAuth(netAPI.HandleSettings))
	mux.HandleFunc("/api/v1/settings/country", requireAuth(netAPI.HandleSettingsCountry))
	mux.HandleFunc("/api/v1/settings/domain", requireAuth(netAPI.HandleSettingsDomain))
	mux.HandleFunc("/api/v1/settings/hostname", requireAuth(netAPI.HandleSettingsHostname))
	netAPI.ProviderDNS = func() []string {
		var out []string
		for _, a := range network.SystemResolvers(os.ReadFile) {
			out = append(out, a.String())
		}
		return out
	}
	mux.HandleFunc("/api/v1/dns", requireAuth(netAPI.HandleDNS))
	mux.HandleFunc("/api/v1/dns/lookup", requireAuth(netAPI.HandleDNSLookup))
	mux.HandleFunc("/api/v1/dns/upstream", requireAuth(netAPI.HandleDNSUpstream))
	mux.HandleFunc("/api/v1/dns/networks/{id}", requireAuth(netAPI.HandleDNSNetwork))
	mux.HandleFunc("/api/v1/dns/hosts", requireAuth(netAPI.HandleDNSHosts))
	mux.HandleFunc("/api/v1/dns/hosts/{id}", requireAuth(netAPI.HandleDNSHost))
	mux.HandleFunc("/api/v1/dns/forwards", requireAuth(netAPI.HandleDNSForwards))
	mux.HandleFunc("/api/v1/dns/forwards/{id}", requireAuth(netAPI.HandleDNSForward))
	mux.HandleFunc("/api/v1/wifi/radios/{name}", requireAuth(netAPI.HandleWifiRadio))
	mux.HandleFunc("/api/v1/wifi/networks", requireAuth(netAPI.HandleWifiNetworks))
	mux.HandleFunc("/api/v1/wifi/networks/{id}", requireAuth(netAPI.HandleWifiNetwork))
	mux.HandleFunc("/api/v1/uplinks", requireAuth(netAPI.HandleUplinks))
	mux.HandleFunc("/api/v1/uplinks/{id}", requireAuth(netAPI.HandleUplink))
	mux.HandleFunc("/api/v1/uplinks/{id}/health", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "wan" {
			http.NotFound(w, r)
			return
		}
		wanProbe.HandleHistory(w, r)
	}))
	mux.HandleFunc("/api/v1/ports", requireAuth(netAPI.HandlePorts))
	mux.HandleFunc("/api/v1/networks", requireAuth(netAPI.HandleNetworks))
	mux.HandleFunc("/api/v1/networks/{id}", requireAuth(netAPI.HandleNetwork))

	// Staged network changes: review, commit, confirm, revert, discard
	mux.HandleFunc("/api/v1/config/status", requireAuth(configSvc.HandleStatus))
	mux.HandleFunc("/api/v1/config/staged", requireAuth(configSvc.HandleStaged))
	mux.HandleFunc("/api/v1/config/commit", requireAuth(configSvc.HandleCommit))
	mux.HandleFunc("/api/v1/config/confirm", requireAuth(configSvc.HandleConfirm))
	mux.HandleFunc("/api/v1/config/revert", requireAuth(configSvc.HandleRevert))
	mux.HandleFunc("/api/v1/config/discard", requireAuth(configSvc.HandleDiscard))

	// System DHCP List Endpoint
	mux.HandleFunc("/api/v1/dhcp/leases", requireAuth(network.LeaseHandler(presenceTracker.Annotate)))

	// Main application routing logic
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Unconfigured state
		if !cfgMgr.IsConfigured() {
			if r.URL.Path == "/first_run" || r.URL.Path == "/first-run" {
				renderFirstRun(w, http.StatusOK, "admin", "")
				return
			}

			if r.URL.Path == "/api/v1/setup/apply" && r.Method == http.MethodPost {
				handleApply(w, r)
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

			sysMode := cfgMgr.SysConfig().OperationMode
			if sysMode == "" {
				sysMode = "ROUTER"
			}

			data := struct {
				SystemMode string
				CSRFToken  string
			}{
				SystemMode: sysMode,
				CSRFToken:  session.CSRFToken,
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
	}

	// Capture OS signals for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		fmt.Printf("[+] PeakRoute OS daemon active on https://0.0.0.0%s\n", listenPort)
		if err := server.ListenAndServeTLS(cert.CertPath, cert.KeyPath); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// renderFirstRun serves the setup page, optionally with an error and the username to keep filled in.
func renderFirstRun(w http.ResponseWriter, status int, username, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	data := map[string]string{"Username": username, "Error": errMsg}
	if err := tmpl.ExecuteTemplate(w, "first_run.html", data); err != nil {
		log.Printf("[-] Template execution error (first_run.html): %v", err)
	}
}

func handleApply(w http.ResponseWriter, r *http.Request) {
	auth.LimitPayloadSize(w, r, 1024*1024)

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	plainPassword := r.FormValue("password")
	confirmPassword := r.FormValue("confirm_password")

	if username == "" {
		username = "admin"
	}

	if err := auth.ValidateUsername(username); err != nil {
		renderFirstRun(w, http.StatusBadRequest, username, err.Error())
		return
	}

	if err := auth.ValidateNewPassword(plainPassword, confirmPassword); err != nil {
		renderFirstRun(w, http.StatusBadRequest, username, err.Error())
		return
	}

	hashedPassword, err := config.HashPassword(plainPassword)
	if err != nil {
		http.Error(w, "Failed to process password", http.StatusInternalServerError)
		return
	}

	// SetupAdmin is atomic: if two setup requests race, only the first creates the account
	err = cfgMgr.SetupAdmin(config.User{
		Username:     username,
		PasswordHash: hashedPassword,
		Role:         "admin",
	})
	if errors.Is(err, config.ErrAlreadyConfigured) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err != nil {
		log.Printf("[-] Setup failed: %v", err)
		http.Error(w, "Failed to save authentication configuration", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/login", http.StatusSeeOther)
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

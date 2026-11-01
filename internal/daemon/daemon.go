// Package daemon is what every PeakRoute build runs: the settings, the commit pipeline, sign-in, the certificate, Wi-Fi, backup,
// updates, the first-run wizard and the web server. The router (main.go at the top of the repository) and the access point
// (cmd/peakroute-ap) each start a Core and add their own parts to its Mux before Serve; what a build does not add is not in its binary.
package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
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
	"peakroute/internal/fsutil"
	"peakroute/internal/netapi"
	"peakroute/internal/network"
	"peakroute/internal/pipeline"
	"peakroute/internal/setup"
	"peakroute/internal/syslogfwd"
	"peakroute/internal/system"
	"peakroute/internal/translator"
	"peakroute/internal/update"
	"peakroute/internal/wireless"
	"peakroute/web"
)

// Options are what the command line and the build choose.
type Options struct {
	Dev        bool
	ConfigDir  string
	SystemRoot string // "" on the appliance; a sandbox directory in -dev so dev never touches the host's /etc
	Port       string
	// APOnly is the access point build: the wizard offers no router, the mode cannot be switched to one, and the settings of a
	// router are refused before they are applied.
	APOnly bool
}

// ParseFlags reads the command line: -dev and -port.
func ParseFlags() Options {
	var o Options
	flag.BoolVar(&o.Dev, "dev", false, "Run in local development mode")
	flag.StringVar(&o.Port, "port", "", "Port to listen on (defaults: 8443 for dev, 443/8443 for prod)")
	flag.Parse()

	mime.AddExtensionType(".css", "text/css")
	mime.AddExtensionType(".js", "application/javascript")

	if o.Port != "" && o.Port[0] != ':' {
		o.Port = ":" + o.Port
	}

	if o.Dev {
		// Keep dev state out of the repo root (dev-data/ is gitignored)
		o.ConfigDir = "./dev-data"
		if err := os.MkdirAll(o.ConfigDir, 0700); err != nil {
			log.Fatalf("[-] Failed to create dev data directory: %v", err)
		}
		cert.EnableDevPaths(filepath.Join(o.ConfigDir, "certs"))

		// Dev runs on a developer's own machine: system files are read from and (later) written to
		// a sandbox under dev-data, never the host's real /etc
		o.SystemRoot = filepath.Join(o.ConfigDir, "sysroot")

		if o.Port == "" {
			o.Port = ":8443"
		}
		fmt.Println("[+] Running in DEVELOPMENT mode (HTTPS)")

	} else {
		o.ConfigDir = "/etc/peakroute"

		if o.Port == "" {
			o.Port = ":443"
		}
		fmt.Println("[+] Running in PRODUCTION mode (HTTPS)")
	}
	return o
}

// Core is the running daemon's shared part. A build adds its own handlers to Mux, its own fields to NetAPI and its own background
// work under Ctx, then calls Serve.
type Core struct {
	Options
	Manager  *config.Manager
	Config   *pipeline.Service
	Sessions *auth.SessionManager
	Runner   translator.Runner
	Mux      *http.ServeMux
	Certs    *cert.Manager
	NetAPI   *netapi.API
	Wifi     wireless.Detector
	Stations *wireless.StationCache
	Backup   *backup.Service
	Syslog   *syslogfwd.Forwarder
	// Ctx ends when the daemon shuts down: background work stops with it.
	Ctx context.Context

	cancel       context.CancelFunc
	tmpl         *template.Template
	loginLimiter *auth.RateLimiter
	closers      []func()
}

// RequireAuth wraps an API handler: a valid session is required (401) and, for requests that
// change state, that session's CSRF token (403).
func (c *Core) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return auth.RequireAuth(c.Sessions, next)
}

// OnClose runs f when the daemon shuts down, after the web server has stopped (in the order they were added).
func (c *Core) OnClose(f func()) { c.closers = append(c.closers, f) }

// Start brings up everything the builds share. It ends the program if something it cannot do without fails.
func Start(o Options) *Core {
	c := &Core{Options: o}
	var err error

	// A restore that included the history left the databases in a directory of their own (they were in use): swap them in now,
	// before anything opens them.
	if done := backup.ApplyPending(o.ConfigDir); len(done) > 0 {
		log.Printf("[+] Restored from a backup: %s", strings.Join(done, ", "))
	}

	// The config manager is the single owner of auth, network and system settings: it loads
	// them from configDir once and everything else reads them from memory
	c.Manager, err = config.NewManager(o.ConfigDir)
	if err != nil {
		log.Fatalf("[-] Failed to initialize config manager: %v", err)
	}
	cfgMgr := c.Manager

	// Record this build's version/arch/kernel in sys.json (only writes if it changed)
	if err := cfgMgr.SyncBuildMetadata(); err != nil {
		log.Printf("Warning: failed to save sys.json: %v", err)
	}

	// Router mode: fill in what network.json leaves blank (LAN defaults to 192.168.1.1/24) and
	// import an existing DHCP range from dnsmasq so a wizard-configured box keeps it. This changes
	// network.json only; nothing is applied to the system.
	if err := cfgMgr.AdoptNetwork(func(cfg *config.NetworkConfig) bool {
		return translator.AdoptExisting(cfg, translator.RootFS(o.SystemRoot), cfg.OperationMode())
	}); err != nil {
		log.Printf("Warning: could not adopt existing network settings: %v", err)
	}

	// A crash between writing a file and putting it in place leaves a ".name.123.tmp" next to it: clear the old ones out of the
	// directories the daemon writes to (its own, the certificates', and those of the files the configuration makes).
	sweepDirs := map[string]bool{o.ConfigDir: true, filepath.Dir(cert.CertPath): true}
	if d, err := translator.Render(cfgMgr.NetworkConfig(), ""); err == nil {
		for _, a := range d.Artifacts {
			sweepDirs[filepath.Join(o.SystemRoot, filepath.Dir(a.Path))] = true
		}
		for _, m := range d.ManagedDirs {
			sweepDirs[filepath.Join(o.SystemRoot, m.Path)] = true
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
	c.Runner = translator.ExecRunner{}
	if o.Dev {
		c.Runner = &translator.LogRunner{}
	}
	runner := c.Runner
	// The setup network: until a local network is committed the wired ports are joined in a temporary bridge with an address, so
	// the welcome page can be reached (docs/setup.md). It lets go of the ports before a commit or a revert touches them and is
	// brought back afterwards if there is still no local network.
	setupNet := &bootstrap.Service{Run: runner, Manager: cfgMgr, Ports: network.GetPhysicalInterfaces, Dir: "/run/peakroute-setup"}
	if o.Dev {
		setupNet.Dir = filepath.Join(o.SystemRoot, "run", "peakroute-setup")
	}
	if o.APOnly {
		// plugged into a network another router runs: ask its DHCP server for an address instead of serving any
		setupNet.Client = true
		setupNet.Hostname = func() string { h, _ := os.Hostname(); return h }
	}
	var allow func(config.NetworkConfig) error
	if o.APOnly {
		allow = config.RequireAP
	}
	c.Config = pipeline.New(pipeline.Options{
		Manager:  cfgMgr,
		FS:       translator.RootFS(o.SystemRoot),
		Runner:   runner,
		StateDir: o.ConfigDir,
		Before:   setupNet.Stop,
		Settled:  setupNet.Reconcile,
		Allow:    allow,
	})
	configSvc := c.Config
	if err := configSvc.Resume(); err != nil {
		log.Printf("Warning: could not resume a pending configuration change: %v", err)
	}
	setupNet.Reconcile()

	// Initialize session tracking and rate limiting
	c.Sessions = auth.NewSessionManager(cfgMgr)
	sessionMgr := c.Sessions
	c.loginLimiter = auth.NewRateLimiter(5, 2*time.Minute)

	// Initialize traffic tracking & time-series database
	dbPath := filepath.Join(o.ConfigDir, "traffic_history.db")
	trafficDB, err := system.NewTrafficDB(dbPath)
	if err != nil {
		log.Fatalf("[-] Failed to initialize traffic database: %v", err)
	}
	system.SetTrafficDB(trafficDB)
	c.OnClose(func() {
		// Flush RAM samples and close SQLite connection
		if err := trafficDB.Close(); err != nil {
			log.Printf("[-] Error closing TrafficDB: %v", err)
		} else {
			log.Println("[+] TrafficDB successfully flushed and closed.")
		}
	})

	// Start 1-second background poller for traffic metrics
	c.Ctx, c.cancel = context.WithCancel(context.Background())
	go startBackgroundNetworkPoller(c.Ctx)

	// Send the logs to syslog servers, as the configuration in effect says
	c.Syslog = &syslogfwd.Forwarder{Rule: func(id string) (string, string, bool) {
		rule, ok := cfgMgr.NetworkConfig().FirewallSettings().FindRule(id)
		return rule.Name, rule.Action, ok
	}}
	syslogForwarder := c.Syslog
	syslogForwarder.Start(c.Ctx)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			syslogForwarder.Update(c.syslogConfig())
			select {
			case <-c.Ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()

	// Drop timed-out web sessions in the background
	sessionMgr.StartJanitor(c.Ctx, time.Minute)

	// The web interface's certificate: made by this router's own web authority for the names and addresses the router has now (unless
	// a custom one was uploaded), checked again every ten minutes, and served without a restart when it changes.
	c.Certs = &cert.Manager{Wanted: func() cert.Names {
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
		n.IPs = certAddresses(cfg, o.APOnly, interfaceAddrs)
		return n
	}}
	certMgr := c.Certs
	if err := certMgr.Ensure(); err != nil {
		log.Fatalf("[-] TLS initialization error: %v", err)
	}
	go func() {
		for {
			select {
			case <-c.Ctx.Done():
				return
			// often enough that an address from DHCP, which comes after the start, is in it soon (it is made again only when the
			// names or addresses changed, or it runs out)
			case <-time.After(10 * time.Minute):
				if err := certMgr.Ensure(); err != nil {
					log.Printf("[-] Checking the web interface's certificate failed: %v", err)
				}
			}
		}
	}()

	// Parse embedded HTML templates
	c.tmpl, err = template.ParseFS(web.Files, "templates/*")
	if err != nil {
		log.Fatalf("[-] Failed to parse embedded templates: %v", err)
	}

	c.Mux = http.NewServeMux()
	mux := c.Mux
	requireAuth := c.RequireAuth

	// Mount embedded static assets (CSS/JS) on /static/
	staticFS, err := fs.Sub(web.Files, "static")
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

	// The administrator of the first-run wizard is also the login of the machine's console (a developer's machine only logs it)
	var consoleAccounts console.Accounts = console.System{}
	if o.Dev {
		consoleAccounts = console.Dev{}
	}
	c.NetAPI = &netapi.API{Manager: cfgMgr, Ports: network.GetPhysicalInterfaces, Editable: configSvc.Idle, APOnly: o.APOnly}
	netAPI := c.NetAPI
	// Wi-Fi adapters and what they can do (read only)
	c.Wifi = wireless.Detector{SysClassNet: "/sys/class/net", Run: func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, name, args...).Output()
		return string(out), err
	}}
	wifi := c.Wifi
	mux.HandleFunc("/api/v1/wireless", requireAuth(wifi.Handler))
	c.Stations = &wireless.StationCache{Detector: wifi, TTL: 2 * time.Second}
	stations := c.Stations
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
	// restoring shows what is in it first and puts the settings in the draft (docs/backup.md). The router adds the nicknames.
	c.Backup = &backup.Service{Env: backup.Env{
		ConfigDir: o.ConfigDir, CertDir: filepath.Dir(cert.CertPath), Manager: cfgMgr, Certs: certMgr, Sessions: sessionMgr,
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
	backupSvc := c.Backup
	mux.HandleFunc("/api/v1/backup", requireAuth(backupSvc.HandleInfo))
	mux.HandleFunc("/api/v1/backup/create", requireAuth(backupSvc.HandleCreate))
	mux.HandleFunc("/api/v1/backup/open", requireAuth(backupSvc.HandleOpen))
	mux.HandleFunc("/api/v1/backup/preview", requireAuth(backupSvc.HandlePreview))
	mux.HandleFunc("/api/v1/backup/restore", requireAuth(backupSvc.HandleRestore))
	mux.HandleFunc("/api/v1/backup/copies/{id}", requireAuth(backupSvc.HandleCopy))
	mux.HandleFunc("/api/v1/backup/copies/{id}/restore", requireAuth(backupSvc.HandleCopyRestore))
	// Updates: a new version is written into the other of the two system slots, started on trial and kept (or not) from the web
	// interface (docs/updates.md)
	product := update.ProductRouter
	if o.APOnly {
		product = update.ProductAP
	}
	updateSvc := update.New(o.ConfigDir, config.BuildVersion, config.DefaultUpdateRepo, configSvc.Idle, backupSvc.SaveCopy)
	// a system whose release file does not say what it is (a development machine, or an image from before) is taken for this build
	updateSvc.Product = product
	if o.Dev {
		// no disk with two slots here: a simulated one in the development folder, with sample update files to try the page with
		if sim, err := update.NewSimulated(filepath.Join(o.ConfigDir, "update-sim"), config.BuildVersion, product, configSvc.Idle, backupSvc.SaveCopy); err != nil {
			log.Printf("Warning: the simulated update slots could not be made: %v", err)
		} else {
			updateSvc = sim
			log.Printf("[+] Updates are simulated: sample update files are in %s", filepath.Join(o.ConfigDir, "update-sim"))
		}
	}
	go updateSvc.Watch(c.Ctx)
	mux.HandleFunc("/api/v1/updates", requireAuth(updateSvc.HandleState))
	mux.HandleFunc("/api/v1/updates/check", requireAuth(updateSvc.HandleCheck))
	mux.HandleFunc("/api/v1/updates/install", requireAuth(updateSvc.HandleInstall))
	mux.HandleFunc("/api/v1/updates/cancel", requireAuth(updateSvc.HandleCancel))
	mux.HandleFunc("/api/v1/updates/upload", requireAuth(updateSvc.HandleUpload))
	mux.HandleFunc("/api/v1/updates/restart", requireAuth(updateSvc.HandleRestart))
	mux.HandleFunc("/api/v1/updates/keep", requireAuth(updateSvc.HandleKeep))
	mux.HandleFunc("/api/v1/updates/revert", requireAuth(updateSvc.HandleRevert))
	mux.HandleFunc("/api/v1/updates/settings", requireAuth(updateSvc.HandleSettings))
	mux.HandleFunc("/api/v1/settings", requireAuth(netAPI.HandleSettings))
	mux.HandleFunc("/api/v1/settings/country", requireAuth(netAPI.HandleSettingsCountry))
	mux.HandleFunc("/api/v1/settings/domain", requireAuth(netAPI.HandleSettingsDomain))
	netAPI.TimeStatus = network.ReadTimeStatus
	netAPI.LinkUp = func(iface string) bool {
		i, err := net.InterfaceByName(iface)
		return err == nil && i.Flags&net.FlagUp != 0
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
		if o.Dev {
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
	mux.HandleFunc("/api/v1/dns/lookup", requireAuth(netAPI.HandleDNSLookup))
	mux.HandleFunc("/api/v1/diagnostics", requireAuth(netAPI.HandleDiagnostics))
	mux.HandleFunc("/api/v1/diagnostics/jobs", requireAuth(netAPI.HandleDiagnosticsJobs))
	mux.HandleFunc("/api/v1/diagnostics/jobs/{id}", requireAuth(netAPI.HandleDiagnosticsJob))
	mux.HandleFunc("/api/v1/diagnostics/connect", requireAuth(netAPI.HandleDiagnosticsConnect))
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

	// First-run wizard: open only while the router is not set up (docs/setup.md)
	setupSvc := &setup.Service{Manager: cfgMgr, Ports: network.GetPhysicalInterfaces, Here: setup.HerePorts, DefaultRoute: setup.DefaultRouteDevice,
		Restore: backupSvc, Console: consoleAccounts, Wireless: wifi.Detect, Scan: wifi.Scan, NewSession: sessionMgr.CreateSession, Hostname: func() string { h, _ := os.Hostname(); return h },
		APOnly: o.APOnly}
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

	// Main application routing logic
	mux.HandleFunc("/", c.handleRoot)
	return c
}

// Serve runs the web server until the daemon is told to stop, then shuts everything down.
func (c *Core) Serve() {
	secureMux := auth.SecurityHeadersMiddleware(c.Mux)

	// Launch HTTPS server
	server := &http.Server{
		Addr:         c.Port,
		Handler:      secureMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		TLSConfig:    &tls.Config{GetCertificate: c.Certs.GetCertificate, MinVersion: tls.VersionTLS12},
	}

	// Capture OS signals for graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		fmt.Printf("[+] PeakRoute OS daemon active on https://0.0.0.0%s\n", c.Port)
		if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("[-] HTTPS server crashed: %v", err)
		}
	}()

	<-stop
	log.Println("[*] Shutting down PeakRoute OS...")

	// Stop background polling loop
	c.cancel()

	// An open confirm window stays on disk, so the next start resumes (or reverts) it
	c.Config.Close()

	// Shutdown server gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[-] Server shutdown error: %v", err)
	}

	for _, f := range c.closers {
		f()
	}
}

// handleRoot serves the pages: the wizard until the device is set up, then the login and the dashboard.
func (c *Core) handleRoot(w http.ResponseWriter, r *http.Request) {
	// Unconfigured state
	if !c.Manager.IsConfigured() {
		if r.URL.Path == "/first_run" || r.URL.Path == "/first-run" {
			c.renderFirstRun(w)
			return
		}

		http.Redirect(w, r, "/first_run", http.StatusSeeOther)
		return
	}

	// Configured state - Login
	if r.URL.Path == "/login" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := c.tmpl.ExecuteTemplate(w, "login.html", map[string]string{"Product": c.product()}); err != nil {
			log.Printf("[-] Template execution error (login.html): %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	if r.URL.Path == "/api/v1/login" && r.Method == http.MethodPost {
		clientIP := r.RemoteAddr

		if !c.loginLimiter.Allow(clientIP) {
			c.tmpl.ExecuteTemplate(w, "login.html", map[string]string{
				"Error":   "Too many login requests. Please wait 2 minutes and try again.",
				"Product": c.product(),
			})
			return
		}

		c.handleLogin(w, r)
		return
	}

	// Configured Route - Logout
	if r.URL.Path == "/api/v1/logout" || r.URL.Path == "/logout" {
		c.handleLogout(w, r)
		return
	}

	// Protected Dashboard
	session, authenticated := c.Sessions.Lookup(r)
	if !authenticated {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if r.URL.Path == "/" || r.URL.Path == "/dashboard" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		sysMode := c.dashboardMode()

		data := struct {
			SystemMode string
			CSRFToken  string
			Username   string
			ReadOnly   bool
			Product    string
			APOnly     bool
		}{
			Product:    c.product(),
			APOnly:     c.APOnly,
			SystemMode: sysMode,
			CSRFToken:  session.CSRFToken,
			Username:   session.Username,
			ReadOnly:   !auth.CanWrite(session.Role),
		}

		if err := c.tmpl.ExecuteTemplate(w, "dashboard.html", data); err != nil {
			log.Printf("[-] Template execution error (dashboard.html): %v", err)
			return
		}
		return
	}

	http.NotFound(w, r)
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
func (c *Core) renderFirstRun(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := c.tmpl.ExecuteTemplate(w, "first_run.html", map[string]any{"Product": c.product(), "APOnly": c.APOnly}); err != nil {
		log.Printf("[-] Template execution error (first_run.html): %v", err)
	}
}

// product is the name the pages show: the access point build is PeakRoute AP.
func (c *Core) product() string {
	if c.APOnly {
		return "PeakRoute AP"
	}
	return "PeakRoute OS"
}

// dashboardMode is what the pages show the device as (config.ShownMode).
func (c *Core) dashboardMode() string {
	staged, has := c.Manager.StagedNetworkConfig()
	return config.ShownMode(c.Manager.NetworkConfig(), staged, has)
}

func (c *Core) handleLogin(w http.ResponseWriter, r *http.Request) {
	auth.LimitPayloadSize(w, r, 1024*1024)

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if !auth.Authenticate(c.Manager.Users(), username, password) {
		c.tmpl.ExecuteTemplate(w, "login.html", map[string]string{
			"Error":   "Invalid username or password",
			"Product": c.product(),
		})
		return
	}

	c.Sessions.CreateSession(username, w)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (c *Core) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Logging out changes state, so it is POST-only and needs the session's CSRF token
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if info, ok := c.Sessions.Lookup(r); ok && !auth.CheckCSRFForm(w, r, info.CSRFToken) {
		http.Error(w, "Missing or invalid CSRF token", http.StatusForbidden)
		return
	}

	c.Sessions.DestroySession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// syslogConfig is the syslog forwarding the configuration in effect asks for.
func (c *Core) syslogConfig() syslogfwd.Config {
	cfg := c.Manager.NetworkConfig()
	s := cfg.SyslogSettings()
	out := syslogfwd.Config{System: s.System, Firewall: s.Firewall, Hostname: cfg.Hostname}
	if out.Hostname == "" {
		out.Hostname = c.Manager.SysConfig().Hostname
	}
	for _, srv := range s.Servers {
		out.Servers = append(out.Servers, syslogfwd.Server{Host: srv.Host, Port: srv.Port, Protocol: srv.Protocol})
	}
	return out
}

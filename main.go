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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"peakroute/internal/auth"
	"peakroute/internal/cert"
	"peakroute/internal/config"
	"peakroute/internal/network"
	"peakroute/internal/system"
)

//go:embed web/templates/* web/static/*
var embeddedFiles embed.FS

var (
	devMode      bool
	configDir    string
	listenPort   string
	tmpl         *template.Template
	cfgMgr       *config.Manager
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

	// Physical Devices Endpoint
	mux.HandleFunc("/api/v1/system/devices", requireAuth(network.HandleGetDevices))

	// System DHCP List Endpoint
	mux.HandleFunc("/api/v1/dhcp/leases", requireAuth(network.HandleDHCPLeases))

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
		_, authenticated := sessionMgr.ValidateSession(r)
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
			}{
				SystemMode: sysMode,
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

	// Shutdown server gracefully
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("[-] Server shutdown error: %v", err)
	}

	// Flush RAM samples and close SQLite connection
	if err := trafficDB.Close(); err != nil {
		log.Printf("[-] Error closing TrafficDB: %v", err)
	} else {
		log.Println("[+] TrafficDB successfully flushed and closed.")
	}
}

// requireAuth rejects requests without a valid session with a JSON 401 before calling next.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, authenticated := sessionMgr.ValidateSession(r); !authenticated {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		next(w, r)
	}
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
	sessionMgr.DestroySession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

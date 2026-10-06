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
	devMode       bool
	configDir     string
	authPath      string
	networkPath   string
	sysConfigPath string
	flagPath      string
	listenPort    string
	tmpl          *template.Template
	cfgMgr        *config.Manager
	sessionMgr    *auth.SessionManager
	loginLimiter  *auth.RateLimiter
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
		authPath = filepath.Join(configDir, "auth.json")
		networkPath = filepath.Join(configDir, "network.json")
		sysConfigPath = filepath.Join(configDir, "sys.json")
		flagPath = filepath.Join(configDir, "configured")
		cert.EnableDevPaths(filepath.Join(configDir, "certs"))

		if listenPort == "" {
			listenPort = ":8443"
		}
		fmt.Println("[+] Running in DEVELOPMENT mode (HTTPS)")

	} else {
		configDir = "/etc/peakroute"
		authPath = "/etc/peakroute/auth.json"
		networkPath = "/etc/peakroute/network.json"
		flagPath = "/etc/peakroute/configured"
		sysConfigPath = "/etc/peakroute/sys.json"

		if listenPort == "" {
			listenPort = ":443"
		}
		fmt.Println("[+] Running in PRODUCTION mode (HTTPS)")
	}

	// Load existing config from disk (or return defaults on error)
	cfg, err := config.LoadSysConfig(sysConfigPath)
	if err != nil {
		log.Println("No existing sys.json found, creating defaults...")
		cfg = &config.SysConfig{
			Hostname:      "peakroute-gateway",
			OperationMode: "ROUTER",
			Configured:    false,
		}
	}

	// Populate/refresh build metadata from Go binary, runtime arch, and kernel
	cfg.PopulateBuildMetadata()

	// Save updated metadata back to sys.json
	if err := config.SaveSysConfig(sysConfigPath, cfg); err != nil {
		log.Printf("Warning: failed to save sys.json: %v", err)
	}
}

func main() {
	initAppConfig()

	var err error

	// Initialize config manager using the resolved configDir
	cfgMgr, err = config.NewManager(configDir)
	if err != nil {
		log.Fatalf("[-] Failed to initialize config manager: %v", err)
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
	go startBackgroundNetworkPoller(pollerCtx, sysConfigPath)

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
		system.HandleSystemStats(w, r, sysConfigPath)
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
		_, err := os.Stat(flagPath)
		isConfigured := !os.IsNotExist(err)

		// Unconfigured state
		if !isConfigured {
			if r.URL.Path == "/first_run" || r.URL.Path == "/first-run" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				if err := tmpl.ExecuteTemplate(w, "first_run.html", nil); err != nil {
					log.Printf("[-] Template execution error (first_run.html): %v", err)
				}
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

			sysMode := "ROUTER"
			if sysCfg, err := config.LoadSysConfig(sysConfigPath); err == nil && sysCfg.OperationMode != "" {
				sysMode = sysCfg.OperationMode
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

func startBackgroundNetworkPoller(ctx context.Context, configPath string) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Initial sample on startup
	_ = system.PollNetworkInterfaces(configPath)

	for {
		select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = system.PollNetworkInterfaces(configPath)
		}
	}
}

func handleApply(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	plainPassword := r.FormValue("password")

	if username == "" {
		username = "admin"
	}

	hashedPassword, err := config.HashPassword(plainPassword)
	if err != nil {
		http.Error(w, "Failed to process password", http.StatusInternalServerError)
		return
	}

	authCfg := &config.AuthConfig{
		Users: []config.User{
			{
				Username:     username,
				PasswordHash: hashedPassword,
				Role:         "admin",
			},
		},
	}

	if err := os.MkdirAll(configDir, 0700); err != nil {
		log.Printf("[-] Failed to create config directory: %v", err)
	}

	if err := config.SaveAuthConfig(authPath, authCfg); err != nil {
		log.Printf("[-] Failed to save auth config: %v", err)
		http.Error(w, "Failed to save authentication configuration", http.StatusInternalServerError)
		return
	}

	if err := os.WriteFile(flagPath, []byte("1"), 0644); err != nil {
		log.Printf("[-] Failed to write configured flag: %v", err)
	}

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	auth.LimitPayloadSize(w, r, 1024*1024)

	username := auth.SanitizeString(r.FormValue("username"))
	password := r.FormValue("password")

	authCfg, err := config.LoadAuthConfig(authPath)
	if err != nil {
		http.Error(w, "Failed to load authentication configuration", http.StatusInternalServerError)
		return
	}

	var validUser bool
	for _, u := range authCfg.Users {
		if u.Username == username && config.CheckPasswordHash(password, u.PasswordHash) {
			validUser = true
			break
		}
	}

	if !validUser {
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

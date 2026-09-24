package main

import (
	"embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"time"

	"peakroute/internal/auth"
	"peakroute/internal/cert"
	"peakroute/internal/config"
	"peakroute/internal/system"
	"peakroute/internal/network"
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
)

func initAppConfig() {
	flag.BoolVar(&devMode, "dev", false, "Run in local development mode")
	flag.StringVar(&listenPort, "port", "", "Port to listen on (defaults: 8443 for dev, 443/8443 for prod)")
	flag.Parse()

	if listenPort != "" && listenPort[0] != ':' {
		listenPort = ":" + listenPort
	}

	if devMode {
		configDir = "."
		authPath = "./auth.json"
		networkPath = "./network.json"
		sysConfigPath = "./sys.conf"
		flagPath = "./configured"
		cert.EnableDevPaths("./certs")

		if listenPort == "" {
			listenPort = ":8443"
		}
		fmt.Println("[+] Running in DEVELOPMENT mode (HTTPS)")

	} else {
		configDir = "/etc/peakroute"
		authPath = "/etc/peakroute/auth.json"
		networkPath = "/etc/peakroute/network.json"
		flagPath = "/etc/peakroute/configured"
		sysConfigPath = "/etc/peakroute/sys.conf"

		if listenPort == "" {
			listenPort = ":443"
		}
		fmt.Println("[+] Running in PRODUCTION mode (HTTPS)")
	}
}

var sessionMgr = auth.NewSessionManager()

var loginLimiter = auth.NewRateLimiter(5, 2*time.Minute)

func main() {
	initAppConfig()
	var err error

	// Bootstrapping self-signed TLS cert if missing
	if err := cert.EnsureTLSCert(); err != nil {
		log.Fatalf("[-] TLS initialization error: %v", err)
	}

	// Parse embedded HTML templates
	tmpl, err = template.ParseFS(embeddedFiles, "web/templates/*")
	if err != nil {
		log.Fatalf("[-] Failed to parse embedded templates: %v", err)
	}

	mux := http.NewServeMux()

	// Mount static embedded files (CSS/JS) on /static/
	fs := http.FileServer(http.Dir("web/static"))
	mux.Handle("/static/", http.StripPrefix("/static/", fs))

	// Register API endpoints
	mux.HandleFunc("/api/v1/cert/upload", cert.HandleUploadCert)

	// System Stats Telemetry Endpoint
	mux.HandleFunc("/api/v1/system/stats", func(w http.ResponseWriter, r *http.Request) {
		_, authenticated := sessionMgr.ValidateSession(r)
		if !authenticated {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		system.HandleSystemStats(w, r)
	})

	// System DHCP List Endpoint
	mux.HandleFunc("/api/v1/dhcp/leases", func(w http.ResponseWriter, r *http.Request) {
		_, authenticated := sessionMgr.ValidateSession(r)
		if !authenticated {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		network.HandleDHCPLeases(w, r)
	})

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

	// Launch single HTTPS server
	server := &http.Server{
		Addr:    listenPort,
		Handler: secureMux,
	}

	fmt.Printf("[+] PeakRoute OS daemon active on https://0.0.0.0%s\n", listenPort)
	if err := server.ListenAndServeTLS(cert.CertPath, cert.KeyPath); err != nil {
		log.Fatalf("[-] HTTPS server crashed: %v", err)
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

	// Create Auth config object
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

	// Save to auth.json
	if err := config.SaveAuthConfig(authPath, authCfg); err != nil {
		log.Printf("[-] Failed to save auth config: %v", err)
		http.Error(w, "Failed to save authentication configuration", http.StatusInternalServerError)
		return
	}

	// Mark setup as complete
	if err := os.WriteFile(flagPath, []byte("1"), 0644); err != nil {
		log.Printf("[-] Failed to write configured flag: %v", err)
	}

	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	auth.LimitPayloadSize(w, r, 1024*1024)

	username := auth.SanitizeString(r.FormValue("username"))
	password := r.FormValue("password")

	// Load auth.json instead of config.json
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

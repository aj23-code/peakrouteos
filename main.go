package main

import (
	"embed"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"os/exec"

	"peakroute/internal/config"
	"peakroute/internal/network"
	"peakroute/internal/auth"
)

//go:embed web/templates/* web/static/*
var embeddedFiles embed.FS

var (
	devMode    bool
	configDir  string
	configPath string
	flagPath   string
	ifacePath  string
	listenPort string
	tmpl     *template.Template
)

func init() {
	flag.BoolVar(&devMode, "dev", false, "Run in local development mode")
	flag.StringVar(&listenPort, "port", "", "Port to listen on (defaults: 8080 for dev, 80 for prod)")
	flag.Parse()

	if devMode {
		configDir = "."
		configPath = "./config.json"
		flagPath = "./configured"
		ifacePath = "./interfaces.generated"
		if listenPort == "" {
			listenPort = ":8080"
		}
		fmt.Println("[+] Running in DEVELOPMENT mode")
	} else {
		configDir = "/etc/peakroute"
		configPath = "/etc/peakroute/config.json"
		flagPath = "/etc/peakroute/configured"
		ifacePath = "/etc/network/interfaces"
		if listenPort == "" {
			listenPort = ":80"
		}
		fmt.Println("[+] Running in PRODUCTION (Alpine) mode")
	}
}

var sessionMgr = auth.NewSessionManager()

func main() {
	var err error

	tmpl, err = template.ParseFS(embeddedFiles, "web/templates/*")
	if err != nil {
		fmt.Printf("[-] Failed to parse embedded templates: %v\n", err)
		os.Exit(1)
	}


	type SetupData struct {
		AvailableIfaces []network.SystemInterface
		DefaultLanIP    string
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, err := os.Stat(flagPath)
		isConfigured := !os.IsNotExist(err)

		// 1. If unconfigured, restrict routes to first-run setup or apply API
		if !isConfigured {
			if r.URL.Path == "/first_run" || r.URL.Path == "/first-run" {
				var ifaces []network.SystemInterface
				var err error

				if devMode {
					ifaces = []network.SystemInterface{
						{Name: "eth0", MAC: "00:11:22:33:44:55"},
						{Name: "eth1", MAC: "00:11:22:33:44:56"},
						{Name: "eth2", MAC: "00:11:22:33:44:57"},
						{Name: "eth3", MAC: "00:11:22:33:44:58"},
					}
				} else {
					ifaces, err = network.GetPhysicalInterfaces()
					if err != nil {
						fmt.Printf("[-] Failed to detect network interfaces: %v\n", err)
					}
				}

				data := SetupData{
					AvailableIfaces: ifaces,
					DefaultLanIP:    "192.168.1.1/24",
				}

				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				tmpl.ExecuteTemplate(w, "first_run.html", data)
				return
			}

			// Allow form submission API during setup
			if r.URL.Path == "/api/v1/setup/apply" && r.Method == http.MethodPost {
				handleApply(w, r)
				return
			}

			// Redirect everything else to first-run when unconfigured
			http.Redirect(w, r, "/first_run", http.StatusSeeOther)
			return
		}

		// 2. Allow public access to login screen when system IS configured
		if r.URL.Path == "/login" {
			tmpl.ExecuteTemplate(w, "login.html", nil)
			return
		}
		if r.URL.Path == "/api/v1/login" && r.Method == http.MethodPost {
			handleLogin(w, r)
			return
		}

		// 3. Auth Check for Dashboard / Protected Routes
		_, authenticated := sessionMgr.ValidateSession(r)
		if !authenticated {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		// Serve Dashboard if authenticated
		if r.URL.Path == "/" || r.URL.Path == "/dashboard" {
			w.Write([]byte("<h1>Welcome to PeakRoute OS Dashboard!</h1>"))
			return
		}

		http.NotFound(w, r)
	})

	fmt.Printf("[+] PeakRoute OS daemon active on %s ...\n", listenPort)
	if err := http.ListenAndServe(listenPort, nil); err != nil {
		fmt.Printf("[-] Server error: %v\n", err)
	}
}

func handleApply(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form data", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	plainPassword := r.FormValue("password")
	if username == "" { username = "admin" }

	hashedPassword, _ := config.HashPassword(plainPassword)

	cfg := &config.RouterConfig{
		WanInterface:  r.FormValue("wan"),
		LanInterfaces: r.Form["lan_ifaces"],
		LanIP:         r.FormValue("lan_ip"),
		Users: []config.User{
			{
				Username:     username,
				PasswordHash: hashedPassword,
				Role:         "admin",
			},
		},
	}

	// Save configuration and write setup flag
	os.MkdirAll(configDir, 0700)
	config.SaveConfig(configPath, cfg)

	ifaceContent := network.GenerateInterfaces(cfg)
	os.WriteFile(ifacePath, []byte(ifaceContent), 0644)
	os.WriteFile(flagPath, []byte("1"), 0644)

	if !devMode {
		exec.Command("rc-service", "networking", "restart").Run()
		exec.Command("lbu", "commit", "-d").Run()
	}

	// Redirect straight to /login post-setup
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.FormValue("username")
	password := r.FormValue("password")

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		http.Error(w, "Failed to load router configuration", http.StatusInternalServerError)
		return
	}

	var validUser bool
	for _, u := range cfg.Users {
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

// Package setup is the first-run wizard of a router that has not been set up (docs/setup.md): a welcome page with the choice of setting
// up as a new router or restoring a backup; for a new router the region and name, the internet connection, the local network and the
// administrator; and a review. Everything the wizard collects ends up in the router's JSON files: the settings in the draft
// (network.json.staged, to be reviewed and applied from the first page the new administrator sees), the account in auth.json, and the
// flag that marks the router as set up. Every route here refuses once the router is set up.
package setup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"peakroute/internal/auth"
	"peakroute/internal/backup"
	"peakroute/internal/config"
	"peakroute/internal/console"
)

// Service is the wizard.
type Service struct {
	Manager *config.Manager
	// Ports lists the router's own ports. Here says which of them the browser is connected through (nil: nothing is known).
	Ports func() ([]config.SystemInterface, error)
	Here  func(r *http.Request) []string
	// DefaultRoute names the port the default route leaves by right now, when there is one (a good guess for the internet connection).
	DefaultRoute func() string
	// Restore opens and restores backups. NewSession signs the new administrator in.
	Restore    *backup.Service
	NewSession func(username string, w http.ResponseWriter) string
	// Console makes the administrator the login of the machine's console too (nil: only the web interface).
	Console console.Accounts
	// Hostname is the machine's own name, suggested for the router.
	Hostname func() string

	mu sync.Mutex // one setup at a time
}

type errBody struct {
	Error  string              `json:"error"`
	Errors []config.FieldError `json:"errors,omitempty"`
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[setup] writing response: %v", err)
	}
}

// Guard lets a handler through only while the router is not set up.
func (s *Service) Guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Manager.IsConfigured() {
			write(w, http.StatusConflict, errBody{Error: "This router is already set up."})
			return
		}
		h(w, r)
	}
}

func methodOnly(w http.ResponseWriter, r *http.Request, m string) bool {
	if r.Method == m {
		return true
	}
	w.Header().Set("Allow", m)
	write(w, http.StatusMethodNotAllowed, errBody{Error: "method not allowed"})
	return false
}

// ---- what the wizard is told ------------------------------------------------------------------------------------------------

// PortInfo is one of the router's ports as the wizard shows it.
type PortInfo struct {
	Name     string `json:"name"`
	MAC      string `json:"mac"`
	State    string `json:"state"` // "up" (a cable is plugged in) or "down"
	Speed    string `json:"speed"`
	Here     bool   `json:"here"`     // the browser is connected through it
	Internet bool   `json:"internet"` // the default route leaves by it now
}

// Defaults are the values the wizard starts a new router from.
type Defaults struct {
	LanIP     string `json:"lan_ip"`
	LanSubnet string `json:"lan_subnet"`
	DHCPStart string `json:"dhcp_start"`
	DHCPEnd   string `json:"dhcp_end"`
}

// InfoResponse is what the page needs to build the wizard.
type InfoResponse struct {
	Ports        []PortInfo        `json:"ports"` // wired ports only
	Countries    []config.Country  `json:"countries"`
	Timezones    []config.Timezone `json:"timezones"`
	Hostname     string            `json:"hostname"`
	Country      string            `json:"country"`
	Timezone     string            `json:"timezone"`
	Defaults     Defaults          `json:"defaults"`
	SuggestWan   string            `json:"suggest_wan"`
	SuggestLan   []string          `json:"suggest_lan"`
	NTPServers   []string          `json:"ntp_servers"`
	MinPassword  int               `json:"min_password"`
	MaxPassword  int               `json:"max_password"`
	BackupFormat int               `json:"backup_schema"`
}

func (s *Service) wiredPorts(r *http.Request) []PortInfo {
	if s.Ports == nil {
		return []PortInfo{}
	}
	all, err := s.Ports()
	if err != nil {
		return []PortInfo{}
	}
	here := map[string]bool{}
	if s.Here != nil {
		for _, h := range s.Here(r) {
			here[h] = true
		}
	}
	def := ""
	if s.DefaultRoute != nil {
		def = s.DefaultRoute()
	}
	out := []PortInfo{}
	for _, p := range all {
		if p.Wireless {
			continue // a Wi-Fi adapter is never a WAN or LAN port: it is set up on the Wireless page
		}
		out = append(out, PortInfo{Name: p.Name, MAC: p.MAC, State: p.State, Speed: p.Speed, Here: here[p.Name], Internet: p.Name == def})
	}
	return out
}

// suggestions: the internet connection is the port the default route uses; failing that a port with a cable that is not the one the
// browser comes through; failing that the last port. Every other port is a LAN port.
func suggest(ports []PortInfo) (wan string, lan []string) {
	for _, p := range ports {
		if p.Internet && !p.Here {
			wan = p.Name
		}
	}
	if wan == "" {
		for _, p := range ports {
			if !p.Here && p.State == "up" {
				wan = p.Name
				break
			}
		}
	}
	if wan == "" {
		for i := len(ports) - 1; i >= 0; i-- {
			if !ports[i].Here {
				wan = ports[i].Name
				break
			}
		}
	}
	for _, p := range ports {
		if p.Name != wan {
			lan = append(lan, p.Name)
		}
	}
	return wan, lan
}

// HandleInfo: GET /api/v1/setup/info.
func (s *Service) HandleInfo(w http.ResponseWriter, r *http.Request) {
	if !methodOnly(w, r, http.MethodGet) {
		return
	}
	base := s.Manager.NetworkConfig()
	ports := s.wiredPorts(r)
	wan, lan := suggest(ports)
	host := base.Hostname
	if host == "" && s.Hostname != nil {
		host = s.Hostname()
	}
	if lan == nil {
		lan = []string{}
	}
	write(w, http.StatusOK, InfoResponse{Ports: ports, Countries: config.Countries, Timezones: config.Timezones, Hostname: host, Country: base.Country, Timezone: base.Timezone,
		Defaults:   Defaults{LanIP: "192.168.1.1", LanSubnet: "255.255.255.0", DHCPStart: "192.168.1.100", DHCPEnd: "192.168.1.200"},
		SuggestWan: wan, SuggestLan: lan, NTPServers: config.DefaultNTPServers, MinPassword: auth.MinPasswordLength, MaxPassword: auth.MaxPasswordBytes, BackupFormat: backup.SchemaVersion})
}

// ---- the form ---------------------------------------------------------------------------------------------------------------------

// Form is everything the wizard collects for a new router.
type Form struct {
	Hostname string `json:"hostname"`
	Country  string `json:"country"`
	Timezone string `json:"timezone"`

	WanPort    string   `json:"wan_port"`
	WanMode    string   `json:"wan_mode"` // dhcp, static or pppoe
	WanIP      string   `json:"wan_ip"`
	WanSubnet  string   `json:"wan_subnet"`
	WanGateway string   `json:"wan_gateway"`
	WanDNS     []string `json:"wan_dns"`
	PPPoEUser  string   `json:"pppoe_username"`
	PPPoEPass  string   `json:"pppoe_password"`

	LanPorts  []string `json:"lan_ports"`
	LanIP     string   `json:"lan_ip"`
	LanSubnet string   `json:"lan_subnet"`
	DHCP      bool     `json:"dhcp"`
	DHCPStart string   `json:"dhcp_start"`
	DHCPEnd   string   `json:"dhcp_end"`

	Username string `json:"username"`
	Password string `json:"password"`
	Confirm  string `json:"confirm"`
}

// The steps, in order. A check goes through the step it is asked for: the fields of that step and the ones before.
var stepOrder = []string{"region", "internet", "lan", "admin"}

func stepIndex(name string) int {
	for i, s := range stepOrder {
		if s == name {
			return i
		}
	}
	return -1
}

// which step each field belongs to
func stepOf(field string) string {
	switch {
	case field == "hostname" || field == "country" || field == "timezone" || strings.HasPrefix(field, "ntp") || field == "send_hostname":
		return "region"
	case strings.HasPrefix(field, "wan") || strings.HasPrefix(field, "pppoe"):
		return "internet"
	case strings.HasPrefix(field, "lan") || strings.HasPrefix(field, "dhcp"):
		return "lan"
	case field == "username" || field == "password" || field == "confirm":
		return "admin"
	}
	return "lan" // anything else about the network (the whole must be consistent) is shown at the last network step
}

// rename the fields of the configuration to the form's
var formField = map[string]string{
	"wan_interface": "wan_port", "lan_interfaces": "lan_ports",
	"dhcp_range_start": "dhcp_start", "dhcp_range_end": "dhcp_end",
}

// maskOf accepts a netmask the way people write one: 255.255.255.0, /24 or 24.
func maskOf(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "/")
	var bits int
	if _, err := fmt.Sscanf(s, "%d", &bits); err == nil && fmt.Sprint(bits) == s && bits >= 0 && bits <= 32 {
		var m uint32
		if bits > 0 {
			m = ^uint32(0) << uint(32-bits)
		}
		return fmt.Sprintf("%d.%d.%d.%d", byte(m>>24), byte(m>>16), byte(m>>8), byte(m))
	}
	return strings.TrimSpace(s)
}

func cleanList(in []string) []string {
	var out []string
	for _, x := range in {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// build lays the form over the settings the router has (what it adopted at start) and checks the whole.
func (s *Service) build(f Form, r *http.Request) (config.NetworkConfig, []config.FieldError) {
	cfg := s.Manager.NetworkConfig()
	var errs []config.FieldError
	add := func(field, msg string) { errs = append(errs, config.FieldError{Field: field, Message: msg}) }

	cfg.Hostname = strings.ToLower(strings.TrimSpace(f.Hostname))
	cfg.Country = strings.ToUpper(strings.TrimSpace(f.Country))
	cfg.Timezone = strings.TrimSpace(f.Timezone)
	if cfg.Timezone == "" || !config.IsTimezone(cfg.Timezone) {
		add("timezone", "choose a time zone from the list")
	}
	if cfg.Country == "" {
		add("country", "choose the country the router is used in (it sets what the Wi-Fi may do)")
	}

	// ports
	wan := strings.TrimSpace(f.WanPort)
	lan := cleanList(f.LanPorts)
	cfg.WanInterface, cfg.LanInterfaces, cfg.PortNetworks = wan, lan, nil
	have := map[string]bool{}
	var here []string
	if s.Ports != nil {
		for _, p := range s.wiredPorts(r) {
			have[p.Name] = true
			if p.Here {
				here = append(here, p.Name)
			}
		}
	}
	if wan == "" {
		add("wan_port", "choose the port the internet connection comes in on")
	} else if len(have) > 0 && !have[wan] {
		add("wan_port", fmt.Sprintf("this router has no wired port %q", wan))
	}
	for _, h := range here {
		if h == wan {
			add("wan_port", "that is the port you are connected through: it cannot be the internet connection, you would lose the connection")
		}
	}
	for _, p := range lan {
		if len(have) > 0 && !have[p] {
			add("lan_ports", fmt.Sprintf("this router has no wired port %q", p))
		}
	}
	if len(here) > 0 {
		in := false
		for _, p := range lan {
			for _, h := range here {
				if p == h {
					in = true
				}
			}
		}
		if !in && len(lan) > 0 {
			add("lan_ports", "include the port you are connected through ("+strings.Join(here, ", ")+"), or you will lose the connection when the settings are applied")
		}
	}

	// the internet connection
	mode := strings.ToLower(strings.TrimSpace(f.WanMode))
	if mode == "" {
		mode = "dhcp"
	}
	cfg.WanMode = mode
	cfg.WanIP, cfg.WanSubnet, cfg.WanGateway, cfg.WanDNS, cfg.WanPPPoE = "", "", "", nil, nil
	switch mode {
	case "static":
		cfg.WanIP, cfg.WanSubnet, cfg.WanGateway = strings.TrimSpace(f.WanIP), maskOf(f.WanSubnet), strings.TrimSpace(f.WanGateway)
		cfg.WanDNS = cleanList(f.WanDNS)
		if len(cfg.WanDNS) == 0 {
			add("wan_dns", "a static connection needs at least one DNS server (your provider's, or 1.1.1.1)")
		}
		if cfg.WanGateway == "" {
			add("wan_gateway", "a static connection needs the provider's gateway address")
		}
	case "pppoe":
		cfg.WanPPPoE = &config.PPPoEConfig{Username: strings.TrimSpace(f.PPPoEUser), Password: f.PPPoEPass}
	}

	// the local network
	cfg.LanIP, cfg.LanSubnet = strings.TrimSpace(f.LanIP), maskOf(f.LanSubnet)
	if f.DHCP {
		cfg.DHCP = &config.DHCPConfig{Enabled: true, RangeStart: strings.TrimSpace(f.DHCPStart), RangeEnd: strings.TrimSpace(f.DHCPEnd), LeaseTime: "12h"}
	} else {
		cfg.DHCP = &config.DHCPConfig{Enabled: false}
	}

	// the whole must hold together
	if err := cfg.Validate(); err != nil {
		var ve config.ValidationError
		if errors.As(err, &ve) {
			for _, e := range ve {
				name := e.Field
				if m, ok := formField[name]; ok {
					name = m
				}
				errs = append(errs, config.FieldError{Field: name, Message: e.Message})
			}
		} else {
			add("", err.Error())
		}
	}
	return cfg, dedupe(errs)
}

func dedupe(in []config.FieldError) []config.FieldError {
	seen := map[string]bool{}
	var out []config.FieldError
	for _, e := range in {
		k := e.Field + "|" + e.Message
		if !seen[k] {
			seen[k] = true
			out = append(out, e)
		}
	}
	return out
}

func (s *Service) adminErrors(f Form) []config.FieldError {
	var errs []config.FieldError
	name := strings.TrimSpace(f.Username)
	if err := auth.ValidateUsername(name); err != nil {
		errs = append(errs, config.FieldError{Field: "username", Message: err.Error()})
	} else {
		// the account is also the login of the console: a name Linux accepts and does not use already
		var err error
		if s.Console != nil {
			err = s.Console.Check(name)
		} else {
			err = console.ValidName(name)
		}
		if err != nil {
			errs = append(errs, config.FieldError{Field: "username", Message: err.Error()})
		}
	}
	if err := auth.ValidateNewPassword(f.Password, f.Confirm); err != nil {
		field := "password"
		if err.Error() == "Passwords do not match." {
			field = "confirm"
		}
		errs = append(errs, config.FieldError{Field: field, Message: err.Error()})
	}
	return errs
}

// forStep keeps the errors of the steps up to and including through.
func forStep(errs []config.FieldError, through string) []config.FieldError {
	limit := stepIndex(through)
	out := []config.FieldError{}
	for _, e := range errs {
		if stepIndex(stepOf(e.Field)) <= limit {
			out = append(out, e)
		}
	}
	return out
}

func refuse(w http.ResponseWriter, errs []config.FieldError) {
	msg := "Some of the answers need attention."
	if len(errs) > 0 && errs[0].Field == "" {
		msg = errs[0].Message
	}
	write(w, http.StatusBadRequest, errBody{Error: msg, Errors: errs})
}

// HandleCheck: POST /api/v1/setup/check {"through": "lan", ...the form} checks the answers up to a step and changes nothing.
func (s *Service) HandleCheck(w http.ResponseWriter, r *http.Request) {
	if !methodOnly(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Through string `json:"through"`
		Form
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 64<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{Error: err.Error()})
		return
	}
	if stepIndex(req.Through) < 0 {
		write(w, http.StatusBadRequest, errBody{Error: "unknown step"})
		return
	}
	_, errs := s.build(req.Form, r)
	if stepIndex(req.Through) >= stepIndex("admin") {
		errs = append(errs, s.adminErrors(req.Form)...)
	}
	if errs = forStep(errs, req.Through); len(errs) > 0 {
		refuse(w, errs)
		return
	}
	write(w, http.StatusOK, map[string]bool{"ok": true})
}

// Summary is what setting up changes, for the last page.
type Summary struct {
	Changes []config.Change `json:"changes"`
}

// HandleApply: POST /api/v1/setup/apply sets the router up as a new one: the account is made, the settings are saved as the draft, the
// router is marked as set up and the new administrator is signed in. Only one setup can succeed.
func (s *Service) HandleApply(w http.ResponseWriter, r *http.Request) {
	if !methodOnly(w, r, http.MethodPost) {
		return
	}
	var f Form
	if err := auth.DecodeJSONStrict(w, r, &f, 64<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{Error: err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Manager.IsConfigured() {
		write(w, http.StatusConflict, errBody{Error: "This router is already set up."})
		return
	}
	cfg, errs := s.build(f, r)
	errs = append(errs, s.adminErrors(f)...)
	if len(errs) > 0 {
		refuse(w, errs)
		return
	}
	hash, err := config.HashPassword(f.Password)
	if err != nil {
		write(w, http.StatusInternalServerError, errBody{Error: "The password could not be processed."})
		return
	}
	if err := s.Manager.StageNetwork(func(c *config.NetworkConfig) error { *c = cfg; return nil }); err != nil {
		write(w, http.StatusInternalServerError, errBody{Error: "The settings could not be saved: " + err.Error()})
		return
	}
	username := strings.TrimSpace(f.Username)
	if s.Console != nil {
		if err := s.Console.Set(username, hash); err != nil {
			_ = s.Manager.DiscardNetwork()
			log.Printf("[-] Setup: console account: %v", err)
			write(w, http.StatusInternalServerError, errBody{Error: "The login of the console could not be made, so nothing was set up."})
			return
		}
	}
	err = s.Manager.SetupAdmin(config.User{Username: username, PasswordHash: hash, Role: auth.RoleAdmin})
	if err != nil {
		_ = s.Manager.DiscardNetwork()
		if s.Console != nil {
			if rerr := s.Console.Remove(username); rerr != nil {
				log.Printf("[-] Setup: removing the console account again: %v", rerr)
			}
		}
		if errors.Is(err, config.ErrAlreadyConfigured) {
			write(w, http.StatusConflict, errBody{Error: "This router is already set up."})
			return
		}
		log.Printf("[-] Setup failed: %v", err)
		write(w, http.StatusInternalServerError, errBody{Error: "The account could not be saved."})
		return
	}
	_, draft := s.Manager.StagedNetworkConfig()
	if s.NewSession != nil {
		s.NewSession(username, w)
	}
	write(w, http.StatusOK, map[string]any{"redirect": "/dashboard", "draft": draft})
}

// ---- restoring a backup instead ---------------------------------------------------------------------------------------------

// HandleRestoreApply: POST /api/v1/setup/restore/apply {"token", "wan", "lan", "history"} restores an opened backup onto this router.
// The administrators come with it (and are required); the settings become the draft; the router is then set up.
func (s *Service) HandleRestoreApply(w http.ResponseWriter, r *http.Request) {
	if !methodOnly(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Token   string   `json:"token"`
		Wan     string   `json:"wan"`
		Lan     []string `json:"lan"`
		History bool     `json:"history"`
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 8<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{Error: err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Manager.IsConfigured() {
		write(w, http.StatusConflict, errBody{Error: "This router is already set up."})
		return
	}
	if req.Wan != "" || len(req.Lan) > 0 {
		if msg := s.hereProblem(r, req.Wan, cleanList(req.Lan)); msg != "" {
			write(w, http.StatusBadRequest, errBody{Error: msg})
			return
		}
	}
	o := backup.Options{Settings: true, Admins: true, Certificates: true, History: req.History}
	if req.Wan != "" || len(req.Lan) > 0 {
		o.Ports = &backup.PortChoice{Wan: req.Wan, Lan: cleanList(req.Lan)}
	}
	res, err := s.Restore.RestoreFirstRun(req.Token, o)
	if err != nil {
		msg := err.Error()
		status := http.StatusConflict
		if strings.Contains(msg, "no longer open") {
			status = http.StatusNotFound
		}
		write(w, status, errBody{Error: strings.ToUpper(msg[:1]) + msg[1:] + "."})
		return
	}
	warning := s.consoleAfterRestore()
	write(w, http.StatusOK, map[string]any{"redirect": "/login", "result": res, "warning": warning})
}

// hereProblem says why a choice of ports would cut the browser off: the port it is connected through must not be the internet
// connection, and must be in the local network.
func (s *Service) hereProblem(r *http.Request, wan string, lan []string) string {
	if s.Here == nil {
		return ""
	}
	for _, h := range s.Here(r) {
		if h == wan {
			return "The port you are connected through (" + h + ") cannot be the internet connection: you would lose the connection."
		}
		in := false
		for _, p := range lan {
			if p == h {
				in = true
			}
		}
		if !in && len(lan) > 0 {
			return "Include the port you are connected through (" + h + ") in the local network, or you will lose the connection."
		}
	}
	return ""
}

// consoleAfterRestore makes the console login of a restored router: the backup's console account, or (a backup made before there was
// one) the first administrator with a name Linux accepts. It answers with something to tell the person, or "".
func (s *Service) consoleAfterRestore() string {
	if s.Console == nil {
		return ""
	}
	users := s.Manager.Users()
	name := s.Manager.ConsoleUser()
	if name == "" {
		for _, u := range users {
			if u.Role == auth.RoleAdmin && console.ValidName(u.Username) == nil {
				name = u.Username
				break
			}
		}
		if name == "" {
			return "None of the restored administrators has a name that the console accepts, so the console has no login. Make an administrator with a lower-case name and it can be made the console login."
		}
		_ = s.Manager.UpdateAuth(func(c *config.AuthConfig) error { c.ConsoleUser = name; return nil })
	}
	for _, u := range users {
		if u.Username == name {
			if err := s.Console.Set(name, u.PasswordHash); err != nil {
				log.Printf("[-] Restore: console account %q: %v", name, err)
				return "The login of the console could not be made (" + err.Error() + "). The web interface works."
			}
			return ""
		}
	}
	return ""
}

// ---- where the browser is connected -----------------------------------------------------------------------------------------

// HerePorts says which of the router's ports the request came in through: the port whose network holds the client's address, or, for a
// bridge, the ports that are part of it.
func HerePorts(r *http.Request) []string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.IsLoopback() {
		return nil
	}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ones, bits := ipn.Mask.Size(); bits == 32 {
				if p, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", ipn.IP, ones)); err == nil && p.Masked().Contains(ip) {
					return narrow(ifc.Name, membersOf(ifc.Name), ip)
				}
			}
		}
	}
	return nil
}

// narrow finds the one member of a bridge that a client is plugged into: its hardware address is in the ARP table (it has just talked to
// the router) and the bridge's forwarding table says which port it was learned on. If either is missing, all the members are returned.
func narrow(bridge string, members []string, ip netip.Addr) []string {
	if len(members) < 2 {
		return members
	}
	arp, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return members
	}
	mac := parseARP(string(arp), ip.String(), bridge)
	if mac == "" {
		return members
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "bridge", "fdb", "show", "br", bridge).Output()
	if err != nil {
		return members
	}
	if port := parseFDB(string(out), mac, members); port != "" {
		return []string{port}
	}
	return members
}

// parseARP reads /proc/net/arp: the hardware address of an IP address seen on a device ("" if none).
func parseARP(text, ip, dev string) string {
	for _, line := range strings.Split(text, "\n")[1:] {
		f := strings.Fields(line)
		if len(f) >= 6 && f[0] == ip && f[5] == dev && f[3] != "00:00:00:00:00:00" {
			return strings.ToLower(f[3])
		}
	}
	return ""
}

// parseFDB reads `bridge fdb show`: the member port a hardware address was learned on ("" if it is not among the members).
func parseFDB(text, mac string, members []string) string {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || strings.ToLower(f[0]) != mac {
			continue
		}
		for i := 1; i+1 < len(f); i++ {
			if f[i] == "dev" {
				for _, m := range members {
					if m == f[i+1] {
						return m
					}
				}
			}
		}
	}
	return ""
}

// membersOf is the interface itself, or the ports of a bridge.
func membersOf(name string) []string {
	entries, err := os.ReadDir("/sys/class/net/" + name + "/brif")
	if err != nil {
		return []string{name}
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// DefaultRouteDevice is the port the default route leaves by ("" when there is none).
func DefaultRouteDevice() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // the header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 4 && fields[1] == "00000000" && fields[7] == "00000000" {
			return fields[0]
		}
	}
	return ""
}

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"peakroute/internal/fsutil"
)

// Default directory paths for config files
const (
	DefaultConfigDir     = "/etc/peakroute"
	DefaultAuthFile      = "/etc/peakroute/auth.json"
	DefaultNetworkFile   = "/etc/peakroute/network.json"
	DefaultStagedNetFile = "/etc/peakroute/network.json.staged"
	DefaultSysFile       = "/etc/peakroute/sys.json"
)

// Idle time before a web session expires, and the bounds applied to a configured value
const (
	DefaultSessionTimeoutMinutes = 15
	MinSessionTimeoutMinutes     = 1
	MaxSessionTimeoutMinutes     = 24 * 60
)

// --- Struct Definitions ---

type User struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
}

type AuthConfig struct {
	SessionTimeoutMinutes int    `json:"session_timeout_minutes,omitempty"`
	Users                 []User `json:"users,omitempty"`
}

type SystemInterface struct {
	Name   string `json:"name"`
	MAC    string `json:"mac"`
	State  string `json:"state"`  // "up" or "down"
	Speed  string `json:"speed"`  // e.g., "1000Mb/s"
	MTU    int    `json:"mtu"`    // e.g., 1500
	Driver string `json:"driver"` // e.g., "igb", "e1000e"
}

// Unused for now until OS can run on a managed switch
type SwitchPortConfig struct {
	Port   int    `json:"port"`
	VLAN   int    `json:"vlan"`
	PoE    bool   `json:"poe"`
	Link   string `json:"link"`
	Status string `json:"status"`
}

// Also unused for the same reason
type ManagedSwitchConfig struct {
	SwitchID string             `json:"switch_id"`
	Ports    []SwitchPortConfig `json:"ports"`
}

// DHCPConfig is the LAN DHCP server's settings. A nil *DHCPConfig on NetworkConfig means the
// JSON doesn't manage DHCP yet (older installs configured it outside the JSON), which is
// different from "disabled".
type DHCPConfig struct {
	Enabled    bool   `json:"enabled"`
	RangeStart string `json:"range_start,omitempty"`
	RangeEnd   string `json:"range_end,omitempty"`
	LeaseTime  string `json:"lease_time,omitempty"` // dnsmasq format, e.g. "12h", "30m", "1d"
}

// Route is a static IPv4 route: traffic for Destination is sent to Gateway. It lives next to the
// rest of the network settings and goes through the same draft, review and commit steps.
type Route struct {
	Name        string `json:"name,omitempty"` // a label for people; never written to a system file
	Destination string `json:"destination"`    // a network in CIDR form, e.g. "10.20.0.0/16"
	Gateway     string `json:"gateway"`        // the next hop, reachable on the LAN or the WAN
	Metric      int    `json:"metric,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"` // kept in the file but not applied
}

type NetworkConfig struct {
	WanInterface    string                `json:"wan_interface,omitempty"`
	WanMode         string                `json:"wan_mode,omitempty"` // "dhcp" or "static"
	WanIP           string                `json:"wan_ip,omitempty"`
	WanSubnet       string                `json:"wan_subnet,omitempty"`
	WanGateway      string                `json:"wan_gateway,omitempty"`
	WanDNS          []string              `json:"wan_dns,omitempty"`
	LanInterfaces   []string              `json:"lan_interfaces,omitempty"`
	LanIP           string                `json:"lan_ip,omitempty"`
	LanSubnet       string                `json:"lan_subnet,omitempty"`
	DHCP            *DHCPConfig           `json:"dhcp,omitempty"`
	Routes          []Route               `json:"routes,omitempty"`
	Interfaces      []SystemInterface     `json:"interfaces,omitempty"`
	ManagedSwitches []ManagedSwitchConfig `json:"managed_switches,omitempty"`
}

type SysConfig struct {
	Hostname       string `json:"hostname"`        // Device Hostname
	OperationMode  string `json:"operation_mode"`  // "ROUTER" or "AP"
	Configured     bool   `json:"configured"`      // Initial setup flag
	PeakRouteBuild string `json:"peakroute_build"` // e.g. "v0.1.0-dev"
	Arch           string `json:"arch"`            // e.g. "amd64"
	Kernel         string `json:"kernel"`          // e.g. "Linux 7.2.6-1-cachyos"
}

// --- Combined State Manager ---

// Files managed inside the config directory
const (
	authFile        = "auth.json"
	networkFile     = "network.json"
	stagedNetFile   = "network.json.staged"
	sysFile         = "sys.json"
	configuredFlag  = "configured" // exists once first-run setup has completed
	hostnameDefault = "peakroute-gateway"
)

// ErrAlreadyConfigured is returned by SetupAdmin once first-run setup has completed.
var ErrAlreadyConfigured = errors.New("system is already configured")

// Manager is the single owner of the daemon's configuration. The state lives in memory,
// is read through accessors that return copies, and changes only through methods that save to
// disk first and update memory second, so memory and disk never disagree after a failed write.
type Manager struct {
	mu          sync.RWMutex
	baseDir     string
	auth        AuthConfig
	network     NetworkConfig // active live state
	staged      NetworkConfig // candidate state
	hasStaged   bool          // uncommitted changes exist
	sys         SysConfig
	configured  bool
	onNetworkCh chan *NetworkConfig // emits ONLY on commit
	onAuthCh    chan *AuthConfig
	onSysCh     chan *SysConfig
}

func NewManager(baseDir string) (*Manager, error) {
	if baseDir == "" {
		baseDir = DefaultConfigDir
	}

	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create config dir: %w", err)
	}

	m := &Manager{
		baseDir:     baseDir,
		onNetworkCh: make(chan *NetworkConfig, 10),
		onAuthCh:    make(chan *AuthConfig, 10),
		onSysCh:     make(chan *SysConfig, 10),
	}

	if err := m.ReloadAll(); err != nil {
		return nil, err
	}

	return m, nil
}

func (m *Manager) path(name string) string { return filepath.Join(m.baseDir, name) }

// --- Copies: callers must never share slices with the live state ---

func (c AuthConfig) clone() AuthConfig {
	c.Users = append([]User(nil), c.Users...)
	return c
}

func (c NetworkConfig) clone() NetworkConfig {
	if c.DHCP != nil {
		d := *c.DHCP
		c.DHCP = &d
	}
	c.WanDNS = append([]string(nil), c.WanDNS...)
	c.LanInterfaces = append([]string(nil), c.LanInterfaces...)
	c.Routes = append([]Route(nil), c.Routes...)
	c.Interfaces = append([]SystemInterface(nil), c.Interfaces...)

	if len(c.ManagedSwitches) > 0 {
		switches := make([]ManagedSwitchConfig, len(c.ManagedSwitches))
		for i, sw := range c.ManagedSwitches {
			sw.Ports = append([]SwitchPortConfig(nil), sw.Ports...)
			switches[i] = sw
		}
		c.ManagedSwitches = switches
	} else {
		c.ManagedSwitches = nil
	}
	return c
}

// --- Read accessors (all return copies) ---

// Users returns the configured accounts.
func (m *Manager) Users() []User {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.auth.clone().Users
}

// SysConfig returns the system settings (hostname, operation mode, build info).
func (m *Manager) SysConfig() SysConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sys
}

// NetworkConfig returns the active (committed) network configuration.
func (m *Manager) NetworkConfig() NetworkConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.network.clone()
}

// StagedNetworkConfig returns the candidate network configuration and whether it has
// uncommitted changes. Without staged changes it is a copy of the active configuration.
func (m *Manager) StagedNetworkConfig() (NetworkConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.hasStaged {
		return m.network.clone(), false
	}
	return m.staged.clone(), true
}

// IsConfigured reports whether first-run setup has completed.
func (m *Manager) IsConfigured() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.configured
}

// Default fallback version if not set via go build -ldflags
var BuildVersion = "v0.1.0-dev"

// GetBinaryVersion returns BuildVersion as the primary version string,
// appending git commit details if present in debug build info.
func GetBinaryVersion() string {
	baseVersion := BuildVersion
	if baseVersion == "" {
		baseVersion = "Unknown Version Info"
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return baseVersion
	}

	// Read embedded VCS settings from go build (Go 1.18+)
	var rev string
	var modified bool
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			rev = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}

	// Append git commit hash if present
	if rev != "" {
		if len(rev) > 7 {
			rev = rev[:7]
		}
		if modified {
			rev += "-dirty"
		}
		return fmt.Sprintf("%s-g%s", baseVersion, rev)
	}

	return baseVersion
}

// GetKernelRelease reads ostype and osrelease from /proc/sys/kernel
func GetKernelRelease() string {
	osTypeBytes, err1 := os.ReadFile("/proc/sys/kernel/ostype")
	osReleaseBytes, err2 := os.ReadFile("/proc/sys/kernel/osrelease")

	if err1 == nil && err2 == nil {
		sysName := strings.TrimSpace(string(osTypeBytes))
		release := strings.TrimSpace(string(osReleaseBytes))
		return fmt.Sprintf("%s %s", sysName, release)
	}

	return "Linux (Unknown)"
}

// PopulateBuildMetadata updates PeakRouteBuild, Arch, and Kernel inside SysConfig with live system data
func (s *SysConfig) PopulateBuildMetadata() {
	s.PeakRouteBuild = GetBinaryVersion()
	s.Arch = runtime.GOARCH // "amd64", "arm64", "arm", etc.
	s.Kernel = GetKernelRelease()
}

func defaultSysConfig() SysConfig {
	return SysConfig{Hostname: hostnameDefault, OperationMode: "ROUTER"}
}

// warnUnlessMissing logs a load failure that isn't simply "file not there yet"
func warnUnlessMissing(name string, err error) {
	if err != nil && !os.IsNotExist(err) {
		log.Printf("[config] could not read %s, using defaults: %v", name, err)
	}
}

// ReloadAll reads all config files from disk into memory, replacing the current state.
func (m *Manager) ReloadAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Auth
	if authCfg, err := LoadAuthConfig(m.path(authFile)); err == nil {
		m.auth = authCfg.clone()
	} else {
		warnUnlessMissing(authFile, err)
		m.auth = AuthConfig{SessionTimeoutMinutes: DefaultSessionTimeoutMinutes, Users: []User{}}
	}

	// Live network
	if netCfg, err := LoadNetworkConfig(m.path(networkFile)); err == nil {
		m.network = netCfg.clone()
	} else {
		warnUnlessMissing(networkFile, err)
		m.network = NetworkConfig{}
	}

	// Staged network, if a candidate file exists. One that matches the active config (an edit
	// that was reverted, or left over from an interrupted run) is not a pending change: drop it.
	if stagedCfg, err := LoadNetworkConfig(m.path(stagedNetFile)); err == nil && len(DiffNetwork(m.network, *stagedCfg)) > 0 {
		m.staged = stagedCfg.clone()
		m.hasStaged = true
	} else if err == nil {
		_ = os.Remove(m.path(stagedNetFile))
		m.staged = m.network.clone()
		m.hasStaged = false
	} else {
		warnUnlessMissing(stagedNetFile, err)
		m.staged = m.network.clone()
		m.hasStaged = false
	}

	// Sys
	if sysCfg, err := LoadSysConfig(m.path(sysFile)); err == nil {
		m.sys = *sysCfg
	} else {
		warnUnlessMissing(sysFile, err)
		m.sys = defaultSysConfig()
	}

	// First-run flag
	_, err := os.Stat(m.path(configuredFlag))
	m.configured = err == nil

	return nil
}

// SetupAdmin completes first-run setup: it stores the initial admin account and marks the
// system configured, atomically with respect to other callers. Only the first caller succeeds;
// later (or concurrent) attempts get ErrAlreadyConfigured and change nothing.
func (m *Manager) SetupAdmin(user User) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.configured {
		return ErrAlreadyConfigured
	}

	next := m.auth.clone()
	next.Users = []User{user}
	if err := SaveAuthConfig(m.path(authFile), &next); err != nil {
		return fmt.Errorf("failed to save auth config: %w", err)
	}

	// The flag is the commit point: it is written last, and durably, so a power cut can never
	// leave a box that has an admin account but offers first-run setup again
	if err := fsutil.WriteFileAtomic(m.path(configuredFlag), []byte("1"), 0644); err != nil {
		return fmt.Errorf("failed to write configured flag: %w", err)
	}

	m.auth = next
	m.configured = true
	notifyAuth(m.onAuthCh, next)
	return nil
}

// SyncBuildMetadata records the running binary's version, architecture and kernel in sys.json.
// It writes only when something changed (or the file is missing) so a restart doesn't rewrite
// the file for no reason.
func (m *Manager) SyncBuildMetadata() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.sys
	next.PopulateBuildMetadata()

	path := m.path(sysFile)
	_, statErr := os.Stat(path)
	if next == m.sys && statErr == nil {
		return nil
	}

	if err := SaveSysConfig(path, &next); err != nil {
		return err
	}
	m.sys = next
	return nil
}

// ErrConfigChanged is returned by AdoptNetwork when the configuration changed while it was
// working out its edit; nothing is written and the caller can simply try again.
var ErrConfigChanged = errors.New("network configuration changed while adopting settings")

// AdoptNetwork lets the daemon fill in or import settings on the *active* configuration at
// startup (router defaults, an existing DHCP range). fn edits a copy and reports whether it
// changed anything; only then is network.json rewritten. This is not a commit: no translator
// run is triggered, and nothing is written when nothing changed. A pending candidate gets the
// same edit, so the adoption never appears as a user change.
//
// fn runs without the Manager's lock held, so it may freely call the Manager's own accessors
// (calling them under the lock would deadlock). If the configuration changes while fn runs, the
// result is discarded and ErrConfigChanged is returned.
func (m *Manager) AdoptNetwork(fn func(cfg *NetworkConfig) bool) error {
	m.mu.RLock()
	baseActive := m.network.clone()
	baseStaged, hadStaged := m.staged.clone(), m.hasStaged
	m.mu.RUnlock()

	nextActive := baseActive.clone()
	activeChanged := fn(&nextActive)

	nextStaged := baseStaged.clone()
	stagedChanged := false
	if hadStaged {
		stagedChanged = fn(&nextStaged)
	}

	if !activeChanged && !stagedChanged {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.hasStaged != hadStaged ||
		!reflect.DeepEqual(m.network.clone(), baseActive) ||
		(hadStaged && !reflect.DeepEqual(m.staged.clone(), baseStaged)) {
		return ErrConfigChanged
	}

	if activeChanged {
		if err := SaveNetworkConfig(m.path(networkFile), &nextActive); err != nil {
			return fmt.Errorf("failed to save adopted network config: %w", err)
		}
		m.network = nextActive
	}
	if stagedChanged {
		if err := SaveNetworkConfig(m.path(stagedNetFile), &nextStaged); err != nil {
			return fmt.Errorf("failed to save adopted staged config: %w", err)
		}
		m.staged = nextStaged
	}
	if !m.hasStaged {
		m.staged = m.network.clone()
	}
	return nil
}

// --- Staging & Commit Handlers ---

// StageNetwork modifies candidate network settings without affecting live operations. If fn
// fails, or the candidate can't be saved, the previous candidate is left untouched.
func (m *Manager) StageNetwork(fn func(cfg *NetworkConfig) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Edit a copy: the first change starts from the active state, later ones from the candidate
	next := m.network.clone()
	if m.hasStaged {
		next = m.staged.clone()
	}

	if err := fn(&next); err != nil {
		return err
	}

	// An edit that puts everything back to the active values leaves nothing to commit, so the
	// candidate disappears instead of lingering as an empty "pending change"
	if len(DiffNetwork(m.network, next)) == 0 {
		m.staged = m.network.clone()
		m.hasStaged = false
		if err := os.Remove(m.path(stagedNetFile)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := SaveNetworkConfig(m.path(stagedNetFile), &next); err != nil {
		return err
	}

	m.staged = next
	m.hasStaged = true
	return nil
}

// RevertNetwork makes prev the active network configuration again (saving network.json) and, if
// restage is not nil, puts that configuration back as the pending candidate so a rejected or
// unconfirmed edit is not lost. It is how a failed or reverted commit is undone. If the save
// fails nothing changes.
func (m *Manager) RevertNetwork(prev NetworkConfig, restage *NetworkConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	active := prev.clone()
	if err := SaveNetworkConfig(m.path(networkFile), &active); err != nil {
		return fmt.Errorf("failed to restore the previous network config: %w", err)
	}

	if restage != nil {
		candidate := restage.clone()
		if len(DiffNetwork(active, candidate)) > 0 {
			if err := SaveNetworkConfig(m.path(stagedNetFile), &candidate); err != nil {
				return fmt.Errorf("failed to keep the rejected change as a candidate: %w", err)
			}
			m.network, m.staged, m.hasStaged = active, candidate, true
			return nil
		}
	}

	m.network, m.staged, m.hasStaged = active, active.clone(), false
	if err := os.Remove(m.path(stagedNetFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DiscardNetwork clears candidate changes and restores active state
func (m *Manager) DiscardNetwork() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.staged = m.network.clone()
	m.hasStaged = false

	_ = os.Remove(m.path(stagedNetFile))
	return nil
}

// CommitNetwork applies the candidate state to the live network state, saves network.json and
// notifies listeners. If the save fails, nothing changes in memory or on disk.
func (m *Manager) CommitNetwork() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.hasStaged {
		return nil // Nothing to commit
	}

	next := m.staged.clone()
	if err := SaveNetworkConfig(m.path(networkFile), &next); err != nil {
		return fmt.Errorf("failed to save live network config: %w", err)
	}

	// Promote staged to active and drop the candidate file
	m.network = next
	m.hasStaged = false
	m.staged = m.network.clone()
	_ = os.Remove(m.path(stagedNetFile))

	notifyNetwork(m.onNetworkCh, m.network.clone())
	return nil
}

// UpdateAuth changes the auth settings. If fn fails or the save fails, nothing changes.
func (m *Manager) UpdateAuth(fn func(cfg *AuthConfig) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.auth.clone()
	if err := fn(&next); err != nil {
		return err
	}

	if err := SaveAuthConfig(m.path(authFile), &next); err != nil {
		return err
	}

	m.auth = next
	notifyAuth(m.onAuthCh, next)
	return nil
}

// UpdateSys changes the system settings. If fn fails or the save fails, nothing changes.
func (m *Manager) UpdateSys(fn func(cfg *SysConfig) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	next := m.sys
	if err := fn(&next); err != nil {
		return err
	}

	if err := SaveSysConfig(m.path(sysFile), &next); err != nil {
		return err
	}

	m.sys = next
	snapshot := next
	select {
	case m.onSysCh <- &snapshot:
	default:
	}
	return nil
}

// Change notifications carry a private copy, never a pointer into the Manager's own state
func notifyNetwork(ch chan *NetworkConfig, cfg NetworkConfig) {
	select {
	case ch <- &cfg:
	default:
	}
}

func notifyAuth(ch chan *AuthConfig, cfg AuthConfig) {
	c := cfg.clone()
	select {
	case ch <- &c:
	default:
	}
}

// SessionTimeout returns the idle time before a web session expires: the configured value
// (session_timeout_minutes in auth.json) clamped to a sane range, or the default if unset.
func (m *Manager) SessionTimeout() time.Duration {
	m.mu.RLock()
	minutes := m.auth.SessionTimeoutMinutes
	m.mu.RUnlock()

	switch {
	case minutes <= 0:
		minutes = DefaultSessionTimeoutMinutes
	case minutes < MinSessionTimeoutMinutes:
		minutes = MinSessionTimeoutMinutes
	case minutes > MaxSessionTimeoutMinutes:
		minutes = MaxSessionTimeoutMinutes
	}
	return time.Duration(minutes) * time.Minute
}

// Event notification channels for reactive engines
func (m *Manager) OnNetworkChange() <-chan *NetworkConfig { return m.onNetworkCh }
func (m *Manager) OnAuthChange() <-chan *AuthConfig       { return m.onAuthCh }
func (m *Manager) OnSysChange() <-chan *SysConfig         { return m.onSysCh }

// --- File I/O Helpers ---

func SaveAuthConfig(filepath string, cfg *AuthConfig) error {
	data, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath, data, 0600)
}

func LoadAuthConfig(filepath string) (*AuthConfig, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	var cfg AuthConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func SaveNetworkConfig(filepath string, cfg *NetworkConfig) error {
	data, err := json.MarshalIndent(cfg, "", " ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath, data, 0644)
}

func LoadNetworkConfig(filepath string) (*NetworkConfig, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return nil, err
	}
	var cfg NetworkConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func LoadSysConfig(filepath string) (*SysConfig, error) {
	data, err := os.ReadFile(filepath)
	if err != nil {
		return &SysConfig{OperationMode: "ROUTER"}, err
	}
	cfg := &SysConfig{OperationMode: "ROUTER"}
	if err := json.Unmarshal(data, cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func SaveSysConfig(filepath string, cfg *SysConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal sysconfig to json: %w", err)
	}

	err = fsutil.WriteFileAtomic(filepath, data, 0644)
	if err != nil {
		return fmt.Errorf("failed to write config file %s: %w", filepath, err)
	}

	return nil
}

// Password helpers
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(bytes), err
}

func CheckPasswordHash(password, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}

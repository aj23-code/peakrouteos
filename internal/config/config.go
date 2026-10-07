package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

//Unused for now until OS can run on a managed switch
type SwitchPortConfig struct {
	Port   int    `json:"port"`
	VLAN   int    `json:"vlan"`
	PoE    bool   `json:"poe"`
	Link   string `json:"link"`
	Status string `json:"status"`
}

//Also unused for the same reason
type ManagedSwitchConfig struct {
	SwitchID string             `json:"switch_id"`
	Ports    []SwitchPortConfig `json:"ports"`
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
type Manager struct {
	mu               sync.RWMutex
	baseDir          string
	Auth             AuthConfig
	Network          NetworkConfig // Active Live State
	StagedNetwork    NetworkConfig // Candidate Staged State
	HasStagedNetwork bool          // Uncommitted Changes Flag
	Sys              SysConfig
	onNetworkCh      chan *NetworkConfig // Emits ONLY on Commit
	onAuthCh         chan *AuthConfig
	onSysCh          chan *SysConfig
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

// ReloadAll reads all separate config files into memory
func (m *Manager) ReloadAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	authPath := filepath.Join(m.baseDir, "auth.json")
	netPath := filepath.Join(m.baseDir, "network.json")
	stagedNetPath := filepath.Join(m.baseDir, "network.json.staged")
	sysPath := filepath.Join(m.baseDir, "sys.json")

	// Load Auth
	if authCfg, err := LoadAuthConfig(authPath); err == nil {
		m.Auth = *authCfg
	} else {
		m.Auth = AuthConfig{SessionTimeoutMinutes: DefaultSessionTimeoutMinutes, Users: []User{}}
	}

	// Load Live Network
	if netCfg, err := LoadNetworkConfig(netPath); err == nil {
		m.Network = *netCfg
	} else {
		m.Network = NetworkConfig{}
	}

	// Load Staged Network if candidate file exists
	if stagedCfg, err := LoadNetworkConfig(stagedNetPath); err == nil {
		m.StagedNetwork = *stagedCfg
		m.HasStagedNetwork = true
	} else {
		m.StagedNetwork = m.Network
		m.HasStagedNetwork = false
	}

	// Load Sys
	if sysCfg, err := LoadSysConfig(sysPath); err == nil {
		m.Sys = *sysCfg
	} else {
		m.Sys = SysConfig{OperationMode: "ROUTER", Configured: false}
	}

	return nil
}

// --- Staging & Commit Handlers ---

// StageNetwork modifies candidate network settings without affecting live operations
func (m *Manager) StageNetwork(fn func(cfg *NetworkConfig) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If no staged changes exist yet, base candidate on active network state
	if !m.HasStagedNetwork {
		m.StagedNetwork = m.Network
	}

	if err := fn(&m.StagedNetwork); err != nil {
		return err
	}

	m.HasStagedNetwork = true
	stagedPath := filepath.Join(m.baseDir, "network.json.staged")
	return SaveNetworkConfig(stagedPath, &m.StagedNetwork)
}

// DiscardNetwork clears candidate changes and restores active state
func (m *Manager) DiscardNetwork() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.StagedNetwork = m.Network
	m.HasStagedNetwork = false

	stagedPath := filepath.Join(m.baseDir, "network.json.staged")
	_ = os.Remove(stagedPath)
	return nil
}

// CommitNetwork applies candidate state to live network state, flushes network.json, and triggers generator
func (m *Manager) CommitNetwork() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.HasStagedNetwork {
		return nil // Nothing to commit
	}

	// Promote Staged to Active
	m.Network = m.StagedNetwork
	m.HasStagedNetwork = false

	netPath := filepath.Join(m.baseDir, "network.json")
	if err := SaveNetworkConfig(netPath, &m.Network); err != nil {
		return fmt.Errorf("failed to save live network config: %w", err)
	}

	// Remove staged candidate file
	stagedPath := filepath.Join(m.baseDir, "network.json.staged")
	_ = os.Remove(stagedPath)

	// Emit change event to reactive listeners
	select {
		case m.onNetworkCh <- &m.Network:
		default:
	}

	return nil
}

func (m *Manager) UpdateAuth(fn func(cfg *AuthConfig) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := fn(&m.Auth); err != nil {
		return err
	}

	authPath := filepath.Join(m.baseDir, "auth.json")
	if err := SaveAuthConfig(authPath, &m.Auth); err != nil {
		return err
	}

	select {
		case m.onAuthCh <- &m.Auth:
		default:
	}

	return nil
}

func (m *Manager) UpdateSys(fn func(cfg *SysConfig) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := fn(&m.Sys); err != nil {
		return err
	}

	sysPath := filepath.Join(m.baseDir, "sys.json")
	if err := SaveSysConfig(sysPath, &m.Sys); err != nil {
		return err
	}

	select {
		case m.onSysCh <- &m.Sys:
		default:
	}

	return nil
}

// SessionTimeout returns the idle time before a web session expires: the configured value
// (session_timeout_minutes in auth.json) clamped to a sane range, or the default if unset.
func (m *Manager) SessionTimeout() time.Duration {
	m.mu.RLock()
	minutes := m.Auth.SessionTimeoutMinutes
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

func IsConfigured(basePath string) bool {
	_, err := os.Stat(filepath.Join(basePath, "configured"))
	return !os.IsNotExist(err)
}

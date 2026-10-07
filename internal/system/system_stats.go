package system

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"peakroute/internal/config"
)

type InterfaceStats struct {
	RxBytes    uint64 `json:"rx_bytes"`
	RxPackets  uint64 `json:"rx_packets"`
	RxBytesSec uint64 `json:"rx_bytes_sec"` // Calculated live throughput (Bytes/sec)
	TxBytes    uint64 `json:"tx_bytes"`
	TxPackets  uint64 `json:"tx_packets"`
	TxBytesSec uint64 `json:"tx_bytes_sec"` // Calculated live throughput (Bytes/sec)
	IsPhysical bool   `json:"is_physical"`
	Role       string `json:"role"` // "wan", "lan", or "unassigned"
}

type DiskStats struct {
	TotalMB     uint64  `json:"total_mb"`
	UsedMB      uint64  `json:"used_mb"`
	FreeMB      uint64  `json:"free_mb"`
	UsedPercent float64 `json:"used_percent"`
}

type StateTableInfo struct {
	Current int `json:"current"`
	Max     int `json:"max"`
}

type SystemStats struct {
	Hostname       string                    `json:"hostname"`
	PeakRouteBuild string                    `json:"peakroute_build"`
	Arch           string                    `json:"arch"`
	Kernel         string                    `json:"kernel"`
	CPUModel       string                    `json:"cpu_model"`
	KernelVersion  string                    `json:"kernel_version"`
	UptimeSeconds  uint64                    `json:"uptime_seconds"`
	CPUUsagePct    float64                   `json:"cpu_usage_pct"`
	CPUTempC       float64                   `json:"cpu_temp_celsius"`
	LoadAvg        [3]float64                `json:"load_avg"`
	MemoryTotalMB  uint64                    `json:"memory_total_mb"`
	MemoryUsedMB   uint64                    `json:"memory_used_mb"`
	MemoryFreeMB   uint64                    `json:"memory_free_mb"`
	SwapTotalMB    uint64                    `json:"swap_total_mb"`
	SwapUsedMB     uint64                    `json:"swap_used_mb"`
	SwapFreeMB     uint64                    `json:"swap_free_mb"`
	Disk           DiskStats                 `json:"disk"`
	StateTable     StateTableInfo            `json:"state_table"`
	DNSServers     []string                  `json:"dns_servers"`
	Interfaces     map[string]InterfaceStats `json:"interfaces"`
}

type CpuSample struct {
	Idle  uint64
	Total uint64
}

type ifaceSample struct {
	rxBytes   uint64
	txBytes   uint64
	timestamp time.Time
	physical  bool // cached on first sight: real hardware, not a bridge/veth/VLAN
}

// sampleRecorder receives one throughput sample per interface per poll (implemented by *TrafficDB)
type sampleRecorder interface {
	RecordSample(iface string, rxBytes, txBytes int64)
}

// ifaceSnapshot is the latest reading of one interface, produced by the background poller
type ifaceSnapshot struct {
	rxBytes, rxPackets, rxBytesSec uint64
	txBytes, txPackets, txBytesSec uint64
	physical                       bool
}

// Sampling state. PollSystemSample is the only writer; GetSystemStats only reads.
var (
	sampleMu        sync.Mutex
	lastIfaceStats  = make(map[string]ifaceSample)
	latestIfaces    map[string]ifaceSnapshot // replaced wholesale each poll, never mutated
	lastCPU         CpuSample
	haveLastCPU     bool
	latestCPUPct    float64
	globalTrafficDB sampleRecorder

	// isPhysical decides which interfaces are tracked in history; replaced in tests
	isPhysical = isPhysicalHardware
)

// SetTrafficDB registers the database instance so stats poller can record history
func SetTrafficDB(db *TrafficDB) {
	if db == nil {
		globalTrafficDB = nil
		return
	}
	globalTrafficDB = db
}

// bytesPerSec returns the rate between two counter readings, and false if it can't be
// computed (no elapsed time, or the counter went backwards after an interface reset)
func bytesPerSec(prev, cur uint64, seconds float64) (uint64, bool) {
	if seconds <= 0 || cur < prev {
		return 0, false
	}
	return uint64(float64(cur-prev) / seconds), true
}

// PollSystemSample reads /proc once and updates the shared snapshot (interface counters and
// rates, CPU usage), recording interface throughput into the history database. Called once a
// second by the background poller; request handlers must not call it.
func PollSystemSample() error {
	raw, err := readNetDev()
	if err != nil {
		return err
	}
	cpu, cpuErr := readCPUSample()
	now := time.Now()

	sampleMu.Lock()
	defer sampleMu.Unlock()

	next := make(map[string]ifaceSnapshot, len(raw))
	for iface, cur := range raw {
		snap := cur

		prev, seen := lastIfaceStats[iface]
		physical := prev.physical
		if !seen {
			physical = isPhysical(iface)
		}
		snap.physical = physical

		if seen {
			dt := now.Sub(prev.timestamp).Seconds()
			rx, rxOK := bytesPerSec(prev.rxBytes, cur.rxBytes, dt)
			tx, txOK := bytesPerSec(prev.txBytes, cur.txBytes, dt)
			snap.rxBytesSec, snap.txBytesSec = rx, tx

			// History is kept for physical interfaces only (that is all the UI charts), and only
			// with a valid previous reading; a first sample or a counter reset would otherwise
			// store a bogus zero rate
			if physical && rxOK && txOK && globalTrafficDB != nil {
				globalTrafficDB.RecordSample(iface, int64(rx), int64(tx))
			}
		}

		lastIfaceStats[iface] = ifaceSample{rxBytes: cur.rxBytes, txBytes: cur.txBytes, timestamp: now, physical: physical}
		next[iface] = snap
	}
	latestIfaces = next

	// Forget interfaces that have disappeared so a returning one starts from a fresh reading
	for iface := range lastIfaceStats {
		if _, ok := raw[iface]; !ok {
			delete(lastIfaceStats, iface)
		}
	}

	if cpuErr == nil {
		if haveLastCPU {
			if totalDelta := cpu.Total - lastCPU.Total; totalDelta > 0 {
				idleDelta := cpu.Idle - lastCPU.Idle
				latestCPUPct = (float64(totalDelta-idleDelta) / float64(totalDelta)) * 100.0
			}
		}
		lastCPU, haveLastCPU = cpu, true
	}

	return nil
}

// latestSample returns the most recent snapshot, taking one first if the poller hasn't run yet
func latestSample() (map[string]ifaceSnapshot, float64) {
	sampleMu.Lock()
	ifaces, cpu := latestIfaces, latestCPUPct
	sampleMu.Unlock()

	if ifaces == nil {
		_ = PollSystemSample()
		sampleMu.Lock()
		ifaces, cpu = latestIfaces, latestCPUPct
		sampleMu.Unlock()
	}
	return ifaces, cpu
}

// GetSystemStats gathers live CPU, Memory, Swap, Uptime, Storage, State Table, and Network metrics
func GetSystemStats(configPath string) (SystemStats, error) {
	var stats SystemStats
	stats.Interfaces = make(map[string]InterfaceStats)

	sysCfg, err := config.LoadSysConfig(configPath)
	if err != nil {
		return stats, fmt.Errorf("failed to load sys config: %w", err)
	}

	stats.Hostname = sysCfg.Hostname
	stats.PeakRouteBuild = sysCfg.PeakRouteBuild
	stats.Arch = sysCfg.Arch
	stats.Kernel = sysCfg.Kernel

	netConfigPath := filepath.Join(filepath.Dir(configPath), "network.json")
	netCfg, _ := config.LoadNetworkConfig(netConfigPath)

	stats.CPUModel = getCPUModel()
	stats.KernelVersion = getKernelVersion()

	if uptimeBytes, err := os.ReadFile("/proc/uptime"); err == nil {
		fields := strings.Fields(string(uptimeBytes))
		if len(fields) > 0 {
			if sec, err := strconv.ParseFloat(fields[0], 64); err == nil {
				stats.UptimeSeconds = uint64(sec)
			}
		}
	}

	if err := parseMemInfo(&stats); err != nil {
		return stats, fmt.Errorf("failed to parse meminfo: %w", err)
	}

	// Interface rates and CPU usage come from the background poller's latest snapshot, so
	// serving this request neither blocks on a sampling window nor records history samples
	snapshot, cpuPct := latestSample()
	stats.CPUUsagePct = cpuPct

	stats.LoadAvg = getLoadAverage()
	stats.CPUTempC = getCPUTemperature()
	stats.Disk = getDiskUsage("/")
	stats.StateTable = getStateTableSize()
	stats.DNSServers = getDNSServers()

	fillInterfaceStats(&stats, snapshot, netCfg)

	return stats, nil
}

func getCPUModel() string {
	file, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "model name") || strings.HasPrefix(line, "Processor") || strings.HasPrefix(line, "Hardware") {
			parts := strings.Split(line, ":")
			if len(parts) > 1 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func getKernelVersion() string {
	ostype, _ := os.ReadFile("/proc/sys/kernel/ostype")
	osrelease, _ := os.ReadFile("/proc/sys/kernel/osrelease")

	sysName := strings.TrimSpace(string(ostype))
	rel := strings.TrimSpace(string(osrelease))
	if sysName == "" && rel == "" {
		return ""
	}

	return fmt.Sprintf("%s %s", sysName, rel)
}

func parseMemInfo(stats *SystemStats) error {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return err
	}
	defer file.Close()

	var memTotal, memFree, buffers, cached, sReclaimable uint64
	var swapTotal, swapFree uint64

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		val, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
			case "MemTotal:":
				memTotal = val
			case "MemFree:":
				memFree = val
			case "Buffers:":
				buffers = val
			case "Cached:":
				cached = val
			case "SReclaimable:":
				sReclaimable = val
			case "SwapTotal:":
				swapTotal = val
			case "SwapFree:":
				swapFree = val
		}
	}

	if memTotal > 0 {
		memAvailable := memFree + buffers + cached + sReclaimable
		memUsed := memTotal - memAvailable

		stats.MemoryTotalMB = memTotal / 1024
		stats.MemoryUsedMB = memUsed / 1024
		stats.MemoryFreeMB = memAvailable / 1024
	}

	if swapTotal > 0 {
		stats.SwapTotalMB = swapTotal / 1024
		stats.SwapFreeMB = swapFree / 1024
		stats.SwapUsedMB = (swapTotal - swapFree) / 1024
	}

	return scanner.Err()
}

func readCPUSample() (CpuSample, error) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return CpuSample{}, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)[1:]
			var total, idle uint64
			for i, field := range fields {
				val, _ := strconv.ParseUint(field, 10, 64)
				total += val
				if i == 3 || i == 4 {
					idle += val
				}
			}
			return CpuSample{Idle: idle, Total: total}, nil
		}
	}
	return CpuSample{}, fmt.Errorf("cpu line not found in /proc/stat")
}

func getLoadAverage() [3]float64 {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return [3]float64{0, 0, 0}
	}
	parts := strings.Fields(string(data))
	var load [3]float64
	for i := 0; i < 3 && i < len(parts); i++ {
		load[i], _ = strconv.ParseFloat(parts[i], 64)
	}
	return load
}

func getCPUTemperature() float64 {
	zones, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err == nil && len(zones) > 0 {
		raw, err := os.ReadFile(zones[0])
		if err == nil {
			mC, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
			if err == nil {
				return mC / 1000.0
			}
		}
	}
	return 0.0
}

func getStateTableSize() StateTableInfo {
	var info StateTableInfo
	if countBytes, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_count"); err == nil {
		info.Current, _ = strconv.Atoi(strings.TrimSpace(string(countBytes)))
	}
	if maxBytes, err := os.ReadFile("/proc/sys/net/netfilter/nf_conntrack_max"); err == nil {
		info.Max, _ = strconv.Atoi(strings.TrimSpace(string(maxBytes)))
	}
	return info
}

func getDiskUsage(path string) DiskStats {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskStats{}
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	used := total - free

	var percent float64
	if total > 0 {
		percent = (float64(used) / float64(total)) * 100.0
	}

	return DiskStats{
		TotalMB:     total / (1024 * 1024),
		UsedMB:      used / (1024 * 1024),
		FreeMB:      free / (1024 * 1024),
		UsedPercent: percent,
	}
}

func getDNSServers() []string {
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer file.Close()

	var servers []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "nameserver") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				servers = append(servers, fields[1])
			}
		}
	}
	return servers
}

func isPhysicalHardware(ifaceName string) bool {
	if ifaceName == "lo" || strings.Contains(ifaceName, ".") {
		return false
	}
	_, err := os.Stat("/sys/class/net/" + ifaceName + "/device")
	return err == nil
}

// readNetDev parses /proc/net/dev into raw per-interface counters (no rates, no side effects)
func readNetDev() (map[string]ifaceSnapshot, error) {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer file.Close()

	result := make(map[string]ifaceSnapshot)

	scanner := bufio.NewScanner(file)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		if lineCount <= 2 {
			continue
		}

		parts := strings.Split(scanner.Text(), ":")
		if len(parts) != 2 {
			continue
		}

		iface := strings.TrimSpace(parts[0])
		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}

		var snap ifaceSnapshot
		snap.rxBytes, _ = strconv.ParseUint(fields[0], 10, 64)
		snap.rxPackets, _ = strconv.ParseUint(fields[1], 10, 64)
		snap.txBytes, _ = strconv.ParseUint(fields[8], 10, 64)
		snap.txPackets, _ = strconv.ParseUint(fields[9], 10, 64)
		result[iface] = snap
	}

	return result, scanner.Err()
}

// fillInterfaceStats turns the poller's snapshot into API output, tagging each interface
// with its configured role. It only reads; it never samples or records anything.
func fillInterfaceStats(stats *SystemStats, snapshot map[string]ifaceSnapshot, netCfg *config.NetworkConfig) {
	wanIface := ""
	var lanIfaces []string

	if netCfg != nil {
		wanIface = strings.TrimSpace(netCfg.WanInterface)
		lanIfaces = netCfg.LanInterfaces
	}

	for iface, snap := range snapshot {
		role := "unassigned"
		if wanIface != "" && iface == wanIface {
			role = "wan"
		} else {
			for _, lan := range lanIfaces {
				if iface == lan {
					role = "lan"
					break
				}
			}
		}

		stats.Interfaces[iface] = InterfaceStats{
			RxBytes:    snap.rxBytes,
			RxPackets:  snap.rxPackets,
			RxBytesSec: snap.rxBytesSec,
			TxBytes:    snap.txBytes,
			TxPackets:  snap.txPackets,
			TxBytesSec: snap.txBytesSec,
			IsPhysical: snap.physical,
			Role:       role,
		}
	}
}

func RegisterStatsRoutes(mux *http.ServeMux, configPath string) {
	mux.HandleFunc("/api/v1/system/stats", func(w http.ResponseWriter, r *http.Request) {
		HandleSystemStats(w, r, configPath)
	})
}

func HandleSystemStats(w http.ResponseWriter, r *http.Request, configPath string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats, err := GetSystemStats(configPath)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to retrieve system metrics: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	json.NewEncoder(w).Encode(stats)
}

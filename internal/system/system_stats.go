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
}

var (
	ifaceMu         sync.Mutex
	lastIfaceStats  = make(map[string]ifaceSample)
	globalTrafficDB *TrafficDB
)

// SetTrafficDB registers the database instance so stats poller can record history
func SetTrafficDB(db *TrafficDB) {
	globalTrafficDB = db
}

// PollNetworkInterfaces strictly parses /proc/net/dev for 1-second background ticks
func PollNetworkInterfaces(configPath string) error {
	netConfigPath := filepath.Join(filepath.Dir(configPath), "network.json")
	netCfg, _ := config.LoadNetworkConfig(netConfigPath)

	var dummyStats SystemStats
	dummyStats.Interfaces = make(map[string]InterfaceStats)

	return parseNetworkInterfaces(&dummyStats, netCfg)
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

	if cpuPct, err := sampleCPUUsage(100 * time.Millisecond); err == nil {
		stats.CPUUsagePct = cpuPct
	}

	stats.LoadAvg = getLoadAverage()
	stats.CPUTempC = getCPUTemperature()
	stats.Disk = getDiskUsage("/")
	stats.StateTable = getStateTableSize()
	stats.DNSServers = getDNSServers()

	if err := parseNetworkInterfaces(&stats, netCfg); err != nil {
		return stats, fmt.Errorf("failed to parse net dev: %w", err)
	}

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

func sampleCPUUsage(interval time.Duration) (float64, error) {
	s1, err := readCPUSample()
	if err != nil {
		return 0, err
	}

	time.Sleep(interval)

	s2, err := readCPUSample()
	if err != nil {
		return 0, err
	}

	totalDelta := float64(s2.Total - s1.Total)
	idleDelta := float64(s2.Idle - s1.Idle)

	if totalDelta == 0 {
		return 0, nil
	}

	return ((totalDelta - idleDelta) / totalDelta) * 100.0, nil
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

func parseNetworkInterfaces(stats *SystemStats, netCfg *config.NetworkConfig) error {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return err
	}
	defer file.Close()

	wanIface := ""
	var lanIfaces []string

	if netCfg != nil {
		wanIface = strings.TrimSpace(netCfg.WanInterface)
		lanIfaces = netCfg.LanInterfaces
	}

	now := time.Now()

	ifaceMu.Lock()
	defer ifaceMu.Unlock()

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

		rxBytes, _ := strconv.ParseUint(fields[0], 10, 64)
		rxPackets, _ := strconv.ParseUint(fields[1], 10, 64)
		txBytes, _ := strconv.ParseUint(fields[8], 10, 64)
		txPackets, _ := strconv.ParseUint(fields[9], 10, 64)

		// Calculate live throughput delta
		var rxBytesSec, txBytesSec uint64
		if prev, ok := lastIfaceStats[iface]; ok {
			dt := now.Sub(prev.timestamp).Seconds()
			if dt > 0 {
				if rxBytes >= prev.rxBytes {
					rxBytesSec = uint64(float64(rxBytes-prev.rxBytes) / dt)
				}
				if txBytes >= prev.txBytes {
					txBytesSec = uint64(float64(txBytes-prev.txBytes) / dt)
				}
			}
		}

		// Record live throughput (Bytes/sec) into SQLite history
		if globalTrafficDB != nil {
			globalTrafficDB.RecordSample(iface, int64(rxBytesSec), int64(txBytesSec))
		}

		lastIfaceStats[iface] = ifaceSample{
			rxBytes:   rxBytes,
			txBytes:   txBytes,
			timestamp: now,
		}

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
			RxBytes:    rxBytes,
			RxPackets:  rxPackets,
			RxBytesSec: rxBytesSec,
			TxBytes:    txBytes,
			TxPackets:  txPackets,
			TxBytesSec: txBytesSec,
			IsPhysical: isPhysicalHardware(iface),
			Role:       role,
		}
	}

	return scanner.Err()
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

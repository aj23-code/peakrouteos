package system

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"encoding/json"
	"net/http"
)

type InterfaceStats struct {
	RxBytes   uint64 `json:"rx_bytes"`
	RxPackets uint64 `json:"rx_packets"`
	TxBytes   uint64 `json:"tx_bytes"`
	TxPackets uint64 `json:"tx_packets"`
}

type SystemStats struct {
	UptimeSeconds uint64                    `json:"uptime_seconds"`
	CPUUsagePct   float64                   `json:"cpu_usage_pct"`
	MemoryTotalMB uint64                    `json:"memory_total_mb"`
	MemoryUsedMB  uint64                    `json:"memory_used_mb"`
	MemoryFreeMB  uint64                    `json:"memory_free_mb"`
	Interfaces    map[string]InterfaceStats `json:"interfaces"`
}

type CpuSample struct {
	Idle uint64
	Total uint64
}

// GetSystemStats gathers live CPU, Memory, Uptime, and Network Interface metrics
func GetSystemStats() (SystemStats, error) {
	var stats SystemStats
	stats.Interfaces = make(map[string]InterfaceStats)

	// 1. Read Uptime
	if uptimeBytes, err := os.ReadFile("/proc/uptime"); err == nil {
		fields := strings.Fields(string(uptimeBytes))
		if len(fields) > 0 {
			if sec, err := strconv.ParseFloat(fields[0], 64); err == nil {
				stats.UptimeSeconds = uint64(sec)
			}
		}
	}

	// 2. Read Memory Info
	if err := parseMemInfo(&stats); err != nil {
		return stats, fmt.Errorf("failed to parse meminfo: %w", err)
	}

	// 3. Sample CPU Usage over 100ms
	if cpuPct, err := sampleCPUUsage(100 * time.Millisecond); err == nil {
		stats.CPUUsagePct = cpuPct
	}

	// 4. Read Interface Statistics
	if err := parseNetworkInterfaces(&stats); err != nil {
		return stats, fmt.Errorf("failed to parse net dev: %w", err)
	}

	return stats, nil
}

func parseMemInfo(stats *SystemStats) error {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return err
	}
	defer file.Close()

	var memTotal, memFree, buffers, cached, sReclaimable uint64
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
		}
	}

	if memTotal > 0 {
		// Available memory calculation matching modern Linux 'free' utility
		memAvailable := memFree + buffers + cached + sReclaimable
		memUsed := memTotal - memAvailable

		stats.MemoryTotalMB = memTotal / 1024
		stats.MemoryUsedMB = memUsed / 1024
		stats.MemoryFreeMB = memAvailable / 1024
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

	usage := ((totalDelta - idleDelta) / totalDelta) * 100.0
	return usage, nil
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
				// Index 3 is idle, index 4 is iowait
				if i == 3 || i == 4 {
					idle += val
				}
			}
			return CpuSample{Idle: idle, Total: total}, nil
		}
	}
	return CpuSample{}, fmt.Errorf("cpu line not found in /proc/stat")
}

func parseNetworkInterfaces(stats *SystemStats) error {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		// Skip header lines
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

		stats.Interfaces[iface] = InterfaceStats{
			RxBytes:   rxBytes,
			RxPackets: rxPackets,
			TxBytes:   txBytes,
			TxPackets: txPackets,
		}
	}

	return scanner.Err()
}

func RegisterStatsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/system/stats", HandleSystemStats)
}

func HandleSystemStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats, err := GetSystemStats()
	if err != nil {
		http.Error(w, "Failed to retrieve system metrics", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")

	json.NewEncoder(w).Encode(stats)
}

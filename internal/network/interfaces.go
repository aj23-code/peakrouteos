package network

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"peakroute/internal/config"
)

// GetPhysicalInterfaces queries host NICs and returns all non-virtual physical interfaces
func GetPhysicalInterfaces() ([]config.SystemInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to query network interfaces: %w", err)
	}

	var physical []config.SystemInterface
	for _, iface := range ifaces {
		// Filter out loopback, bridges, docker, veth pairs, tun/tap, wireguard, virbr
		name := iface.Name
		if iface.Flags&net.FlagLoopback != 0 ||
			strings.HasPrefix(name, "br") ||
			strings.HasPrefix(name, "docker") ||
			strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "tun") ||
			strings.HasPrefix(name, "tap") ||
			strings.HasPrefix(name, "wg") ||
			strings.HasPrefix(name, "virbr") ||
			strings.Contains(name, ".") { // Skip VLAN sub-interfaces like br0.10
			continue
		}

		mac := iface.HardwareAddr.String()

		// Fetch sysfs details (state, speed, mtu, driver)
		state, speed, mtu, driver := EnrichInterfaceDetails(name)

		physical = append(physical, config.SystemInterface{
			Name:     name,
			MAC:      mac,
			State:    state,
			Speed:    speed,
			MTU:      mtu,
			Driver:   driver,
			Wireless: IsWireless("/sys/class/net", name),
		})
	}

	return physical, nil
}

func EnrichInterfaceDetails(ifaceName string) (state, speed string, mtu int, driver string) {
	basePath := filepath.Join("/sys/class/net", ifaceName)

	// 1. Read Link State (operstate: "up", "down", etc.)
	if data, err := os.ReadFile(filepath.Join(basePath, "operstate")); err == nil {
		state = strings.TrimSpace(string(data))
	} else {
		state = "unknown"
	}

	// 2. Read MTU
	if data, err := os.ReadFile(filepath.Join(basePath, "mtu")); err == nil {
		var m int
		if _, err := fmt.Sscanf(string(data), "%d", &m); err == nil {
			mtu = m
		}
	}
	if mtu == 0 {
		mtu = 1500 // Fallback default
	}

	// 3. Read Speed (in Mbps, e.g., "1000")
	if data, err := os.ReadFile(filepath.Join(basePath, "speed")); err == nil {
		spd := strings.TrimSpace(string(data))
		if spd != "" && spd != "-1" {
			speed = spd + " Mbps"
		} else {
			speed = "N/A"
		}
	} else {
		speed = "N/A"
	}

	// 4. Read Driver (resolved via device/driver symlink)
	if driverPath, err := os.Readlink(filepath.Join(basePath, "device", "driver")); err == nil {
		driver = filepath.Base(driverPath)
	} else {
		driver = "unknown"
	}

	return state, speed, mtu, driver
}

// SyncPhysicalInterfaces updates the staged JSON state with discovered hardware interfaces and commits
func SyncPhysicalInterfaces(mgr *config.Manager) error {
	physIfaces, err := GetPhysicalInterfaces()
	if err != nil {
		return err
	}

	err = mgr.StageNetwork(func(cfg *config.NetworkConfig) error {
		cfg.Interfaces = physIfaces
		return nil
	})
	if err != nil {
		return err
	}

	return mgr.CommitNetwork()
}

// StagePhysicalInterfacesUpdates updates candidate/staged JSON state with hardware interface discovery
func StagePhysicalInterfacesUpdates(mgr *config.Manager) error {
	physIfaces, err := GetPhysicalInterfaces()
	if err != nil {
		return err
	}

	return mgr.StageNetwork(func(cfg *config.NetworkConfig) error {
		cfg.Interfaces = physIfaces
		return nil
	})
}

// IsWireless reports whether a network interface is a Wi-Fi adapter: the kernel gives those a
// "wireless" folder or a "phy80211" link next to the interface.
func IsWireless(sysClassNet, name string) bool {
	base := filepath.Join(sysClassNet, name)
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return true
	}
	_, err := os.Lstat(filepath.Join(base, "phy80211"))
	return err == nil
}

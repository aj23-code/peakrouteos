package network

import (
	"fmt"
	"net"
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

			physical = append(physical, config.SystemInterface{
				Name: name,
				MAC:  mac,
			})
	}

	return physical, nil
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

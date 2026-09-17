package network

import (
	"net"
	"strings"
)

type SystemInterface struct {
	Name string
	MAC  string
}

// GetPhysicalInterfaces returns all available non-virtual NICs
func GetPhysicalInterfaces() ([]SystemInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var physical []SystemInterface
	for _, iface := range ifaces {
		// Filter out loopback, bridge, docker, and virtual ethernet pairs
		if iface.Flags&net.FlagLoopback != 0 ||
			strings.HasPrefix(iface.Name, "br") ||
			strings.HasPrefix(iface.Name, "docker") ||
			strings.HasPrefix(iface.Name, "veth") {
				continue
			}

			physical = append(physical, SystemInterface{
				Name: iface.Name,
				MAC:  iface.HardwareAddr.String(),
			})
	}

	return physical, nil
}

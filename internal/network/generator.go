package network

import (
	"fmt"
	"strings"
	"peakroute/internal/config"
)

func GenerateInterfaces(cfg *config.NetworkConfig) string {
	var sb strings.Builder

	sb.WriteString("auto lo\niface lo inet loopback\n\n")

	// WAN Interface Configuration
	sb.WriteString(fmt.Sprintf("# WAN Interface\nauto %s\niface %s inet dhcp\n\n", cfg.WanInterface, cfg.WanInterface))

	// Physical LAN Interfaces
	sb.WriteString("# LAN Member Ports\n")
	for _, iface := range cfg.LanInterfaces {
		sb.WriteString(fmt.Sprintf("auto %s\niface %s inet manual\n\n", iface, iface))
	}

	// LAN Bridge Configuration
	ipParts := strings.Split(cfg.LanIP, "/")
	ip := ipParts[0]

	sb.WriteString("# LAN Bridge\nauto br0\niface br0 inet static\n")
	sb.WriteString(fmt.Sprintf(" address %s\n", ip))
	sb.WriteString(" netmask 255.255.255.0\n")
	sb.WriteString(fmt.Sprintf(" bridge_ports %s\n", strings.Join(cfg.LanInterfaces, " ")))
	sb.WriteString(" bridge_stp off\n")
	sb.WriteString(" bridge_fd 0\n")

	return sb.String()
}

package config

import (
	"fmt"
	"net/netip"
)

// Router-mode LAN defaults, used only where network.json leaves a value blank.
const (
	DefaultLanIP      = "192.168.1.1"
	DefaultLanNetmask = "255.255.255.0"
	DefaultDHCPLease  = "12h"
)

// DefaultDHCP returns the default DHCP server settings for a LAN: hosts .100 to .200 of the
// router's own /24 and a 12 hour lease. It returns nil for anything other than a /24 with a valid
// router address, because that range is only guaranteed to fit a /24.
func DefaultDHCP(lanIP, lanNetmask string) *DHCPConfig {
	ip, ok := parseIPv4(lanIP)
	if !ok {
		return nil
	}
	if ones, ok := parseNetmask(lanNetmask); !ok || ones != 24 {
		return nil
	}
	b := ip.As4()
	return &DHCPConfig{
		Enabled:    true,
		RangeStart: netip.AddrFrom4([4]byte{b[0], b[1], b[2], 100}).String(),
		RangeEnd:   netip.AddrFrom4([4]byte{b[0], b[1], b[2], 200}).String(),
		LeaseTime:  DefaultDHCPLease,
	}
}

// DHCPRangeFitsLan reports whether a DHCP range is usable on the given LAN: both ends are host
// addresses inside the subnet, in order, and the router's own address is not inside the range.
func DHCPRangeFitsLan(lanIP, lanNetmask, start, end string) error {
	cfg := NetworkConfig{
		WanInterface: "wan0", LanInterfaces: []string{"lan0"},
		LanIP: lanIP, LanSubnet: lanNetmask,
		DHCP: &DHCPConfig{Enabled: true, RangeStart: start, RangeEnd: end, LeaseTime: DefaultDHCPLease},
	}
	err := cfg.Validate()
	if err == nil {
		return nil
	}
	if ve, ok := err.(ValidationError); ok {
		for _, e := range ve {
			if e.Field == "dhcp_range_start" || e.Field == "dhcp_range_end" || e.Field == "lan_ip" || e.Field == "lan_subnet" {
				return fmt.Errorf("%s: %s", e.Field, e.Message)
			}
		}
		return nil // only unrelated fields (the placeholder interface names) complained
	}
	return err
}

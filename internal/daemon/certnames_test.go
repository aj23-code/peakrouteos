package daemon

import (
	"net"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func ipList(ips []net.IP) string {
	var s []string
	for _, ip := range ips {
		s = append(s, ip.String())
	}
	return strings.Join(s, " ")
}

// The certificate names the addresses the device is reached at: a router's from its settings; an access point's from its bridge (the
// lease from the main router), its fixed address and the recovery address, never a router's leftover 192.168.1.1 (seen on a U6+).
func TestTheCertificateNamesTheAddressesTheDeviceHas(t *testing.T) {
	bridges := func(name string) []net.IP {
		switch name {
		case "br0":
			return []net.IP{net.ParseIP("192.168.1.254"), net.ParseIP("192.168.77.1")}
		case "brsetup":
			return []net.IP{net.ParseIP("192.168.1.165")}
		}
		return nil
	}
	router := config.NetworkConfig{LanIP: "192.168.1.1", Networks: []config.Network{{IP: "10.20.0.1"}}}
	if got := ipList(certAddresses(router, false, bridges)); got != "192.168.1.1 10.20.0.1" {
		t.Errorf("router: %s", got)
	}
	ap := router
	ap.AP = &config.APConfig{Enabled: true, Ports: []string{"eth0"}}
	if got := ipList(certAddresses(ap, false, bridges)); got != "192.168.1.254 192.168.77.1 192.168.1.165" {
		t.Errorf("an access point (DHCP): %s", got)
	}
	ap.AP.Mode, ap.AP.IP = config.APStatic, "192.168.1.50"
	if got := ipList(certAddresses(ap, false, func(string) []net.IP { return nil })); got != "192.168.1.50 192.168.77.1" {
		t.Errorf("an access point with a fixed address and no bridge up yet: %s", got)
	}
	// the access point build before its first settings: the setup network's lease
	if got := ipList(certAddresses(router, true, bridges)); !strings.Contains(got, "192.168.1.165") || strings.Contains(got, "192.168.1.1 ") || strings.HasPrefix(got, "192.168.1.1") {
		t.Errorf("the access point build, not set up: %s", got)
	}
}

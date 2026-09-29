package network

import (
	"bufio"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"peakroute/internal/config"
)

// TrafficTracker manages per-MAC byte counts in a thread-safe map
type TrafficTracker struct {
	mu        sync.RWMutex
	macBytes  map[string]uint64
	ipToMac   map[string]string
	ifaceName string
	mgr       *config.Manager
}

var GlobalTracker *TrafficTracker

// htons converts a short integer from host to network byte order
func htons(i uint16) uint16 {
	return (i<<8)&0xff00 | (i>>8)&0x00ff
}

// InitTrafficTracker initializes and starts the background traffic monitoring engine
func InitTrafficTracker(mgr *config.Manager, ifaceName string) {
	if ifaceName == "" {
		if netCfg, err := config.LoadNetworkConfig("/etc/peakroute/network.json"); err == nil && len(netCfg.LanInterfaces) > 0 {
			ifaceName = netCfg.LanInterfaces[0]
		}
	}

	GlobalTracker = &TrafficTracker{
		macBytes:  make(map[string]uint64),
		ipToMac:   make(map[string]string),
		ifaceName: ifaceName,
		mgr:       mgr,
	}

	// Always start polling ARP table so IP-to-MAC mappings build up
	go GlobalTracker.pollARPTableLoop(10 * time.Second)

	// Only launch packet capture if an interface is configured
	if ifaceName != "" {
		log.Printf("[+] Traffic Tracker initializing on interface: %s", ifaceName)
		go GlobalTracker.startPacketCapture()
	} else {
		log.Printf("[!] Traffic Tracker: No LAN interface configured yet. Awaiting setup wizard completion.")
	}
}

// GetBytesForMAC returns the accumulated byte count for a given MAC address
func (t *TrafficTracker) GetBytesForMAC(mac string) uint64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.macBytes[strings.ToLower(mac)]
}

// pollARPTableLoop parses /proc/net/arp to update IP-to-MAC mappings
func (t *TrafficTracker) pollARPTableLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial parse
	t.updateARPTable()

	for range ticker.C {
		t.updateARPTable()
	}
}

func (t *TrafficTracker) updateARPTable() {
	file, err := os.Open("/proc/net/arp")
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Skip header line
	if scanner.Scan() {
		_ = scanner.Text()
	}

	newMap := make(map[string]string)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}

		ip := fields[0]
		hwFlags := fields[2]
		mac := strings.ToLower(fields[3])
		dev := fields[5]

		// Filter for valid completed ARP entries on target interface or bridges
		if hwFlags != "0x0" && mac != "00:00:00:00:00:00" {
			if t.ifaceName == "" || dev == t.ifaceName || strings.HasPrefix(dev, "br") {
				newMap[ip] = mac
			}
		}
	}

	t.mu.Lock()
	t.ipToMac = newMap
	t.mu.Unlock()
}

// startPacketCapture opens a raw AF_PACKET socket to accumulate frame lengths per IP/MAC
func (t *TrafficTracker) startPacketCapture() {
	// Create raw AF_PACKET socket capturing all protocols (ETH_P_ALL)
	sock, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		log.Printf("[!] Traffic Tracker raw socket error: %v (Ensure CAP_NET_RAW is set)", err)
		return
	}
	defer syscall.Close(sock)

	// Bind raw socket directly to target LAN interface
	iface, err := net.InterfaceByName(t.ifaceName)
	if err != nil {
		log.Printf("[!] Traffic Tracker failed to resolve interface %s: %v", t.ifaceName, err)
		return
	}

	sll := &syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_ALL),
		Ifindex:  iface.Index,
	}

	if err := syscall.Bind(sock, sll); err != nil {
		log.Printf("[!] Traffic Tracker failed to bind socket to %s: %v", t.ifaceName, err)
		return
	}

	log.Printf("[+] Traffic Tracker successfully capturing on interface: %s", t.ifaceName)

	buf := make([]byte, 65535)
	for {
		n, _, err := syscall.Recvfrom(sock, buf, 0)
		if err != nil || n < 34 { // Min size: 14 byte Ethernet header + 20 byte IPv4 header
			continue
		}

		// Inspect EtherType (offset 12-13) for IPv4 (0x0800)
		etherType := uint16(buf[12])<<8 | uint16(buf[13])
		if etherType != 0x0800 {
			continue
		}

		// Parse Source IP (bytes 26-29) and Destination IP (bytes 30-33) from IPv4 header
		srcIP := net.IP(buf[26:30]).String()
		dstIP := net.IP(buf[30:34]).String()

		t.mu.Lock()
		// Credit frame size to client MAC whether packet is outbound (src) or inbound (dst)
		if mac, ok := t.ipToMac[srcIP]; ok {
			t.macBytes[mac] += uint64(n)
		} else if mac, ok := t.ipToMac[dstIP]; ok {
			t.macBytes[mac] += uint64(n)
		}
		t.mu.Unlock()
	}
}

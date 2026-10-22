package network

import (
	"bufio"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"peakroute/internal/config"
)

// TrafficTracker manages per-MAC byte counts in a thread-safe map. It listens on every LAN-side interface that has neighbours (the
// default network's bridge or port, each other network's bridge, the setup network's bridge), found from the ARP table, so it follows
// the committed settings instead of the ones at the time the daemon started (on a new device there are none yet).
type TrafficTracker struct {
	mu       sync.RWMutex
	macBytes map[string]uint64
	ipToMac  map[string]string
	capture  map[string]bool      // interfaces a capture is running on
	retry    map[string]time.Time // when a capture that ended may be started again (a failing one is not tried every ten seconds)
	mgr      *config.Manager
	arpPath  string
}

var GlobalTracker *TrafficTracker

// htons converts a short integer from host to network byte order
func htons(i uint16) uint16 {
	return (i<<8)&0xff00 | (i>>8)&0x00ff
}

// InitTrafficTracker initializes and starts the background traffic monitoring engine. ifaceName is kept for the callers that name one
// interface to watch; empty means every LAN-side interface.
func InitTrafficTracker(mgr *config.Manager, ifaceName string) {
	GlobalTracker = &TrafficTracker{
		macBytes: make(map[string]uint64),
		ipToMac:  make(map[string]string),
		capture:  make(map[string]bool),
		retry:    make(map[string]time.Time),
		mgr:      mgr,
		arpPath:  "/proc/net/arp",
	}
	go GlobalTracker.pollARPTableLoop(10 * time.Second)
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

// pollARPTableLoop reads the ARP table for IP-to-MAC mappings and starts a capture on each LAN-side interface it finds
func (t *TrafficTracker) pollARPTableLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		t.refresh()
		<-ticker.C
	}
}

func (t *TrafficTracker) refresh() {
	devs := t.updateARPTable()
	var wan []string
	if t.mgr != nil {
		cfg := t.mgr.NetworkConfig()
		wan = []string{cfg.WanInterface, cfg.WanLink(), cfg.WanDevice()}
	}
	for _, dev := range captureTargets(devs, wan) {
		t.mu.Lock()
		start := !t.capture[dev] && !time.Now().Before(t.retry[dev])
		if start {
			t.capture[dev] = true
		}
		t.mu.Unlock()
		if start {
			go t.startPacketCapture(dev)
		}
	}
}

// captureTargets are the interfaces to count on: those with neighbours, without the internet connection (all of its traffic is a client's
// too, seen on the LAN side, and the router's own would be counted for nobody), the loopback and tunnels.
func captureTargets(devs, wan []string) []string {
	skip := map[string]bool{"": true, "lo": true}
	for _, w := range wan {
		skip[w] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range devs {
		if skip[d] || seen[d] || strings.HasPrefix(d, "wg") || strings.HasPrefix(d, "tun") || strings.HasPrefix(d, "ppp") {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// updateARPTable replaces the IP-to-MAC mappings with the completed entries of the ARP table and returns the interfaces they are on.
func (t *TrafficTracker) updateARPTable() []string {
	file, err := os.Open(t.arpPath)
	if err != nil {
		return nil
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Skip header line
	if scanner.Scan() {
		_ = scanner.Text()
	}

	newMap := make(map[string]string)
	var devs []string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}

		ip := fields[0]
		hwFlags := fields[2]
		mac := strings.ToLower(fields[3])

		// only completed entries
		if hwFlags != "0x0" && mac != "00:00:00:00:00:00" {
			newMap[ip] = mac
			devs = append(devs, fields[5])
		}
	}

	t.mu.Lock()
	t.ipToMac = newMap
	t.mu.Unlock()
	return devs
}

// startPacketCapture opens a raw AF_PACKET socket on one interface and accumulates frame lengths per client MAC until the interface
// goes away.
func (t *TrafficTracker) startPacketCapture(ifaceName string) {
	defer func() {
		t.mu.Lock()
		delete(t.capture, ifaceName)
		if t.retry != nil {
			t.retry[ifaceName] = time.Now().Add(time.Minute)
		}
		t.mu.Unlock()
	}()

	// Create raw AF_PACKET socket capturing all protocols (ETH_P_ALL)
	sock, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_ALL)))
	if err != nil {
		log.Printf("[!] Traffic Tracker raw socket error: %v (Ensure CAP_NET_RAW is set)", err)
		return
	}
	defer syscall.Close(sock)

	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		log.Printf("[!] Traffic Tracker failed to resolve interface %s: %v", ifaceName, err)
		return
	}

	sll := &syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_ALL),
		Ifindex:  iface.Index,
	}

	if err := syscall.Bind(sock, sll); err != nil {
		log.Printf("[!] Traffic Tracker failed to bind socket to %s: %v", ifaceName, err)
		return
	}

	log.Printf("[+] Traffic Tracker counting on %s", ifaceName)

	buf := make([]byte, 65535)
	for {
		n, _, err := syscall.Recvfrom(sock, buf, 0)
		if err != nil {
			if err == syscall.EINTR || err == syscall.EAGAIN {
				continue
			}
			// the interface was removed (a network was deleted, networking restarted): the next look at the ARP table starts a new capture
			log.Printf("[+] Traffic Tracker stopped counting on %s: %v", ifaceName, err)
			return
		}
		if n < 34 { // Min size: 14 byte Ethernet header + 20 byte IPv4 header
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

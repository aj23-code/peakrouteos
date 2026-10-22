package network

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"peakroute/internal/config"
)

type ClientLease struct {
	MACAddress string `json:"mac_address"`
	IPAddress  string `json:"ip_address"`
	Hostname   string `json:"hostname"`
	Expiry     int64  `json:"expiry_epoch"`
	ExpiryText string `json:"expiry_text"`
	TotalBytes uint64 `json:"total_bytes"`
	// How the device is connected: "wifi" (with the network name and band), "wired", or "unknown". For a
	// device that is not here right now this is how it was last seen.
	Connection string `json:"connection"`
	SSID       string `json:"ssid,omitempty"`
	Band       string `json:"band,omitempty"`
	// Network is the name of the local network the device's address belongs to ("LAN", "IoT", "Guest"...); empty when it is in none.
	Network string `json:"network,omitempty"`
	// Reserved: the DHCP server keeps an address for this device (Settings of the DHCP page); Reservation is that address, which may
	// differ from the one it holds now until it asks again.
	Reserved    bool   `json:"reserved,omitempty"`
	Reservation string `json:"reservation,omitempty"`
	// ReserveChange: what the draft does to the device's reservation, until it is committed ("added", "removed", "changed"; empty: nothing).
	ReserveChange string `json:"reserve_change,omitempty"`
	// Whether the device is here now: "online", "offline" (the lease is still held, the device is gone) or
	// "unknown" (it cannot be told on this system). LastSeen is when it was last seen, Unix seconds (0: never).
	Status   string `json:"status"`
	LastSeen int64  `json:"last_seen_epoch"`
}

// ParseLeases reads a standard dnsmasq.leases file
func ParseLeases(filepath string) ([]ClientLease, error) {
	file, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var leases []ClientLease
	scanner := bufio.NewScanner(file)
	now := time.Now().Unix()

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}

		expiry, _ := strconv.ParseInt(fields[0], 10, 64)
		mac := strings.ToLower(fields[1])
		ip := fields[2]
		hostname := fields[3]

		if hostname == "*" || hostname == "" {
			hostname = "Unknown Device"
		}

		expiryText := "Expired"
		if expiry > now {
			diff := time.Duration(expiry-now) * time.Second
			expiryText = fmt.Sprintf("%dh %dm", int(diff.Hours()), int(diff.Minutes())%60)
		}

		var bytes uint64
		if GlobalTracker != nil {
			bytes = GlobalTracker.GetBytesForMAC(mac)
		}

		leases = append(leases, ClientLease{
			Expiry:     expiry,
			ExpiryText: expiryText,
			MACAddress: mac,
			IPAddress:  ip,
			Hostname:   hostname,
			TotalBytes: bytes,
		})
	}

	return leases, scanner.Err()
}

// GetTopClientsByUsage sorts leases by total bytes descending
func GetTopClientsByUsage(leases []ClientLease, limit int) []ClientLease {
	sort.Slice(leases, func(i, j int) bool {
		return leases[i].TotalBytes > leases[j].TotalBytes
	})

	if limit > 0 && len(leases) > limit {
		return leases[:limit]
	}
	return leases
}

// LeasePaths are where dnsmasq keeps its leases; the first that exists is read.
var LeasePaths = []string{"/var/lib/misc/dnsmasq.leases", "/tmp/dhcp.leases"}

// ReadLeases reads the lease file, or returns no leases when there is none.
func ReadLeases(paths []string) []ClientLease {
	path := paths[len(paths)-1]
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	leases, err := ParseLeases(path)
	if err != nil || leases == nil {
		return []ClientLease{}
	}
	return leases
}

// LabelNetworks says which local network each lease is on (the one whose subnet holds the address; the default network is called LAN)
// and whether the device has an address reserved.
func LabelNetworks(leases []ClientLease, cfg config.NetworkConfig) {
	type subnet struct {
		name string
		net  netip.Prefix
	}
	var subnets []subnet
	add := func(name, ip, mask string) {
		a, err := netip.ParseAddr(ip)
		m := net.IPMask(net.ParseIP(mask).To4())
		if err != nil || m == nil {
			return
		}
		ones, bits := m.Size()
		if bits == 0 {
			return
		}
		subnets = append(subnets, subnet{name, netip.PrefixFrom(a, ones).Masked()})
	}
	add("LAN", cfg.LanIP, cfg.LanSubnet)
	for _, n := range cfg.Networks {
		add(n.Name, n.IP, n.Netmask)
	}
	reserved := map[string]string{}
	for _, r := range cfg.Reservations {
		reserved[strings.ToLower(r.MAC)] = r.IP
	}
	for i := range leases {
		if ip, ok := reserved[strings.ToLower(leases[i].MACAddress)]; ok {
			leases[i].Reserved, leases[i].Reservation = true, ip
		}
		a, err := netip.ParseAddr(leases[i].IPAddress)
		if err != nil {
			continue
		}
		for _, s := range subnets {
			if s.net.Contains(a) {
				leases[i].Network = s.name
				break
			}
		}
	}
}

// MarkReservationChanges says, for each device, what the draft changes about its reserved address, so the page can show that a change
// waits for a commit (the list itself follows what is committed).
func MarkReservationChanges(leases []ClientLease, active, draft config.NetworkConfig) {
	ip := func(rs []config.Reservation) map[string]string {
		m := map[string]string{}
		for _, r := range rs {
			m[strings.ToLower(r.MAC)] = r.IP
		}
		return m
	}
	before, after := ip(active.Reservations), ip(draft.Reservations)
	for i := range leases {
		mac := strings.ToLower(leases[i].MACAddress)
		b, had := before[mac]
		a, has := after[mac]
		switch {
		case !had && has:
			leases[i].ReserveChange = "added"
		case had && !has:
			leases[i].ReserveChange = "removed"
		case had && has && a != b:
			leases[i].ReserveChange = "changed"
		}
	}
}

// LeaseHandler serves the active DHCP leases. annotate, when given, fills in how each device is connected and
// whether it is online; without it those fields are "unknown".
func LeaseHandler(annotate func(context.Context, []ClientLease)) http.HandlerFunc {
	return leaseHandler(LeasePaths, annotate)
}

func leaseHandler(paths []string, annotate func(context.Context, []ClientLease)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leases := ReadLeases(paths)
		if annotate != nil {
			annotate(r.Context(), leases)
		} else {
			for i := range leases {
				leases[i].Connection, leases[i].Status = "unknown", "unknown"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(leases)
	}
}

package network

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ClientLease struct {
	MACAddress string `json:"mac_address"`
	IPAddress  string `json:"ip_address"`
	Hostname   string `json:"hostname"`
	Expiry     int64  `json:"expiry_epoch"`
	ExpiryText string `json:"expiry_text"`
	TotalBytes uint64 `json:"total_bytes"`
	// How the device is connected: "wifi" (with the network name and band), "wired", or "unknown" when
	// the Wi-Fi side could not be read.
	Connection string `json:"connection"`
	SSID       string `json:"ssid,omitempty"`
	Band       string `json:"band,omitempty"`
}

// Link is how a device is attached to one of this router's Wi-Fi networks.
type Link struct {
	SSID string
	Band string
}

// Links reports the devices connected over Wi-Fi, by MAC address (lower case). known is false when that
// could not be found out; devices not in the map are then of unknown connection, not wired.
type Links func(ctx context.Context) (wifi map[string]Link, known bool)

// Connect fills in how each lease's device is connected.
func Connect(leases []ClientLease, wifi map[string]Link, known bool) {
	for i := range leases {
		if l, ok := wifi[leases[i].MACAddress]; ok {
			leases[i].Connection, leases[i].SSID, leases[i].Band = "wifi", l.SSID, l.Band
		} else if known {
			leases[i].Connection = "wired"
		} else {
			leases[i].Connection = "unknown"
		}
	}
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

// LeaseHandler serves the active DHCP leases, each with how the device is connected. links may be nil
// (then the connection is "unknown").
func LeaseHandler(links Links) http.HandlerFunc {
	return leaseHandler([]string{"/var/lib/misc/dnsmasq.leases", "/tmp/dhcp.leases"}, links)
}

// leaseHandler reads the first of the paths that exists.
func leaseHandler(paths []string, links Links) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		leasePath := paths[len(paths)-1]
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				leasePath = p
				break
			}
		}

		leases, err := ParseLeases(leasePath)
		if err != nil || leases == nil {
			leases = []ClientLease{}
		}
		var wifi map[string]Link
		known := false
		if links != nil {
			wifi, known = links(r.Context())
		}
		Connect(leases, wifi, known)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(leases)
	}
}

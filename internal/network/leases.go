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
	// How the device is connected: "wifi" (with the network name and band), "wired", or "unknown". For a
	// device that is not here right now this is how it was last seen.
	Connection string `json:"connection"`
	SSID       string `json:"ssid,omitempty"`
	Band       string `json:"band,omitempty"`
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

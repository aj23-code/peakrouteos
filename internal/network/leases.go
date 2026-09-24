package network

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"encoding/json"
	"net/http"
)

type ClientLease struct {
	MACAddress string `json:"mac_address"`
	IPAddress  string `json:"ip_address"`
	Hostname   string `json:"hostname"`
	Expiry     int64  `json:"expiry_epoch"`
	ExpiryText string `json:"expiry_text"`
	TotalBytes uint64 `json:"total_bytes"`
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

		leases = append(leases, ClientLease{
			Expiry:     expiry,
			ExpiryText: expiryText,
			MACAddress: mac,
			IPAddress:  ip,
			Hostname:   hostname,
			TotalBytes: 0, // Populated via traffic tracking
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

// HandleDHCPLeases handles HTTP requests for retrieving active DHCP client leases
func HandleDHCPLeases(w http.ResponseWriter, r *http.Request) {
	leasePath := "/var/lib/misc/dnsmasq.leases"
	if _, err := os.Stat(leasePath); os.IsNotExist(err) {
		leasePath = "/tmp/dhcp.leases"
	}

	leases, err := ParseLeases(leasePath)
	if err != nil {
		leases = []ClientLease{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(leases)
}

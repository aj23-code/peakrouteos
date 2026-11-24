package network

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TimeStatus is how the router's clock stands right now.
type TimeStatus struct {
	Timezone     string // the zone the machine is in (what timedatectl reports)
	NTPEnabled   bool   // the time service is switched on
	Synchronized bool   // the clock has been set from a time server
	Server       string // the server the clock follows, as the time service names it ("" when none)
	ServerIP     net.IP // its address (nil when unknown)
	Stratum      int    // that server's stratum (0 when unknown)
}

// HasHardwareClock reports whether the machine has a real-time clock: the chip that keeps the time while the power is off. Whether
// a battery keeps it going cannot be asked of the hardware; without the chip there is certainly nothing to keep it. root is "" or
// "/" on a real machine.
func HasHardwareClock(root string) bool {
	entries, err := os.ReadDir(filepath.Join(root, "/sys/class/rtc"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "rtc") {
			return true
		}
	}
	return false
}

// ParseTimesyncStatus reads `timedatectl timesync-status`: the server ("Server: 162.159.200.1 (time.cloudflare.com)" gives the
// whole text, its address) and the stratum.
func ParseTimesyncStatus(out string) (server string, ip net.IP, stratum int) {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Server":
			server = v
			if f := strings.Fields(v); len(f) > 0 {
				ip = net.ParseIP(f[0])
			}
		case "Stratum":
			stratum, _ = strconv.Atoi(v)
		}
	}
	return server, ip, stratum
}

// ParseTimedatectlShow reads the output of `timedatectl show` (key=value lines).
func ParseTimedatectlShow(out string) TimeStatus {
	var st TimeStatus
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "Timezone":
			st.Timezone = v
		case "NTP":
			st.NTPEnabled = v == "yes"
		case "NTPSynchronized":
			st.Synchronized = v == "yes"
		}
	}
	return st
}

// ParseTimesyncServer reads the server out of `timedatectl timesync-status` ("Server: 162.159.200.1 (time.cloudflare.com)").
func ParseTimesyncServer(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && k == "Server" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ReadTimeStatus asks timedatectl. ok is false when it cannot be asked (no systemd, say).
func ReadTimeStatus(ctx context.Context) (TimeStatus, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "timedatectl", "show", "--property=Timezone", "--property=NTP", "--property=NTPSynchronized").Output()
	if err != nil {
		return TimeStatus{}, false
	}
	st := ParseTimedatectlShow(string(out))
	if st.Synchronized {
		if sync, err := exec.CommandContext(ctx, "timedatectl", "timesync-status").Output(); err == nil {
			st.Server, st.ServerIP, st.Stratum = ParseTimesyncStatus(string(sync))
		}
	}
	return st, true
}

package ovpn

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// StatusDir is where the package's service writes the status file of each server (openvpn-server@NAME writes status-NAME.log).
const StatusDir = "/run/openvpn-server"

// Connection is a device or a peer that is connected to a server now.
type Connection struct {
	CommonName     string
	RealAddress    string // where it connects from
	VirtualAddress string // its address inside the tunnel
	BytesReceived  uint64
	BytesSent      uint64
	Since          time.Time
}

// ParseStatus reads a status file (version 2: comma separated lines; CLIENT_LIST lines are the connected clients).
func ParseStatus(text string) []Connection {
	var out []Connection
	for _, line := range strings.Split(text, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), ",")
		if len(f) < 9 || f[0] != "CLIENT_LIST" {
			continue
		}
		c := Connection{CommonName: f[1], RealAddress: f[2], VirtualAddress: f[3]}
		c.BytesReceived, _ = strconv.ParseUint(f[5], 10, 64)
		c.BytesSent, _ = strconv.ParseUint(f[6], 10, 64)
		if n, err := strconv.ParseInt(f[8], 10, 64); err == nil && n > 0 {
			c.Since = time.Unix(n, 0)
		}
		out = append(out, c)
	}
	return out
}

// Reader reads the status files of the servers. The page asks every few seconds, so an answer is kept for a moment.
type Reader struct {
	Dir  string        // default StatusDir; tests point it elsewhere
	Keep time.Duration // default 2 seconds

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	at   time.Time
	list []Connection
	ok   bool
}

// Connections lists what is connected to the server of that name (pr-ra, pr-s1...). The second result is false when there is no status
// file: the server is not running (or has not written one yet).
func (r *Reader) Connections(name string) ([]Connection, bool) {
	dir, keep := r.Dir, r.Keep
	if dir == "" {
		dir = StatusDir
	}
	if keep == 0 {
		keep = 2 * time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.cache[name]; ok && time.Since(c.at) < keep {
		return c.list, c.ok
	}
	b, err := os.ReadFile(filepath.Join(dir, "status-"+name+".log"))
	c := cached{at: time.Now()}
	if err == nil {
		c.list, c.ok = ParseStatus(string(b)), true
	}
	if r.cache == nil {
		r.cache = map[string]cached{}
	}
	r.cache[name] = c
	return c.list, c.ok
}

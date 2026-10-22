package network

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"peakroute/internal/fsutil"
)

// How much each device has used the internet connection, counted by the kernel and read here.
//
// The firewall file (translator, renderAccounting) makes a table of four sets of addresses, upload and download for IPv4 and IPv6, and
// every packet routed between a local network and the internet connection adds its sender (upload) or receiver (download) to the
// matching set, where the element carries its own byte counter. This reads the sets, finds which hardware address each address
// belongs to in the neighbour table, and adds up what each counter grew by since the last look. Nothing is captured in the daemon: it
// costs the kernel nothing per packet beyond the counter, covers every port, bridge and VLAN, and counts IPv6 too.
//
// The counters start again from zero when the firewall is loaded (every commit that changes it) and when an element expires after ten
// idle minutes; a counter that is lower than at the last look therefore counts as a new one. Totals and the last counters seen are
// kept in a file, so a restart of the daemon neither loses the totals nor counts the counters that are still in the kernel twice.

// UsageTable is the nftables table the counters are in. It is the name renderAccounting writes.
const UsageTable = "peakroute_acct"

var usageSets = []string{"up4", "down4", "up6", "down6"}

// Runner runs a command and returns what it printed.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// UsageTracker keeps the byte totals by hardware address.
type UsageTracker struct {
	run  Runner
	path string
	// Ensure loads the counting table into the kernel (nil: it is only read). It is called when the table is missing, at most once a
	// minute: the firewall file of an older version has none, and a reload of the firewall by something else removes it.
	Ensure func(ctx context.Context) error

	mu     sync.RWMutex
	totals map[string]uint64 // hardware address (lower case) -> bytes
	seen   map[string]uint64 // "set|address" -> the counter at the last look
	dirty  bool
	warned bool
	tried  time.Time // when Ensure was last called
}

// GlobalTracker is the tracker the lease list asks (nil: nobody counts, every device shows 0 B).
var GlobalTracker *UsageTracker

// NewUsageTracker counts with the given runner and keeps its totals in the file (empty: in memory only).
func NewUsageTracker(run Runner, path string) *UsageTracker {
	return &UsageTracker{run: run, path: path, totals: map[string]uint64{}, seen: map[string]uint64{}}
}

type usageFile struct {
	Totals map[string]uint64 `json:"totals"`
	Seen   map[string]uint64 `json:"seen"`
}

// Load reads what was kept. A missing or unreadable file starts empty.
func (t *UsageTracker) Load() {
	if t.path == "" {
		return
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var f usageFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if f.Totals != nil {
		t.totals = f.Totals
	}
	if f.Seen != nil {
		t.seen = f.Seen
	}
}

// Save writes the totals if they changed.
func (t *UsageTracker) Save() {
	if t.path == "" {
		return
	}
	t.mu.Lock()
	if !t.dirty {
		t.mu.Unlock()
		return
	}
	b, err := json.Marshal(usageFile{Totals: t.totals, Seen: t.seen})
	t.dirty = false
	t.mu.Unlock()
	if err != nil {
		return
	}
	if err := fsutil.WriteFileAtomic(t.path, b, 0600); err != nil {
		log.Printf("[!] Usage totals could not be saved: %v", err)
		t.mu.Lock()
		t.dirty = true
		t.mu.Unlock()
	}
}

// GetBytesForMAC returns the bytes a device has sent to and received from the internet.
func (t *UsageTracker) GetBytesForMAC(mac string) uint64 {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.totals[strings.ToLower(mac)]
}

// Run looks at the counters every interval and saves the totals about once a minute and when ctx ends.
func (t *UsageTracker) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	saved := time.Now()
	for {
		t.Poll(ctx)
		if time.Since(saved) >= time.Minute {
			t.Save()
			saved = time.Now()
		}
		select {
		case <-ctx.Done():
			t.Save()
			return
		case <-tick.C:
		}
	}
}

type nftSet struct {
	Name string `json:"name"`
	Elem []struct {
		Elem struct {
			Val     string `json:"val"`
			Counter struct {
				Bytes uint64 `json:"bytes"`
			} `json:"counter"`
		} `json:"elem"`
	} `json:"elem"`
}

// parseUsageSets returns, for each set, the bytes counted for each address.
func parseUsageSets(out string) (map[string]map[string]uint64, error) {
	var doc struct {
		Nftables []struct {
			Set *nftSet `json:"set"`
		} `json:"nftables"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, err
	}
	sets := map[string]map[string]uint64{}
	for _, item := range doc.Nftables {
		if item.Set == nil {
			continue
		}
		m := map[string]uint64{}
		for _, e := range item.Set.Elem {
			if e.Elem.Val != "" {
				m[e.Elem.Val] = e.Elem.Counter.Bytes
			}
		}
		sets[item.Set.Name] = m
	}
	return sets, nil
}

// parseNeighbours returns the hardware address of each address the router has a neighbour entry for.
func parseNeighbours(out string) map[string]string {
	var entries []struct {
		Dst    string   `json:"dst"`
		LLAddr string   `json:"lladdr"`
		State  []string `json:"state"`
	}
	if json.Unmarshal([]byte(out), &entries) != nil {
		return nil
	}
	m := map[string]string{}
	for _, e := range entries {
		if e.LLAddr == "" {
			continue
		}
		failed := false
		for _, s := range e.State {
			if s == "FAILED" || s == "INCOMPLETE" {
				failed = true
			}
		}
		if !failed {
			m[e.Dst] = strings.ToLower(e.LLAddr)
		}
	}
	return m
}

// shouldEnsure says whether the counting table may be loaded now: not more than once a minute, so that a machine that cannot load it
// (no nft, an access point) is not asked every ten seconds.
func (t *UsageTracker) shouldEnsure() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.tried) < time.Minute {
		return false
	}
	t.tried = time.Now()
	return true
}

// Poll reads the counters once and adds what they grew by to the totals.
func (t *UsageTracker) Poll(ctx context.Context) {
	if t == nil || t.run == nil {
		return
	}
	out, err := t.run.Run(ctx, "nft", "-j", "list", "table", "inet", UsageTable)
	if err != nil && t.Ensure != nil && t.shouldEnsure() {
		if eerr := t.Ensure(ctx); eerr != nil {
			log.Printf("[!] Usage counters: the counting table could not be loaded: %v", eerr)
		} else {
			out, err = t.run.Run(ctx, "nft", "-j", "list", "table", "inet", UsageTable)
		}
	}
	if err != nil {
		// no table: an access point, or the firewall is not loaded yet. Nothing to count, nothing to complain about.
		return
	}
	if strings.TrimSpace(out) == "" {
		return // nothing came back (-dev only logs the command)
	}
	sets, err := parseUsageSets(out)
	if err != nil {
		t.mu.Lock()
		warn := !t.warned
		t.warned = true
		t.mu.Unlock()
		if warn {
			log.Printf("[!] Usage counters could not be read: %v", err)
		}
		return
	}
	nout, _ := t.run.Run(ctx, "ip", "-j", "neigh", "show")
	macs := parseNeighbours(nout)

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, name := range usageSets {
		cur, ok := sets[name]
		if !ok {
			continue
		}
		for ip, bytes := range cur {
			key := name + "|" + ip
			mac, known := macs[ip]
			if !known {
				continue // who it is is not known yet: the counter is looked at again, nothing is lost while it is not advanced
			}
			prev := t.seen[key]
			delta := bytes
			if bytes >= prev {
				delta = bytes - prev
			}
			if delta > 0 {
				t.totals[mac] += delta
				t.dirty = true
			}
			if t.seen[key] != bytes {
				t.seen[key] = bytes
				t.dirty = true
			}
		}
		// an element that is gone (expired, or the table was loaded again) starts from zero when it comes back
		for key := range t.seen {
			if strings.HasPrefix(key, name+"|") {
				if _, present := cur[strings.TrimPrefix(key, name+"|")]; !present {
					delete(t.seen, key)
					t.dirty = true
				}
			}
		}
	}
}

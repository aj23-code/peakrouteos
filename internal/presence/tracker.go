// Package presence tells whether the devices that hold a DHCP lease are here right now, how they are
// connected, and when they were last seen. A lease outlives the device (it is held until it expires), so
// the lease list alone cannot say. Wi-Fi devices are known from the access point's station list; the
// others are asked with ARP, and everything seen is remembered across restarts.
package presence

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"peakroute/internal/fsutil"
	"peakroute/internal/network"
)

// Link is how a device is attached to one of this router's Wi-Fi networks.
type Link struct {
	SSID string
	Band string
}

// Record is what is remembered about a device.
type Record struct {
	LastSeen   int64  `json:"last_seen"` // Unix seconds
	Connection string `json:"connection,omitempty"`
	SSID       string `json:"ssid,omitempty"`
	Band       string `json:"band,omitempty"`
}

const (
	saveEvery = 5 * time.Minute
	keepFor   = 60 * 24 * time.Hour
)

// Tracker follows the devices. Fill in the fields, call Load, then Run it (or call Round yourself).
type Tracker struct {
	Path    string                                            // where the memory is kept ("" keeps it in memory only)
	Window  time.Duration                                     // how long after its last answer a device still counts as online (75 s when 0)
	Every   time.Duration                                     // between rounds (20 s when 0)
	Now     func() time.Time                                  // time.Now when nil
	Wifi    func(ctx context.Context) (map[string]Link, bool) // the devices on Wi-Fi by MAC; false: could not be read
	Probe   Prober                                            // nil: devices off Wi-Fi cannot be asked
	Targets func() []Target                                   // the devices to look for (from the leases)

	mu      sync.Mutex
	seen    map[string]Record
	probeOK bool
	dirty   bool // something other than a newer "last seen" changed, or that changed long ago: worth saving
	stale   bool // only "last seen" times have moved since the last save
	savedAt time.Time
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Tracker) window() time.Duration {
	if t.Window > 0 {
		return t.Window
	}
	return 75 * time.Second
}

func (t *Tracker) wifi(ctx context.Context) (map[string]Link, bool) {
	if t.Wifi == nil {
		return nil, false
	}
	return t.Wifi(ctx)
}

// Load reads what was remembered. A missing or unreadable file starts empty.
func (t *Tracker) Load() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen = map[string]Record{}
	if t.Path == "" {
		return
	}
	b, err := os.ReadFile(t.Path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, &t.seen); err != nil || t.seen == nil {
		log.Printf("[!] Ignoring the unreadable client memory %s: %v", t.Path, err)
		t.seen = map[string]Record{}
	}
}

// observe notes that a device was seen now. conn "" leaves what is known about how it connects as it is.
// The caller holds the lock.
func (t *Tracker) observe(mac string, now time.Time, conn string, l Link) {
	if t.seen == nil {
		t.seen = map[string]Record{}
	}
	old, had := t.seen[mac]
	rec := old
	rec.LastSeen = now.Unix()
	if conn != "" {
		rec.Connection, rec.SSID, rec.Band = conn, l.SSID, l.Band
	}
	t.seen[mac] = rec
	if !had || rec.Connection != old.Connection || rec.SSID != old.SSID || rec.Band != old.Band {
		t.dirty = true
	} else if rec.LastSeen != old.LastSeen {
		t.stale = true
	}
}

// Round looks at the Wi-Fi stations and asks the other devices that hold a lease.
func (t *Tracker) Round(ctx context.Context) {
	now := t.now()
	wifi, known := t.wifi(ctx)

	t.mu.Lock()
	for mac, l := range wifi {
		t.observe(mac, now, "wifi", l)
	}
	t.mu.Unlock()

	if t.Probe == nil || t.Targets == nil {
		return
	}
	var targets []Target
	for _, tg := range t.Targets() {
		if _, onWifi := wifi[tg.MAC]; !onWifi {
			targets = append(targets, tg)
		}
	}
	if len(targets) == 0 {
		return
	}
	replied, err := t.Probe(ctx, targets)
	if ctx.Err() != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.probeOK = err == nil
	for mac := range replied {
		conn := ""
		if known {
			conn = "wired"
		}
		t.observe(mac, now, conn, Link{})
	}
}

// Annotate fills in each lease's connection, status and last-seen time.
func (t *Tracker) Annotate(ctx context.Context, leases []network.ClientLease) {
	now := t.now()
	wifi, known := t.wifi(ctx)

	t.mu.Lock()
	defer t.mu.Unlock()
	for mac, l := range wifi {
		t.observe(mac, now, "wifi", l)
	}
	for i := range leases {
		l := &leases[i]
		rec, have := t.seen[l.MACAddress]
		_, onWifi := wifi[l.MACAddress]
		online := have && now.Sub(time.Unix(rec.LastSeen, 0)) <= t.window()

		switch {
		case onWifi:
			l.Status = "online"
		case t.probeOK && online:
			l.Status = "online"
		case t.probeOK:
			l.Status = "offline"
		default:
			l.Status = "unknown" // devices off Wi-Fi cannot be asked here
		}

		switch {
		case onWifi:
			l.Connection, l.SSID, l.Band = "wifi", wifi[l.MACAddress].SSID, wifi[l.MACAddress].Band
		case l.Status == "online" && known:
			l.Connection = "wired" // here and not on our Wi-Fi
		case have && rec.Connection != "":
			l.Connection, l.SSID, l.Band = rec.Connection, rec.SSID, rec.Band // how it was last seen
		case l.Status == "unknown" && known:
			l.Connection = "wired" // nothing better is known: not on our Wi-Fi
		default:
			l.Connection = "unknown"
		}
		if l.Status == "online" {
			l.LastSeen = now.Unix()
		} else if have {
			l.LastSeen = rec.LastSeen
		}
	}
}

// Save writes the memory when something worth keeping changed, or when only times moved and a while
// has passed (flash storage should not be written every few seconds). force writes regardless.
func (t *Tracker) Save(force bool) {
	if t.Path == "" {
		return
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !force && !t.dirty && !(t.stale && now.Sub(t.savedAt) >= saveEvery) {
		return
	}
	for mac, rec := range t.seen {
		if now.Sub(time.Unix(rec.LastSeen, 0)) > keepFor {
			delete(t.seen, mac)
		}
	}
	b, err := json.Marshal(t.seen)
	if err == nil {
		err = fsutil.WriteFileAtomic(t.Path, b, 0o600)
	}
	if err != nil {
		log.Printf("[!] Could not save the client memory: %v", err)
		return
	}
	t.dirty, t.stale, t.savedAt = false, false, now
}

// Run does a round now and then every Every, until ctx ends.
func (t *Tracker) Run(ctx context.Context) {
	every := t.Every
	if every <= 0 {
		every = 20 * time.Second
	}
	t.Round(ctx)
	t.Save(false)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Save(true)
			return
		case <-tick.C:
			t.Round(ctx)
			t.Save(false)
		}
	}
}

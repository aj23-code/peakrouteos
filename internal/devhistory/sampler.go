package devhistory

import (
	"context"
	"log"
	"sync"
	"time"
)

// Client is what the sampler needs to know about a device now.
type Client struct {
	MAC        string
	IP         string
	Status     string // online, offline or unknown (nothing is recorded while it is unknown)
	Connection string
	SSID       string
	Band       string
	Network    string
}

// Sampler looks every minute at how much each device has sent and received and whether it is here, and writes the difference to the store.
type Sampler struct {
	Store      *Store
	Directions func() map[string][2]uint64 // cumulative bytes sent and received by each device (network.UsageTracker.Directions)
	Clients    func(ctx context.Context) []Client
	Every      time.Duration    // a minute when zero
	Now        func() time.Time // time.Now when nil

	mu     sync.Mutex
	last   map[string][2]uint64 // the cumulative bytes at the last look
	states map[string]Event     // the last presence event of each device
	loaded bool
}

func (s *Sampler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Sample looks once.
func (s *Sampler) Sample(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		s.loaded = true
		if last, err := s.Store.LastEvents(); err == nil {
			s.states = last
		}
	}
	if s.states == nil {
		s.states = map[string]Event{}
	}

	// the traffic since the last look. The first look only sets the starting point: bytes counted while the daemon was not running (or
	// before this was kept) are not put into one minute
	cur := s.Directions()
	if s.last != nil {
		for mac, v := range cur {
			prev := s.last[mac]
			var up, down uint64
			if v[0] >= prev[0] {
				up = v[0] - prev[0]
			}
			if v[1] >= prev[1] {
				down = v[1] - prev[1]
			}
			if err := s.Store.AddUsage(mac, now.Add(-time.Minute), up, down); err != nil {
				log.Printf("[!] Device history: %v", err)
			}
		}
	}
	s.last = cur

	// who is here
	for _, c := range s.Clients(ctx) {
		if c.Status != "online" && c.Status != "offline" {
			continue
		}
		mac := norm(c.MAC)
		e := Event{MAC: mac, At: now.Unix(), Online: c.Status == "online", IP: c.IP}
		if e.Online {
			e.Connection, e.SSID, e.Band, e.Network = c.Connection, c.SSID, c.Band, c.Network
		}
		prev, had := s.states[mac]
		changed := !had || prev.Online != e.Online ||
			(e.Online && (prev.Connection != e.Connection || prev.SSID != e.SSID || prev.Band != e.Band || prev.Network != e.Network))
		if !changed {
			continue
		}
		if err := s.Store.AddEvent(e); err != nil {
			log.Printf("[!] Device history: %v", err)
			continue
		}
		s.states[mac] = e
	}
}

// Run samples every minute and tidies the store every hour, until ctx ends.
func (s *Sampler) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	tidied := time.Time{}
	for {
		s.Sample(ctx)
		if s.now().Sub(tidied) >= time.Hour {
			if err := s.Store.Compact(); err != nil {
				log.Printf("[!] Device history could not be tidied: %v", err)
			}
			tidied = s.now()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

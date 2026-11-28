package netdevices

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Status is the last that is known of one watched device.
type Status struct {
	Reachable bool
	Error     string // why the last poll failed
	Record    *Record
	LastOK    time.Time
	Since     time.Time // since when it has been reachable, or not
	PolledAt  time.Time
}

// Service polls the watched devices and answers where a hardware address is.
type Service struct {
	Store   *Store
	Drivers map[string]Driver // by kind
	Every   time.Duration     // between rounds (a minute when zero)
	Now     func() time.Time

	mu     sync.Mutex
	status map[string]*Status
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Run polls every enabled device each round until ctx ends.
func (s *Service) Run(ctx context.Context) {
	every := s.Every
	if every <= 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.PollAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PollAll polls every enabled device, one after the other.
func (s *Service) PollAll(ctx context.Context) {
	live := map[string]bool{}
	for _, d := range s.Store.List() {
		live[d.ID] = true
		if d.Enabled {
			s.Poll(ctx, d)
		}
	}
	// what is known of a device that was taken out goes with it
	s.mu.Lock()
	for id := range s.status {
		if !live[id] {
			delete(s.status, id)
		}
	}
	s.mu.Unlock()
}

// Poll reads one device now.
func (s *Service) Poll(ctx context.Context, d Device) Status {
	var rec Record
	var err error
	drv := s.Drivers[d.Kind]
	if drv == nil {
		err = fmt.Errorf("there is no reader for %q devices on this system", d.Kind)
	} else {
		pctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		rec, err = drv.Poll(pctx, d)
		cancel()
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		s.status = map[string]*Status{}
	}
	st := s.status[d.ID]
	if st == nil {
		st = &Status{}
		s.status[d.ID] = st
	}
	reachable := err == nil
	if st.PolledAt.IsZero() || st.Reachable != reachable {
		st.Since = now
	}
	st.Reachable, st.PolledAt = reachable, now
	if err != nil {
		st.Error = err.Error()
	} else {
		st.Error, st.LastOK, st.Record = "", now, &rec // (a failed poll keeps the last good record: where things were)
	}
	return *st
}

// StatusOf returns what is known of a device (the zero Status before its first poll).
func (s *Service) StatusOf(id string) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[id]; st != nil {
		return *st
	}
	return Status{}
}

// Location is where a hardware address was found on the watched equipment.
type Location struct {
	DeviceID   string     `json:"device_id"`
	DeviceName string     `json:"device_name"`
	Kind       string     `json:"kind"`
	Port       string     `json:"port"`
	Alias      string     `json:"alias,omitempty"`
	SpeedMbps  int        `json:"speed_mbps,omitempty"`
	VLAN       int        `json:"vlan,omitempty"`
	Up         bool       `json:"up"`
	MACsOnPort int        `json:"macs_on_port"`
	Uplink     bool       `json:"uplink"` // the port has another switch or an access point behind it: the device is further away, and this is only the nearest the watched equipment can say
	Neighbour  *Neighbour `json:"neighbour,omitempty"`
	Client     Client     `json:"client"`
	Stale      bool       `json:"stale"` // the device is not answering now: this is where it was
}

// Locate says where a hardware address is: of the ports that have learned it, the one that is the end of the line. A port with another switch
// or an access point behind it (LLDP) is not; without LLDP, the port that has learned the fewest addresses is the likelier edge (an uplink
// carries everything behind it).
func (s *Service) Locate(mac string) *Location {
	mac = strings.ToLower(mac)
	var best *Location
	bestScore := 0
	for _, d := range s.Store.List() {
		st := s.StatusOf(d.ID)
		if st.Record == nil {
			continue
		}
		for _, c := range st.Record.Clients {
			if c.MAC != mac {
				continue
			}
			l := Location{DeviceID: d.ID, DeviceName: d.Name, Kind: d.Kind, Port: c.Port, VLAN: c.VLAN, Client: c, Stale: !st.Reachable}
			score := 500
			for _, p := range st.Record.Ports {
				if p.Name != c.Port {
					continue
				}
				l.Alias, l.SpeedMbps, l.Up, l.MACsOnPort, l.Neighbour = p.Alias, p.SpeedMbps, p.Up, p.MACs, p.Neighbour
				if l.VLAN == 0 {
					l.VLAN = p.VLAN
				}
				score = p.MACs
				if p.Neighbour.Bridging() {
					l.Uplink, score = true, score+1000
				}
			}
			if d.Kind == KindAP {
				score = -1 // an access point that has the device associated is its edge
			}
			if !st.Reachable {
				score += 5000 // what a device that is not answering said is the last choice
			}
			if best == nil || score < bestScore {
				cp := l
				best, bestScore = &cp, score
			}
		}
	}
	return best
}

// Names are the watched devices' names, for the pages.
func (s *Service) Names() []string {
	var out []string
	for _, d := range s.Store.List() {
		out = append(out, d.Name)
	}
	sort.Strings(out)
	return out
}

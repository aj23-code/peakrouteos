package wanhealth

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// A minute counts as an outage minute when every probe in it was lost, and at least as many were
// sent as it takes to call the connection offline. Minutes with fewer probes (the daemon was
// starting or stopping) say nothing either way.
const (
	minProbesPerMinute = offlineAfter
	maxOutagesListed   = 20
	availabilityWindow = 30 * 24 * time.Hour
)

// Outage is a stretch of consecutive minutes in which the DNS server could not be reached.
type Outage struct {
	Start   int64 `json:"start"`   // Unix ms
	Seconds int64 `json:"seconds"` // whole minutes
	Ongoing bool  `json:"ongoing"` // still going on right now
}

// Availability is how reachable the internet has been, from the per-minute history of the DNS series.
type Availability struct {
	Uptime24h   *float64 `json:"uptime_24h"` // percent of observed minutes; null before anything was observed
	Uptime30d   *float64 `json:"uptime_30d"`
	OutageCount int      `json:"outage_count"` // in the last 30 days
	Outages     []Outage `json:"outages"`      // newest first, at most maxOutagesListed
}

type minuteTotal struct{ sent, lost int64 }

// minutesSince returns the per-minute totals of a series since a time, oldest first, newest
// minutes from RAM (including ones not written yet) and older ones from the database.
func (s *Store) minutesSince(series string, since time.Time, now time.Time) (map[int64]minuteTotal, []int64, error) {
	totals := map[int64]minuteTotal{}

	s.mu.Lock()
	oldestRAM := now.Unix()/minuteSeconds*minuteSeconds + minuteSeconds
	for k := range s.mins {
		if k.series == series && k.minute < oldestRAM {
			oldestRAM = k.minute
		}
	}
	for k, a := range s.mins {
		if k.series == series && k.minute >= since.Unix() {
			t := totals[k.minute]
			t.sent += a.sent
			t.lost += a.lost
			totals[k.minute] = t
		}
	}
	s.mu.Unlock()

	if s.db != nil {
		rows, err := s.db.Query(`SELECT minute, SUM(sent), SUM(lost) FROM wan_health
			WHERE series = ? AND minute >= ? AND minute < ? GROUP BY minute`, series, since.Unix(), oldestRAM)
		if err != nil {
			return nil, nil, fmt.Errorf("query wan health: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var m int64
			var t minuteTotal
			if err := rows.Scan(&m, &t.sent, &t.lost); err != nil {
				return nil, nil, err
			}
			totals[m] = t
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
	}

	minutes := make([]int64, 0, len(totals))
	for m := range totals {
		minutes = append(minutes, m)
	}
	sort.Slice(minutes, func(i, j int) bool { return minutes[i] < minutes[j] })
	return totals, minutes, nil
}

// Availability computes the uptime and the outage list of the DNS series as of now.
func (s *Store) Availability(now time.Time) (Availability, error) {
	totals, minutes, err := s.minutesSince(SeriesDNS, now.Add(-availabilityWindow), now)
	if err != nil {
		return Availability{}, err
	}

	var out Availability
	out.Outages = []Outage{}
	dayAgo := now.Add(-24 * time.Hour).Unix()
	var obs24, down24, obs30, down30 int64

	var cur *Outage
	var curLast int64
	var found []Outage
	flush := func(last bool) {
		if cur != nil {
			cur.Seconds = curLast + minuteSeconds - cur.Start/1000
			// Only the last outage can still be going on, and only if it reaches the present
			// (the newest minute may not be over yet)
			cur.Ongoing = last && now.Unix() < curLast+2*minuteSeconds
			found = append(found, *cur)
			cur = nil
		}
	}

	for _, m := range minutes {
		t := totals[m]
		if t.sent < minProbesPerMinute {
			flush(false) // no information about this minute: it cannot continue an outage
			continue
		}
		obs30++
		if m >= dayAgo {
			obs24++
		}
		if t.lost < t.sent {
			flush(false)
			continue
		}
		down30++
		if m >= dayAgo {
			down24++
		}
		if cur != nil && m == curLast+minuteSeconds {
			curLast = m
			continue
		}
		flush(false)
		cur = &Outage{Start: m * 1000}
		curLast = m
	}
	flush(true)

	pct := func(obs, down int64) *float64 {
		if obs == 0 {
			return nil
		}
		v := float64(obs-down) * 100 / float64(obs)
		return &v
	}
	out.Uptime24h, out.Uptime30d = pct(obs24, down24), pct(obs30, down30)

	out.OutageCount = len(found)
	for i := len(found) - 1; i >= 0 && len(out.Outages) < maxOutagesListed; i-- {
		out.Outages = append(out.Outages, found[i])
	}
	return out, nil
}

// availabilityCache keeps the answer for a few seconds: the page asks every few seconds, and
// scanning 30 days of minutes each time would be wasted work.
type availabilityCache struct {
	mu   sync.Mutex
	at   time.Time
	data Availability
	ok   bool
}

const availabilityTTL = 20 * time.Second

func (p *Prober) availability(now time.Time) (Availability, error) {
	c := &p.avail
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ok && now.Sub(c.at) < availabilityTTL && now.After(c.at) {
		return c.data, nil
	}
	a, err := p.Store.Availability(now)
	if err != nil {
		return Availability{}, err
	}
	c.at, c.data, c.ok = now, a, true
	return a, nil
}

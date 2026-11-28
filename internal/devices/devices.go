// Package devices answers the questions of a device's page (docs/device-pages.md, phase 1): who it is, how it is connected, what it has used
// over time and when it was here. The list of devices (leases, neighbours, presence, nicknames) comes from the caller; the history from
// internal/devhistory.
package devices

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"peakroute/internal/devhistory"
	"peakroute/internal/network"
	"peakroute/internal/oui"
	"peakroute/internal/wireless"
)

// Service serves GET /api/v1/clients/{mac} and GET /api/v1/clients/{mac}/history.
type Service struct {
	// List is the devices as the Clients page has them (annotated with presence, nicknames and networks).
	List func(ctx context.Context) []network.ClientLease
	// History is where usage and presence are kept (nil: no history, and the page says so).
	History *devhistory.Store
	// Stations are the devices on the router's own Wi-Fi radios with their signal and rates (nil: not known).
	Stations func(ctx context.Context) wireless.StationReport
	Now      func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

var macPattern = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// Detail is a device's page, apart from its history.
type Detail struct {
	MAC            string `json:"mac"`
	Vendor         string `json:"vendor,omitempty"`
	PrivateAddress bool   `json:"private_address"` // the hardware address is one the device made up (so it names no maker, and may change)
	Name           string `json:"name"`            // what the pages call it: the nickname, else the host name, else the address
	Hostname       string `json:"hostname,omitempty"`
	Nickname       string `json:"nickname,omitempty"`
	IP             string `json:"ip"`
	Network        string `json:"network,omitempty"`
	Static         bool   `json:"static,omitempty"`
	SeenOn         string `json:"seen_on,omitempty"`
	Reserved       bool   `json:"reserved"`
	Reservation    string `json:"reservation,omitempty"`
	LeaseText      string `json:"lease_text,omitempty"`
	Status         string `json:"status"` // online, offline or unknown
	LastSeen       int64  `json:"last_seen"`
	Connection     string `json:"connection"`
	SSID           string `json:"ssid,omitempty"`
	Band           string `json:"band,omitempty"`
	// Wifi is how the device is doing on one of the router's own radios, while it is there.
	Wifi       *wireless.Station `json:"wifi,omitempty"`
	TotalBytes uint64            `json:"total_bytes"`
	// HistoryKept says whether usage and presence are being kept at all.
	HistoryKept bool               `json:"history_kept"`
	Events      []devhistory.Event `json:"events"` // the newest first
}

func displayName(l network.ClientLease) string {
	switch {
	case l.Nickname != "":
		return l.Nickname
	case l.Hostname != "" && l.Hostname != "*":
		return l.Hostname
	}
	return l.IPAddress
}

func (s *Service) find(ctx context.Context, mac string) (network.ClientLease, bool) {
	for _, l := range s.List(ctx) {
		if strings.EqualFold(l.MACAddress, mac) {
			return l, true
		}
	}
	return network.ClientLease{}, false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func macOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	mac := strings.ToLower(r.PathValue("mac"))
	if !macPattern.MatchString(mac) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "that is not a hardware address"})
		return "", false
	}
	return mac, true
}

// HandleClient: GET /api/v1/clients/{mac}.
func (s *Service) HandleClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mac, ok := macOf(w, r)
	if !ok {
		return
	}
	l, found := s.find(r.Context(), mac)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "there is no device with this hardware address"})
		return
	}
	d := Detail{
		MAC: mac, Vendor: oui.Vendor(mac), PrivateAddress: oui.Private(mac), Name: displayName(l), Hostname: l.Hostname, Nickname: l.Nickname,
		IP: l.IPAddress, Network: l.Network, Static: l.Static, SeenOn: l.SeenOn, Reserved: l.Reserved, Reservation: l.Reservation,
		LeaseText: l.ExpiryText, Status: l.Status, LastSeen: l.LastSeen, Connection: l.Connection, SSID: l.SSID, Band: l.Band,
		TotalBytes: l.TotalBytes, HistoryKept: s.History != nil, Events: []devhistory.Event{},
	}
	if d.Hostname == "*" {
		d.Hostname = ""
	}
	if s.Stations != nil && l.Connection == "wifi" {
		for _, st := range s.Stations(r.Context()).Stations {
			if strings.EqualFold(st.MAC, mac) {
				st := st
				d.Wifi = &st
				break
			}
		}
	}
	if s.History != nil {
		if ev, err := s.History.Events(mac, s.now().Add(-30*24*time.Hour), 20); err == nil && ev != nil {
			d.Events = ev
		}
	}
	writeJSON(w, http.StatusOK, d)
}

// Ranges the history can be asked for: how far back, and how long each point is.
var ranges = map[string]struct{ back, bucket time.Duration }{
	"1h":  {time.Hour, time.Minute},
	"24h": {24 * time.Hour, 5 * time.Minute},
	"30d": {30 * 24 * time.Hour, time.Hour},
}

// Segment is a stretch of time in which a device was here (Online) or not.
type Segment struct {
	From   int64 `json:"from"` // Unix seconds
	To     int64 `json:"to"`
	Online bool  `json:"online"`
}

// History is what a device used and when it was here over a range.
type History struct {
	Range         string             `json:"range"`
	From          int64              `json:"from"`
	To            int64              `json:"to"`
	BucketSeconds int                `json:"bucket_seconds"`
	Points        []devhistory.Point `json:"points"`
	TotalUp       uint64             `json:"total_up"` // bytes sent by the device in the range
	TotalDown     uint64             `json:"total_down"`
	PeakUp        float64            `json:"peak_up_bps"` // the busiest bucket, in bits a second
	PeakDown      float64            `json:"peak_down_bps"`
	Segments      []Segment          `json:"segments"` // when it was here; empty before anything was known
}

// HandleHistory: GET /api/v1/clients/{mac}/history?range=1h|24h|30d.
func (s *Service) HandleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mac, ok := macOf(w, r)
	if !ok {
		return
	}
	name := r.URL.Query().Get("range")
	if name == "" {
		name = "24h"
	}
	rg, ok := ranges[name]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "range must be 1h, 24h or 30d"})
		return
	}
	if s.History == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "the history is not kept on this system"})
		return
	}
	if _, found := s.find(r.Context(), mac); !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "there is no device with this hardware address"})
		return
	}
	now := s.now()
	to := now.Add(time.Minute).Truncate(time.Minute)
	from := to.Add(-rg.back)
	pts, err := s.History.Usage(mac, from, to, rg.bucket)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the history could not be read"})
		return
	}
	h := History{Range: name, From: from.Unix(), To: now.Unix(), BucketSeconds: int(rg.bucket / time.Second), Points: pts, Segments: []Segment{}}
	if h.Points == nil {
		h.Points = []devhistory.Point{}
	}
	secs := float64(h.BucketSeconds)
	for _, p := range h.Points {
		h.TotalUp += p.Up
		h.TotalDown += p.Down
		if v := float64(p.Up) * 8 / secs; v > h.PeakUp {
			h.PeakUp = v
		}
		if v := float64(p.Down) * 8 / secs; v > h.PeakDown {
			h.PeakDown = v
		}
	}
	h.Segments = s.segments(mac, from, now)
	writeJSON(w, http.StatusOK, h)
}

// segments turns the changes of presence into stretches: what it was at the start of the range (from the change before it), and each
// change after. Nothing is said before the first change that is known.
func (s *Service) segments(mac string, from, to time.Time) []Segment {
	out := []Segment{}
	start, have := int64(0), false
	online := false
	if e, ok, err := s.History.EventBefore(mac, from); err == nil && ok {
		start, have, online = from.Unix(), true, e.Online
	}
	evs, err := s.History.Events(mac, from, 0) // newest first
	if err != nil {
		return out
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if have && e.At > start {
			out = append(out, Segment{From: start, To: e.At, Online: online})
		}
		start, have, online = e.At, true, e.Online
	}
	if have && to.Unix() > start {
		out = append(out, Segment{From: start, To: to.Unix(), Online: online})
	}
	return out
}

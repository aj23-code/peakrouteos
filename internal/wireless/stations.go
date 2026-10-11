package wireless

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Interface is a wireless network interface as `iw dev` describes it.
type Interface struct {
	Name    string
	SSID    string
	Type    string // AP, managed, ...
	Channel int
	MHz     int
}

// Station is a device connected to one of this machine's access points.
type Station struct {
	MAC       string  `json:"mac"`
	Interface string  `json:"interface"`
	SSID      string  `json:"ssid"`
	Band      string  `json:"band"`
	Channel   int     `json:"channel"`
	Signal    *int    `json:"signal_dbm"` // nil: not stated
	InactiveS float64 `json:"inactive_seconds"`
	RxBytes   uint64  `json:"rx_bytes"` // what the device sent to us
	TxBytes   uint64  `json:"tx_bytes"` // what we sent to it
	RxMbps    float64 `json:"rx_mbps"`
	TxMbps    float64 `json:"tx_mbps"`
	Connected int64   `json:"connected_seconds"`
}

// StationReport is what GET /api/v1/wifi/stations returns.
type StationReport struct {
	Stations []Station `json:"stations"`
	// Known is false when the wireless side could not be read (no `iw`): the list is then empty because
	// it is unknown, not because nobody is connected.
	Known bool `json:"known"`
}

// BandOfMHz names the band a centre frequency belongs to.
func BandOfMHz(mhz int) string {
	switch {
	case mhz >= 2400 && mhz < 2500:
		return "2.4 GHz"
	case mhz >= 5000 && mhz < 5900:
		return "5 GHz"
	case mhz >= 5925 && mhz <= 7125:
		return "6 GHz"
	}
	return ""
}

// ParseDev reads `iw dev`: every interface with its name, mode, network name and channel.
func ParseDev(out string) []Interface {
	var list []Interface
	cur := -1
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if strings.HasPrefix(f[0], "phy#") || f[0] == "Unnamed/non-netdev" {
			cur = -1 // the lines below belong to a radio or an interface without a name, not to a listed one
			continue
		}
		switch f[0] {
		case "Interface":
			if len(f) >= 2 {
				list = append(list, Interface{Name: f[1]})
				cur = len(list) - 1
			}
		case "ssid":
			if cur >= 0 {
				// the name can hold spaces: everything after the word
				list[cur].SSID = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "ssid"))
			}
		case "type":
			if cur >= 0 && len(f) >= 2 {
				list[cur].Type = f[1]
			}
		case "channel":
			// channel 36 (5180 MHz), width: 80 MHz, center1: 5210 MHz
			if cur >= 0 && len(f) >= 3 {
				list[cur].Channel, _ = strconv.Atoi(f[1])
				list[cur].MHz, _ = strconv.Atoi(strings.TrimPrefix(f[2], "("))
			}
		}
	}
	return list
}

// ParseStations reads `iw dev <interface> station dump`. Devices that are not (yet) authorized, that is,
// still in the middle of the password exchange, are left out.
func ParseStations(out string) []Station {
	var list []Station
	var cur *Station
	authorized := true
	flush := func() {
		if cur != nil && authorized {
			list = append(list, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(line, "Station ") {
			flush()
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			cur = &Station{MAC: strings.ToLower(f[1])}
			if len(f) >= 4 {
				cur.Interface = strings.TrimSuffix(f[3], ")")
			}
			authorized = true
			continue
		}
		if cur == nil {
			continue
		}
		key, val, ok := strings.Cut(trim, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		f := strings.Fields(val)
		switch key {
		case "inactive time":
			if len(f) >= 1 {
				ms, _ := strconv.ParseFloat(f[0], 64)
				cur.InactiveS = ms / 1000
			}
		case "rx bytes":
			cur.RxBytes, _ = strconv.ParseUint(val, 10, 64)
		case "tx bytes":
			cur.TxBytes, _ = strconv.ParseUint(val, 10, 64)
		case "signal":
			if len(f) >= 1 {
				if v, err := strconv.Atoi(f[0]); err == nil {
					cur.Signal = &v
				}
			}
		case "tx bitrate":
			if len(f) >= 1 {
				cur.TxMbps, _ = strconv.ParseFloat(f[0], 64)
			}
		case "rx bitrate":
			if len(f) >= 1 {
				cur.RxMbps, _ = strconv.ParseFloat(f[0], 64)
			}
		case "authorized":
			authorized = val == "yes"
		case "connected time":
			if len(f) >= 1 {
				cur.Connected, _ = strconv.ParseInt(f[0], 10, 64)
			}
		}
	}
	flush()
	return list
}

// Stations lists the devices connected to this machine's access points: every interface that `iw dev`
// shows in AP mode is asked for its stations, and each is labelled with the network name and band.
func (d Detector) Stations(ctx context.Context) StationReport {
	rep := StationReport{Stations: []Station{}}
	if len(d.interfaceNames()) == 0 {
		rep.Known = true // no Wi-Fi adapter: nobody is connected through one
		return rep
	}
	if d.Run == nil {
		return rep
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	out, err := d.Run(ctx, "iw", "dev")
	if err != nil {
		return rep
	}
	rep.Known = true
	for _, ifc := range ParseDev(out) {
		if ifc.Type != "AP" {
			continue
		}
		dump, err := d.Run(ctx, "iw", "dev", ifc.Name, "station", "dump")
		if err != nil {
			continue
		}
		for _, s := range ParseStations(dump) {
			s.Interface, s.SSID, s.Channel, s.Band = ifc.Name, ifc.SSID, ifc.Channel, BandOfMHz(ifc.MHz)
			rep.Stations = append(rep.Stations, s)
		}
	}
	sort.Slice(rep.Stations, func(i, j int) bool {
		a, b := rep.Stations[i], rep.Stations[j]
		if a.SSID != b.SSID {
			return a.SSID < b.SSID
		}
		return a.MAC < b.MAC
	})
	return rep
}

// StationCache keeps the last answer for a few seconds: the pages that ask (the lease list refreshes every
// second) would otherwise run `iw` once per interface each time.
type StationCache struct {
	Detector Detector
	TTL      time.Duration
	Now      func() time.Time // time.Now when nil

	mu  sync.Mutex
	at  time.Time
	rep StationReport
}

func (c *StationCache) Get(ctx context.Context) StationReport {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && now().Sub(c.at) < c.TTL {
		return c.rep
	}
	c.rep = c.Detector.Stations(ctx)
	c.at = now()
	return c.rep
}

// Handler serves GET /api/v1/wifi/stations.
func (c *StationCache) Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(c.Get(r.Context()))
}

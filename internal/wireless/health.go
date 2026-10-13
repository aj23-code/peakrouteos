package wireless

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// How an adapter is doing: how busy the channel is and how much noise there is (`iw dev X survey dump`), and, for an adapter that is a
// client of another access point, whether it is connected and how well (`iw dev X link`).

// Survey is what an adapter measured on one channel. The times are totals since the adapter started (a rate is the change between two
// readings).
type Survey struct {
	MHz       int
	NoiseDBm  int
	HaveNoise bool
	ActiveMS  uint64 // time the radio was listening on the channel
	BusyMS    uint64 // of which the channel was busy (anything on air: ours, others', noise)
	RxMS      uint64
	TxMS      uint64
	InUse     bool
}

// ParseSurvey reads `iw dev <interface> survey dump`.
func ParseSurvey(out string) []Survey {
	var list []Survey
	cur := -1
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Survey data from") {
			list = append(list, Survey{})
			cur = len(list) - 1
			continue
		}
		if cur < 0 {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		num := func() uint64 {
			f := strings.Fields(v)
			if len(f) == 0 {
				return 0
			}
			n, _ := strconv.ParseUint(f[0], 10, 64)
			return n
		}
		switch k {
		case "frequency":
			list[cur].MHz = int(num())
			list[cur].InUse = strings.Contains(v, "[in use]")
		case "noise":
			f := strings.Fields(v)
			if len(f) > 0 {
				if n, err := strconv.Atoi(f[0]); err == nil {
					list[cur].NoiseDBm, list[cur].HaveNoise = n, true
				}
			}
		case "channel active time":
			list[cur].ActiveMS = num()
		case "channel busy time":
			list[cur].BusyMS = num()
		case "channel receive time":
			list[cur].RxMS = num()
		case "channel transmit time":
			list[cur].TxMS = num()
		}
	}
	return list
}

// Survey reads the measurements of the channel an adapter is on now.
func (d Detector) Survey(ctx context.Context, iface string) (Survey, bool) {
	if d.Run == nil {
		return Survey{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := d.Run(ctx, "iw", "dev", iface, "survey", "dump")
	if err != nil {
		return Survey{}, false
	}
	for _, s := range ParseSurvey(out) {
		if s.InUse {
			return s, true
		}
	}
	return Survey{}, false
}

// Link is how an adapter that is a Wi-Fi client is connected.
type Link struct {
	Connected bool    `json:"connected"`
	BSSID     string  `json:"bssid,omitempty"`
	SSID      string  `json:"ssid,omitempty"`
	MHz       int     `json:"mhz,omitempty"`
	SignalDBm *int    `json:"signal_dbm"`
	RxMbps    float64 `json:"rx_mbps"`
	TxMbps    float64 `json:"tx_mbps"`
	Band      string  `json:"band,omitempty"`
}

// ParseLink reads `iw dev <interface> link`.
func ParseLink(out string) Link {
	var l Link
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "Connected to "):
			l.Connected = true
			f := strings.Fields(t)
			if len(f) >= 3 {
				l.BSSID = f[2]
			}
		case strings.HasPrefix(t, "SSID:"):
			l.SSID = strings.TrimSpace(strings.TrimPrefix(t, "SSID:"))
		case strings.HasPrefix(t, "freq:"):
			if f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(t, "freq:")), 64); err == nil {
				l.MHz = int(f)
				l.Band = BandOfMHz(l.MHz)
			}
		case strings.HasPrefix(t, "signal:"):
			f := strings.Fields(strings.TrimPrefix(t, "signal:"))
			if len(f) > 0 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					n := int(v)
					l.SignalDBm = &n
				}
			}
		case strings.HasPrefix(t, "rx bitrate:"), strings.HasPrefix(t, "tx bitrate:"):
			f := strings.Fields(t)
			if len(f) >= 3 {
				if v, err := strconv.ParseFloat(f[2], 64); err == nil {
					if strings.HasPrefix(t, "rx") {
						l.RxMbps = v
					} else {
						l.TxMbps = v
					}
				}
			}
		}
	}
	return l
}

// Link reads how an adapter is connected as a client.
func (d Detector) Link(ctx context.Context, iface string) (Link, bool) {
	if d.Run == nil {
		return Link{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := d.Run(ctx, "iw", "dev", iface, "link")
	if err != nil {
		return Link{}, false
	}
	return ParseLink(out), true
}

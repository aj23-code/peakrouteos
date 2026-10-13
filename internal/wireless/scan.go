package wireless

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Scanning for Wi-Fi networks to join (the Wireless page's Wi-Fi client). `iw dev <adapter> scan` triggers a scan (it needs root, which the
// daemon has); where it is refused (a developer's machine) the last scan the kernel remembers is shown instead (`scan dump`).

// Security names for a network found: the ones the client can join, and what it cannot.
const (
	SecWPA2       = "wpa2"
	SecWPA3       = "wpa3"
	SecWPA2WPA3   = "wpa2-wpa3"
	SecOpen       = "open"
	SecWEP        = "wep"
	SecEnterprise = "enterprise"
)

// Network is a Wi-Fi network seen by a scan.
type Network struct {
	SSID      string `json:"ssid"`
	Signal    int    `json:"signal_dbm"` // the strongest access point of this name
	Band      string `json:"band"`       // "2.4 GHz", "5 GHz" or "6 GHz"
	Security  string `json:"security"`
	Supported bool   `json:"supported"` // the client can join it (WPA2, WPA3 or both with a password)
	Connected bool   `json:"connected"` // this adapter is on it now
}

type bssInfo struct {
	freq       float64
	signal     int
	hasSignal  bool
	ssid       string
	haveSSID   bool
	rsn, wpa   bool
	psk, sae   bool
	eap        bool
	privacy    bool
	associated bool
}

// unescape reads iw's \xNN escapes (it prints anything but plain text that way).
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if n, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func bandOfFreq(mhz float64) string {
	switch {
	case mhz < 3000:
		return "2.4 GHz"
	case mhz < 5900:
		return "5 GHz"
	}
	return "6 GHz"
}

func securityOf(b bssInfo) string {
	switch {
	case b.eap:
		return SecEnterprise
	case (b.rsn || b.wpa) && b.sae && b.psk:
		return SecWPA2WPA3
	case b.rsn && b.sae:
		return SecWPA3
	case b.rsn || b.wpa:
		if b.psk {
			return SecWPA2
		}
		return SecEnterprise // something this client does not do
	case b.privacy:
		return SecWEP
	}
	return SecOpen
}

// ParseScan reads the output of `iw dev <adapter> scan` (or `scan dump`): the networks by name, the strongest access point of each name
// and security, strongest first. Access points that do not say their name are left out, and so are 6 GHz networks (not offered).
func ParseScan(out string) []Network {
	var all []bssInfo
	var cur *bssInfo
	section := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "BSS ") {
			all = append(all, bssInfo{associated: strings.Contains(line, "-- associated")})
			cur = &all[len(all)-1]
			section = ""
			continue
		}
		if cur == nil {
			continue
		}
		t := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, "\t"))
		switch {
		case indent == 1 && strings.HasPrefix(t, "freq:"):
			cur.freq, _ = strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(t, "freq:")), 64)
		case indent == 1 && strings.HasPrefix(t, "signal:"):
			f := strings.Fields(strings.TrimPrefix(t, "signal:"))
			if len(f) > 0 {
				if v, err := strconv.ParseFloat(f[0], 64); err == nil {
					cur.signal, cur.hasSignal = int(v), true
				}
			}
		case indent == 1 && strings.HasPrefix(t, "capability:"):
			cur.privacy = strings.Contains(t, "Privacy")
		case indent == 1 && strings.HasPrefix(t, "SSID:") && !cur.haveSSID:
			cur.ssid, cur.haveSSID = unescape(strings.TrimPrefix(line[strings.Index(line, "SSID:")+len("SSID:"):], " ")), true
		case indent == 1 && strings.HasPrefix(t, "RSN:"):
			cur.rsn, section = true, "rsn"
		case indent == 1 && strings.HasPrefix(t, "WPA:"):
			cur.wpa, section = true, "wpa"
		case indent == 1:
			section = ""
		case (section == "rsn" || section == "wpa") && strings.Contains(t, "Authentication suites:"):
			suites := strings.Fields(strings.SplitN(t, "Authentication suites:", 2)[1])
			for _, s := range suites {
				switch {
				case s == "PSK" || s == "PSK/SHA-256":
					cur.psk = true
				case s == "SAE":
					cur.sae = true
				case strings.Contains(s, "802.1X") || s == "IEEE" || s == "EAP" || strings.HasPrefix(s, "FT/802.1X"):
					cur.eap = true
				}
			}
		}
	}

	type key struct{ ssid, security string }
	best := map[key]*Network{}
	var order []key
	for _, b := range all {
		name := strings.TrimRight(b.ssid, "\x00")
		if !b.haveSSID || strings.TrimSpace(name) == "" || strings.Trim(b.ssid, "\x00") == "" || b.freq >= 5925 {
			continue
		}
		sec := securityOf(b)
		k := key{b.ssid, sec}
		n := Network{SSID: b.ssid, Signal: b.signal, Band: bandOfFreq(b.freq), Security: sec,
			Supported: sec == SecWPA2 || sec == SecWPA3 || sec == SecWPA2WPA3, Connected: b.associated}
		if cur, ok := best[k]; ok {
			if b.associated {
				cur.Connected = true
			}
			if n.Signal > cur.Signal {
				connected := cur.Connected
				*cur = n
				cur.Connected = connected
			}
			continue
		}
		best[k] = &n
		order = append(order, k)
	}
	out2 := make([]Network, 0, len(order))
	for _, k := range order {
		out2 = append(out2, *best[k])
	}
	sort.SliceStable(out2, func(i, j int) bool { return out2[i].Signal > out2[j].Signal })
	return out2
}

func errText(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return string(ee.Stderr)
	}
	return err.Error()
}

// ErrScanUnavailable is returned when neither a scan nor the remembered results can be had.
var ErrScanUnavailable = errors.New("the adapter could not be scanned")

// Scan looks for Wi-Fi networks with one adapter. A scan that the system refuses falls back to the results it remembers (cached is then
// true); an adapter that is down is switched up first.
func (d Detector) Scan(ctx context.Context, iface string) (networks []Network, cached bool, err error) {
	if d.Run == nil {
		return nil, false, ErrScanUnavailable
	}
	out, err := d.Run(ctx, "iw", "dev", iface, "scan")
	if err != nil && strings.Contains(strings.ToLower(errText(err)), "down") {
		if _, upErr := d.Run(ctx, "ip", "link", "set", "dev", iface, "up"); upErr == nil {
			out, err = d.Run(ctx, "iw", "dev", iface, "scan")
		}
	}
	if err == nil {
		return ParseScan(out), false, nil
	}
	dump, dumpErr := d.Run(ctx, "iw", "dev", iface, "scan", "dump")
	if dumpErr != nil {
		return nil, false, ErrScanUnavailable
	}
	return ParseScan(dump), true, nil
}

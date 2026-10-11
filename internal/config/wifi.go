package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Wi-Fi: the access point side. An adapter (a "radio") serves one band on one channel; each Wi-Fi
// network (an SSID) joins one of the local networks, on one band or both. With both bands and band
// steering on, devices that can use 5 GHz are pushed to it.

const (
	Band24 = "2.4 GHz"
	Band5  = "5 GHz"

	SecurityWPA2     = "wpa2"      // WPA2-Personal
	SecurityWPA3     = "wpa3"      // WPA3-Personal (SAE)
	SecurityWPA2WPA3 = "wpa2-wpa3" // both: older devices use WPA2, newer ones WPA3

	MaxWifiNetworks = 16
	MaxWifiRadios   = 4
)

// WifiRadio is how one adapter is used as an access point.
type WifiRadio struct {
	Name     string `json:"name"`    // the adapter, e.g. wlp2s0
	Band     string `json:"band"`    // Band24 or Band5
	Channel  int    `json:"channel"` // 0 lets the adapter pick one
	Width    int    `json:"width"`   // 20, 40, 80 or 160 MHz
	Disabled bool   `json:"disabled,omitempty"`
	// What the adapter supports, as read from it when the radio was set up (802.11n, ac and ax).
	// hostapd refuses a setting the hardware lacks, so the configuration is written from these.
	HT  bool `json:"ht,omitempty"`
	VHT bool `json:"vht,omitempty"`
	HE  bool `json:"he,omitempty"`
}

// WifiNetwork is one SSID.
type WifiNetwork struct {
	ID         string   `json:"id"`
	SSID       string   `json:"ssid"`    // the name devices see
	Network    string   `json:"network"` // the local network it joins ("lan" is the default one)
	Security   string   `json:"security"`
	Passphrase string   `json:"passphrase"`
	Bands      []string `json:"bands"`
	// BandSteering pushes devices that can use 5 GHz off 2.4 GHz. It needs both bands.
	BandSteering    bool `json:"band_steering"`
	Hidden          bool `json:"hidden,omitempty"`           // not announced in beacons
	ClientIsolation bool `json:"client_isolation,omitempty"` // devices on the SSID cannot talk to each other
	Disabled        bool `json:"disabled,omitempty"`
}

// WifiConfig is the whole Wi-Fi setup.
type WifiConfig struct {
	Country  string        `json:"country,omitempty"` // ISO 3166-1 code; it decides which channels and powers are allowed
	Radios   []WifiRadio   `json:"radios,omitempty"`
	Networks []WifiNetwork `json:"networks,omitempty"`
}

func (w WifiConfig) clone() WifiConfig {
	w.Radios = append([]WifiRadio(nil), w.Radios...)
	if len(w.Networks) > 0 {
		nets := make([]WifiNetwork, len(w.Networks))
		for i, n := range w.Networks {
			n.Bands = append([]string(nil), n.Bands...)
			nets[i] = n
		}
		w.Networks = nets
	} else {
		w.Networks = nil
	}
	return w
}

// WifiSettings returns the Wi-Fi setup, empty when there is none.
func (c NetworkConfig) WifiSettings() WifiConfig {
	if c.Wifi == nil {
		return WifiConfig{}
	}
	return *c.Wifi
}

// FindWifiNetwork returns an SSID by id.
func (w WifiConfig) FindNetwork(id string) (WifiNetwork, bool) {
	for _, n := range w.Networks {
		if n.ID == id {
			return n, true
		}
	}
	return WifiNetwork{}, false
}

// Has reports whether the SSID is broadcast on a band.
func (n WifiNetwork) Has(band string) bool {
	for _, b := range n.Bands {
		if b == band {
			return true
		}
	}
	return false
}

// Active is the part of the Wi-Fi setup that is switched on: radios that are enabled, and the SSIDs
// that are enabled. Nothing is rendered for the rest.
func (w WifiConfig) Active() (radios []WifiRadio, networks []WifiNetwork) {
	for _, r := range w.Radios {
		if !r.Disabled {
			radios = append(radios, r)
		}
	}
	for _, n := range w.Networks {
		if !n.Disabled {
			networks = append(networks, n)
		}
	}
	return radios, networks
}

// ---- channels ------------------------------------------------------------------------------------

// The channels an access point can be set to. 6 GHz needs WPA3 and a different set-up and is not offered.
var channels5 = []int{36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165}

// 40 MHz channels pair a primary channel with a neighbour; 80 and 160 MHz use fixed blocks.
var (
	pairs40  = [][2]int{{36, 40}, {44, 48}, {52, 56}, {60, 64}, {100, 104}, {108, 112}, {116, 120}, {124, 128}, {132, 136}, {140, 144}, {149, 153}, {157, 161}}
	blocks80 = []struct {
		first, last, centre int
	}{{36, 48, 42}, {52, 64, 58}, {100, 112, 106}, {116, 128, 122}, {132, 144, 138}, {149, 161, 155}}
	blocks160 = []struct {
		first, last, centre int
	}{{36, 64, 50}, {100, 128, 114}}
)

// ValidChannel reports whether n is a channel of the band.
func ValidChannel(band string, n int) bool {
	switch band {
	case Band24:
		return n >= 1 && n <= 13
	case Band5:
		for _, c := range channels5 {
			if c == n {
				return true
			}
		}
	}
	return false
}

// ValidWidth reports whether a width (in MHz) can exist in the band at all.
func ValidWidth(band string, w int) bool {
	switch band {
	case Band24:
		return w == 20 || w == 40
	case Band5:
		return w == 20 || w == 40 || w == 80 || w == 160
	}
	return false
}

// ChannelFitsWidth reports whether the channel can be the primary channel of a channel of that width.
func ChannelFitsWidth(band string, ch, width int) bool {
	if !ValidChannel(band, ch) || !ValidWidth(band, width) {
		return false
	}
	switch {
	case width == 20:
		return true
	case band == Band24:
		return true // 40 MHz: a neighbour exists on at least one side of every channel
	case width == 40:
		for _, p := range pairs40 {
			if ch == p[0] || ch == p[1] {
				return true
			}
		}
		return false
	case width == 80:
		for _, b := range blocks80 {
			if ch >= b.first && ch <= b.last {
				return true
			}
		}
		return false
	default:
		for _, b := range blocks160 {
			if ch >= b.first && ch <= b.last {
				return true
			}
		}
		return false
	}
}

// HT40Direction says on which side of the primary channel the 40 MHz channel extends: "+" (above) or
// "-" (below). It is "" for a channel that has no 40 MHz form.
func HT40Direction(band string, ch int) string {
	if band == Band24 {
		if ch <= 7 {
			return "+"
		}
		return "-"
	}
	for _, p := range pairs40 {
		if ch == p[0] {
			return "+"
		}
		if ch == p[1] {
			return "-"
		}
	}
	return ""
}

// CentreChannel returns the middle channel of the 80 or 160 MHz block holding ch, or 0 if none.
func CentreChannel(ch, width int) int {
	blocks := blocks80
	if width == 160 {
		blocks = blocks160
	} else if width != 80 {
		return 0
	}
	for _, b := range blocks {
		if ch >= b.first && ch <= b.last {
			return b.centre
		}
	}
	return 0
}

// ---- validation ----------------------------------------------------------------------------------

var wifiIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// ValidSSID reports whether s is a usable network name: 1 to 32 bytes of text, no control characters,
// and no space at either end (devices and hostapd treat those unreliably).
func ValidSSID(s string) bool {
	if len(s) < 1 || len(s) > 32 || !utf8.ValidString(s) || s != strings.TrimSpace(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ValidPassphrase reports whether s is a usable WPA passphrase: 8 to 63 printable ASCII characters.
func ValidPassphrase(s string) bool {
	if len(s) < 8 || len(s) > 63 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func validSecurity(s string) bool {
	return s == SecurityWPA2 || s == SecurityWPA3 || s == SecurityWPA2WPA3
}

// validateWifi checks the Wi-Fi setup. Errors are reported on wifi.country, wifi.radios[i].field,
// wifi.networks[i].field, or wifi.coverage for a network that no radio can carry.
func (c NetworkConfig) validateWifi(add func(field, format string, args ...any)) {
	w := c.WifiSettings()
	if c.Wifi == nil {
		return
	}

	if w.Country != "" && !IsCountry(w.Country) {
		add("wifi.country", "must be a two-letter country code such as US or DE")
	}

	// radios
	if len(w.Radios) > MaxWifiRadios {
		add("wifi.radios", "at most %d adapters can be used as access points", MaxWifiRadios)
	}
	seen := map[string]int{}
	for i, r := range w.Radios {
		field := func(n string) string { return fmt.Sprintf("wifi.radios[%d].%s", i, n) }
		if !IsValidInterfaceName(r.Name) {
			add(field("name"), "must be an adapter name (letters, digits, '.', '_' or '-', up to 15 characters)")
		} else if first, dup := seen[r.Name]; dup {
			add(field("name"), "this adapter is listed more than once")
			add(fmt.Sprintf("wifi.radios[%d].name", first), "this adapter is listed more than once")
		} else {
			seen[r.Name] = i
		}
		if r.Band != Band24 && r.Band != Band5 {
			add(field("band"), "must be %s or %s", Band24, Band5)
			continue
		}
		if !ValidWidth(r.Band, r.Width) {
			add(field("width"), "the %s band allows %s", r.Band, widthsText(r.Band))
			continue
		}
		if r.Width >= 40 && !r.HT {
			add(field("width"), "this adapter does not support channels wider than 20 MHz")
			continue
		}
		if r.Width >= 80 && !r.VHT && !r.HE {
			add(field("width"), "this adapter does not support %d MHz channels", r.Width)
			continue
		}
		if r.Channel != 0 {
			switch {
			case !ValidChannel(r.Band, r.Channel):
				add(field("channel"), "%d is not a %s channel", r.Channel, r.Band)
			case !ChannelFitsWidth(r.Band, r.Channel, r.Width):
				add(field("channel"), "channel %d cannot carry a %d MHz channel; pick another channel or a narrower width", r.Channel, r.Width)
			}
		}
	}

	// networks
	if len(w.Networks) > MaxWifiNetworks {
		add("wifi.networks", "at most %d Wi-Fi networks are supported", MaxWifiNetworks)
	}
	ids, ssids := map[string]int{}, map[string]int{}
	for i, n := range w.Networks {
		field := func(name string) string { return fmt.Sprintf("wifi.networks[%d].%s", i, name) }

		switch {
		case !wifiIDPattern.MatchString(n.ID):
			add(field("id"), "must be lower-case letters, digits and '-', starting with a letter (up to 16 characters)")
		case hasKey(ids, n.ID):
			add(field("id"), "is used by another Wi-Fi network")
		default:
			ids[n.ID] = i
		}

		if !ValidSSID(n.SSID) {
			add(field("ssid"), "use 1 to 32 characters, with no space at either end")
		} else if first, dup := ssids[n.SSID]; dup {
			add(field("ssid"), "another Wi-Fi network already has this name")
			add(fmt.Sprintf("wifi.networks[%d].ssid", first), "another Wi-Fi network already has this name")
		} else {
			ssids[n.SSID] = i
		}

		if n.Network != DefaultNetworkID {
			if _, ok := c.FindNetwork(n.Network); !ok {
				add(field("network"), "choose the local network this Wi-Fi network belongs to")
			}
		}

		if !validSecurity(n.Security) {
			add(field("security"), "must be WPA2, WPA3 or both")
		}
		if !ValidPassphrase(n.Passphrase) {
			add(field("passphrase"), "use 8 to 63 characters (letters, digits and punctuation)")
		}

		if len(n.Bands) == 0 {
			add(field("bands"), "choose at least one band")
		}
		bands := map[string]bool{}
		for _, b := range n.Bands {
			switch {
			case b != Band24 && b != Band5:
				add(field("bands"), "%q is not a band that can be used", b)
			case bands[b]:
				add(field("bands"), "%s is listed more than once", b)
			}
			bands[b] = true
		}
		if n.BandSteering && !(bands[Band24] && bands[Band5]) {
			add(field("band_steering"), "band steering needs both bands")
		}
	}

	// every band a switched-on network uses needs a switched-on radio for it
	radios, networks := w.Active()
	have := map[string]bool{}
	for _, r := range radios {
		have[r.Band] = true
	}
	var missing []string
	for _, n := range networks {
		for _, b := range n.Bands {
			if (b == Band24 || b == Band5) && !have[b] {
				missing = append(missing, fmt.Sprintf("%s (%s)", n.SSID, b))
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		add("wifi.coverage", "no adapter is switched on for: %s", strings.Join(missing, ", "))
	}
	if len(radios) > 0 && len(networks) > 0 && w.Country == "" {
		add("wifi.country", "choose your country before switching Wi-Fi on: it decides which channels may be used")
	}
}

func widthsText(band string) string {
	if band == Band24 {
		return "20 or 40 MHz"
	}
	return "20, 40, 80 or 160 MHz"
}

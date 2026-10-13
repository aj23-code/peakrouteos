package setup

import (
	"sort"

	"peakroute/internal/config"
	"peakroute/internal/wireless"
)

// WifiAdapter is an adapter the wizard can use for Wi-Fi.
type WifiAdapter struct {
	Name  string   `json:"name"`
	Bands []string `json:"bands"`
}

// usableAdapters are the adapters that can be an access point and whose abilities are known, by name.
func usableAdapters(rep wireless.Report) []wireless.Adapter {
	var out []wireless.Adapter
	for _, a := range rep.Adapters {
		if a.CapabilitiesKnown && a.APCapable && len(a.Bands) > 0 && (a.MaxAP == 0 || a.MaxAP >= 1) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func bandOf(a wireless.Adapter, name string) (wireless.Band, bool) {
	for _, b := range a.Bands {
		if b.Name == name {
			return b, true
		}
	}
	return wireless.Band{}, false
}

// widthFor is the widest channel width that suits the band: 20 MHz on 2.4 GHz (wider channels crowd it), up to 80 MHz on 5 GHz.
func widthFor(b wireless.Band) int {
	has := func(w int) bool {
		for _, x := range b.Widths {
			if x == w {
				return true
			}
		}
		return false
	}
	if b.Name == config.Band5 {
		switch {
		case has(80) && (b.VHT || b.HE):
			return 80
		case has(40) && b.HT:
			return 40
		}
	}
	return 20
}

// wifiFor makes the Wi-Fi setup for one network on the adapters there are, on the local network, with the usual security:
//   - one adapter serves one band, 2.4 GHz (every device can use it; the Wireless page moves it to 5 GHz);
//   - with two or more adapters the first that can serves 5 GHz and the next 2.4 GHz, so both bands are covered, with band steering.
//
// The channel is left automatic. It returns an empty setup if no adapter qualifies.
func wifiFor(rep wireless.Report, name, passphrase string) config.WifiConfig {
	adapters := usableAdapters(rep)
	var wf config.WifiConfig
	covered := map[string]bool{}
	add := func(a wireless.Adapter, band wireless.Band) {
		wf.Radios = append(wf.Radios, config.WifiRadio{Name: a.Name, Band: band.Name, Channel: 0, Width: widthFor(band), HT: band.HT, VHT: band.VHT, HE: band.HE})
		covered[band.Name] = true
	}
	switch {
	case len(adapters) == 1:
		a := adapters[0]
		if b, ok := bandOf(a, config.Band24); ok {
			add(a, b)
		} else if b, ok := bandOf(a, config.Band5); ok {
			add(a, b)
		}
	case len(adapters) >= 2:
		for _, want := range []string{config.Band5, config.Band24} {
			for _, a := range adapters {
				used := false
				for _, r := range wf.Radios {
					used = used || r.Name == a.Name
				}
				if used {
					continue
				}
				if b, ok := bandOf(a, want); ok {
					add(a, b)
					break
				}
			}
		}
	}
	if len(wf.Radios) == 0 {
		return config.WifiConfig{}
	}
	var bands []string
	for _, b := range []string{config.Band24, config.Band5} {
		if covered[b] {
			bands = append(bands, b)
		}
	}
	wf.Networks = []config.WifiNetwork{{ID: "wifi1", SSID: name, Network: config.DefaultNetworkID, Security: config.SecurityWPA2WPA3, Passphrase: passphrase,
		Bands: bands, BandSteering: len(bands) == 2}}
	return wf
}

// wifiAdapters is what the wizard page needs to know about Wi-Fi.
func (s *Service) wifiAdapters() []WifiAdapter {
	out := []WifiAdapter{}
	if s.Wireless == nil {
		return out
	}
	rep := s.wirelessReport()
	for _, a := range usableAdapters(rep) {
		wa := WifiAdapter{Name: a.Name, Bands: []string{}}
		for _, b := range a.Bands {
			wa.Bands = append(wa.Bands, b.Name)
		}
		out = append(out, wa)
	}
	return out
}

package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// Wi-Fi: one hostapd configuration file per adapter that is switched on and has Wi-Fi networks. A
// single hostapd process serves all of them (the wifi service starts it with every file), which is what
// lets band steering work: a 2.4 GHz network can refuse devices it has already seen on 5 GHz.

// PathHostapdDir holds one <adapter>.conf per access point adapter. Anything else the translator finds
// there that it did not write is removed, so an adapter taken out of the setup stops being served.
const PathHostapdDir = "/etc/peakroute/hostapd"

func hostapdPath(adapter string) string { return PathHostapdDir + "/" + adapter + ".conf" }

// bss is one Wi-Fi network on one adapter.
type bss struct {
	net   config.WifiNetwork
	iface string // the first network on an adapter uses the adapter's own name, the next ones name_1, name_2...
}

type radioPlan struct {
	radio config.WifiRadio
	bss   []bss
}

// wifiPlan decides which networks each switched-on adapter serves, and which interface each network is
// on in each band (band steering points a 2.4 GHz network at its 5 GHz twin).
func wifiPlan(cfg config.NetworkConfig) (plans []radioPlan, firstOn map[string]string) {
	radios, nets := cfg.WifiSettings().Active()
	firstOn = map[string]string{} // network id + band -> interface
	for _, r := range radios {
		p := radioPlan{radio: r}
		for _, n := range nets {
			if !n.Has(r.Band) {
				continue
			}
			name := r.Name
			if len(p.bss) > 0 {
				name = fmt.Sprintf("%s_%d", r.Name, len(p.bss))
			}
			p.bss = append(p.bss, bss{net: n, iface: name})
			if _, ok := firstOn[n.ID+"|"+r.Band]; !ok {
				firstOn[n.ID+"|"+r.Band] = name
			}
		}
		if len(p.bss) > 0 {
			plans = append(plans, p)
		}
	}
	return plans, firstOn
}

// wifiOnDefaultNetwork reports whether any Wi-Fi network joins the default network. That network then
// needs a bridge for the access point to join, even if only one cable port carries it.
func wifiOnDefaultNetwork(cfg config.NetworkConfig) bool {
	plans, _ := wifiPlan(cfg)
	for _, p := range plans {
		for _, b := range p.bss {
			if b.net.Network == config.DefaultNetworkID {
				return true
			}
		}
	}
	return false
}

// wifiCarries reports whether a Wi-Fi network (that is switched on and has an adapter) joins a local network.
func wifiCarries(cfg config.NetworkConfig, id string) bool {
	plans, _ := wifiPlan(cfg)
	for _, p := range plans {
		for _, b := range p.bss {
			if b.net.Network == id {
				return true
			}
		}
	}
	return false
}

// bridgeOf is the interface of a local network that an access point joins.
func bridgeOf(cfg config.NetworkConfig, id string) string {
	if id == config.DefaultNetworkID {
		return LanTarget(cfg)
	}
	if n, ok := cfg.FindNetwork(id); ok {
		return networkIface(n)
	}
	return ""
}

// safeWifiText is the render-time twin of the config checks for the two free-text values that end up in a
// hostapd file: nothing that could end the line and start another setting.
func safeWifiText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return s != ""
}

func ensureSafeWifi(cfg config.NetworkConfig) error {
	w := cfg.WifiSettings()
	radios, nets := w.Active()
	for _, r := range radios {
		if err := ensureSafe(r.Name); err != nil {
			return err
		}
	}
	if cfg.Country != "" && len(cfg.Country) != 2 {
		return fmt.Errorf("refusing to render unsafe country %q", cfg.Country)
	}
	for _, c := range []byte(cfg.Country) {
		if c < 'A' || c > 'Z' {
			return fmt.Errorf("refusing to render unsafe country %q", cfg.Country)
		}
	}
	for _, n := range nets {
		if !safeWifiText(n.SSID) || !safeWifiText(n.Passphrase) {
			return fmt.Errorf("refusing to render an unsafe Wi-Fi name or password")
		}
		if bridgeOf(cfg, n.Network) == "" {
			return fmt.Errorf("Wi-Fi network %q joins a network that does not exist", n.SSID)
		}
	}
	return nil
}

func renderHostapd(cfg config.NetworkConfig, p radioPlan, firstOn map[string]string) string {
	r := p.radio
	country := cfg.Country
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.")
	line("interface=%s", r.Name)
	line("driver=nl80211")
	line("ctrl_interface=/run/hostapd")
	line("ctrl_interface_group=0")
	line("country_code=%s", country)
	line("ieee80211d=1")
	if r.Band == config.Band5 {
		line("ieee80211h=1") // listen for radar on the channels that share spectrum with it
		line("hw_mode=a")
	} else {
		line("hw_mode=g")
	}
	line("channel=%d", r.Channel)
	if r.Channel == 0 && r.Band == config.Band5 {
		line("acs_exclude_dfs=1") // an automatic choice avoids the channels that need a radar wait
	}
	line("beacon_int=100")
	line("dtim_period=2")
	line("wmm_enabled=1")

	// the 802.11 generations the adapter supports, at the width chosen
	if r.HT {
		line("ieee80211n=1")
		if r.Width >= 40 && r.Channel != 0 {
			if dir := config.HT40Direction(r.Band, r.Channel); dir != "" {
				line("ht_capab=[HT40%s]", dir)
			}
		}
	}
	chwidth := map[int]int{20: 0, 40: 0, 80: 1, 160: 2}[r.Width]
	centre := 0
	if r.Channel != 0 {
		centre = config.CentreChannel(r.Channel, r.Width)
	}
	if r.Band == config.Band5 && r.VHT {
		line("ieee80211ac=1")
		line("vht_oper_chwidth=%d", chwidth)
		if centre != 0 {
			line("vht_oper_centr_freq_seg0_idx=%d", centre)
		}
	}
	if r.HE {
		line("ieee80211ax=1")
		if r.Band == config.Band5 {
			line("he_oper_chwidth=%d", chwidth)
			if centre != 0 {
				line("he_oper_centr_freq_seg0_idx=%d", centre)
			}
		}
	}

	for i, x := range p.bss {
		n := x.net
		line("")
		if i > 0 {
			line("bss=%s", x.iface)
		}
		line("ssid=%s", n.SSID)
		line("bridge=%s", bridgeOf(cfg, n.Network))
		line("auth_algs=1")
		line("wpa=2")
		switch n.Security {
		case config.SecurityWPA3:
			line("wpa_key_mgmt=SAE")
			line("sae_password=%s", n.Passphrase)
			line("ieee80211w=2")
			line("sae_require_mfp=1")
			line("sae_pwe=2")
		case config.SecurityWPA2WPA3:
			line("wpa_key_mgmt=WPA-PSK SAE")
			line("wpa_passphrase=%s", n.Passphrase)
			line("ieee80211w=1")
			line("sae_require_mfp=1")
			line("sae_pwe=2")
		default:
			line("wpa_key_mgmt=WPA-PSK")
			line("wpa_passphrase=%s", n.Passphrase)
		}
		line("rsn_pairwise=CCMP")
		if n.Hidden {
			line("ignore_broadcast_ssid=1")
		}
		if n.ClientIsolation {
			line("ap_isolate=1")
		}
		// 802.11k and v let devices ask where to roam to and let the access point suggest it
		line("rrm_neighbor_report=1")
		line("bss_transition=1")

		// Band steering: on the 2.4 GHz side of a network that is also on 5 GHz, ignore a device that
		// has already been seen on the 5 GHz side, so it stays there
		if n.BandSteering && r.Band == config.Band24 {
			if twin, ok := firstOn[n.ID+"|"+config.Band5]; ok {
				line("no_probe_resp_if_seen_on=%s", twin)
				line("no_auth_if_seen_on=%s", twin)
			}
		}
	}
	return b.String()
}

// wifiArtifacts renders the hostapd files: one per adapter that has networks to serve, and a removal for
// every other adapter in the setup (a managed directory catches any file left by one no longer listed).
func wifiArtifacts(cfg config.NetworkConfig) ([]Artifact, error) {
	if cfg.Wifi == nil {
		return nil, nil
	}
	if err := ensureSafeWifi(cfg); err != nil {
		return nil, err
	}
	plans, firstOn := wifiPlan(cfg)
	served := map[string]bool{}
	var out []Artifact
	for _, p := range plans {
		served[p.radio.Name] = true
		out = append(out, Artifact{Path: hostapdPath(p.radio.Name), Content: []byte(renderHostapd(cfg, p, firstOn)), Mode: 0600, Service: SvcWifi})
	}
	for _, r := range cfg.Wifi.Radios {
		if !served[r.Name] {
			out = append(out, Artifact{Path: hostapdPath(r.Name), Remove: true, Service: SvcWifi})
		}
	}
	return out, nil
}

// wifiManaged reports whether the plan leaves any access point configuration in place.
func wifiManaged(d Desired) bool {
	for _, a := range d.Artifacts {
		if strings.HasPrefix(a.Path, PathHostapdDir+"/") && !a.Remove {
			return true
		}
	}
	return false
}

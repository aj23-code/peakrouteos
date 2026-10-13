package translator

import (
	"encoding/hex"
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// Wi-Fi client ("station") adapters: the router joins another access point with wpa_supplicant, started by ifupdown's own hook (the
// wpasupplicant package) through a "wpa-conf" line in the adapter's stanza.
//
//   - As the internet connection the adapter is the WAN port: the usual WAN stanza (DHCP or a fixed address) gets the wpa-conf line.
//   - As a link into a local network the adapter has a stanza of its own (inet manual, in 4-address mode so that the devices behind
//     it keep their own addresses) and is a member of that network's bridge, like a cable port.
//
// A change to a file here restarts networking: that is what reads it again.

// PathWpaDir holds one <adapter>.conf per Wi-Fi client adapter in use (0600: it holds the password). Anything else there that the
// translator did not write is removed.
const PathWpaDir = "/etc/peakroute/wpa"

func wpaPath(adapter string) string { return PathWpaDir + "/" + adapter + ".conf" }

// stationsInUse are the Wi-Fi clients that are switched on and used: the internet connection when the adapter is the internet port, and
// every link into a local network.
func stationsInUse(cfg config.NetworkConfig) []config.WifiStation {
	var out []config.WifiStation
	if st, ok := cfg.UplinkStation(); ok {
		out = append(out, st)
	}
	for _, st := range cfg.WifiSettings().ActiveStations() {
		if st.Role == config.StationBridge {
			out = append(out, st)
		}
	}
	return out
}

// renderWpa writes wpa_supplicant's file for a station. The name of the network is written in hexadecimal, which cannot be broken by any
// character in it.
func renderWpa(cfg config.NetworkConfig, st config.WifiStation) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.")
	line("ctrl_interface=/run/wpa_supplicant")
	line("update_config=0")
	line("country=%s", cfg.Country)
	line("network={")
	line("    ssid=%s", hex.EncodeToString([]byte(st.SSID)))
	if st.Hidden {
		line("    scan_ssid=1")
	}
	line("    proto=RSN")
	line("    pairwise=CCMP")
	line("    group=CCMP")
	switch st.Security {
	case config.SecurityWPA3:
		line("    key_mgmt=SAE")
		line("    sae_password=\"%s\"", st.Passphrase)
		line("    ieee80211w=2")
	case config.SecurityWPA2WPA3:
		line("    key_mgmt=WPA-PSK SAE")
		line("    psk=\"%s\"", st.Passphrase)
		line("    ieee80211w=1")
	default:
		line("    key_mgmt=WPA-PSK")
		line("    psk=\"%s\"", st.Passphrase)
	}
	line("}")
	return b.String()
}

// writeStationStanzas writes the interfaces file's part for the Wi-Fi clients that join a local network.
func writeStationStanzas(b *strings.Builder, cfg config.NetworkConfig) {
	for _, st := range cfg.WifiSettings().ActiveStations() {
		if st.Role != config.StationBridge {
			continue
		}
		fmt.Fprintf(b, "auto %s\niface %s inet manual\n", st.Name, st.Name)
		// 4-address mode lets a bridge sit behind the link; it can only be set while the adapter is down
		fmt.Fprintf(b, "    pre-up iw dev $IFACE set 4addr on\n")
		fmt.Fprintf(b, "    wpa-conf %s\n\n", wpaPath(st.Name))
	}
}

func ensureSafeStations(cfg config.NetworkConfig) error {
	for _, st := range stationsInUse(cfg) {
		if err := ensureSafe(st.Name); err != nil {
			return err
		}
		if !safeWifiText(st.SSID) || !safeWifiText(st.Passphrase) {
			return fmt.Errorf("refusing to render an unsafe Wi-Fi name or password")
		}
		if st.Role == config.StationBridge && bridgeOf(cfg, st.Network) == "" {
			return fmt.Errorf("Wi-Fi link %q joins a network that does not exist", st.Name)
		}
	}
	return nil
}

// stationArtifacts renders the wpa_supplicant files: one for each station in use, and a removal for every other station in the setup.
func stationArtifacts(cfg config.NetworkConfig) ([]Artifact, error) {
	if cfg.Wifi == nil {
		return nil, nil
	}
	if err := ensureSafeStations(cfg); err != nil {
		return nil, err
	}
	used := map[string]bool{}
	var out []Artifact
	for _, st := range stationsInUse(cfg) {
		used[st.Name] = true
		out = append(out, Artifact{Path: wpaPath(st.Name), Content: []byte(renderWpa(cfg, st)), Mode: 0600, Service: SvcNetworking})
	}
	for _, st := range cfg.Wifi.Stations {
		if !used[st.Name] {
			out = append(out, Artifact{Path: wpaPath(st.Name), Remove: true, Service: SvcNetworking})
		}
	}
	return out, nil
}

// PathRegdom makes the kernel's regulatory country (which channels and powers Wi-Fi may use) the router's, now and at every start: the
// cfg80211 module reads it when it loads, and the commit tells the running kernel with `iw reg set`.
const PathRegdom = "/etc/modprobe.d/peakroute-regdom.conf"

// regdomArtifact writes the country for the kernel, or removes the file when there is none.
func regdomArtifact(cfg config.NetworkConfig) (Artifact, error) {
	if cfg.Country == "" {
		return Artifact{Path: PathRegdom, Remove: true, Service: SvcRegdom}, nil
	}
	if len(cfg.Country) != 2 || cfg.Country[0] < 'A' || cfg.Country[0] > 'Z' || cfg.Country[1] < 'A' || cfg.Country[1] > 'Z' {
		return Artifact{}, fmt.Errorf("refusing to render unsafe country %q", cfg.Country)
	}
	return Artifact{Path: PathRegdom, Content: []byte("options cfg80211 ieee80211_regdom=" + cfg.Country + "\n"), Mode: 0644, Service: SvcRegdom}, nil
}

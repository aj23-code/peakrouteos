package config

import (
	"strings"
	"testing"
)

func uplinkStn() WifiStation {
	return WifiStation{Name: "wlan0", SSID: "Cafe", Security: SecurityWPA2, Passphrase: "correct horse battery", Role: StationUplink}
}

func linkStn() WifiStation {
	return WifiStation{Name: "wlan1", SSID: "RoofLink", Security: SecurityWPA3, Passphrase: "correct horse battery", Role: StationBridge, Network: "lan"}
}

func withStations(c NetworkConfig, country string, st ...WifiStation) NetworkConfig {
	c.Country = country
	c.Wifi = &WifiConfig{Stations: st}
	return c
}

func TestValidWifiStations(t *testing.T) {
	asWan := withStations(validNet(), "US", uplinkStn())
	asWan.WanInterface = "wlan0"
	guestNet := withNets(validNet(), guest())
	guestLink := linkStn()
	guestLink.Network = "guest"
	both := withStations(validNet(), "US", uplinkStn(), linkStn())
	both.WanInterface = "wlan0"
	apOff := withStations(validNet(), "US", linkStn())
	apOff.Wifi.Radios = []WifiRadio{{Name: "wlan1", Band: Band24, Channel: 6, Width: 20, HT: true, Disabled: true}}
	cases := map[string]NetworkConfig{
		"the adapter's access point is switched off": apOff,
		"an uplink that is the internet port":        asWan,
		"an uplink that is not in use (off)":         withStations(validNet(), "US", func() WifiStation { s := uplinkStn(); s.Disabled = true; return s }()),
		"a link into the default network":            withStations(validNet(), "US", linkStn()),
		"a link into another network":                withStations(guestNet, "US", guestLink),
		"an uplink and a link":                       both,
		"a hidden network and a WPA2/WPA3 mixed":     withStations(validNet(), "US", func() WifiStation { s := linkStn(); s.Security = SecurityWPA2WPA3; s.Hidden = true; return s }()),
		"switched off needs no country":              withStations(validNet(), "", func() WifiStation { s := linkStn(); s.Disabled = true; return s }()),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInvalidWifiStations(t *testing.T) {
	mut := func(f func(*WifiStation)) NetworkConfig {
		s := linkStn()
		f(&s)
		return withStations(validNet(), "US", s)
	}
	wanVlan := withStations(validNet(), "US", uplinkStn())
	wanVlan.WanInterface, wanVlan.WanVlan = "wlan0", 835
	pppoe := withStations(validNet(), "US", uplinkStn())
	pppoe.WanInterface, pppoe.WanMode, pppoe.WanPPPoE = "wlan0", "pppoe", &PPPoEConfig{Username: "u", Password: "p"}
	two := withStations(validNet(), "US", uplinkStn(), func() WifiStation { s := uplinkStn(); s.Name = "wlan2"; return s }())
	ap := withStations(validNet(), "US", linkStn())
	ap.Wifi.Radios = []WifiRadio{{Name: "wlan1", Band: Band24, Channel: 6, Width: 20, HT: true}}
	lan := withStations(validNet(), "US", linkStn())
	lan.LanInterfaces = []string{"eth1", "wlan1"}
	wan := withStations(validNet(), "US", linkStn())
	wan.WanInterface = "wlan1"
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"no country while on", withStations(validNet(), "", linkStn()), "country"},
		{"a bad adapter name", mut(func(s *WifiStation) { s.Name = "wlan1;reboot" }), "wifi.stations[0].name"},
		{"an empty name", mut(func(s *WifiStation) { s.SSID = "" }), "wifi.stations[0].ssid"},
		{"a name with a newline", mut(func(s *WifiStation) { s.SSID = "a\nb" }), "wifi.stations[0].ssid"},
		{"an unknown security", mut(func(s *WifiStation) { s.Security = "wep" }), "wifi.stations[0].security"},
		{"a short password", mut(func(s *WifiStation) { s.Passphrase = "short" }), "wifi.stations[0].passphrase"},
		{"a password with a newline", mut(func(s *WifiStation) { s.Passphrase = "eight chars\nnetwork={" }), "wifi.stations[0].passphrase"},
		{"no role", mut(func(s *WifiStation) { s.Role = "" }), "wifi.stations[0].role"},
		{"a link into a network that is not there", mut(func(s *WifiStation) { s.Network = "nope" }), "wifi.stations[0].network"},
		{"a link with no network", mut(func(s *WifiStation) { s.Network = "" }), "wifi.stations[0].network"},
		{"a link that is the internet port", wan, "wifi.stations[0].role"},
		{"a link that is a LAN port", lan, "wifi.stations[0].role"},
		{"an adapter that is also an access point", ap, "wifi.stations[0].name"},
		{"the same adapter twice", withStations(validNet(), "US", linkStn(), linkStn()), "wifi.stations[1].name"},
		{"two internet connections", two, "wifi.stations"},
		{"too many", withStations(validNet(), "US", linkStn(), func() WifiStation { s := linkStn(); s.Name = "wlan2"; return s }(), func() WifiStation { s := linkStn(); s.Name = "wlan3"; return s }()), "wifi.stations"},
		{"a VLAN on Wi-Fi", wanVlan, "wan_vlan"},
		{"PPPoE over Wi-Fi", pppoe, "wan_mode"},
	}
	for _, c := range cases {
		err := c.cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted, want an error on %s", c.name, c.field)
			continue
		}
		if ve := fieldsOf(t, err); !ve.Has(c.field) {
			t.Errorf("%s: errors were %v, want one on %s", c.name, ve, c.field)
		}
	}
}

func TestStationHelpers(t *testing.T) {
	c := withStations(validNet(), "US", uplinkStn(), linkStn())
	if _, ok := c.UplinkStation(); ok {
		t.Fatal("an uplink that is not the internet port is in use")
	}
	c.WanInterface = "wlan0"
	if st, ok := c.UplinkStation(); !ok || st.Name != "wlan0" {
		t.Fatalf("%+v %v", st, ok)
	}
	if got := c.BridgeStations("lan"); len(got) != 1 || got[0].Name != "wlan1" {
		t.Fatalf("%+v", got)
	}
	if got := c.BridgeStations("guest"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	c.Wifi.Stations[1].Disabled = true
	if got := c.BridgeStations("lan"); len(got) != 0 {
		t.Fatal("a switched-off link is in use")
	}
	// a copy shares nothing with the original
	d := c.clone()
	d.Wifi.Stations[0].SSID = "changed"
	if c.Wifi.Stations[0].SSID == "changed" {
		t.Fatal("the clone shares the stations")
	}
}

func TestTheReviewNeverShowsAStationPassword(t *testing.T) {
	a := validNet()
	b := withStations(validNet(), "US", linkStn())
	changes := DiffNetwork(a, b)
	var got *Change
	for i := range changes {
		if changes[i].Field == "wifi_stations" {
			got = &changes[i]
		}
	}
	if got == nil || got.Label != "Wi-Fi connection wlan1" || !strings.Contains(got.New, "password set") || !strings.Contains(got.New, "link into LAN") {
		t.Fatalf("%+v", changes)
	}
	for _, c := range changes {
		if strings.Contains(c.Old+c.New, "correct horse") {
			t.Fatal("the password is in the review")
		}
	}
	// a changed password is reported without showing it
	c := withStations(validNet(), "US", func() WifiStation { s := linkStn(); s.Passphrase = "another password 1"; return s }())
	for _, ch := range DiffNetwork(b, c) {
		if ch.Field == "wifi_stations" && (!strings.Contains(ch.New, "new password") || strings.Contains(ch.Old+ch.New, "another")) {
			t.Fatalf("%+v", ch)
		}
	}
	// removing it
	for _, ch := range DiffNetwork(b, validNet()) {
		if ch.Field == "wifi_stations" && ch.New != "(removed)" {
			t.Fatalf("%+v", ch)
		}
	}
}

func TestAnAccessPointCanAcceptPointToPointLinks(t *testing.T) {
	n := homeWifi()
	n.WDS = true
	c := withWifi(validNet(), "US", []WifiRadio{radio24(), radio5()}, n)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	old := withWifi(validNet(), "US", []WifiRadio{radio24(), radio5()}, homeWifi())
	found := false
	for _, ch := range DiffNetwork(old, c) {
		if strings.Contains(ch.New, "accepts point-to-point links") {
			found = true
		}
	}
	if !found {
		t.Fatal("the change is not in the review")
	}
}

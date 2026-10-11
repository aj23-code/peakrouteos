package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func radio24() WifiRadio {
	return WifiRadio{Name: "wlan0", Band: Band24, Channel: 6, Width: 20, HT: true}
}
func radio5() WifiRadio {
	return WifiRadio{Name: "wlan1", Band: Band5, Channel: 36, Width: 80, HT: true, VHT: true}
}

func homeWifi() WifiNetwork {
	return WifiNetwork{ID: "wifi1", SSID: "Home", Network: "lan", Security: SecurityWPA2WPA3, Passphrase: "correct horse battery",
		Bands: []string{Band24, Band5}, BandSteering: true}
}

func withWifi(c NetworkConfig, country string, radios []WifiRadio, nets ...WifiNetwork) NetworkConfig {
	c.Country = country
	c.Wifi = &WifiConfig{Radios: radios, Networks: nets}
	return c
}

func goodWifi() NetworkConfig {
	return withWifi(validNet(), "US", []WifiRadio{radio24(), radio5()}, homeWifi())
}

func TestValidWifi(t *testing.T) {
	guestNet := withNets(validNet(), guest())
	cases := map[string]NetworkConfig{
		"no wifi at all":                   validNet(),
		"an empty section":                 withWifi(validNet(), "", nil),
		"dual band":                        goodWifi(),
		"one band":                         withWifi(validNet(), "DE", []WifiRadio{radio24()}, func() WifiNetwork { n := homeWifi(); n.Bands = []string{Band24}; n.BandSteering = false; return n }()),
		"a guest network":                  withWifi(guestNet, "US", []WifiRadio{radio24()}, WifiNetwork{ID: "wifi2", SSID: "Guest WiFi", Network: "guest", Security: SecurityWPA2, Passphrase: "visitors only!", Bands: []string{Band24}}),
		"automatic channel":                withWifi(validNet(), "US", []WifiRadio{{Name: "wlan0", Band: Band5, Channel: 0, Width: 80, HT: true, VHT: true}}, func() WifiNetwork { n := homeWifi(); n.Bands = []string{Band5}; n.BandSteering = false; return n }()),
		"radios set up before any network": withWifi(validNet(), "US", []WifiRadio{radio24()}),
		"a country alone":                  withWifi(validNet(), "JP", nil),
		"switched off things are not covered": withWifi(validNet(), "US", []WifiRadio{func() WifiRadio { r := radio5(); r.Disabled = true; return r }()},
			func() WifiNetwork { n := homeWifi(); n.Disabled = true; return n }()),
		"wpa3": withWifi(validNet(), "US", []WifiRadio{radio24()}, func() WifiNetwork {
			n := homeWifi()
			n.Security = SecurityWPA3
			n.Bands = []string{Band24}
			n.BandSteering = false
			return n
		}()),
		"hidden and isolated": withWifi(validNet(), "US", []WifiRadio{radio24()}, func() WifiNetwork {
			n := homeWifi()
			n.Hidden, n.ClientIsolation = true, true
			n.Bands = []string{Band24}
			n.BandSteering = false
			return n
		}()),
		"a name with spaces and punctuation inside": withWifi(validNet(), "US", []WifiRadio{radio24()}, func() WifiNetwork {
			n := homeWifi()
			n.SSID = "Ünïcode café #1 - 'x'"
			n.Bands = []string{Band24}
			n.BandSteering = false
			return n
		}()),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInvalidWifi(t *testing.T) {
	mutN := func(f func(*WifiNetwork)) NetworkConfig {
		n := homeWifi()
		f(&n)
		return withWifi(validNet(), "US", []WifiRadio{radio24(), radio5()}, n)
	}
	mutR := func(f func(*WifiRadio)) NetworkConfig {
		r := radio5()
		f(&r)
		return withWifi(validNet(), "US", []WifiRadio{radio24(), r})
	}
	cases := []struct {
		name  string
		cfg   NetworkConfig
		field string
	}{
		{"unknown country", withWifi(validNet(), "ZZ", nil), "country"},
		{"lower-case country", withWifi(validNet(), "us", nil), "country"},
		{"no country while switched on", withWifi(validNet(), "", []WifiRadio{radio24(), radio5()}, homeWifi()), "country"},

		{"bad adapter name", mutR(func(r *WifiRadio) { r.Name = "wlan1;reboot" }), "wifi.radios[1].name"},
		{"adapter listed twice", withWifi(validNet(), "US", []WifiRadio{radio24(), radio24()}), "wifi.radios[1].name"},
		{"adapter listed twice, shown on the first too", withWifi(validNet(), "US", []WifiRadio{radio24(), radio24()}), "wifi.radios[0].name"},
		{"unknown band", mutR(func(r *WifiRadio) { r.Band = "6 GHz" }), "wifi.radios[1].band"},
		{"width the band lacks", mutR(func(r *WifiRadio) { r.Band, r.Channel, r.Width = Band24, 6, 80 }), "wifi.radios[1].width"},
		{"width zero", mutR(func(r *WifiRadio) { r.Width = 0 }), "wifi.radios[1].width"},
		{"channel of the other band", mutR(func(r *WifiRadio) { r.Channel = 6 }), "wifi.radios[1].channel"},
		{"channel 14", mutR(func(r *WifiRadio) { r.Band, r.Channel, r.Width = Band24, 14, 20 }), "wifi.radios[1].channel"},
		{"negative channel", mutR(func(r *WifiRadio) { r.Channel = -1 }), "wifi.radios[1].channel"},
		{"80 MHz on a channel that is in no 80 MHz block", mutR(func(r *WifiRadio) { r.Channel, r.Width = 165, 80 }), "wifi.radios[1].channel"},
		{"160 MHz in the upper block", mutR(func(r *WifiRadio) { r.Channel, r.Width = 149, 160 }), "wifi.radios[1].channel"},
		{"40 MHz on an adapter without 802.11n", mutR(func(r *WifiRadio) { r.Width, r.HT, r.VHT = 40, false, false }), "wifi.radios[1].width"},
		{"80 MHz on an adapter without 802.11ac", mutR(func(r *WifiRadio) { r.VHT = false }), "wifi.radios[1].width"},
		{"too many adapters", withWifi(validNet(), "US", []WifiRadio{{Name: "a", Band: Band24, Width: 20}, {Name: "b", Band: Band24, Width: 20}, {Name: "c", Band: Band24, Width: 20}, {Name: "d", Band: Band24, Width: 20}, {Name: "e", Band: Band24, Width: 20}}), "wifi.radios"},

		{"bad id", mutN(func(n *WifiNetwork) { n.ID = "Wi Fi" }), "wifi.networks[0].id"},
		{"duplicate id", withWifi(validNet(), "US", []WifiRadio{radio24(), radio5()}, homeWifi(), func() WifiNetwork { n := homeWifi(); n.SSID = "Other"; return n }()), "wifi.networks[1].id"},
		{"empty name", mutN(func(n *WifiNetwork) { n.SSID = "" }), "wifi.networks[0].ssid"},
		{"name too long", mutN(func(n *WifiNetwork) { n.SSID = strings.Repeat("a", 33) }), "wifi.networks[0].ssid"},
		{"name with a newline", mutN(func(n *WifiNetwork) { n.SSID = "a\nb" }), "wifi.networks[0].ssid"},
		{"name with a leading space", mutN(func(n *WifiNetwork) { n.SSID = " Home" }), "wifi.networks[0].ssid"},
		{"name with a trailing space", mutN(func(n *WifiNetwork) { n.SSID = "Home " }), "wifi.networks[0].ssid"},
		{"name with a control character", mutN(func(n *WifiNetwork) { n.SSID = "Ho\x01me" }), "wifi.networks[0].ssid"},
		{"name not UTF-8", mutN(func(n *WifiNetwork) { n.SSID = "Ho\xffme" }), "wifi.networks[0].ssid"},
		{"duplicate name", withWifi(validNet(), "US", []WifiRadio{radio24(), radio5()}, homeWifi(), func() WifiNetwork { n := homeWifi(); n.ID = "wifi2"; return n }()), "wifi.networks[1].ssid"},
		{"duplicate name, shown on the first too", withWifi(validNet(), "US", []WifiRadio{radio24(), radio5()}, homeWifi(), func() WifiNetwork { n := homeWifi(); n.ID = "wifi2"; return n }()), "wifi.networks[0].ssid"},
		{"unknown local network", mutN(func(n *WifiNetwork) { n.Network = "nope" }), "wifi.networks[0].network"},
		{"empty local network", mutN(func(n *WifiNetwork) { n.Network = "" }), "wifi.networks[0].network"},
		{"unknown security", mutN(func(n *WifiNetwork) { n.Security = "wep" }), "wifi.networks[0].security"},
		{"open network", mutN(func(n *WifiNetwork) { n.Security = "" }), "wifi.networks[0].security"},
		{"short passphrase", mutN(func(n *WifiNetwork) { n.Passphrase = "short" }), "wifi.networks[0].passphrase"},
		{"long passphrase", mutN(func(n *WifiNetwork) { n.Passphrase = strings.Repeat("a", 64) }), "wifi.networks[0].passphrase"},
		{"passphrase with a newline", mutN(func(n *WifiNetwork) { n.Passphrase = "eight chars\nwpa=0" }), "wifi.networks[0].passphrase"},
		{"passphrase not ASCII", mutN(func(n *WifiNetwork) { n.Passphrase = "pässwörd-long" }), "wifi.networks[0].passphrase"},
		{"no bands", mutN(func(n *WifiNetwork) { n.Bands = nil; n.BandSteering = false }), "wifi.networks[0].bands"},
		{"unknown band", mutN(func(n *WifiNetwork) { n.Bands = []string{"6 GHz"}; n.BandSteering = false }), "wifi.networks[0].bands"},
		{"band twice", mutN(func(n *WifiNetwork) { n.Bands = []string{Band24, Band24}; n.BandSteering = false }), "wifi.networks[0].bands"},
		{"band steering on one band", mutN(func(n *WifiNetwork) { n.Bands = []string{Band5} }), "wifi.networks[0].band_steering"},
		{"no adapter for a band", withWifi(validNet(), "US", []WifiRadio{radio24()}, homeWifi()), "wifi.coverage"},
		{"the adapter for a band is switched off", withWifi(validNet(), "US", []WifiRadio{radio24(), func() WifiRadio { r := radio5(); r.Disabled = true; return r }()}, homeWifi()), "wifi.coverage"},
		{"too many networks", func() NetworkConfig {
			c := validNet()
			c.Country = "US"
			c.Wifi = &WifiConfig{Radios: []WifiRadio{radio24()}}
			for i := 0; i < 17; i++ {
				c.Wifi.Networks = append(c.Wifi.Networks, WifiNetwork{ID: "w" + itoa(i), SSID: "n" + itoa(i), Network: "lan", Security: SecurityWPA2, Passphrase: "password" + itoa(i), Bands: []string{Band24}})
			}
			return c
		}(), "wifi.networks"},
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

func TestAWiFi6AdapterMayUse80MHzWithoutAdvertisingAC(t *testing.T) {
	r := radio5()
	r.VHT, r.HE = false, true
	c := withWifi(validNet(), "US", []WifiRadio{radio24(), r}, homeWifi())
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestAWifiNetworkCanJoinAnAddedLocalNetworkButOnlyOneThatExists(t *testing.T) {
	good := withWifi(withNets(validNet(), guest()), "US", []WifiRadio{radio24()},
		WifiNetwork{ID: "wifi1", SSID: "Guest", Network: "guest", Security: SecurityWPA2, Passphrase: "visitors only!", Bands: []string{Band24}})
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	good.Networks = nil // the local network goes away while the Wi-Fi network still points at it
	if err := good.Validate(); err == nil || !fieldsOf(t, err).Has("wifi.networks[0].network") {
		t.Fatalf("%v", err)
	}
}

// ---- the channel rules ---------------------------------------------------------------------------

func TestChannelRules(t *testing.T) {
	for _, ch := range []int{1, 6, 13} {
		if !ValidChannel(Band24, ch) || HT40Direction(Band24, ch) == "" {
			t.Errorf("2.4 GHz channel %d", ch)
		}
	}
	for _, ch := range []int{0, 14, -1, 36} {
		if ValidChannel(Band24, ch) {
			t.Errorf("%d is not a 2.4 GHz channel", ch)
		}
	}
	for _, ch := range []int{36, 64, 100, 144, 149, 165} {
		if !ValidChannel(Band5, ch) {
			t.Errorf("5 GHz channel %d", ch)
		}
	}
	for _, ch := range []int{0, 1, 35, 68, 96, 148, 169, 177} {
		if ValidChannel(Band5, ch) {
			t.Errorf("%d is not a 5 GHz channel an access point offers", ch)
		}
	}
	if ValidChannel("6 GHz", 5) || ValidWidth("6 GHz", 20) {
		t.Error("6 GHz is not offered")
	}

	widths := map[string][]int{Band24: {20, 40}, Band5: {20, 40, 80, 160}}
	for band, ok := range widths {
		for _, w := range []int{0, 10, 20, 40, 80, 160, 320} {
			want := false
			for _, o := range ok {
				want = want || o == w
			}
			if ValidWidth(band, w) != want {
				t.Errorf("%s %d MHz: %v", band, w, !want)
			}
		}
	}

	// 40 MHz pairs, 80 and 160 MHz blocks
	for ch, want := range map[int]bool{36: true, 40: true, 48: true, 52: true, 100: true, 144: true, 149: true, 161: true, 165: false} {
		if got := ChannelFitsWidth(Band5, ch, 40); got != want {
			t.Errorf("5 GHz channel %d at 40 MHz: %v", ch, got)
		}
	}
	for ch, want := range map[int]bool{36: true, 48: true, 52: true, 64: true, 100: true, 112: true, 116: true, 144: true, 149: true, 161: true, 165: false} {
		if got := ChannelFitsWidth(Band5, ch, 80); got != want {
			t.Errorf("5 GHz channel %d at 80 MHz: %v", ch, got)
		}
	}
	for ch, want := range map[int]bool{36: true, 64: true, 100: true, 128: true, 132: false, 149: false, 165: false} {
		if got := ChannelFitsWidth(Band5, ch, 160); got != want {
			t.Errorf("5 GHz channel %d at 160 MHz: %v", ch, got)
		}
	}
	if !ChannelFitsWidth(Band24, 11, 40) || ChannelFitsWidth(Band24, 11, 80) || ChannelFitsWidth(Band5, 36, 0) {
		t.Error("2.4 GHz widths")
	}

	for _, c := range []struct {
		band string
		ch   int
		want string
	}{{Band24, 1, "+"}, {Band24, 7, "+"}, {Band24, 8, "-"}, {Band24, 13, "-"}, {Band5, 36, "+"}, {Band5, 40, "-"}, {Band5, 149, "+"}, {Band5, 161, "-"}, {Band5, 165, ""}} {
		if got := HT40Direction(c.band, c.ch); got != c.want {
			t.Errorf("%s %d: %q, want %q", c.band, c.ch, got, c.want)
		}
	}
	for _, c := range []struct{ ch, width, want int }{{36, 80, 42}, {48, 80, 42}, {52, 80, 58}, {100, 80, 106}, {132, 80, 138}, {149, 80, 155}, {161, 80, 155}, {36, 160, 50}, {64, 160, 50}, {100, 160, 114}, {165, 80, 0}, {36, 40, 0}, {36, 20, 0}, {149, 160, 0}} {
		if got := CentreChannel(c.ch, c.width); got != c.want {
			t.Errorf("centre of %d at %d MHz: %d, want %d", c.ch, c.width, got, c.want)
		}
	}
}

func TestEveryOfferedChannelFitsItsOwnWidthRules(t *testing.T) {
	// the helpers must agree with each other: a channel that fits 80 MHz has a centre channel, one that
	// fits 40 MHz has a direction
	for _, ch := range channels5 {
		if ChannelFitsWidth(Band5, ch, 80) != (CentreChannel(ch, 80) != 0) {
			t.Errorf("channel %d: 80 MHz fit and centre disagree", ch)
		}
		if ChannelFitsWidth(Band5, ch, 160) != (CentreChannel(ch, 160) != 0) {
			t.Errorf("channel %d: 160 MHz fit and centre disagree", ch)
		}
		if ChannelFitsWidth(Band5, ch, 40) != (HT40Direction(Band5, ch) != "") {
			t.Errorf("channel %d: 40 MHz fit and direction disagree", ch)
		}
	}
}

func TestPassphraseAndNameHelpers(t *testing.T) {
	for in, want := range map[string]bool{"12345678": true, strings.Repeat("a", 63): true, "with spaces in it": true, "!\"#$%&'()*+,-./": true, "1234567": false, strings.Repeat("a", 64): false, "": false, "tab\tchar12": false, "é12345678": false} {
		if ValidPassphrase(in) != want {
			t.Errorf("passphrase %q: %v", in, !want)
		}
	}
	for in, want := range map[string]bool{"A": true, strings.Repeat("a", 32): true, "Café": true, "": false, strings.Repeat("a", 33): false, "x ": false, " x": false, "a\x00b": false, "a\x7fb": false} {
		if ValidSSID(in) != want {
			t.Errorf("ssid %q: %v", in, !want)
		}
	}
	if len("日本語日本語日本語日本") <= 32 && !ValidSSID("日本語日本語日本語日本") {
		t.Error("32 bytes or fewer of UTF-8 text is fine")
	}
	if ValidSSID(strings.Repeat("é", 17)) {
		t.Error("a name is limited to 32 BYTES, and 17 two-byte characters are 34")
	}
}

func TestCountries(t *testing.T) {
	if len(Countries) != 249 || !IsCountry("US") || !IsCountry("DE") || IsCountry("ZZ") || IsCountry("") || IsCountry("us") || IsCountry("USA") {
		t.Fatalf("%d countries", len(Countries))
	}
	for i := 1; i < len(Countries); i++ {
		if Countries[i-1].Name > Countries[i].Name {
			t.Fatalf("not sorted by name at %s", Countries[i].Name)
		}
	}
}

// ---- model --------------------------------------------------------------------------------------

func TestWifiJSONAndClone(t *testing.T) {
	a := goodWifi()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var out NetworkConfig
	if err := json.Unmarshal(b, &out); err != nil || out.Wifi == nil || len(out.Wifi.Radios) != 2 || out.Wifi.Networks[0].SSID != "Home" || !out.Wifi.Networks[0].BandSteering || out.Country != "US" {
		t.Fatalf("%v %+v", err, out.Wifi)
	}
	if plain, _ := json.Marshal(validNet()); strings.Contains(string(plain), "wifi") {
		t.Errorf("a configuration without Wi-Fi writes nothing about it: %s", plain)
	}

	c := a.clone()
	c.Wifi.Radios[0].Channel = 11
	c.Wifi.Networks[0].Bands[0] = "x"
	c.Wifi.Networks[0].SSID = "changed"
	if a.Wifi.Radios[0].Channel != 6 || a.Wifi.Networks[0].Bands[0] != Band24 || a.Wifi.Networks[0].SSID != "Home" {
		t.Fatalf("the copy shares its Wi-Fi settings with the original: %+v", a.Wifi)
	}
	if validNet().clone().Wifi != nil {
		t.Error("a copy of a configuration without Wi-Fi gained some")
	}

	if len((NetworkConfig{}).WifiSettings().Networks) != 0 || len(WifiConfig{}.Networks) != 0 {
		t.Error("an absent section reads as empty")
	}
	if n, ok := a.Wifi.FindNetwork("wifi1"); !ok || !n.Has(Band5) || n.Has("6 GHz") {
		t.Error("FindNetwork and Has")
	}
	r, n := goodWifi().Wifi.Active()
	if len(r) != 2 || len(n) != 1 {
		t.Fatalf("%v %v", r, n)
	}
	off := *a.Wifi
	off.Radios = []WifiRadio{func() WifiRadio { x := radio24(); x.Disabled = true; return x }(), radio5()}
	off.Networks = []WifiNetwork{func() WifiNetwork { x := homeWifi(); x.Disabled = true; return x }()}
	if r, n := off.Active(); len(r) != 1 || r[0].Name != "wlan1" || len(n) != 0 {
		t.Fatalf("%v %v", r, n)
	}
}

func TestTheNetworkFileIsNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.json")
	if err := SaveNetworkConfig(path, func() *NetworkConfig { c := goodWifi(); return &c }()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v, err %v: the file holds Wi-Fi passphrases", info.Mode().Perm(), err)
	}
}

// ---- diff -------------------------------------------------------------------------------------------

func wifiChanges(a, b NetworkConfig) []Change {
	var out []Change
	for _, c := range DiffNetwork(a, b) {
		if strings.HasPrefix(c.Field, "wifi_") {
			out = append(out, c)
		}
	}
	return out
}

func TestWifiDiff(t *testing.T) {
	base := validNet()

	if got := wifiChanges(base, goodWifi()); len(got) != 3 { // two adapters, one network
		t.Fatalf("%+v", got)
	}
	if got := wifiChanges(goodWifi(), goodWifi().clone()); len(got) != 0 {
		t.Fatalf("%+v", got)
	}

	// the country is a router-wide setting: it is reviewed as "Country", not as part of Wi-Fi
	de := goodWifi()
	de.Country = "DE"
	if got := wifiChanges(goodWifi(), de); len(got) != 0 {
		t.Fatalf("a country change is not a Wi-Fi line: %+v", got)
	}
	ch := DiffNetwork(goodWifi(), de)
	if len(ch) != 1 || ch[0].Field != "country" || ch[0].Label != "Country" || ch[0].Old != "US" || ch[0].New != "DE" {
		t.Fatalf("%+v", ch)
	}
	if ch := DiffNetwork(validNet(), withCountry(validNet(), "US")); len(ch) != 1 || ch[0].Old != valueNone || ch[0].New != "US" {
		t.Fatalf("%+v", ch)
	}

	// an adapter
	r := goodWifi()
	r.Wifi.Radios[1].Channel = 0
	r.Wifi.Radios[1].Width = 40
	ch = wifiChanges(goodWifi(), r)
	if len(ch) != 1 || ch[0].Label != "Wi-Fi adapter wlan1" || ch[0].Old != "5 GHz, channel 36, 80 MHz" || ch[0].New != "5 GHz, automatic channel, 40 MHz" {
		t.Fatalf("%+v", ch)
	}
	r.Wifi.Radios[1].Disabled = true
	if ch = wifiChanges(goodWifi(), r); !strings.Contains(ch[0].New, "switched off") {
		t.Fatalf("%+v", ch)
	}
	removed := goodWifi()
	removed.Wifi.Radios = removed.Wifi.Radios[:1]
	if ch = wifiChanges(goodWifi(), removed); len(ch) != 1 || ch[0].New != "(removed)" {
		t.Fatalf("%+v", ch)
	}

	// a Wi-Fi network: added, with the local network named
	nets := withNets(goodWifi(), guest())
	nets.Wifi.Networks[0].Network = "guest"
	ch = wifiChanges(withNets(goodWifi(), guest()), nets)
	if len(ch) != 1 || ch[0].Label != "Wi-Fi network Home" || !strings.Contains(ch[0].Old, "joins LAN") || !strings.Contains(ch[0].New, "joins Guest") {
		t.Fatalf("%+v", ch)
	}
	added := wifiChanges(withWifi(validNet(), "US", []WifiRadio{radio24(), radio5()}), goodWifi())
	if len(added) != 1 || added[0].Old != "(none)" || added[0].New != "joins LAN, WPA2/WPA3, 2.4 GHz + 5 GHz, band steering, password set" {
		t.Fatalf("%+v", added)
	}
	// rename, options, removal
	n := goodWifi()
	n.Wifi.Networks[0].SSID = "Casa"
	n.Wifi.Networks[0].Hidden, n.Wifi.Networks[0].ClientIsolation, n.Wifi.Networks[0].BandSteering = true, true, false
	ch = wifiChanges(goodWifi(), n)
	if len(ch) != 1 || ch[0].Label != "Wi-Fi network Home (now Casa)" || !strings.Contains(ch[0].New, "hidden") || !strings.Contains(ch[0].New, "devices isolated from each other") || strings.Contains(ch[0].New, "band steering") {
		t.Fatalf("%+v", ch)
	}
	gone := goodWifi()
	gone.Wifi.Networks = nil
	if ch = wifiChanges(goodWifi(), gone); len(ch) != 1 || ch[0].New != "(removed)" {
		t.Fatalf("%+v", ch)
	}
}

func TestThePasswordNeverAppearsInTheReview(t *testing.T) {
	changed := goodWifi()
	changed.Wifi.Networks[0].Passphrase = "a brand new secret!"
	ch := wifiChanges(goodWifi(), changed)
	if len(ch) != 1 || !strings.HasSuffix(ch[0].New, ", new password") || ch[0].Old != strings.TrimSuffix(ch[0].New, ", new password") {
		t.Fatalf("a changed password is reported as such: %+v", ch)
	}
	all, _ := json.Marshal(DiffNetwork(validNet(), changed))
	for _, secret := range []string{"a brand new secret!", "correct horse battery"} {
		if strings.Contains(string(all), secret) {
			t.Fatalf("the review contains a passphrase: %s", all)
		}
	}
	old, _ := json.Marshal(DiffNetwork(goodWifi(), changed))
	if strings.Contains(string(old), "correct horse battery") || strings.Contains(string(old), "brand new secret") {
		t.Fatalf("%s", old)
	}
}

func TestALongAdapterNameCanCarryOnlyOneNetwork(t *testing.T) {
	long := radio24()
	long.Name = "wlx00e04c680001" // 15 characters: fine alone, no room for the _1 of a second network
	one := withWifi(validNet(), "US", []WifiRadio{long}, func() WifiNetwork { n := homeWifi(); n.Bands = []string{Band24}; n.BandSteering = false; return n }())
	if err := one.Validate(); err != nil {
		t.Fatal(err)
	}
	two := one.clone()
	two.Wifi.Networks = append(two.Wifi.Networks, WifiNetwork{ID: "wifi2", SSID: "Guest", Network: "lan", Security: SecurityWPA2, Passphrase: "visitors only!", Bands: []string{Band24}})
	err := two.Validate()
	if err == nil || !fieldsOf(t, err).Has("wifi.radios[0].name") {
		t.Fatalf("%v", err)
	}
	// a short name is fine with many
	short := two.clone()
	short.Wifi.Radios[0].Name = "wlan0"
	if err := short.Validate(); err != nil {
		t.Fatal(err)
	}
	// a switched-off adapter is not checked
	off := two.clone()
	off.Wifi.Radios[0].Disabled = true
	for _, e := range func() ValidationError { e := off.Validate(); v, _ := e.(ValidationError); return v }() {
		if strings.Contains(e.Field, ".name") {
			t.Errorf("%v", e)
		}
	}
}

func withCountry(c NetworkConfig, country string) NetworkConfig {
	c.Country = country
	return c
}

func TestTheCountryIsValidatedWithoutAnyWifi(t *testing.T) {
	if err := withCountry(validNet(), "DE").Validate(); err != nil {
		t.Fatal(err)
	}
	if err := withCountry(validNet(), "").Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ZZ", "de", "DEU", "1"} {
		err := withCountry(validNet(), bad).Validate()
		var ve ValidationError
		if !errors.As(err, &ve) || len(ve) != 1 || ve[0].Field != "country" {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

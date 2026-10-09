package wireless

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/intel-ax-phy-info.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func band(t *testing.T, p Phy, name string) Band {
	t.Helper()
	for _, b := range p.Bands {
		if b.Name == name {
			return b
		}
	}
	t.Fatalf("no %s band in %+v", name, p.Bands)
	return Band{}
}

func channel(t *testing.T, b Band, n int) Channel {
	t.Helper()
	for _, c := range b.Channels {
		if c.Number == n {
			return c
		}
	}
	t.Fatalf("no channel %d in the %s band", n, b.Name)
	return Channel{}
}

func ints(in []int) string {
	var s []string
	for _, i := range in {
		s = append(s, itoa(i))
	}
	return strings.Join(s, ",")
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestParsingARealAdapter(t *testing.T) {
	phy := ParsePhyInfo(fixture(t))
	if !phy.CanAP() || strings.Join(phy.Modes, ",") != "IBSS,managed,AP,AP/VLAN,monitor,P2P-client,P2P-GO,P2P-device" {
		t.Fatalf("modes %v", phy.Modes)
	}
	if len(phy.Bands) != 2 {
		t.Fatalf("bands %+v", phy.Bands)
	}

	b24 := band(t, phy, "2.4 GHz")
	if len(b24.Channels) != 14 || ints(b24.Widths) != "20,40" {
		t.Fatalf("2.4 GHz: %d channels, widths %v", len(b24.Channels), b24.Widths)
	}
	if c := channel(t, b24, 6); c.MHz != 2437 || !c.Usable() || c.DFS {
		t.Errorf("channel 6: %+v", c)
	}
	if c := channel(t, b24, 14); !c.Disabled || c.Usable() {
		t.Errorf("channel 14 is not allowed anywhere but Japan: %+v", c)
	}
	usable := 0
	for _, c := range b24.Channels {
		if c.Usable() {
			usable++
		}
	}
	if usable != 13 {
		t.Errorf("usable 2.4 GHz channels: %d", usable)
	}

	b5 := band(t, phy, "5 GHz")
	if len(b5.Channels) != 37 || ints(b5.Widths) != "20,40,80" {
		t.Fatalf("5 GHz: %d channels, widths %v (VHT, but 'neither 160 nor 80+80')", len(b5.Channels), b5.Widths)
	}
	if c := channel(t, b5, 36); c.MHz != 5180 || !c.NoIR || c.Usable() || c.DFS {
		t.Errorf("channel 36: %+v", c)
	}
	if c := channel(t, b5, 100); !c.NoIR || !c.DFS {
		t.Errorf("channel 100 needs radar detection: %+v", c)
	}
	if c := channel(t, b5, 68); !c.Disabled {
		t.Errorf("channel 68: %+v", c)
	}
	for _, c := range b5.Channels { // this machine has no country set, so no 5 GHz channel can host an access point
		if c.Usable() {
			t.Errorf("channel %d is usable in the world domain", c.Number)
		}
	}
}

func TestParsingVariants(t *testing.T) {
	const he6 = "Wiphy phy1\n\tSupported interface modes:\n\t\t * managed\n\t\t * AP\n" +
		"\tBand 1:\n\t\tCapabilities: 0x1\n\t\t\tHT20/HT40\n\t\tFrequencies:\n\t\t\t* 2412.0 MHz [1] (20.0 dBm)\n" +
		"\tBand 2:\n\t\tVHT Capabilities (0x1):\n\t\t\tSupported Channel Width: 160 MHz, 80+80 MHz\n\t\tFrequencies:\n\t\t\t* 5180.0 MHz [36] (23.0 dBm)\n\t\t\t* 5260.0 MHz [52] (23.0 dBm) (radar detection)\n" +
		"\tBand 4:\n\t\tIftypes: AP\n\t\tHE Iftypes: AP\n\t\t\tHE PHY Capabilities: (0x0)\n\t\t\t\tHE40/HE80/5GHz\n\t\tFrequencies:\n\t\t\t* 5955.0 MHz [1] (24.0 dBm)\n\t\t\t* 5975.0 MHz [5] (24.0 dBm)\n" +
		"\tBand 3:\n\t\tFrequencies:\n\t\t\t* 58320.0 MHz [1] (40.0 dBm)\n" +
		"\tSupported commands:\n\t\t * start_ap\n"
	phy := ParsePhyInfo(he6)
	if len(phy.Bands) != 3 {
		t.Fatalf("the 60 GHz band should be left out: %+v", phy.Bands)
	}
	if got := ints(band(t, phy, "5 GHz").Widths); got != "20,80,160" {
		t.Errorf("5 GHz widths %s (no HT40 here, VHT with 160)", got)
	}
	if c := channel(t, band(t, phy, "5 GHz"), 52); !c.DFS || !c.Usable() {
		t.Errorf("a DFS channel can host an access point after listening for radar: %+v", c)
	}
	if got := ints(band(t, phy, "6 GHz").Widths); got != "20,80" {
		t.Errorf("6 GHz widths %s", got)
	}
	if got := ints(band(t, phy, "2.4 GHz").Widths); got != "20,40" {
		t.Errorf("2.4 GHz widths %s", got)
	}

	// an adapter that cannot be an access point
	noAP := ParsePhyInfo("Wiphy phy0\n\tSupported interface modes:\n\t\t * managed\n\t\t * monitor\n\tBand 1:\n\t\tFrequencies:\n\t\t\t* 2412.0 MHz [1] (20.0 dBm)\n")
	if noAP.CanAP() || len(noAP.Modes) != 2 {
		t.Errorf("%+v", noAP)
	}

	// nothing useful in, nothing out
	for _, in := range []string{"", "garbage\nmore garbage", "Wiphy phy0\n"} {
		if p := ParsePhyInfo(in); p.CanAP() || len(p.Bands) != 0 || len(p.Modes) != 0 {
			t.Errorf("%q gave %+v", in, p)
		}
	}
}

func TestParseCountry(t *testing.T) {
	for in, want := range map[string]string{
		"global\ncountry 00: DFS-UNSET\n\t(755 - 928 @ 2)\n":             "00",
		"global\ncountry US: DFS-FCC\n\t(2402 - 2472 @ 40)\n":            "US",
		"global\ncountry DE: DFS-ETSI\n\nphy#0\ncountry 99: DFS-UNSET\n": "DE", // the first one is the global one
	} {
		if got, ok := ParseCountry(in); !ok || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "no country here", "country : x", "country USA: x"} {
		if got, ok := ParseCountry(in); ok {
			t.Errorf("%q gave %q", in, got)
		}
	}
}

// ---- detection -------------------------------------------------------------------------------

// fakeSys builds a /sys/class/net with a wired port and the given wireless interfaces
func fakeSys(t *testing.T, wireless map[string]string) string {
	t.Helper()
	root := t.TempDir()
	mk := func(p string) { os.MkdirAll(filepath.Join(root, p), 0755) }
	write := func(p, s string) { os.WriteFile(filepath.Join(root, p), []byte(s), 0644) }

	mk("eth0")
	write("eth0/address", "aa:bb:cc:00:00:01\n")
	write("eth0/operstate", "up\n")

	for name, phy := range wireless {
		mk(name + "/wireless")
		mk("drivers/iwlwifi")
		mk("ieee80211/" + phy)
		write(name+"/address", "f8:94:c2:00:00:01\n")
		write(name+"/operstate", "down\n")
		os.Symlink("../../ieee80211/"+phy, filepath.Join(root, name, "phy80211"))
		mk(name + "/device")
		os.Symlink("../../../drivers/iwlwifi", filepath.Join(root, name, "device", "driver"))
	}
	return root
}

func runnerWith(fixtureText string, calls *[]string) Runner {
	return func(ctx context.Context, name string, args ...string) (string, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		if len(args) >= 2 && args[0] == "reg" {
			return "global\ncountry US: DFS-FCC\n", nil
		}
		return fixtureText, nil
	}
}

func TestNoAdapterIsReportedAsNone(t *testing.T) {
	rep := Detector{SysClassNet: fakeSys(t, nil), Run: func(context.Context, string, ...string) (string, error) {
		t.Fatal("nothing should be run when there is no adapter")
		return "", nil
	}}.Detect(context.Background())
	if rep.Adapters == nil || len(rep.Adapters) != 0 || rep.IWAvailable || rep.CountryKnown {
		t.Fatalf("%+v", rep)
	}
}

func TestAMissingDirectoryIsNoAdapter(t *testing.T) {
	rep := Detector{SysClassNet: filepath.Join(t.TempDir(), "nope")}.Detect(context.Background())
	if rep.Adapters == nil || len(rep.Adapters) != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestAdaptersAreFoundWithTheirCapabilities(t *testing.T) {
	var calls []string
	rep := Detector{SysClassNet: fakeSys(t, map[string]string{"wlp2s0": "phy0", "wlan1": "phy1"}), Run: runnerWith(fixture(t), &calls)}.Detect(context.Background())

	if len(rep.Adapters) != 2 || rep.Adapters[0].Name != "wlan1" || rep.Adapters[1].Name != "wlp2s0" {
		t.Fatalf("a wired port must not count, and the order is by name: %+v", rep.Adapters)
	}
	a := rep.Adapters[1]
	if a.Phy != "phy0" || a.MAC != "f8:94:c2:00:00:01" || a.Driver != "iwlwifi" || a.State != "down" {
		t.Errorf("%+v", a)
	}
	if !a.CapabilitiesKnown || !a.APCapable || len(a.Bands) != 2 || len(a.Modes) == 0 {
		t.Errorf("%+v", a)
	}
	if !rep.IWAvailable || !rep.CountryKnown || rep.Country != "US" {
		t.Errorf("%+v", rep)
	}
	if strings.Join(calls, "|") != "iw phy phy1 info|iw phy phy0 info|iw reg get" {
		t.Errorf("commands run: %v", calls)
	}
}

func TestWithoutIwTheAdaptersAreStillListed(t *testing.T) {
	rep := Detector{SysClassNet: fakeSys(t, map[string]string{"wlan0": "phy0"}),
		Run: func(context.Context, string, ...string) (string, error) {
			return "", errors.New("executable file not found")
		}}.Detect(context.Background())
	if len(rep.Adapters) != 1 || rep.IWAvailable || rep.CountryKnown {
		t.Fatalf("%+v", rep)
	}
	a := rep.Adapters[0]
	if a.CapabilitiesKnown || a.APCapable || a.Modes == nil || a.Bands == nil || len(a.Bands) != 0 {
		t.Fatalf("an adapter whose capabilities could not be read must say so: %+v", a)
	}
	if rep2 := (Detector{SysClassNet: fakeSys(t, map[string]string{"wlan0": "phy0"})}).Detect(context.Background()); len(rep2.Adapters) != 1 || rep2.IWAvailable {
		t.Fatalf("no runner at all: %+v", rep2)
	}
}

func TestAnInterfaceWithOnlyAPhyLinkIsWirelessToo(t *testing.T) {
	root := fakeSys(t, nil)
	os.MkdirAll(filepath.Join(root, "wlx1", "x"), 0755)
	os.Symlink("../../ieee80211/phy5", filepath.Join(root, "wlx1", "phy80211"))
	rep := Detector{SysClassNet: root}.Detect(context.Background())
	if len(rep.Adapters) != 1 || rep.Adapters[0].Name != "wlx1" || rep.Adapters[0].Phy != "phy5" {
		t.Fatalf("%+v", rep.Adapters)
	}
}

func TestTheHandler(t *testing.T) {
	var calls []string
	d := Detector{SysClassNet: fakeSys(t, nil), Run: runnerWith("", &calls)}
	rec := httptest.NewRecorder()
	d.Handler(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), `"adapters":[]`) {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}

	d.SysClassNet = fakeSys(t, map[string]string{"wlan0": "phy0"})
	d.Run = runnerWith(fixture(t), &calls)
	rec = httptest.NewRecorder()
	d.Handler(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	var rep Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil || len(rep.Adapters) != 1 || !rep.Adapters[0].APCapable {
		t.Fatalf("%v %s", err, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"channels"`) || !strings.Contains(rec.Body.String(), `"no_ir":true`) {
		t.Errorf("%s", rec.Body.String()[:200])
	}

	rec = httptest.NewRecorder()
	d.Handler(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST %d", rec.Code)
	}
}

func TestMoreParsingEdges(t *testing.T) {
	// widths above 40 MHz are not a thing in the 2.4 GHz band, whatever the adapter claims
	p := ParsePhyInfo("\tBand 1:\n\t\tHT20/HT40\n\t\tVHT Capabilities (0x1):\n\t\t\tSupported Channel Width: neither 160 nor 80+80\n\t\tFrequencies:\n\t\t\t* 2412.0 MHz [1] (20.0 dBm)\n")
	if got := ints(band(t, p, "2.4 GHz").Widths); got != "20,40" {
		t.Errorf("2.4 GHz widths %s", got)
	}
	// a 5 GHz band is named by its channels wherever they start (some adapters list only the upper part)
	p = ParsePhyInfo("\tBand 2:\n\t\tFrequencies:\n\t\t\t* 5500.0 MHz [100] (20.0 dBm) (radar detection)\n")
	if len(p.Bands) != 1 || p.Bands[0].Name != "5 GHz" {
		t.Errorf("%+v", p.Bands)
	}
	// an HE-only band names its widest widths in the PHY capabilities
	p = ParsePhyInfo("\tBand 2:\n\t\tHE Iftypes: AP\n\t\t\tHE PHY Capabilities: (0x0)\n\t\t\t\tHE40/HE80/5GHz\n\t\t\t\tHE160/5GHz\n\t\tFrequencies:\n\t\t\t* 5180.0 MHz [36] (20.0 dBm)\n")
	if got := ints(band(t, p, "5 GHz").Widths); got != "20,80,160" {
		t.Errorf("widths %s", got)
	}
}

func TestDetectionEdges(t *testing.T) {
	// an adapter that is an access point candidate or not follows what iw says about it
	noAP := "Wiphy phy0\n\tSupported interface modes:\n\t\t * managed\n"
	var calls []string
	rep := Detector{SysClassNet: fakeSys(t, map[string]string{"wlan0": "phy0"}), Run: runnerWith(noAP, &calls)}.Detect(context.Background())
	if a := rep.Adapters[0]; !a.CapabilitiesKnown || a.APCapable {
		t.Errorf("%+v", a)
	}

	// the capabilities can be read even when the country cannot
	rep = Detector{SysClassNet: fakeSys(t, map[string]string{"wlan0": "phy0"}), Run: func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] == "reg" {
			return "", errors.New("not available")
		}
		return fixture(t), nil
	}}.Detect(context.Background())
	if !rep.IWAvailable || rep.CountryKnown || !rep.Adapters[0].CapabilitiesKnown {
		t.Errorf("%+v", rep)
	}

	// an interface with a wireless side but no radio to ask about is listed and never asked about
	root := fakeSys(t, nil)
	os.MkdirAll(filepath.Join(root, "wlan9", "wireless"), 0755)
	calls = nil
	rep = Detector{SysClassNet: root, Run: runnerWith(fixture(t), &calls)}.Detect(context.Background())
	if len(rep.Adapters) != 1 || rep.Adapters[0].Phy != "" {
		t.Fatalf("%+v", rep.Adapters)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "iw phy") {
			t.Errorf("asked about a radio that does not exist: %q", c)
		}
	}
}

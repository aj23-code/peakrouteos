package wireless

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseDevFindsInterfacesModesNamesAndChannels(t *testing.T) {
	got := ParseDev(readFixture(t, "iw-dev-ap.txt"))
	if len(got) != 4 {
		t.Fatalf("%+v", got)
	}
	want := []Interface{
		{Name: "wlan1_1", SSID: "Guest", Type: "AP", Channel: 36, MHz: 5180},
		{Name: "wlan1", SSID: "Home", Type: "AP", Channel: 36, MHz: 5180},
		{Name: "wlan0", SSID: "Home", Type: "AP", Channel: 6, MHz: 2437},
		{Name: "wlan9", Type: "managed"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestANetworkNameWithSpacesIsKeptWhole(t *testing.T) {
	got := ParseDev("phy#0\n\tInterface wlan0\n\t\tssid Leberito Stand  \n\t\ttype AP\n")
	if len(got) != 1 || got[0].SSID != "Leberito Stand" {
		t.Fatalf("%+v", got)
	}
}

func TestAnInterfaceWithoutANameDoesNotChangeTheOneAboveIt(t *testing.T) {
	got := ParseDev("phy#0\n\tInterface wlan0\n\t\tssid Home\n\t\ttype AP\n\tUnnamed/non-netdev interface\n\t\twdev 0x8\n\t\ttype P2P-device\n\t\tssid Other\n")
	if len(got) != 1 || got[0].Type != "AP" || got[0].SSID != "Home" {
		t.Fatalf("%+v", got)
	}
}

func TestParseStationsReadsWhatAConnectedDeviceReports(t *testing.T) {
	got := ParseStations(readFixture(t, "iw-station-dump.txt"))
	if len(got) != 2 {
		t.Fatalf("the device that is still joining must be left out: %+v", got)
	}
	a, b := got[0], got[1]
	if a.MAC != "0e:ea:14:4f:9c:56" || a.Interface != "wlan1" || a.Signal == nil || *a.Signal != -44 ||
		a.RxBytes != 15016565 || a.TxBytes != 29808060 || a.RxMbps != 433.3 || a.TxMbps != 866.7 ||
		a.Connected != 7239 || a.InactiveS != 0.041 {
		t.Fatalf("%+v", a)
	}
	if b.MAC != "aa:bb:cc:00:00:02" || *b.Signal != -71 || b.TxMbps != 6.0 || b.RxMbps != 0 || b.Connected != 12 || b.InactiveS != 3.5 {
		t.Fatalf("%+v", b)
	}
}

func TestParseStationsOfNothing(t *testing.T) {
	if got := ParseStations(""); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestBandOfMHz(t *testing.T) {
	for mhz, want := range map[int]string{2412: "2.4 GHz", 2484: "2.4 GHz", 5180: "5 GHz", 5825: "5 GHz", 5955: "6 GHz", 6415: "6 GHz", 7115: "6 GHz", 7126: "", 0: "", 900: ""} {
		if got := BandOfMHz(mhz); got != want {
			t.Errorf("%d: %q, want %q", mhz, got, want)
		}
	}
}

// sysfs with one Wi-Fi adapter
func sysWithWifi(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wlan0", "wireless"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeIW(t *testing.T, calls *[]string, failDev bool) Runner {
	dump := readFixture(t, "iw-station-dump.txt")
	return func(ctx context.Context, name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		*calls = append(*calls, line)
		switch line {
		case "iw dev":
			if failDev {
				return "", errors.New("no iw")
			}
			return readFixture(t, "iw-dev-ap.txt"), nil
		case "iw dev wlan1 station dump":
			return dump, nil
		case "iw dev wlan1_1 station dump":
			return "Station ff:aa:00:00:00:09 (on wlan1_1)\n\tauthorized:\tyes\n\tsignal:  \t-60 dBm\n", nil
		}
		return "", errors.New("unexpected " + line)
	}
}

func TestStationsAreLabelledWithTheirNetworkAndBand(t *testing.T) {
	var calls []string
	rep := Detector{SysClassNet: sysWithWifi(t), Run: fakeIW(t, &calls, false)}.Stations(context.Background())
	if !rep.Known || len(rep.Stations) != 3 {
		t.Fatalf("%+v", rep)
	}
	// sorted by network name, then address
	got := []string{}
	for _, s := range rep.Stations {
		got = append(got, s.SSID+"|"+s.Band+"|"+s.Interface+"|"+s.MAC)
	}
	want := []string{
		"Guest|5 GHz|wlan1_1|ff:aa:00:00:00:09",
		"Home|5 GHz|wlan1|0e:ea:14:4f:9c:56",
		"Home|5 GHz|wlan1|aa:bb:cc:00:00:02",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %s, want %s", i, got[i], want[i])
		}
	}
	if rep.Stations[1].Channel != 36 {
		t.Fatalf("%+v", rep.Stations[1])
	}
	// wlan0 is an access point too (its dump fails here: skipped), wlan9 is not one: never asked
	for _, c := range calls {
		if strings.Contains(c, "wlan9") {
			t.Fatalf("asked about an interface that is not an access point: %s", c)
		}
	}
}

func TestStationsAreUnknownWithoutIw(t *testing.T) {
	var calls []string
	rep := Detector{SysClassNet: sysWithWifi(t), Run: fakeIW(t, &calls, true)}.Stations(context.Background())
	if rep.Known || rep.Stations == nil || len(rep.Stations) != 0 {
		t.Fatalf("%+v", rep)
	}
	if rep = (Detector{SysClassNet: sysWithWifi(t)}).Stations(context.Background()); rep.Known {
		t.Fatalf("no runner: %+v", rep)
	}
}

func TestWithoutWifiAdaptersNobodyIsConnectedOverWifiAndThatIsKnown(t *testing.T) {
	var calls []string
	rep := Detector{SysClassNet: t.TempDir(), Run: fakeIW(t, &calls, true)}.Stations(context.Background())
	if !rep.Known || len(rep.Stations) != 0 || len(calls) != 0 {
		t.Fatalf("%+v %v", rep, calls)
	}
}

func TestStationCacheKeepsTheAnswerForTheTTL(t *testing.T) {
	var calls []string
	now := time.Unix(1000, 0)
	c := &StationCache{Detector: Detector{SysClassNet: sysWithWifi(t), Run: fakeIW(t, &calls, false)}, TTL: 2 * time.Second, Now: func() time.Time { return now }}
	c.Get(context.Background())
	first := len(calls)
	now = now.Add(time.Second)
	c.Get(context.Background())
	if len(calls) != first {
		t.Fatal("asked again within the TTL")
	}
	now = now.Add(time.Second) // exactly the TTL: stale
	c.Get(context.Background())
	if len(calls) != 2*first {
		t.Fatalf("%d calls, want %d", len(calls), 2*first)
	}
}

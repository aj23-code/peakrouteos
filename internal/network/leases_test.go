package network

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"peakroute/internal/config"
)

const leaseFile = `1999999999 aa:bb:cc:00:00:01 192.168.1.10 laptop *
1999999999 AA:BB:CC:00:00:02 192.168.1.11 phone *
1999999999 aa:bb:cc:00:00:03 192.168.1.12 * *
`

func leaseFileAt(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leases")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func serve(t *testing.T, paths []string, annotate func(context.Context, []ClientLease)) (string, []ClientLease) {
	t.Helper()
	rec := httptest.NewRecorder()
	leaseHandler(paths, annotate)(rec, httptest.NewRequest("GET", "/x", nil))
	var out []ClientLease
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v\n%s", err, rec.Body)
	}
	return rec.Body.String(), out
}

func TestLeasesAreAnnotatedByTheGivenFunction(t *testing.T) {
	path := leaseFileAt(t, leaseFile)
	_, got := serve(t, []string{filepath.Join(t.TempDir(), "missing"), path}, func(_ context.Context, ls []ClientLease) {
		for i := range ls {
			ls[i].Status, ls[i].Connection = "online", "wired"
			if ls[i].MACAddress == "aa:bb:cc:00:00:02" { // MACs are lower case by now
				ls[i].Connection, ls[i].SSID, ls[i].Band, ls[i].LastSeen = "wifi", "Home", "5 GHz", 42
			}
		}
	})
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	l := got[1]
	if l.Hostname != "phone" || l.Connection != "wifi" || l.SSID != "Home" || l.Band != "5 GHz" || l.Status != "online" || l.LastSeen != 42 {
		t.Fatalf("%+v", l)
	}
	if got[0].Connection != "wired" || got[2].Hostname != "Unknown Device" {
		t.Fatalf("%+v", got)
	}
}

func TestWithoutAnAnnotatorEverythingIsUnknown(t *testing.T) {
	_, got := serve(t, []string{leaseFileAt(t, leaseFile)}, nil)
	for _, l := range got {
		if l.Connection != "unknown" || l.Status != "unknown" {
			t.Fatalf("%+v", l)
		}
	}
}

func TestNoLeaseFileOrAnEmptyOneGivesAnEmptyListNotNull(t *testing.T) {
	for name, paths := range map[string][]string{
		"missing": {filepath.Join(t.TempDir(), "missing")},
		"empty":   {leaseFileAt(t, "")},
	} {
		if body, _ := serve(t, paths, nil); body != "[]\n" {
			t.Errorf("%s: %q", name, body)
		}
	}
}

func TestLeasesSayWhichLocalNetworkTheyAreOn(t *testing.T) {
	cfg := config.NetworkConfig{
		LanIP: "192.168.1.1", LanSubnet: "255.255.255.0",
		Networks: []config.Network{
			{ID: "net2", Name: "IoT", IP: "10.20.0.1", Netmask: "255.255.255.0"},
			{ID: "net3", Name: "Guest", IP: "172.16.5.1", Netmask: "255.255.252.0"},
		},
	}
	leases := []ClientLease{
		{IPAddress: "192.168.1.119"}, {IPAddress: "10.20.0.44"}, {IPAddress: "172.16.7.200"},
		{IPAddress: "172.16.8.1"}, {IPAddress: "not an address"},
	}
	LabelNetworks(leases, cfg)
	var got []string
	for _, l := range leases {
		got = append(got, l.Network)
	}
	if want := []string{"LAN", "IoT", "Guest", "", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("networks %q, want %q", got, want)
	}
}

func TestLeasesSayWhetherTheDeviceHasAReservedAddress(t *testing.T) {
	cfg := config.NetworkConfig{
		LanIP: "192.168.1.1", LanSubnet: "255.255.255.0",
		Reservations: []config.Reservation{{MAC: "f8:94:c2:a4:13:a7", IP: "192.168.1.50"}},
	}
	leases := []ClientLease{
		{MACAddress: "F8:94:C2:A4:13:A7", IPAddress: "192.168.1.119"}, // still holds a dynamic address
		{MACAddress: "48:b0:2d:f1:fe:ca", IPAddress: "192.168.1.120"},
	}
	LabelNetworks(leases, cfg)
	if !leases[0].Reserved || leases[0].Reservation != "192.168.1.50" || leases[1].Reserved || leases[1].Reservation != "" {
		t.Errorf("%+v", leases)
	}
}

func TestLeasesSayWhatTheDraftChangesAboutAReservation(t *testing.T) {
	r := func(mac, ip string) config.Reservation { return config.Reservation{MAC: mac, IP: ip} }
	active := config.NetworkConfig{Reservations: []config.Reservation{r("aa:00:00:00:00:01", "192.168.1.10"), r("aa:00:00:00:00:02", "192.168.1.11"), r("aa:00:00:00:00:03", "192.168.1.12")}}
	draft := config.NetworkConfig{Reservations: []config.Reservation{r("aa:00:00:00:00:02", "192.168.1.99"), r("aa:00:00:00:00:03", "192.168.1.12"), r("aa:00:00:00:00:04", "192.168.1.13")}}
	leases := []ClientLease{{MACAddress: "AA:00:00:00:00:01"}, {MACAddress: "aa:00:00:00:00:02"}, {MACAddress: "aa:00:00:00:00:03"}, {MACAddress: "aa:00:00:00:00:04"}, {MACAddress: "aa:00:00:00:00:05"}}
	MarkReservationChanges(leases, active, draft)
	var got []string
	for _, l := range leases {
		got = append(got, l.ReserveChange)
	}
	if want := []string{"removed", "changed", "", "added", ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("changes %q, want %q", got, want)
	}
}

package network

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const leaseFile = `1999999999 aa:bb:cc:00:00:01 192.168.1.10 laptop *
1999999999 AA:BB:CC:00:00:02 192.168.1.11 phone *
1999999999 aa:bb:cc:00:00:03 192.168.1.12 * *
`

func leasesFrom(t *testing.T, links Links) []ClientLease {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leases")
	if err := os.WriteFile(path, []byte(leaseFile), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	leaseHandler([]string{filepath.Join(t.TempDir(), "missing"), path}, links)(rec, httptest.NewRequest("GET", "/x", nil))
	var out []ClientLease
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v\n%s", err, rec.Body)
	}
	return out
}

func TestLeasesSayHowEachDeviceIsConnected(t *testing.T) {
	links := func(context.Context) (map[string]Link, bool) {
		return map[string]Link{"aa:bb:cc:00:00:02": {SSID: "Home", Band: "5 GHz"}}, true
	}
	got := leasesFrom(t, links)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	by := map[string]ClientLease{}
	for _, l := range got {
		by[l.Hostname] = l
	}
	if l := by["phone"]; l.Connection != "wifi" || l.SSID != "Home" || l.Band != "5 GHz" {
		t.Fatalf("%+v", l)
	}
	for _, name := range []string{"laptop", "Unknown Device"} {
		if l := by[name]; l.Connection != "wired" || l.SSID != "" || l.Band != "" {
			t.Fatalf("%s: %+v", name, l)
		}
	}
}

func TestWithoutKnowingTheWifiSideNobodyIsCalledWired(t *testing.T) {
	links := func(context.Context) (map[string]Link, bool) {
		return map[string]Link{"aa:bb:cc:00:00:02": {SSID: "Home", Band: "5 GHz"}}, false
	}
	for _, l := range leasesFrom(t, links) {
		want := "unknown"
		if l.Hostname == "phone" {
			want = "wifi" // what is known is still shown
		}
		if l.Connection != want {
			t.Fatalf("%+v", l)
		}
	}
	for _, l := range leasesFrom(t, nil) {
		if l.Connection != "unknown" {
			t.Fatalf("%+v", l)
		}
	}
}

func TestNoLeaseFileGivesAnEmptyListNotNull(t *testing.T) {
	rec := httptest.NewRecorder()
	leaseHandler([]string{filepath.Join(t.TempDir(), "missing")}, nil)(rec, httptest.NewRequest("GET", "/x", nil))
	if got := rec.Body.String(); got != "[]\n" {
		t.Fatalf("%q", got)
	}
}

func TestAnEmptyLeaseFileGivesAnEmptyListNotNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leases")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	leaseHandler([]string{path}, nil)(rec, httptest.NewRequest("GET", "/x", nil))
	if got := rec.Body.String(); got != "[]\n" {
		t.Fatalf("%q", got)
	}
}

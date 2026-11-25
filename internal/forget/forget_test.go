package forget

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"peakroute/internal/network"
)

type fixture struct {
	mu        sync.Mutex
	list      []network.ClientLease
	file      string // the lease file
	runs      []string
	releaseOK bool // dhcp_release takes the lease out of the file
	failStop  bool
	seen      []string
	nicknames []string
	svc       *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{file: filepath.Join(t.TempDir(), "dnsmasq.leases")}
	os.WriteFile(f.file, []byte("1999999999 aa:bb:cc:dd:ee:01 192.168.1.20 laptop *\n1999999999 aa:bb:cc:dd:ee:02 192.168.1.21 tv *\n"), 0644)
	f.list = []network.ClientLease{
		{MACAddress: "aa:bb:cc:dd:ee:01", IPAddress: "192.168.1.20", Status: "offline", Network: "LAN"},
		{MACAddress: "aa:bb:cc:dd:ee:02", IPAddress: "192.168.1.21", Status: "online", Network: "LAN"},
		{MACAddress: "aa:bb:cc:dd:ee:03", IPAddress: "10.20.0.9", Status: "offline", Network: "Guest", Static: true},
	}
	f.svc = &Service{
		List: func(context.Context) []network.ClientLease { return f.list },
		Interface: func(l network.ClientLease) string {
			return map[string]string{"LAN": "br0", "Guest": "br20"}[l.Network]
		},
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.runs = append(f.runs, name+" "+strings.Join(args, " "))
			switch {
			case name == "dhcp_release" && f.releaseOK:
				RemoveLease(f.file, args[2])
			case name == "systemctl" && f.failStop && args[0] == "stop":
				return "", errors.New("boom")
			}
			return "", nil
		},
		Leases:       func() []network.ClientLease { return network.ReadLeases([]string{f.file}) },
		LeaseFile:    f.file,
		Real:         true,
		ForgetSeen:   func(mac string) { f.seen = append(f.seen, mac) },
		DropNickname: func(mac string) error { f.nicknames = append(f.nicknames, mac); return nil },
		Wait:         time.Millisecond,
	}
	return f
}

func (f *fixture) delete(mac string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, "/x", nil)
	req.SetPathValue("mac", mac)
	rec := httptest.NewRecorder()
	f.svc.HandleDevice(rec, req)
	return rec
}

func TestADeviceThatIsGoneIsReleasedFlushedAndForgotten(t *testing.T) {
	f := newFixture(t)
	f.releaseOK = true
	if rec := f.delete("AA:BB:CC:DD:EE:01"); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	want := "dhcp_release br0 192.168.1.20 aa:bb:cc:dd:ee:01|ip neigh flush to 192.168.1.20"
	if got := strings.Join(f.runs, "|"); got != want {
		t.Errorf("%s", got)
	}
	if len(f.seen) != 1 || f.seen[0] != "aa:bb:cc:dd:ee:01" || len(f.nicknames) != 1 {
		t.Errorf("%v %v", f.seen, f.nicknames)
	}
	b, _ := os.ReadFile(f.file)
	if strings.Contains(string(b), "ee:01") || !strings.Contains(string(b), "ee:02") {
		t.Errorf("%s", b)
	}
}

func TestADeviceThatIsHereNowIsNotRemoved(t *testing.T) {
	f := newFixture(t)
	rec := f.delete("aa:bb:cc:dd:ee:02")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "here now") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if len(f.runs) != 0 || len(f.seen) != 0 || len(f.nicknames) != 0 {
		t.Errorf("something was done anyway: %v %v %v", f.runs, f.seen, f.nicknames)
	}
}

func TestAnUnknownOrBadAddressIsRefused(t *testing.T) {
	f := newFixture(t)
	if rec := f.delete("aa:bb:cc:dd:ee:99"); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
	for _, bad := range []string{"nonsense", "aa:bb:cc:dd:ee", "../etc/passwd", ""} {
		if rec := f.delete(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	f.svc.HandleDevice(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("%d", rec.Code)
	}
}

func TestAStaticDeviceHasNoLeaseToReleaseOnlyItsNeighbourEntry(t *testing.T) {
	f := newFixture(t)
	if rec := f.delete("aa:bb:cc:dd:ee:03"); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := strings.Join(f.runs, "|"); got != "ip neigh flush to 10.20.0.9" {
		t.Errorf("%s", got)
	}
}

func TestWhenTheServerKeepsTheLeaseItIsStoppedTheLineTakenOutAndItIsStartedAgain(t *testing.T) {
	f := newFixture(t) // dhcp_release does nothing here
	if rec := f.delete("aa:bb:cc:dd:ee:01"); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := strings.Join(f.runs, "|")
	if !strings.Contains(got, "systemctl stop dnsmasq|systemctl start dnsmasq") {
		t.Errorf("%s", got)
	}
	b, _ := os.ReadFile(f.file)
	if strings.Contains(string(b), "ee:01") || !strings.Contains(string(b), "ee:02") {
		t.Errorf("%s", b)
	}
	fi, _ := os.Stat(f.file)
	if fi.Mode().Perm() != 0644 {
		t.Errorf("the file's mode changed: %v", fi.Mode())
	}
}

func TestTheServerIsNeverLeftStoppedAndNothingIsForgottenIfTheLeaseStays(t *testing.T) {
	f := newFixture(t)
	f.failStop = true
	rec := f.delete("aa:bb:cc:dd:ee:01")
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "could not be stopped") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if len(f.seen) != 0 || len(f.nicknames) != 0 {
		t.Errorf("the device was forgotten although it is still leased: %v %v", f.seen, f.nicknames)
	}

	// the file cannot be edited: the server is started again all the same
	f2 := newFixture(t)
	f2.svc.LeaseFile = filepath.Join(t.TempDir(), "missing")
	rec = f2.delete("aa:bb:cc:dd:ee:01")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("%d", rec.Code)
	}
	if got := strings.Join(f2.runs, "|"); !strings.HasSuffix(got, "systemctl stop dnsmasq|systemctl start dnsmasq") {
		t.Errorf("dnsmasq was left stopped: %s", got)
	}
}

func TestOnADevelopmentMachineNothingIsRunnedOrEdited(t *testing.T) {
	f := newFixture(t)
	f.svc.Real = false
	before, _ := os.ReadFile(f.file)
	if rec := f.delete("aa:bb:cc:dd:ee:01"); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	after, _ := os.ReadFile(f.file)
	if len(f.runs) != 0 || string(before) != string(after) || len(f.seen) != 1 {
		t.Errorf("%v", f.runs)
	}
}

func TestTheReservationOfARemovedDeviceIsTakenOutOfTheDraft(t *testing.T) {
	f := newFixture(t)
	f.releaseOK = true
	var asked []string
	f.svc.Unreserve = func(mac string) (bool, error) { asked = append(asked, mac); return true, nil }
	rec := f.delete("aa:bb:cc:dd:ee:01")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reservation_removed":true`) || len(asked) != 1 || asked[0] != "aa:bb:cc:dd:ee:01" {
		t.Errorf("%d %s %v", rec.Code, rec.Body, asked)
	}
	// no reservation: nothing said about one
	f2 := newFixture(t)
	f2.releaseOK = true
	f2.svc.Unreserve = func(string) (bool, error) { return false, nil }
	if rec := f2.delete("aa:bb:cc:dd:ee:01"); !strings.Contains(rec.Body.String(), `"reservation_removed":false`) || strings.Contains(rec.Body.String(), "reservation_note") {
		t.Errorf("%s", rec.Body)
	}
	// the draft cannot be changed now: the device is removed all the same, and the answer says the reservation is still there
	f3 := newFixture(t)
	f3.releaseOK = true
	f3.svc.Unreserve = func(string) (bool, error) { return false, errors.New("changes are locked") }
	rec = f3.delete("aa:bb:cc:dd:ee:01")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "still on the DHCP page: changes are locked") || len(f3.seen) != 1 {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	// a device that was refused (here now) keeps its reservation
	f4 := newFixture(t)
	f4.svc.Unreserve = func(string) (bool, error) {
		t.Error("the reservation of a device that is here was removed")
		return true, nil
	}
	f4.delete("aa:bb:cc:dd:ee:02")
}

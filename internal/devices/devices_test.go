package devices

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"peakroute/internal/devhistory"
	"peakroute/internal/lldp"
	"peakroute/internal/network"
	"peakroute/internal/wireless"
)

var now = time.Date(2026, 11, 28, 12, 30, 20, 0, time.UTC)

func rig(t *testing.T) (*Service, *devhistory.Store) {
	t.Helper()
	st, err := devhistory.Open("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sig := -58
	s := &Service{
		History: st,
		Now:     func() time.Time { return now },
		List: func(context.Context) []network.ClientLease {
			return []network.ClientLease{
				{MACAddress: "a8:bb:cc:00:00:01", IPAddress: "192.168.1.50", Hostname: "laptop", Nickname: "Alec's laptop", Network: "LAN", Status: "online", Connection: "wifi",
					SSID: "Home", Band: "5 GHz", TotalBytes: 12345, Reserved: true, Reservation: "192.168.1.50", ExpiryText: "in 11 h", LastSeen: now.Unix()},
				{MACAddress: "02:11:22:33:44:55", IPAddress: "192.168.1.60", Hostname: "*", Status: "offline", Connection: "wired", Static: false},
			}
		},
		Stations: func(context.Context) wireless.StationReport {
			return wireless.StationReport{Known: true, Stations: []wireless.Station{{MAC: "a8:bb:cc:00:00:01", Interface: "wlan0", SSID: "Home", Band: "5 GHz", Channel: 36, Signal: &sig, RxMbps: 866, TxMbps: 780}}}
		},
	}
	return s, st
}

func get(t *testing.T, h http.HandlerFunc, path, mac string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("mac", mac)
	rec := httptest.NewRecorder()
	h(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestADevicesPageHasItsNamesConnectionAndWifiDetails(t *testing.T) {
	s, st := rig(t)
	st.AddEvent(devhistory.Event{MAC: "a8:bb:cc:00:00:01", At: now.Add(-2 * time.Hour).Unix(), Online: true, Connection: "wifi", SSID: "Home"})
	st.AddEvent(devhistory.Event{MAC: "a8:bb:cc:00:00:01", At: now.Add(-time.Hour).Unix(), Online: false})
	rec, b := get(t, s.HandleClient, "/x", "A8:BB:CC:00:00:01") // upper case is fine
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if b["name"] != "Alec's laptop" || b["hostname"] != "laptop" || b["ip"] != "192.168.1.50" || b["network"] != "LAN" || b["status"] != "online" || b["connection"] != "wifi" ||
		b["reserved"] != true || b["total_bytes"].(float64) != 12345 || b["history_kept"] != true || b["private_address"] != false {
		t.Fatalf("%+v", b)
	}
	wifi, _ := b["wifi"].(map[string]any)
	if wifi == nil || wifi["signal_dbm"].(float64) != -58 || wifi["channel"].(float64) != 36 || wifi["rx_mbps"].(float64) != 866 {
		t.Fatalf("wifi: %+v", b["wifi"])
	}
	ev := b["events"].([]any)
	if len(ev) != 2 || ev[0].(map[string]any)["online"] != false {
		t.Fatalf("events newest first: %+v", ev)
	}
	// a device with a made-up address, no host name and no Wi-Fi
	_, b = get(t, s.HandleClient, "/x", "02:11:22:33:44:55")
	if b["name"] != "192.168.1.60" || b["private_address"] != true || b["hostname"] != nil || b["wifi"] != nil || b["status"] != "offline" {
		t.Fatalf("%+v", b)
	}
}

func TestADeviceThatIsNotThereOrNotAnAddressIsRefused(t *testing.T) {
	s, _ := rig(t)
	if rec, _ := get(t, s.HandleClient, "/x", "a8:bb:cc:00:00:99"); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
	for _, bad := range []string{"nope", "aa:bb:cc:00:00", "a8:bb:cc:00:00:0g", "../../etc/passwd", ""} {
		if rec, _ := get(t, s.HandleClient, "/x", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, rec.Code)
		}
		if rec, _ := get(t, s.HandleHistory, "/x", bad); rec.Code != http.StatusBadRequest {
			t.Errorf("history %q: %d", bad, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.SetPathValue("mac", "a8:bb:cc:00:00:01")
	rec := httptest.NewRecorder()
	s.HandleClient(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("%d", rec.Code)
	}
}

func TestTheHistoryHasPointsTotalsPeaksAndWhenTheDeviceWasHere(t *testing.T) {
	s, st := rig(t)
	mac := "a8:bb:cc:00:00:01"
	minute := now.Truncate(time.Minute)
	st.AddUsage(mac, minute.Add(-10*time.Minute), 600, 6000)
	st.AddUsage(mac, minute.Add(-9*time.Minute), 60, 60000) // the busiest download minute: 60000 bytes
	st.AddUsage(mac, minute.Add(-2*time.Hour), 1, 1)        // outside an hour, inside a day
	st.AddEvent(devhistory.Event{MAC: mac, At: now.Add(-3 * time.Hour).Unix(), Online: true})
	st.AddEvent(devhistory.Event{MAC: mac, At: now.Add(-30 * time.Minute).Unix(), Online: false})
	st.AddEvent(devhistory.Event{MAC: mac, At: now.Add(-10 * time.Minute).Unix(), Online: true})

	rec, b := get(t, s.HandleHistory, "/x?range=1h", mac)
	if rec.Code != http.StatusOK || b["bucket_seconds"].(float64) != 60 || b["range"] != "1h" {
		t.Fatalf("%d %+v", rec.Code, b)
	}
	pts := b["points"].([]any)
	if len(pts) != 2 || b["total_up"].(float64) != 660 || b["total_down"].(float64) != 66000 {
		t.Fatalf("%+v", b)
	}
	if got := b["peak_down_bps"].(float64); got != 60000*8.0/60 {
		t.Errorf("peak down %v", got)
	}
	// the stretches: it was here (from before the range), gone from 30 minutes ago, back 10 minutes ago
	seg := b["segments"].([]any)
	if len(seg) != 3 {
		t.Fatalf("%+v", seg)
	}
	first, second, third := seg[0].(map[string]any), seg[1].(map[string]any), seg[2].(map[string]any)
	if first["online"] != true || second["online"] != false || third["online"] != true || second["from"].(float64) != float64(now.Add(-30*time.Minute).Unix()) ||
		third["to"].(float64) != float64(now.Unix()) || first["from"].(float64) != b["from"].(float64) {
		t.Fatalf("%+v", seg)
	}
	// a day has bigger points and the older traffic
	_, b = get(t, s.HandleHistory, "/x?range=24h", mac)
	if b["bucket_seconds"].(float64) != 300 || b["total_up"].(float64) != 661 {
		t.Fatalf("%+v", b)
	}
	// the default is a day, and anything else is refused
	if _, b = get(t, s.HandleHistory, "/x", mac); b["range"] != "24h" {
		t.Errorf("%+v", b)
	}
	if rec, _ = get(t, s.HandleHistory, "/x?range=1y", mac); rec.Code != http.StatusBadRequest {
		t.Errorf("%d", rec.Code)
	}
	// nothing is known before the first change: a device with no events has no stretches, and no points is an empty list, not null
	_, b = get(t, s.HandleHistory, "/x?range=1h", "02:11:22:33:44:55")
	if len(b["segments"].([]any)) != 0 || b["points"] == nil || len(b["points"].([]any)) != 0 {
		t.Fatalf("%+v", b)
	}
}

func TestWithoutAHistoryTheDevicePageSaysSo(t *testing.T) {
	s, _ := rig(t)
	s.History = nil
	_, b := get(t, s.HandleClient, "/x", "a8:bb:cc:00:00:01")
	if b["history_kept"] != false || len(b["events"].([]any)) != 0 {
		t.Fatalf("%+v", b)
	}
	if rec, _ := get(t, s.HandleHistory, "/x?range=1h", "a8:bb:cc:00:00:01"); rec.Code != http.StatusNotImplemented {
		t.Errorf("%d", rec.Code)
	}
}

// The page asks for /api/v1/clients/a8%3Abb%3A...: the route has to find the address in an escaped path, and has to live next to the route that
// removes a device (the same path, DELETE) and the one that names it.
func TestTheRoutesFindTheAddressInAnEscapedPathAndLiveNextToTheRemoveRoute(t *testing.T) {
	s, _ := rig(t)
	mux := http.NewServeMux()
	removed := false
	mux.HandleFunc("/api/v1/clients/{mac}", func(w http.ResponseWriter, r *http.Request) { removed = r.Method == http.MethodDelete })
	mux.HandleFunc("GET /api/v1/clients/{mac}", s.HandleClient)
	mux.HandleFunc("GET /api/v1/clients/{mac}/history", s.HandleHistory)
	mux.HandleFunc("/api/v1/clients/{mac}/name", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })

	for _, path := range []string{"/api/v1/clients/a8%3Abb%3Acc%3A00%3A00%3A01", "/api/v1/clients/a8:bb:cc:00:00:01"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+"/history?range=1h", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s history: %d", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/clients/a8:bb:cc:00:00:01", nil))
	if !removed {
		t.Errorf("a DELETE did not reach the remove route: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/clients/a8:bb:cc:00:00:01/name", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("the name route: %d", rec.Code)
	}
}

func TestAWiredDeviceIsSaidToBeBehindThePortAndTheSwitchOnIt(t *testing.T) {
	s, _ := rig(t)
	s.Where = func(_ context.Context, mac string) *Where {
		if mac != "02:11:22:33:44:55" {
			return nil
		}
		return &Where{Port: "eth2", Bridge: "br0", VLAN: 20, Neighbour: &lldp.Neighbour{LocalPort: "eth2", Name: "switch-hall", PortID: "Port 7"}}
	}
	_, b := get(t, s.HandleClient, "/x", "02:11:22:33:44:55")
	at, _ := b["attached"].(map[string]any)
	if at == nil || at["port"] != "eth2" || at["vlan"].(float64) != 20 {
		t.Fatalf("%+v", b)
	}
	if nb, _ := at["neighbour"].(map[string]any); nb == nil || nb["name"] != "switch-hall" || nb["port_id"] != "Port 7" {
		t.Fatalf("%+v", at)
	}
	// a device on Wi-Fi is not placed on a cable port, and one the bridge has not learned has nothing
	if _, b = get(t, s.HandleClient, "/x", "a8:bb:cc:00:00:01"); b["attached"] != nil {
		t.Errorf("%+v", b["attached"])
	}
	s.Where = func(context.Context, string) *Where { return nil }
	if _, b = get(t, s.HandleClient, "/x", "02:11:22:33:44:55"); b["attached"] != nil {
		t.Errorf("%+v", b["attached"])
	}
}

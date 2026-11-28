package netdevices

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const apJSON = `{"ports":[{"name":"eth0","kind":"port","state":"up","speed":"1000Mb/s"},{"name":"wlan2","kind":"link","state":"","ssid":"Upstairs","connected":true}],
"radios":[{"name":"wlan0","band":"5 GHz","channel":36,"up":true,"clients":1},{"name":"wlan1","band":"2.4 GHz","channel":6,"up":false,"clients":0}],
"devices":[{"mac":"A8:BB:CC:00:00:01","interface":"wlan0_1","ssid":"Guest","band":"5 GHz","signal_dbm":-61}],"devices_known":true}`

func TestTheMonitoringTokenIsKeptHashedAndOpensOnlyTheStatusPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.json")
	m, err := OpenMonitor(path)
	if err != nil || m.Enabled() || m.Check("x") || m.Check("") {
		t.Fatalf("%v", err)
	}
	tok, err := m.Generate()
	if err != nil || len(tok) < 40 || !m.Enabled() || !m.Check(tok) || m.Check(tok+"x") {
		t.Fatalf("%q %v", tok, err)
	}
	// only the hash is on disk, and it survives a restart
	if b := readFile(t, path); strings.Contains(b, tok) {
		t.Fatal("the token itself was written")
	}
	m2, _ := OpenMonitor(path)
	if !m2.Check(tok) {
		t.Error("the token was lost")
	}
	// a new token replaces the old one
	tok2, _ := m.Generate()
	if m.Check(tok) || !m.Check(tok2) {
		t.Error("the old token still works")
	}
	// the wrapper: the token only for GET, anything else goes to the sign-in check
	signIn := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unauthorized", http.StatusUnauthorized) }
	}
	h := m.Allow(signIn, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("status")) })
	for _, c := range []struct {
		method, auth string
		want         int
	}{{"GET", "Bearer " + tok2, 200}, {"GET", "Bearer " + tok, 401}, {"GET", "", 401}, {"GET", tok2, 401}, {"POST", "Bearer " + tok2, 401}} {
		req := httptest.NewRequest(c.method, "/api/v1/ap/status", nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %q: %d", c.method, c.auth, rec.Code)
		}
	}
	if err := m.Revoke(); err != nil || m.Enabled() || m.Check(tok2) {
		t.Errorf("%v", err)
	}
	if m3, _ := OpenMonitor(path); m3.Enabled() {
		t.Error("a revoked token came back")
	}
}

func TestTheAccessPointDriverTrustsTheFirstCertificateAndRefusesAnotherOne(t *testing.T) {
	m, _ := OpenMonitor(filepath.Join(t.TempDir(), "m.json"))
	tok, _ := m.Generate()
	status := func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(apJSON)) }
	srv := httptest.NewTLSServer(http.HandlerFunc(m.Allow(func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusUnauthorized) }
	}, status)))
	defer srv.Close()
	host, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	sum := sha256.Sum256(srv.Certificate().Raw)
	fp := hex.EncodeToString(sum[:])

	var pinned string
	drv := &APDriver{Pin: func(id, f string) { pinned = id + ":" + f }}
	dev := Device{ID: "nd7", Name: "AP hall", Address: host, Kind: KindAP, Enabled: true, AP: &APAuth{Port: port, Token: tok}}
	rec, err := drv.Poll(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	if pinned != "nd7:"+fp {
		t.Errorf("the first certificate was not remembered: %q", pinned)
	}
	if len(rec.Ports) != 4 || rec.Ports[0].Name != "eth0" || !rec.Ports[0].Up || rec.Ports[0].SpeedMbps != 1000 || !rec.Ports[1].Up || rec.Ports[1].Alias != "Wi-Fi link to Upstairs" {
		t.Errorf("%+v", rec.Ports)
	}
	if p := rec.Ports[2]; p.Name != "wlan0" || !p.Up || p.MACs != 1 || p.Alias != "5 GHz channel 36" || rec.Ports[3].Up {
		t.Errorf("%+v", rec.Ports[2:])
	}
	// the station on the second network of the adapter is placed on the adapter, with its Wi-Fi
	if len(rec.Clients) != 1 || rec.Clients[0].MAC != "a8:bb:cc:00:00:01" || rec.Clients[0].Port != "wlan0" || rec.Clients[0].SSID != "Guest" || rec.Clients[0].Signal == nil || *rec.Clients[0].Signal != -61 {
		t.Errorf("%+v", rec.Clients)
	}

	// with the certificate remembered, nothing is pinned again; the right one passes, another is refused
	pinned = ""
	dev.AP.Fingerprint = fp
	if _, err := drv.Poll(context.Background(), dev); err != nil || pinned != "" {
		t.Errorf("%v %q", err, pinned)
	}
	dev.AP.Fingerprint = strings.Repeat("ab", 32)
	if _, err := drv.Poll(context.Background(), dev); !errors.Is(err, ErrCertificateChanged) {
		t.Errorf("a certificate that differs was accepted: %v", err)
	}
	// a wrong token says so, and pins nothing
	dev.AP.Fingerprint, dev.AP.Token = "", strings.Repeat("x", 43)
	if _, err := drv.Poll(context.Background(), dev); err == nil || !strings.Contains(err.Error(), "token") || pinned != "" {
		t.Errorf("%v %q", err, pinned)
	}
}

func TestAnAccessPointIsCheckedLikeAnySwitchAndItsSecretsStayInside(t *testing.T) {
	s, f := rig(t)
	good := Device{Name: "AP hall", Address: "192.168.1.9", Kind: KindAP, Enabled: true, AP: &APAuth{Token: strings.Repeat("t", 43)}}
	for name, mod := range map[string]func(*Device){
		"ap":             func(d *Device) { d.AP = nil },
		"ap.token":       func(d *Device) { d.AP = &APAuth{Token: "short"} },
		"ap.port":        func(d *Device) { d.AP = &APAuth{Token: strings.Repeat("t", 43), Port: 70000} },
		"ap.fingerprint": func(d *Device) { d.AP = &APAuth{Token: strings.Repeat("t", 43), Fingerprint: "zz"} },
	} {
		d := good
		mod(&d)
		_, err := s.Store.Add(d)
		var errs Errors
		if !errors.As(err, &errs) || errs[0].Field != name {
			t.Errorf("%s: %v", name, err)
		}
	}
	// over the API: the token is never sent back; a change without it keeps it; a new address forgets the certificate
	rec, v := call(t, s, s.HandleCollection, http.MethodPost, `{"name":"AP hall","address":"192.168.1.9","kind":"peakroute-ap","ap":{"token":"`+strings.Repeat("t", 43)+`"}}`, "")
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), strings.Repeat("t", 43)) || v["ap"].(map[string]any)["token_set"] != true {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	id := v["id"].(string)
	fp := strings.Repeat("ab", 32)
	if err := s.Store.SetFingerprint(id, fp); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetFingerprint(id, strings.Repeat("cd", 32)); err != nil { // only the first is remembered
		t.Fatal(err)
	}
	if d, _ := s.Store.Get(id); d.AP.Fingerprint != fp {
		t.Fatalf("%+v", d.AP)
	}
	call(t, s, s.HandleItem, http.MethodPut, `{"name":"AP hall (stairs)","address":"192.168.1.9","ap":{}}`, id)
	if d, _ := s.Store.Get(id); d.AP.Token != strings.Repeat("t", 43) || d.AP.Fingerprint != fp || d.Name != "AP hall (stairs)" {
		t.Fatalf("%+v", d.AP)
	}
	call(t, s, s.HandleItem, http.MethodPut, `{"name":"AP hall (stairs)","address":"192.168.1.9","ap":{"trust_new_certificate":true}}`, id)
	if d, _ := s.Store.Get(id); d.AP.Fingerprint != "" || d.AP.Token == "" {
		t.Fatalf("%+v", d.AP)
	}
	s.Store.SetFingerprint(id, fp)
	call(t, s, s.HandleItem, http.MethodPut, `{"name":"AP hall (stairs)","address":"192.168.1.10","ap":{}}`, id)
	if d, _ := s.Store.Get(id); d.AP.Fingerprint != "" {
		t.Fatalf("a new address kept the old certificate: %+v", d.AP)
	}
	_ = f
}

func TestLinkSpeedsAreRead(t *testing.T) {
	for in, want := range map[string]int{"1000Mb/s": 1000, "100Mb/s": 100, "2.5Gb/s": 2500, "10Gb/s": 10000, "": 0, "unknown": 0} {
		if got := speedMbps(in); got != want {
			t.Errorf("%q: %d", in, got)
		}
	}
}

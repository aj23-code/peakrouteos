package netapi

import (
	"net/http"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func (e *env) station(method, name, body string) (code int, view WifiStation, raw string) {
	e.t.Helper()
	rec := e.wifiCall(e.api.HandleWifiStation, method, name, "", body)
	if rec.Code == http.StatusOK && method == http.MethodPut {
		view = decodeInto[WifiStation](e.t, rec)
	}
	return rec.Code, view, rec.Body.String()
}

const cafe = `{"ssid":"Cafe Guest","passphrase":"correct horse battery","role":"uplink"}`

func TestAWifiAdapterBecomesTheInternetConnection(t *testing.T) {
	e := wifiEnv(t)
	code, st, raw := e.station(http.MethodPut, "wlan2", cafe)
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	// what was left out was filled in: WPA2/WPA3, switched on; and it is used, and it is pending
	if st.Security != config.SecurityWPA2WPA3 || !st.Enabled || !st.InUse || !st.Pending || st.Role != "uplink" || st.Network != "" || st.SSID != "Cafe Guest" {
		t.Fatalf("%+v", st)
	}
	d := e.draft()
	if d.WanInterface != "wlan2" || len(d.LanInterfaces) != 1 || d.LanInterfaces[0] != "eth1" {
		t.Fatalf("the adapter is not the internet port: %+v", d)
	}
	if w := e.wifi(); len(w.Stations) != 1 || w.WanPort != "wlan2" || w.Stations[0].Passphrase != "correct horse battery" {
		t.Fatalf("%+v", w)
	}
	// nothing is on the running system until it is committed
	if e.mgr.NetworkConfig().WanInterface != "eth0" {
		t.Fatal("the running settings changed")
	}
}

func TestStopUsingTheAdapterAndTheInternetPortGoesBackToACable(t *testing.T) {
	e := wifiEnv(t)
	e.station(http.MethodPut, "wlan2", cafe)
	if code, _, raw := e.station(http.MethodDelete, "wlan2", ""); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	d := e.draft()
	if d.WanInterface != "eth0" || d.Wifi != nil {
		t.Fatalf("%+v", d)
	}
	// switching it off does the same, and keeps what was typed
	e.station(http.MethodPut, "wlan2", cafe)
	code, st, _ := e.station(http.MethodPut, "wlan2", `{"enabled":false}`)
	if code != 200 || st.Enabled || st.InUse || st.SSID != "Cafe Guest" {
		t.Fatalf("%d %+v", code, st)
	}
	if e.draft().WanInterface != "eth0" {
		t.Fatalf("%+v", e.draft())
	}
	// and on again
	if _, st, _ := e.station(http.MethodPut, "wlan2", `{"enabled":true}`); !st.InUse || e.draft().WanInterface != "wlan2" {
		t.Fatalf("%+v", st)
	}
}

func TestWithNoCablePortLeftTheInternetCannotGoBack(t *testing.T) {
	e := wifiEnv(t)
	e.station(http.MethodPut, "wlan2", cafe)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error { c.LanInterfaces = []string{"eth0", "eth1", "eth2"}; return nil })
	code, _, raw := e.station(http.MethodDelete, "wlan2", "")
	if code != 400 || !strings.Contains(raw, "free one on the Ports page") {
		t.Fatalf("%d %s", code, raw)
	}
	if e.draft().WanInterface != "wlan2" || len(e.draft().Wifi.Stations) != 1 {
		t.Fatal("the refused change was made")
	}
}

func TestALinkIntoALocalNetwork(t *testing.T) {
	e := wifiEnv(t)
	code, st, raw := e.station(http.MethodPut, "wlan1", `{"ssid":"RoofLink","passphrase":"correct horse battery","role":"bridge","security":"wpa3"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if st.Role != "bridge" || st.Network != "lan" || !st.InUse || st.Security != "wpa3" {
		t.Fatalf("%+v", st)
	}
	d := e.draft()
	if d.WanInterface != "eth0" || len(d.LanInterfaces) != 1 {
		t.Fatalf("a link changed the ports: %+v", d)
	}
	// from an uplink to a link: the internet goes back to a cable
	e.station(http.MethodPut, "wlan2", cafe)
	if e.draft().WanInterface != "wlan2" {
		t.Fatal("not the uplink")
	}
	if code, _, raw := e.station(http.MethodPut, "wlan2", `{"role":"bridge"}`); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if d := e.draft(); d.WanInterface != "eth0" {
		t.Fatalf("%+v", d)
	}
	// a link into a network that does not exist
	code, _, raw = e.station(http.MethodPut, "wlan1", `{"network":"nowhere"}`)
	if code != 400 || !errorFieldsOf(t, raw)["network"] {
		t.Fatalf("%d %s", code, raw)
	}
}

func errorFieldsOf(t *testing.T, raw string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, part := range strings.Split(raw, `"field":"`)[1:] {
		out[part[:strings.Index(part, `"`)]] = true
	}
	return out
}

func TestAStationIsRefusedWhatItCannotDo(t *testing.T) {
	e := wifiEnv(t)
	for name, c := range map[string]struct{ adapter, body string }{
		"an adapter that is not there": {"wlan9", cafe},
		"an invalid password":          {"wlan2", `{"ssid":"Cafe","passphrase":"short","role":"uplink"}`},
		"no role":                      {"wlan2", `{"ssid":"Cafe","passphrase":"correct horse battery","role":"nothing"}`},
		"a name that is nothing":       {"wlan2", `{"ssid":"","passphrase":"correct horse battery","role":"uplink"}`},
		"an unknown security":          {"wlan2", `{"ssid":"Cafe","passphrase":"correct horse battery","role":"uplink","security":"wep"}`},
		"an unknown field":             {"wlan2", `{"ssid":"Cafe","root":true}`},
	} {
		code, _, raw := e.station(http.MethodPut, c.adapter, c.body)
		if code != 400 {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	if e.draft().Wifi != nil || e.draft().WanInterface != "eth0" {
		t.Fatalf("a refused request changed the draft: %+v", e.draft())
	}
	// an adapter whose abilities cannot be read is allowed (the hardware may not say)
	if code, _, raw := e.station(http.MethodPut, "wlan3", `{"ssid":"Cafe","passphrase":"correct horse battery","role":"bridge"}`); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	// an adapter that is already an access point is either one or the other
	e.setRadio("wlan0", radio24)
	if code, _, raw := e.station(http.MethodPut, "wlan0", `{"ssid":"Cafe","passphrase":"correct horse battery","role":"bridge"}`); code != 400 || !errorFieldsOf(t, raw)["name"] {
		t.Fatalf("%d %s", code, raw)
	}
}

func TestWifiCannotCarryAVlanOrPPPoE(t *testing.T) {
	b := baseline()
	b.WanVlan = 835
	e := newEnv(t, &b)
	e.api.Wireless = wifiEnv(t).api.Wireless
	code, _, raw := e.station(http.MethodPut, "wlan2", cafe)
	if code != 400 || !strings.Contains(raw, "VLAN or PPPoE") {
		t.Fatalf("%d %s", code, raw)
	}
	if e.draft().WanInterface != "eth0" {
		t.Fatal("changed")
	}
}

func TestOnlyAnEditableRouterTakesStationChanges(t *testing.T) {
	e := wifiEnv(t)
	e.editable = false
	if code, _, _ := e.station(http.MethodPut, "wlan2", cafe); code != http.StatusConflict {
		t.Fatalf("%d", code)
	}
	if code, _, _ := e.station(http.MethodDelete, "wlan2", ""); code != http.StatusConflict {
		t.Fatalf("%d", code)
	}
	if code, _, _ := e.station(http.MethodPost, "wlan2", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", code)
	}
}

func TestANetworkThatALinkJoinsCannotBeDeleted(t *testing.T) {
	e := wifiEnv(t)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.Networks = []config.Network{{ID: "guest", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0"}}
		return nil
	})
	if code, _, raw := e.station(http.MethodPut, "wlan1", `{"ssid":"RoofLink","passphrase":"correct horse battery","role":"bridge","network":"guest"}`); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	rec := e.wifiCall(e.api.HandleNetwork, http.MethodDelete, "", "guest", "")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Wi-Fi link on wlan1") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnAccessPointCanAcceptPointToPointLinks(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan1", radio5)
	rec := e.addSSID(`{"ssid":"RoofLink","passphrase":"correct horse battery","bands":["5 GHz"],"wds":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if v := decodeInto[WifiNetwork](t, rec); !v.WDS {
		t.Fatalf("%+v", v)
	}
	rec = e.ssid(http.MethodPut, "wifi1", `{"wds":false}`)
	if v := decodeInto[WifiNetwork](t, rec); v.WDS {
		t.Fatalf("%+v", v)
	}
}

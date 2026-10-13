package netapi

import (
	"net/http"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func (e *env) ap(method, body string) (int, apResponse, string) {
	e.t.Helper()
	h := e.api.HandleAP
	if method == http.MethodPut {
		h = e.api.HandleAPSet
	}
	rec := e.do(h, method, body, "application/json")
	var v apResponse
	if rec.Code == http.StatusOK {
		v = decodeInto[apResponse](e.t, rec)
	}
	return rec.Code, v, rec.Body.String()
}

func TestTheDeviceBecomesAnAccessPointWithEveryCablePort(t *testing.T) {
	e := wifiEnv(t)
	code, v, raw := e.ap(http.MethodGet, "")
	if code != 200 || v.Mode != "router" || v.ActiveMode != "router" || len(v.Ports) != 3 || v.Recovery != "192.168.77.1/24" {
		t.Fatalf("%d %s", code, raw)
	}
	code, v, raw = e.ap(http.MethodPut, `{"mode":"ap"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if v.Mode != "ap" || v.ActiveMode != "router" || !v.Pending || strings.Join(v.AP.Ports, ",") != "eth0,eth1,eth2" || v.AP.Mode != "dhcp" {
		t.Fatalf("%+v", v)
	}
	d := e.draft()
	if !d.IsAP() || d.WanInterface != "eth0" || len(d.LanInterfaces) != 1 {
		t.Fatalf("the router settings must stay: %+v", d)
	}
	if e.mgr.NetworkConfig().IsAP() {
		t.Fatal("the running settings changed")
	}
}

func TestTheAccessPointNetworkIsSetAndChecked(t *testing.T) {
	e := wifiEnv(t)
	e.ap(http.MethodPut, `{"mode":"ap"}`)
	code, v, raw := e.ap(http.MethodPut, `{"ports":["eth1","eth2"],"address_mode":"static","ip":"192.168.1.2","netmask":"255.255.255.0","gateway":"192.168.1.1","dns":["192.168.1.1"," "]}`)
	if code != 200 || strings.Join(v.AP.Ports, ",") != "eth1,eth2" || v.AP.IP != "192.168.1.2" || len(v.AP.DNS) != 1 {
		t.Fatalf("%d %s", code, raw)
	}
	for name, body := range map[string]string{
		"an address that is not one": `{"address_mode":"static","ip":"nope"}`,
		"a gateway outside":          `{"gateway":"10.9.9.9"}`,
		"no ports":                   `{"ports":[]}`,
		"an unknown way to be found": `{"address_mode":"magic"}`,
		"an unknown mode":            `{"mode":"switch"}`,
		"an unknown field":           `{"root":true}`,
	} {
		code, _, raw := e.ap(http.MethodPut, body)
		if code != 400 {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	if d := e.draft(); d.AP.IP != "192.168.1.2" || len(d.AP.Ports) != 2 {
		t.Fatalf("a refused request changed the draft: %+v", d.AP)
	}
}

func TestBackToARouterKeepsWhatWasThere(t *testing.T) {
	e := wifiEnv(t)
	e.ap(http.MethodPut, `{"mode":"ap"}`)
	code, v, raw := e.ap(http.MethodPut, `{"mode":"router"}`)
	if code != 200 || v.Mode != "router" {
		t.Fatalf("%d %s", code, raw)
	}
	d := e.draft()
	// switching there and back is no change at all, so nothing is left in the draft
	if d.IsAP() || d.WanInterface != "eth0" || d.LanIP != "192.168.1.1" || len(e.mgr.StagedChanges()) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestADeviceThatWasNeverARouterGetsTheUsualOneBack(t *testing.T) {
	e := newEnv(t, nil) // nothing committed: no internet port, no LAN
	e.api.Wireless = wifiEnv(t).api.Wireless
	if code, _, raw := e.ap(http.MethodPut, `{"mode":"ap"}`); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	code, _, raw := e.ap(http.MethodPut, `{"mode":"router"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	d := e.draft()
	if d.WanInterface != "eth0" || strings.Join(d.LanInterfaces, ",") != "eth1,eth2" || d.LanIP != "192.168.1.1" || d.DHCP == nil || !d.DHCP.Enabled {
		t.Fatalf("%+v", d)
	}
	// with one cable port there is no router to make
	e2 := newEnv(t, nil)
	e2.ports = e2.ports[:1]
	e2.ap(http.MethodPut, `{"mode":"ap"}`)
	if code, _, raw := e2.ap(http.MethodPut, `{"mode":"router"}`); code != 400 || !strings.Contains(raw, "two cable ports") {
		t.Fatalf("%d %s", code, raw)
	}
	if !e2.draft().IsAP() {
		t.Fatal("a refused switch changed the mode")
	}
}

func TestWiFiAdaptersAreNotCablePorts(t *testing.T) {
	e := wifiEnv(t)
	e.ports = append(e.ports, config.SystemInterface{Name: "wlp3s0", Wireless: true})
	_, v, _ := e.ap(http.MethodGet, "")
	for _, p := range v.Ports {
		if p.Name == "wlp3s0" {
			t.Fatal("a Wi-Fi adapter is offered as a cable port")
		}
	}
	e.ap(http.MethodPut, `{"mode":"ap"}`)
	for _, p := range e.draft().AP.Ports {
		if p == "wlp3s0" {
			t.Fatal("a Wi-Fi adapter joined the bridge as a port")
		}
	}
}

func TestOnlyAnEditableDeviceChangesMode(t *testing.T) {
	e := wifiEnv(t)
	e.editable = false
	if code, _, _ := e.ap(http.MethodPut, `{"mode":"ap"}`); code != http.StatusConflict {
		t.Fatalf("%d", code)
	}
	if code, _, _ := e.ap(http.MethodGet, ""); code != 200 {
		t.Fatalf("%d", code)
	}
	if rec := e.do(e.api.HandleAP, http.MethodPost, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
}

func TestAWifiNetworkOnAnAccessPointTakesAVLANAndARouterRefusesOne(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan1", radio5)
	// a router: a VLAN ID is refused, with the message on the field
	rec := e.addSSID(`{"ssid":"Guest","passphrase":"correct horse battery","bands":["5 GHz"],"vlan":20}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["vlan"] {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if w := e.wifi(); w.APMode || len(w.Networks) != 0 {
		t.Fatalf("%+v", w)
	}
	// an access point: accepted, shown, changed, and checked
	e.ap(http.MethodPut, `{"mode":"ap"}`)
	rec = e.addSSID(`{"ssid":"Guest","passphrase":"correct horse battery","bands":["5 GHz"],"vlan":20}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if v := decodeInto[WifiNetwork](t, rec); v.VLAN != 20 {
		t.Fatalf("%+v", v)
	}
	if w := e.wifi(); !w.APMode || len(w.Networks) != 1 || w.Networks[0].VLAN != 20 {
		t.Fatalf("%+v", w)
	}
	for _, bad := range []string{`{"vlan":1}`, `{"vlan":4095}`, `{"vlan":-2}`} {
		if rec := e.ssid(http.MethodPut, "wifi1", bad); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["vlan"] {
			t.Errorf("%s: %d %s", bad, rec.Code, rec.Body.String())
		}
	}
	if rec := e.ssid(http.MethodPut, "wifi1", `{"vlan":0}`); rec.Code != 200 || decodeInto[WifiNetwork](t, rec).VLAN != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAnAccessPointHasNoInternetConnectionToMake(t *testing.T) {
	e := wifiEnv(t)
	e.ap(http.MethodPut, `{"mode":"ap"}`)
	code, _, raw := e.station(http.MethodPut, "wlan2", `{"ssid":"Cafe","passphrase":"correct horse battery","role":"uplink"}`)
	if code != 400 || !strings.Contains(raw, "link into a local network") {
		t.Fatalf("%d %s", code, raw)
	}
	if code, _, raw := e.station(http.MethodPut, "wlan2", `{"ssid":"Main","passphrase":"correct horse battery","role":"bridge"}`); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
}

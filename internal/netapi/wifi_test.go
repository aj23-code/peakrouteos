package netapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"peakroute/internal/config"
	"peakroute/internal/wireless"
)

func chans(nums ...int) []wireless.Channel {
	var out []wireless.Channel
	for _, n := range nums {
		out = append(out, wireless.Channel{Number: n})
	}
	return out
}

// wlan0 is 2.4 GHz only (802.11n); wlan1 is 5 GHz (802.11ac) and can run two networks at once.
func fakeWireless() wireless.Report {
	return wireless.Report{IWAvailable: true, Adapters: []wireless.Adapter{
		{Name: "wlan0", CapabilitiesKnown: true, APCapable: true, Modes: []string{"managed", "AP"}, MaxAP: 1, Bands: []wireless.Band{
			{Name: config.Band24, Widths: []int{20, 40}, HT: true, Channels: chans(1, 6, 11)}}},
		{Name: "wlan1", CapabilitiesKnown: true, APCapable: true, Modes: []string{"managed", "AP"}, MaxAP: 2, Bands: []wireless.Band{
			{Name: config.Band5, Widths: []int{20, 40, 80}, HT: true, VHT: true, Channels: append(chans(36, 40, 44, 48),
				wireless.Channel{Number: 52, DFS: true}, wireless.Channel{Number: 149, NoIR: true}, wireless.Channel{Number: 165, Disabled: true})}}},
		{Name: "wlan2", CapabilitiesKnown: true, APCapable: false, Modes: []string{"managed"}, Bands: []wireless.Band{{Name: config.Band24, Widths: []int{20}, Channels: chans(1)}}},
		{Name: "wlan3", CapabilitiesKnown: false},
		// 802.11n, but this one only offers 20 MHz channels: the configuration rules alone would allow 40
		{Name: "wlan4", CapabilitiesKnown: true, APCapable: true, Modes: []string{"managed", "AP"}, MaxAP: 1, Bands: []wireless.Band{
			{Name: config.Band24, Widths: []int{20}, HT: true, Channels: chans(1, 6, 11)}}},
	}}
}

func wifiEnv(t *testing.T) *env {
	e := standard(t)
	e.api.Wireless = func(context.Context) wireless.Report { return fakeWireless() }
	return e
}

func (e *env) wifiCall(h http.HandlerFunc, method, name, id, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetPathValue("name", name)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func (e *env) wifi() wifiResponse {
	e.t.Helper()
	return decodeInto[wifiResponse](e.t, e.wifiCall(e.api.HandleWifi, http.MethodGet, "", "", ""))
}

func (e *env) setCountry(body string) *httptest.ResponseRecorder {
	return e.do(e.api.HandleSettingsCountry, http.MethodPut, body, "application/json")
}

func (e *env) setRadio(name, body string) *httptest.ResponseRecorder {
	return e.wifiCall(e.api.HandleWifiRadio, http.MethodPut, name, "", body)
}

func (e *env) addSSID(body string) *httptest.ResponseRecorder {
	return e.wifiCall(e.api.HandleWifiNetworks, http.MethodPost, "", "", body)
}

func (e *env) ssid(method, id, body string) *httptest.ResponseRecorder {
	return e.wifiCall(e.api.HandleWifiNetwork, method, "", id, body)
}

const radio24 = `{"band":"2.4 GHz","channel":6,"width":20}`
const radio5 = `{"band":"5 GHz","channel":0,"width":80}`
const home = `{"ssid":"Home","passphrase":"correct horse battery","bands":["2.4 GHz","5 GHz"]}`

func TestWifiStartsEmpty(t *testing.T) {
	e := wifiEnv(t)
	w := e.wifi()
	if w.Country != "" || len(w.Radios) != 0 || w.Networks == nil || len(w.Networks) != 0 || !w.Editable {
		t.Fatalf("%+v", w)
	}
	e.setCountry(`{"country":"DE"}`)
	if got := e.wifi().Country; got != "DE" {
		t.Fatalf("the Wi-Fi page shows the router's country (set elsewhere), got %q", got)
	}
}

func TestAnAdapterIsSetUpFromWhatItCanDo(t *testing.T) {
	e := wifiEnv(t)
	rec := e.setRadio("wlan1", radio5)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	w := decodeInto[wifiResponse](t, rec)
	if len(w.Radios) != 1 {
		t.Fatalf("%+v", w.Radios)
	}
	r := w.Radios[0]
	if r.Name != "wlan1" || r.Band != config.Band5 || r.Channel != 0 || r.Width != 80 || !r.Enabled || !r.HT || !r.VHT || r.HE || !r.Pending {
		t.Fatalf("%+v", r)
	}
	// setting it again replaces it
	e.setRadio("wlan1", `{"band":"5 GHz","channel":36,"width":40,"enabled":false}`)
	d := e.draft().Wifi.Radios
	if len(d) != 1 || d[0].Channel != 36 || d[0].Width != 40 || !d[0].Disabled {
		t.Fatalf("%+v", d)
	}
}

func TestAnAdapterThatCannotHostAnAccessPointIsRefused(t *testing.T) {
	e := wifiEnv(t)
	for name, why := range map[string]string{"wlan2": "cannot act as an access point", "wlan3": "could not be read", "nope": "no Wi-Fi adapter"} {
		rec := e.setRadio(name, radio24)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["name"] || !strings.Contains(rec.Body.String(), why) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if e.draft().Wifi != nil {
		t.Fatal("something was staged")
	}
}

func TestAnAdapterWithoutAWifiReportCannotBeSetUp(t *testing.T) {
	e := wifiEnv(t)
	e.api.Wireless = nil
	if rec := e.setRadio("wlan0", radio24); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestRadioSettingsMustFitTheAdapter(t *testing.T) {
	e := wifiEnv(t)
	cases := []struct{ name, body, field string }{
		{"wlan0", `{"band":"5 GHz","channel":36,"width":20}`, "band"},
		{"wlan0", `{"band":"2.4 GHz","channel":6,"width":80}`, "width"},
		{"wlan0", `{"band":"2.4 GHz","channel":7,"width":20}`, "channel"},
		{"wlan1", `{"band":"5 GHz","channel":165,"width":20}`, "channel"}, // disabled here
		{"wlan0", `{"band":"2.4 GHz","channel":6,"width":0}`, "width"},
		{"wlan4", `{"band":"2.4 GHz","channel":6,"width":40}`, "width"},
	}
	for _, c := range cases {
		rec := e.setRadio(c.name, c.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[c.field] {
			t.Errorf("%s %s: %d %s", c.name, c.body, rec.Code, rec.Body)
		}
	}
	// channels that need the country to be known, or that share spectrum with radar, are not refused here
	for _, ch := range []string{"52", "149"} {
		if rec := e.setRadio("wlan1", `{"band":"5 GHz","channel":`+ch+`,"width":20}`); rec.Code != http.StatusOK {
			t.Errorf("channel %s: %d %s", ch, rec.Code, rec.Body)
		}
	}
}

func TestAnAdapterCanBeLeftAlone(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan1", radio5)
	rec := e.wifiCall(e.api.HandleWifiRadio, http.MethodDelete, "wlan1", "", "")
	if rec.Code != http.StatusOK || len(decodeInto[wifiResponse](t, rec).Radios) != 0 || e.draft().Wifi != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestAddingAWifiNetworkFillsInWhatIsLeftOut(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan0", radio24)
	e.setRadio("wlan1", radio5)
	rec := e.addSSID(home)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	n := decodeInto[WifiNetwork](t, rec)
	if n.ID != "wifi1" || n.SSID != "Home" || n.Network != "lan" || n.Security != config.SecurityWPA2WPA3 || !n.BandSteering ||
		n.Hidden || n.ClientIsolation || !n.Enabled || !n.Pending || len(n.Bands) != 2 || n.Passphrase != "correct horse battery" {
		t.Fatalf("%+v", n)
	}
	if len(e.mgr.NetworkConfig().WifiSettings().Networks) != 0 {
		t.Fatal("the active configuration changed")
	}
	// a network on one band has nothing to steer between
	one := decodeInto[WifiNetwork](t, e.addSSID(`{"ssid":"Guest","passphrase":"another passphrase","bands":["5 GHz"]}`))
	if one.ID != "wifi2" || one.BandSteering {
		t.Fatalf("%+v", one)
	}
	if got := e.wifi().Networks; len(got) != 2 || got[0].ID != "wifi1" {
		t.Fatalf("%+v", got)
	}
}

func TestBadWifiNetworksNameTheirFields(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan0", radio24)
	rec := e.addSSID(`{"ssid":"","passphrase":"short","bands":["2.4 GHz"],"network":"nope","security":"wep"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := errorFields(t, rec)
	for _, want := range []string{"ssid", "passphrase", "network", "security"} {
		if !f[want] {
			t.Errorf("no error for %s: %v", want, f)
		}
	}
	if len(e.draft().Wifi.Networks) != 0 {
		t.Fatalf("a bad network was staged: %+v", e.draft().Wifi)
	}
}

func TestTheNameOfAWifiNetworkMustBeUnique(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan0", radio24)
	e.addSSID(`{"ssid":"Home","passphrase":"correct horse battery","bands":["2.4 GHz"]}`)
	rec := e.addSSID(`{"ssid":"Home","passphrase":"another passphrase","bands":["2.4 GHz"]}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["ssid"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestAnAdapterCannotRunMoreNetworksThanItAllows(t *testing.T) {
	e := wifiEnv(t) // wlan0 can run one network
	e.setRadio("wlan0", radio24)
	e.addSSID(`{"ssid":"One","passphrase":"correct horse battery","bands":["2.4 GHz"]}`)
	rec := e.addSSID(`{"ssid":"Two","passphrase":"correct horse battery","bands":["2.4 GHz"]}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["bands"] || !strings.Contains(rec.Body.String(), "at most 1 Wi-Fi network") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.draft().Wifi.Networks) != 1 {
		t.Fatal("the extra network was staged")
	}
	// wlan1 runs two
	e.setRadio("wlan1", radio5)
	for _, s := range []string{"A", "B"} {
		if rec := e.addSSID(`{"ssid":"` + s + `","passphrase":"correct horse battery","bands":["5 GHz"]}`); rec.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", s, rec.Code, rec.Body)
		}
	}
	if rec := e.addSSID(`{"ssid":"C","passphrase":"correct horse battery","bands":["5 GHz"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestChangingAWifiNetwork(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan0", radio24)
	e.setRadio("wlan1", radio5)
	e.addSSID(home)
	e.commit()
	if w := e.wifi(); len(w.Networks) != 1 || w.Networks[0].Pending {
		t.Fatalf("%+v", w.Networks)
	}
	// the password is left as it is when the request does not mention it
	rec := e.ssid(http.MethodPut, "wifi1", `{"ssid":"Home 2","hidden":true,"band_steering":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	n := decodeInto[WifiNetwork](t, rec)
	if n.SSID != "Home 2" || !n.Hidden || n.BandSteering || n.Passphrase != "correct horse battery" || !n.Pending || len(n.Bands) != 2 {
		t.Fatalf("%+v", n)
	}
	// putting the old values back is no change
	n = decodeInto[WifiNetwork](t, e.ssid(http.MethodPut, "wifi1", `{"ssid":"Home","hidden":false,"band_steering":true}`))
	if n.Pending {
		t.Fatalf("%+v", n)
	}
	n = decodeInto[WifiNetwork](t, e.ssid(http.MethodPut, "wifi1", `{"passphrase":"a brand new passphrase","enabled":false,"client_isolation":true,"security":"wpa3","bands":["5 GHz"],"band_steering":false}`))
	if n.Passphrase != "a brand new passphrase" || n.Enabled || !n.ClientIsolation || n.Security != config.SecurityWPA3 || len(n.Bands) != 1 || !n.Pending {
		t.Fatalf("%+v", n)
	}
}

func TestAWifiNetworkEditWithBadValuesChangesNothing(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan0", radio24)
	e.addSSID(`{"ssid":"Home","passphrase":"correct horse battery","bands":["2.4 GHz"]}`)
	rec := e.ssid(http.MethodPut, "wifi1", `{"passphrase":"x","ssid":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := errorFields(t, rec)
	if !f["passphrase"] || !f["ssid"] {
		t.Fatalf("%v", f)
	}
	n := e.draft().Wifi.Networks[0]
	if n.SSID != "Home" || n.Passphrase != "correct horse battery" {
		t.Fatalf("%+v", n)
	}
}

func TestEditingAWifiNetworkIgnoresAnotherNetworksProblems(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan0", radio24)
	e.addSSID(`{"ssid":"One","passphrase":"correct horse battery","bands":["2.4 GHz"]}`)
	// make the first one invalid behind the API's back: an unknown network
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error { c.Wifi.Networks[0].Network = "gone"; return nil })
	e.setRadio("wlan1", radio5)
	rec := e.addSSID(`{"ssid":"Two","passphrase":"correct horse battery","bands":["5 GHz"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestDeletingAWifiNetwork(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan1", radio5)
	e.addSSID(`{"ssid":"A","passphrase":"correct horse battery","bands":["5 GHz"]}`)
	e.addSSID(`{"ssid":"B","passphrase":"correct horse battery","bands":["5 GHz"]}`)
	rec := e.ssid(http.MethodDelete, "wifi1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := decodeInto[wifiResponse](t, rec).Networks; len(got) != 1 || got[0].ID != "wifi2" {
		t.Fatalf("%+v", got)
	}
	// the lowest free id is used again
	if n := decodeInto[WifiNetwork](t, e.addSSID(`{"ssid":"C","passphrase":"correct horse battery","bands":["5 GHz"]}`)); n.ID != "wifi1" {
		t.Fatalf("%+v", n)
	}
	if rec := e.ssid(http.MethodDelete, "wifi9", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.ssid(http.MethodPut, "wifi9", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestALocalNetworkCannotBeDeletedWhileAWifiNetworkJoinsIt(t *testing.T) {
	e := wifiEnv(t)
	e.create(newGuest)
	e.setRadio("wlan1", radio5)
	e.addSSID(`{"ssid":"Guest","passphrase":"correct horse battery","bands":["5 GHz"],"network":"net2"}`)
	rec := e.net(http.MethodDelete, "net2", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `Guest`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.ssid(http.MethodDelete, "wifi1", "")
	if rec := e.net(http.MethodDelete, "net2", ""); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestWifiWritesAreRefusedWhileApplyingAndRequestsAreStrict(t *testing.T) {
	e := wifiEnv(t)
	e.editable = false
	if rec := e.setCountry(`{"country":"US"}`); rec.Code != http.StatusConflict {
		t.Fatalf("country %d", rec.Code)
	}
	if rec := e.setRadio("wlan0", radio24); rec.Code != http.StatusConflict {
		t.Fatalf("radio %d", rec.Code)
	}
	if rec := e.addSSID(home); rec.Code != http.StatusConflict {
		t.Fatalf("add %d", rec.Code)
	}
	if e.wifi().Editable {
		t.Fatal("editable while applying")
	}
	e.editable = true
	if rec := e.addSSID(`{"ssid":"x","colour":"red"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field %d", rec.Code)
	}
	if rec := e.setCountry(`{"country":"US"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	if rec := e.wifiCall(e.api.HandleWifi, http.MethodPost, "", "", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method %d", rec.Code)
	}
}

func TestAnAdapterThatDoesNotStateALimitAndTheListLimit(t *testing.T) {
	e := wifiEnv(t)
	e.api.Wireless = func(context.Context) wireless.Report {
		r := fakeWireless()
		r.Adapters[1].MaxAP = 0 // wlan1 does not say how many networks it can run
		return r
	}
	e.setRadio("wlan1", radio5)
	for i := 1; i <= config.MaxWifiNetworks; i++ {
		body := `{"ssid":"Net ` + itoa(i) + `","passphrase":"correct horse battery","bands":["5 GHz"]}`
		if rec := e.addSSID(body); rec.Code != http.StatusCreated {
			t.Fatalf("network %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := e.addSSID(`{"ssid":"One too many","passphrase":"correct horse battery","bands":["5 GHz"]}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["networks"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.draft().Wifi.Networks) != config.MaxWifiNetworks {
		t.Fatal("the extra network was staged")
	}
}

func TestChangingAWifiNetworkIsRefusedWhileApplying(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan1", radio5)
	e.addSSID(`{"ssid":"A","passphrase":"correct horse battery","bands":["5 GHz"]}`)
	e.editable = false
	for _, m := range []string{http.MethodPut, http.MethodDelete} {
		if rec := e.ssid(m, "wifi1", `{"ssid":"B"}`); rec.Code != http.StatusConflict {
			t.Fatalf("%s: %d %s", m, rec.Code, rec.Body)
		}
	}
	if e.draft().Wifi.Networks[0].SSID != "A" || len(e.draft().Wifi.Networks) != 1 {
		t.Fatal("changed while applying")
	}
}

func TestAnAdapterIsPendingOnlyWhileItDiffersFromTheRunningSetup(t *testing.T) {
	e := wifiEnv(t)
	e.setRadio("wlan1", radio5)
	e.addSSID(`{"ssid":"A","passphrase":"correct horse battery","bands":["5 GHz"]}`)
	e.setCountry(`{"country":"US"}`)
	e.commit()
	if r := e.wifi().Radios[0]; r.Pending {
		t.Fatalf("pending right after the commit: %+v", r)
	}
	e.setRadio("wlan1", `{"band":"5 GHz","channel":36,"width":80}`)
	if r := e.wifi().Radios[0]; !r.Pending || r.Channel != 36 {
		t.Fatalf("%+v", r)
	}
	e.setRadio("wlan1", radio5)
	if r := e.wifi().Radios[0]; r.Pending {
		t.Fatalf("put back, still pending: %+v", r)
	}
}

func TestUndoingEveryWifiChangeLeavesNoEmptySectionInTheDraft(t *testing.T) {
	e := wifiEnv(t)
	e.create(newGuest) // another change keeps the draft alive
	e.setRadio("wlan1", radio5)
	e.wifiCall(e.api.HandleWifiRadio, http.MethodDelete, "wlan1", "", "")
	e.setRadio("wlan1", radio5)
	e.addSSID(`{"ssid":"A","passphrase":"correct horse battery","bands":["5 GHz"]}`)
	e.ssid(http.MethodDelete, "wifi1", "")
	e.wifiCall(e.api.HandleWifiRadio, http.MethodDelete, "wlan1", "", "")
	if w := e.draft().Wifi; w != nil {
		t.Fatalf("%+v", w)
	}
}

func TestAWifiNetworkSurvivesRemovingTheAdapters(t *testing.T) {
	e := wifiEnv(t)
	// a network can be drafted before an adapter is chosen: the commit is what insists on one
	if rec := e.addSSID(`{"ssid":"A","passphrase":"correct horse battery","bands":["5 GHz"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.setRadio("wlan1", radio5)
	e.wifiCall(e.api.HandleWifiRadio, http.MethodDelete, "wlan1", "", "")
	if w := e.draft().Wifi; w == nil || len(w.Networks) != 1 {
		t.Fatalf("%+v", w)
	}
}

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

func TestTheDefaultGatewayFromTheRoutingTable(t *testing.T) {
	table := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n" +
		"br0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"br0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"wlan0\t00000000\t0100000A\t0003\t0\t0\t100\t00000000\t0\t0\t0\n"
	if got := ParseRouteGateway(table, "br0"); got != "192.168.1.1" {
		t.Fatalf("%q", got)
	}
	if got := ParseRouteGateway(table, "wlan0"); got != "10.0.0.1" {
		t.Fatalf("%q", got)
	}
	if got := ParseRouteGateway(table, ""); got != "192.168.1.1" {
		t.Fatalf("the first default route: %q", got)
	}
	for _, in := range []string{"", "Iface\n", "Iface\nbr0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n", "Iface\nbr0\t00000000\tZZZZZZZZ\t0003\t0\t0\t0\t00000000\n"} {
		if got := ParseRouteGateway(in, "br0"); got != "" {
			t.Errorf("%q: %q", in, got)
		}
	}
	if got := ParseRouteGateway(table, "eth9"); got != "" {
		t.Fatalf("a device with no default route: %q", got)
	}
}

func TestTheNameServersOfResolvConf(t *testing.T) {
	got := ParseResolvConf("# comment\nsearch lan\nnameserver 192.168.1.1\nnameserver 1.1.1.1\noptions edns0\nnameserver\n")
	if len(got) != 2 || got[0] != "192.168.1.1" || got[1] != "1.1.1.1" {
		t.Fatalf("%v", got)
	}
	if ParseResolvConf("") != nil {
		t.Fatal("garbage")
	}
}

func apEnv(t *testing.T) *env {
	e := wifiEnv(t)
	e.ap(http.MethodPut, `{"mode":"ap","ports":["eth0","eth1"]}`)
	e.setRadio("wlan1", radio5)
	e.addSSID(`{"ssid":"Home","passphrase":"correct horse battery","bands":["5 GHz"]}`)
	e.addSSID(`{"ssid":"Guest","passphrase":"correct horse battery","bands":["5 GHz"],"vlan":20}`)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error { c.Country = "US"; return nil })
	e.commit()
	return e
}

func sig(n int) *int { return &n }

func (e *env) apStatus() (int, apStatusResponse, string) {
	rec := e.do(e.api.HandleAPStatus, http.MethodGet, "", "")
	var v apStatusResponse
	if rec.Code == 200 {
		v = decodeInto[apStatusResponse](e.t, rec)
	}
	return rec.Code, v, rec.Body.String()
}

func TestTheHomePageOfAnAccessPoint(t *testing.T) {
	e := apEnv(t)
	e.ports = []config.SystemInterface{hw("eth0", "up"), hw("eth1", "down")}
	e.api.Bridge = func() ([]string, bool) { return []string{"192.168.1.2/24", config.APRecoveryAddress}, true }
	e.api.Gateway = func() string { return "192.168.1.1" }
	e.api.Resolvers = func() []string { return []string{"192.168.1.1"} }
	e.api.WifiDev = func(context.Context) ([]wireless.Interface, bool) {
		return []wireless.Interface{{Name: "wlan1", Type: "AP", SSID: "Home", Channel: 36, MHz: 5180, Width: 80, TxPowerDBM: 20, HaveTxPower: true}}, true
	}
	e.api.Survey = func(context.Context, string) (wireless.Survey, bool) {
		return wireless.Survey{NoiseDBm: -94, HaveNoise: true, ActiveMS: 1000, BusyMS: 250, InUse: true}, true
	}
	e.api.Stations = func(context.Context) wireless.StationReport {
		return wireless.StationReport{Known: true, Stations: []wireless.Station{
			{MAC: "aa:bb:cc:00:00:01", Interface: "wlan1", SSID: "Home", Signal: sig(-50)},
			{MAC: "aa:bb:cc:00:00:02", Interface: "wlan1", SSID: "Home", Signal: sig(-70)},
			{MAC: "aa:bb:cc:00:00:03", Interface: "wlan1_1", SSID: "Guest", Signal: sig(-60)},
		}}
	}
	code, v, raw := e.apStatus()
	if code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	a := v.Address
	if !a.Up || a.Mode != "dhcp" || len(a.Addresses) != 1 || a.Addresses[0] != "192.168.1.2/24" || !a.Recovery || a.Gateway != "192.168.1.1" || len(a.DNS) != 1 {
		t.Fatalf("%+v", a)
	}
	if len(v.Ports) != 2 || v.Ports[0].State != "up" || v.Ports[1].State != "down" || v.Ports[0].Kind != "port" {
		t.Fatalf("%+v", v.Ports)
	}
	if len(v.Radios) != 1 {
		t.Fatalf("%+v", v.Radios)
	}
	r := v.Radios[0]
	if !r.Up || r.Channel != 36 || r.Width != 80 || r.TxPowerDBm == nil || *r.TxPowerDBm != 20 || r.NoiseDBm == nil || *r.NoiseDBm != -94 || r.BusyPercent == nil || *r.BusyPercent != 25 {
		t.Fatalf("%+v", r)
	}
	// the devices behind each adapter, with their average signal; the second network is on name_1
	if r.Clients != 3 || r.AvgSignalDBm == nil || *r.AvgSignalDBm != -60 {
		t.Fatalf("%+v", r)
	}
	if len(v.Networks) != 2 || v.Networks[0].SSID != "Guest" || v.Networks[0].Clients != 1 || v.Networks[0].VLAN != 20 || v.Networks[1].SSID != "Home" || v.Networks[1].Clients != 2 {
		t.Fatalf("%+v", v.Networks)
	}
	if !v.DevicesKnown || len(v.Devices) != 3 || v.Pending {
		t.Fatalf("%+v", v)
	}
}

func TestAnAdapterThatIsNotRunningAndABridgeThatIsDown(t *testing.T) {
	e := apEnv(t)
	e.api.Bridge = func() ([]string, bool) { return nil, false }
	e.api.WifiDev = func(context.Context) ([]wireless.Interface, bool) { return nil, true }
	e.api.Survey = func(context.Context, string) (wireless.Survey, bool) {
		t.Fatal("measured an adapter that is not running")
		return wireless.Survey{}, false
	}
	_, v, _ := e.apStatus()
	if v.Address.Up || len(v.Address.Addresses) != 0 || v.Address.Recovery {
		t.Fatalf("%+v", v.Address)
	}
	if len(v.Radios) != 1 || v.Radios[0].Up || v.Radios[0].BusyPercent != nil || v.Radios[0].Channel != 0 {
		t.Fatalf("%+v", v.Radios)
	}
	// nothing is known without the Wi-Fi tools: said so, not "nobody is connected"
	if v.DevicesKnown || len(v.Devices) != 0 {
		t.Fatalf("%+v", v)
	}
}

func TestHowBusyTheChannelWasSinceTheLastLook(t *testing.T) {
	var a API
	first := a.busyPercent("wlan0", wireless.Survey{ActiveMS: 10000, BusyMS: 1000})
	if first == nil || *first != 10 {
		t.Fatalf("since the adapter started: %v", first)
	}
	// 5 s later it was busy for 4 s of them
	second := a.busyPercent("wlan0", wireless.Survey{ActiveMS: 15000, BusyMS: 5000})
	if second == nil || *second != 80 {
		t.Fatalf("since the last look: %v", second)
	}
	// the channel changed (the counters start again): the total of the new one
	third := a.busyPercent("wlan0", wireless.Survey{ActiveMS: 2000, BusyMS: 500})
	if third == nil || *third != 25 {
		t.Fatalf("%v", third)
	}
	if a.busyPercent("wlan1", wireless.Survey{}) != nil {
		t.Fatal("a reading of nothing")
	}
	if over := a.busyPercent("wlan2", wireless.Survey{ActiveMS: 100, BusyMS: 500}); over == nil || *over != 100 {
		t.Fatalf("%v", over)
	}
}

func TestALinkThatIsConnectedOrNot(t *testing.T) {
	e := apEnv(t)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.Wifi.Stations = []config.WifiStation{{Name: "wlan1", SSID: "Main", Security: config.SecurityWPA2, Passphrase: "correct horse battery", Role: config.StationBridge, Network: "lan"}}
		return nil
	})
	e.commit()
	e.api.Link = func(context.Context, string) (wireless.Link, bool) {
		return wireless.Link{Connected: true, SignalDBm: sig(-48), RxMbps: 433, TxMbps: 866, Band: "5 GHz"}, true
	}
	_, v, _ := e.apStatus()
	var link *APPortStatus
	for i := range v.Ports {
		if v.Ports[i].Kind == "link" {
			link = &v.Ports[i]
		}
	}
	if link == nil || link.Name != "wlan1" || link.SSID != "Main" || link.Connected == nil || !*link.Connected || *link.SignalDBm != -48 || link.TxMbps != 866 {
		t.Fatalf("%+v", v.Ports)
	}
	e.api.Link = func(context.Context, string) (wireless.Link, bool) { return wireless.Link{}, true }
	_, v, _ = e.apStatus()
	for _, p := range v.Ports {
		if p.Kind == "link" && (p.Connected == nil || *p.Connected) {
			t.Fatalf("%+v", p)
		}
	}
}

func TestOnlyAnAccessPointHasAHomePage(t *testing.T) {
	e := wifiEnv(t) // a router
	code, _, raw := e.apStatus()
	if code != http.StatusConflict || !strings.Contains(raw, "not an access point") {
		t.Fatalf("%d %s", code, raw)
	}
	if rec := e.do(e.api.HandleAPStatus, http.MethodPost, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
}

func TestTheRoutersPagesAnswerARouterAndRefuseAnAccessPoint(t *testing.T) {
	isAP := false
	called := 0
	h := RouterOnly(func() bool { return isAP }, func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != 204 || called != 1 {
		t.Fatalf("%d %d", rec.Code, called)
	}
	isAP = true
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPost} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(m, "/x", nil))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "access point") {
			t.Fatalf("%s: %d %s", m, rec.Code, rec.Body.String())
		}
	}
	if called != 1 {
		t.Fatal("the handler ran for an access point")
	}
}

func TestWhatTheDeviceIsShownAs(t *testing.T) {
	router := config.NetworkConfig{WanInterface: "eth0", LanInterfaces: []string{"eth1"}}
	ap := config.NetworkConfig{AP: &config.APConfig{Enabled: true, Ports: []string{"eth0"}}}
	blank := config.NetworkConfig{}
	for name, c := range map[string]struct {
		active, staged config.NetworkConfig
		has            bool
		want           string
	}{
		"a router":                            {router, router, false, "ROUTER"},
		"an access point":                     {ap, ap, false, "AP"},
		"fresh from setup as an access point": {blank, ap, true, "AP"},
		"fresh from setup as a router":        {blank, router, true, "ROUTER"},
		"a router with a draft that makes it an access point": {router, ap, true, "ROUTER"},
		"an access point with a draft back to a router":       {ap, router, true, "AP"},
		"nothing at all": {blank, blank, false, "ROUTER"},
	} {
		if got := config.ShownMode(c.active, c.staged, c.has); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

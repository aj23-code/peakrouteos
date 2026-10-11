package netapi

import (
	"net/http"
	"net/http/httptest"
	"peakroute/internal/config"
	"strings"
	"testing"
)

func (e *env) uplink(method, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetPathValue("id", "wan")
	rec := httptest.NewRecorder()
	e.api.HandleUplink(rec, req)
	return rec
}

const staticWAN = `{"mode":"static","ip":"203.0.113.10","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":["9.9.9.9","149.112.112.112"]}`

func TestUplinksListsTheInternetConnection(t *testing.T) {
	e := standard(t)
	rec := e.get(e.api.HandleUplinks)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	r := decodeInto[uplinksResponse](t, rec)
	if !r.Editable || len(r.Uplinks) != 1 {
		t.Fatalf("%+v", r)
	}
	u := r.Uplinks[0]
	if u.ID != "wan" || u.Name != "Internet" || u.Port != "eth0" || u.Mode != "dhcp" || u.Pending || u.Live != nil {
		t.Errorf("%+v", u)
	}
	if !strings.Contains(rec.Body.String(), `"dns":[]`) {
		t.Error("dns should be an empty list, not null")
	}
}

func TestEmptyModeCountsAsDHCP(t *testing.T) {
	c := baseline()
	c.WanMode = ""
	e := newEnv(t, &c)
	if m := decodeInto[Uplink](t, e.uplink(http.MethodGet, "")).Mode; m != "dhcp" {
		t.Fatalf("mode %q", m)
	}
}

func TestUplinkLiveStatusDescribesTheActivePort(t *testing.T) {
	e := standard(t)
	var asked string
	e.api.Live = func(c config.NetworkConfig) *Live {
		asked = c.WanInterface
		return &Live{Up: true, Address: "203.0.113.7/24", Gateway: "203.0.113.1", DNS: []string{"1.1.1.1"}}
	}
	// the draft moves the WAN to another port; the live view must still describe the one in effect
	if rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth0":"lan","eth1":"wan"}}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	u := decodeInto[Uplink](t, e.uplink(http.MethodGet, ""))
	if asked != "eth0" || u.Port != "eth1" || u.Live == nil || !u.Live.Up || u.Live.Address != "203.0.113.7/24" {
		t.Fatalf("asked %q, uplink %+v", asked, u)
	}
}

func TestUplinkWithoutAnActivePortHasNoLiveStatus(t *testing.T) {
	e := newEnv(t, nil)
	called := false
	e.api.Live = func(config.NetworkConfig) *Live { called = true; return &Live{} }
	if u := decodeInto[Uplink](t, e.uplink(http.MethodGet, "")); u.Live != nil || called {
		t.Fatalf("%+v called=%v", u, called)
	}
}

func TestSavingAStaticAddressStagesADraft(t *testing.T) {
	e := standard(t)
	rec := e.uplink(http.MethodPut, staticWAN)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	u := decodeInto[Uplink](t, rec)
	if u.Mode != "static" || u.IP != "203.0.113.10" || u.Gateway != "203.0.113.1" || len(u.DNS) != 2 || !u.Pending {
		t.Fatalf("%+v", u)
	}
	if e.mgr.NetworkConfig().WanMode != "dhcp" {
		t.Fatal("the active configuration changed: saving must only stage")
	}
	fields := map[string]bool{}
	for _, c := range e.staged() {
		fields[c.Field] = true
	}
	for _, f := range []string{"wan_mode", "wan_ip", "wan_subnet", "wan_gateway", "wan_dns"} {
		if !fields[f] {
			t.Errorf("no staged change for %s: %+v", f, e.staged())
		}
	}
}

func TestSwitchingBackToDHCPClearsTheStaticValues(t *testing.T) {
	c := baseline()
	c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway, c.WanDNS = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1", []string{"9.9.9.9"}
	e := newEnv(t, &c)

	rec := e.uplink(http.MethodPut, `{"mode":"dhcp","ip":"1.2.3.4","netmask":"x","gateway":"y"}`) // the static values sent along are ignored
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	u := decodeInto[Uplink](t, rec)
	if u.Mode != "dhcp" || u.IP != "" || u.Netmask != "" || u.Gateway != "" || len(u.DNS) != 0 || !u.Pending {
		t.Fatalf("%+v", u)
	}
	draft, _ := e.mgr.StagedNetworkConfig()
	if draft.WanIP != "" || draft.WanSubnet != "" || draft.WanGateway != "" || draft.WanDNS != nil {
		t.Fatalf("static leftovers in the draft: %+v", draft)
	}
}

func TestStaticAddressErrorsNameTheRightFields(t *testing.T) {
	e := standard(t)
	for name, tc := range map[string]struct{ body, field string }{
		"bad ip":             {`{"mode":"static","ip":"300.1.1.1","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":["9.9.9.9"]}`, "ip"},
		"network address":    {`{"mode":"static","ip":"203.0.113.0","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":["9.9.9.9"]}`, "ip"},
		"bad netmask":        {`{"mode":"static","ip":"203.0.113.10","netmask":"255.0.255.0","gateway":"203.0.113.1","dns":["9.9.9.9"]}`, "netmask"},
		"gateway elsewhere":  {`{"mode":"static","ip":"203.0.113.10","netmask":"255.255.255.0","gateway":"198.51.100.1","dns":["9.9.9.9"]}`, "gateway"},
		"missing gateway":    {`{"mode":"static","ip":"203.0.113.10","netmask":"255.255.255.0","gateway":"","dns":["9.9.9.9"]}`, "gateway"},
		"bad dns":            {`{"mode":"static","ip":"203.0.113.10","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":["nope"]}`, "dns"},
		"missing dns":        {`{"mode":"static","ip":"203.0.113.10","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":[]}`, "dns"},
		"only blank dns":     {`{"mode":"static","ip":"203.0.113.10","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":["  ",""]}`, "dns"},
		"four dns":           {`{"mode":"static","ip":"203.0.113.10","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":["1.1.1.1","8.8.8.8","9.9.9.9","8.8.4.4"]}`, "dns"},
		"no mode":            {`{"ip":"203.0.113.10"}`, "mode"},
		"unknown mode":       {`{"mode":"l2tp"}`, "mode"},
		"overlaps the LAN":   {`{"mode":"static","ip":"192.168.1.50","netmask":"255.255.255.0","gateway":"192.168.1.1","dns":["9.9.9.9"]}`, "netmask"},
		"newline in the dns": {`{"mode":"static","ip":"203.0.113.10","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":["9.9.9.9\nnameserver 6.6.6.6"]}`, "dns"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.uplink(http.MethodPut, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if f := errorFields(t, rec); !f[tc.field] {
				t.Fatalf("errors %v lack %q", f, tc.field)
			}
			if e.stageFileExists() {
				t.Fatal("an invalid save left a draft behind")
			}
		})
	}
}

func TestUplinkEditDoesNotJudgeOtherPages(t *testing.T) {
	// A half-set-up box (no LAN yet) can still have its internet connection edited
	c := baseline()
	c.LanInterfaces, c.LanIP, c.LanSubnet = nil, "", ""
	e := newEnv(t, nil)
	e.mgr.StageNetwork(func(n *config.NetworkConfig) error { *n = c; return nil })
	rec := e.uplink(http.MethodPut, staticWAN)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUplinkRejectsUnknownFieldsAndWrongTypes(t *testing.T) {
	e := standard(t)
	if rec := e.uplink(http.MethodPut, `{"mode":"dhcp","port":"eth1"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(`{"mode":"dhcp"}`))
	req.SetPathValue("id", "wan")
	rec := httptest.NewRecorder()
	e.api.HandleUplink(rec, req) // no JSON content type
	if rec.Code == http.StatusOK {
		t.Errorf("accepted a body without a JSON content type")
	}
}

func TestUplinkEditsAreRefusedWhileApplying(t *testing.T) {
	e := standard(t)
	e.editable = false
	if rec := e.uplink(http.MethodPut, staticWAN); rec.Code != http.StatusConflict {
		t.Fatalf("%d", rec.Code)
	}
	if e.stageFileExists() {
		t.Fatal("a refused edit left a draft")
	}
	if r := decodeInto[uplinksResponse](t, e.get(e.api.HandleUplinks)); r.Editable {
		t.Fatal("editable should be false")
	}
}

func TestUnknownUplinkAndWrongMethods(t *testing.T) {
	e := standard(t)
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.SetPathValue("id", "wan2")
	rec := httptest.NewRecorder()
	e.api.HandleUplink(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id %d", rec.Code)
	}
	if rec := e.uplink(http.MethodDelete, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE %d", rec.Code)
	}
	if rec := e.do(e.api.HandleUplinks, http.MethodPost, "{}", "application/json"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST list %d", rec.Code)
	}
}

func TestPuttingTheSameValuesBackLeavesNoDraft(t *testing.T) {
	e := standard(t)
	if rec := e.uplink(http.MethodPut, `{"mode":"dhcp"}`); rec.Code != http.StatusOK {
		t.Fatalf("%d", rec.Code)
	}
	if e.stageFileExists() || len(e.staged()) != 0 {
		t.Fatal("saving what is already in effect created a draft")
	}
}

func TestStaticValuesAreTrimmedAndBlankDNSRowsDropped(t *testing.T) {
	e := standard(t)
	rec := e.uplink(http.MethodPut, `{"mode":"static","ip":" 203.0.113.10 ","netmask":"255.255.255.0 ","gateway":" 203.0.113.1","dns":[" 9.9.9.9 ",""," ","149.112.112.112"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	u := decodeInto[Uplink](t, rec)
	if u.IP != "203.0.113.10" || u.Gateway != "203.0.113.1" || len(u.DNS) != 2 || u.DNS[0] != "9.9.9.9" || u.DNS[1] != "149.112.112.112" {
		t.Fatalf("%+v", u)
	}
}

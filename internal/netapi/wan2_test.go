package netapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// uplinkOf is the handler for /api/v1/uplinks/{id}.
func (e *env) uplinkOf(id, method, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	e.api.HandleUplink(rec, req)
	return rec
}

func TestAPortCanBeGivenTheSecondInternetRoleAndTakenOffAgain(t *testing.T) {
	e := standard(t)
	if rec := e.uplinkOf("wan2", http.MethodGet, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("a second connection that is not set up: %d", rec.Code)
	}
	if rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"wan2"}}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if !staged.HasWan2() || staged.Wan2.Interface != "eth2" || staged.WanInterface != "eth0" {
		t.Fatalf("%+v", staged.Wan2)
	}
	ports := decodeInto[portsResponse](t, e.get(e.api.HandlePorts))
	if p := portByName(t, ports.Ports, "eth2"); p.Role != "wan2" || !p.Pending || p.ActiveRole != "unassigned" {
		t.Errorf("eth2: %+v", p)
	}
	// both connections are listed, the second with its own name
	list := decodeInto[uplinksResponse](t, e.get(e.api.HandleUplinks)).Uplinks
	if len(list) != 2 || list[0].ID != "wan" || list[1].ID != "wan2" || list[1].Name != "Internet 2" || list[1].Port != "eth2" || list[1].Mode != "dhcp" {
		t.Fatalf("%+v", list)
	}
	if !list[0].Preferred || list[1].Preferred {
		t.Errorf("the first connection is the preferred one until the other is chosen: %+v", list)
	}
	// the port is taken off again: no second connection, and no preference for it
	if rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"unassigned"}}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}
	if staged, _ = e.mgr.StagedNetworkConfig(); staged.Wan2 != nil || staged.HasWan2() {
		t.Errorf("%+v", staged.Wan2)
	}
}

func TestOnlyOnePortIsTheSecondInternetAndNotTheSameAsAnotherRole(t *testing.T) {
	e := standard(t)
	e.ports = append(e.ports, hw("eth3", "up"))
	rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"wan2","eth3":"wan2"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "only one port can be WAN 2") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// a port has one role: giving the WAN port the second role leaves no first connection
	rec = e.putJSON(e.api.HandlePorts, `{"assignments":{"eth0":"wan2"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "one port must be the WAN") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if staged, _ := e.mgr.StagedNetworkConfig(); staged.Wan2 != nil {
		t.Error("a refused change was staged")
	}
}

func TestTheSecondConnectionHasItsOwnSettingsAndTheFirstIsLeftAlone(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"wan2"}}`)
	rec := e.uplinkOf("wan2", http.MethodPut, `{"mode":"static","ip":"198.51.100.7","netmask":"255.255.255.0","gateway":"198.51.100.1","dns":["9.9.9.9"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}
	u := decodeInto[Uplink](t, rec)
	if u.ID != "wan2" || u.Mode != "static" || u.IP != "198.51.100.7" || u.Gateway != "198.51.100.1" || !u.Pending {
		t.Errorf("%+v", u)
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.WanMode != "dhcp" || staged.WanIP != "" {
		t.Errorf("the first connection changed: %q %q", staged.WanMode, staged.WanIP)
	}
	if staged.Wan2.Mode != "static" || staged.Wan2.Interface != "eth2" {
		t.Errorf("%+v", staged.Wan2)
	}
	first := decodeInto[Uplink](t, e.uplinkOf("wan", http.MethodGet, ""))
	if first.Mode != "dhcp" {
		t.Errorf("%+v", first)
	}
	// the review names it
	fields := map[string]bool{}
	for _, c := range e.staged() {
		fields[c.Field] = true
	}
	if !fields["wan2"] || !fields["wan2.ip"] || fields["wan_ip"] {
		t.Errorf("staged: %+v", e.staged())
	}
}

func TestTheSecondConnectionsMistakesAreReportedUnderTheNamesOfTheForm(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"wan2"}}`)
	// a static address inside the LAN's own subnet
	rec := e.uplinkOf("wan2", http.MethodPut, `{"mode":"static","ip":"192.168.1.9","netmask":"255.255.255.0","gateway":"192.168.1.1","dns":["9.9.9.9"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"netmask"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if staged, _ := e.mgr.StagedNetworkConfig(); staged.Wan2 != nil && staged.Wan2.Mode == "static" {
		t.Error("a refused change was staged")
	}
	// and the same subnet as the first connection
	e.uplinkOf("wan", http.MethodPut, staticWAN)
	rec = e.uplinkOf("wan2", http.MethodPut, `{"mode":"static","ip":"203.0.113.20","netmask":"255.255.255.0","gateway":"203.0.113.1","dns":["9.9.9.9"]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "first connection") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestPreferringTheSecondConnectionIsADraftChange(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"wan2"}}`)
	if rec := e.uplinkOf("wan2", http.MethodPut, `{"mode":"dhcp","preferred":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}
	list := decodeInto[uplinksResponse](t, e.get(e.api.HandleUplinks)).Uplinks
	if list[0].Preferred || !list[1].Preferred {
		t.Fatalf("%+v", list)
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.WanPreferred != "wan2" {
		t.Fatalf("%q", staged.WanPreferred)
	}
	// and back to the first
	e.uplinkOf("wan", http.MethodPut, `{"mode":"dhcp","preferred":true}`)
	if staged, _ = e.mgr.StagedNetworkConfig(); staged.WanPreferred != "" {
		t.Fatalf("%q", staged.WanPreferred)
	}
}

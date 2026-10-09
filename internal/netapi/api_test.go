package netapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

type env struct {
	t        *testing.T
	mgr      *config.Manager
	api      *API
	dir      string
	ports    []config.SystemInterface
	portsErr error
	editable bool
}

func hw(name, state string) config.SystemInterface {
	return config.SystemInterface{Name: name, MAC: "aa:bb:cc:00:00:" + name[len(name)-1:] + "1", State: state, Speed: "1000 Mbps", MTU: 1500, Driver: "igb"}
}

func baseline() config.NetworkConfig {
	return config.NetworkConfig{
		WanInterface:  "eth0",
		WanMode:       "dhcp",
		LanInterfaces: []string{"eth1"},
		LanIP:         "192.168.1.1",
		LanSubnet:     "255.255.255.0",
		DHCP:          &config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"},
	}
}

func newEnv(t *testing.T, committed *config.NetworkConfig) *env {
	t.Helper()
	dir := t.TempDir()
	mgr, err := config.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if committed != nil {
		if err := mgr.StageNetwork(func(c *config.NetworkConfig) error { *c = *committed; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := mgr.CommitNetwork(); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t, mgr: mgr, dir: dir, editable: true,
		ports: []config.SystemInterface{hw("eth0", "up"), hw("eth1", "up"), hw("eth2", "down")}}
	e.api = &API{
		Manager:  mgr,
		Ports:    func() ([]config.SystemInterface, error) { return e.ports, e.portsErr },
		Editable: func() bool { return e.editable },
	}
	return e
}

func standard(t *testing.T) *env { b := baseline(); return newEnv(t, &b) }

func (e *env) do(h http.HandlerFunc, method, body, contentType string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.SetPathValue("id", "lan")
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func (e *env) putJSON(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	return e.do(h, http.MethodPut, body, "application/json")
}

func (e *env) get(h http.HandlerFunc) *httptest.ResponseRecorder {
	return e.do(h, http.MethodGet, "", "")
}

func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not the expected JSON (status %d): %v\n%s", rec.Code, err, rec.Body.String())
	}
	return v
}

func errorFields(t *testing.T, rec *httptest.ResponseRecorder) map[string]bool {
	t.Helper()
	eb := decodeInto[errorBody](t, rec)
	out := map[string]bool{}
	for _, e := range eb.Errors {
		out[e.Field] = true
	}
	return out
}

func (e *env) staged() []config.Change { return e.mgr.StagedChanges() }

func (e *env) stageFileExists() bool {
	_, err := os.Stat(filepath.Join(e.dir, "network.json.staged"))
	return err == nil
}

// ---------------------------------------------------------------- networks

func TestNetworksListsTheLAN(t *testing.T) {
	e := standard(t)
	rec := e.get(e.api.HandleNetworks)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d headers %v", rec.Code, rec.Header())
	}

	resp := decodeInto[networksResponse](t, rec)
	if !resp.Editable || len(resp.Networks) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	n := resp.Networks[0]
	if n.ID != "lan" || n.Name != "LAN" || n.IP != "192.168.1.1" || n.Netmask != "255.255.255.0" ||
		len(n.Ports) != 1 || n.Ports[0] != "eth1" || n.Bridged || n.Pending {
		t.Errorf("network = %+v", n)
	}
	if n.DHCP == nil || !n.DHCP.Enabled || n.DHCP.RangeStart != "192.168.1.100" || n.DHCP.RangeEnd != "192.168.1.200" || n.DHCP.LeaseTime != "12h" {
		t.Errorf("dhcp = %+v", n.DHCP)
	}
}

func TestDhcpNotManagedIsNull(t *testing.T) {
	c := baseline()
	c.DHCP = nil
	e := newEnv(t, &c)
	n := decodeInto[networksResponse](t, e.get(e.api.HandleNetworks)).Networks[0]
	if n.DHCP != nil {
		t.Errorf("dhcp = %+v, want null for a configuration that does not manage it", n.DHCP)
	}
	if !strings.Contains(e.get(e.api.HandleNetworks).Body.String(), `"dhcp":null`) {
		t.Error("the JSON should carry an explicit null")
	}
}

func TestOneNetworkAndUnknownIdsAre404(t *testing.T) {
	e := standard(t)
	rec := e.get(e.api.HandleNetwork)
	if rec.Code != http.StatusOK || decodeInto[Network](t, rec).ID != "lan" {
		t.Errorf("GET lan = %d %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.SetPathValue("id", "vlan10")
	rec = httptest.NewRecorder()
	e.api.HandleNetwork(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", rec.Code)
	}
}

const moveLAN = `{"ip":"10.0.0.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"10.0.0.100","range_end":"10.0.0.200","lease_time":"12h"}}`

func TestSavingTheLANStagesADraftAndLeavesTheSystemAlone(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleNetwork, moveLAN)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}

	n := decodeInto[Network](t, rec)
	if n.IP != "10.0.0.1" || n.DHCP == nil || n.DHCP.RangeStart != "10.0.0.100" || !n.Pending {
		t.Errorf("response = %+v, want the saved values marked pending", n)
	}
	if got := e.mgr.NetworkConfig().LanIP; got != "192.168.1.1" {
		t.Errorf("the active config changed to %q: saving must only stage a draft", got)
	}

	fields := map[string]bool{}
	for _, c := range e.staged() {
		fields[c.Field] = true
	}
	for _, want := range []string{"lan_ip", "dhcp_range_start", "dhcp_range_end"} {
		if !fields[want] {
			t.Errorf("staged changes lack %s: %+v", want, e.staged())
		}
	}

	// the same draft is what a later GET shows
	if got := decodeInto[Network](t, e.get(e.api.HandleNetwork)); got.IP != "10.0.0.1" || !got.Pending {
		t.Errorf("GET after PUT = %+v", got)
	}
}

func TestSavingTheValuesAlreadyInEffectLeavesNoDraft(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleNetwork, `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"192.168.1.100","range_end":"192.168.1.200","lease_time":"12h"}}`)
	if rec.Code != http.StatusOK || decodeInto[Network](t, rec).Pending {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if e.stageFileExists() || len(e.staged()) != 0 {
		t.Error("saving identical values must not leave a pending change")
	}
}

func TestAnEditThatIsPutBackRemovesTheDraft(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandleNetwork, moveLAN)
	if !e.stageFileExists() {
		t.Fatal("expected a draft")
	}
	e.putJSON(e.api.HandleNetwork, `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"192.168.1.100","range_end":"192.168.1.200","lease_time":"12h"}}`)
	if e.stageFileExists() || len(e.staged()) != 0 {
		t.Error("putting the values back should clear the draft")
	}
}

func TestInvalidSettingsAreRejectedWithTheFieldAndNothingIsStaged(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"network address as the router address", `{"ip":"192.168.1.0","netmask":"255.255.255.0"}`, "ip"},
		{"broadcast address", `{"ip":"192.168.1.255","netmask":"255.255.255.0"}`, "ip"},
		{"not an address", `{"ip":"router","netmask":"255.255.255.0"}`, "ip"},
		{"empty address", `{"ip":"","netmask":"255.255.255.0"}`, "ip"},
		{"injection attempt in the address", `{"ip":"192.168.1.1\nauto evil","netmask":"255.255.255.0"}`, "ip"},
		{"bad netmask", `{"ip":"192.168.1.1","netmask":"255.0.255.0"}`, "netmask"},
		{"netmask as prefix length", `{"ip":"192.168.1.1","netmask":"24"}`, "netmask"},
		{"range outside the subnet", `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"10.9.9.5","range_end":"10.9.9.9","lease_time":"12h"}}`, "dhcp.range_start"},
		{"range reversed", `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"192.168.1.200","range_end":"192.168.1.100","lease_time":"12h"}}`, "dhcp.range_end"},
		{"range covers the router", `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"192.168.1.1","range_end":"192.168.1.50","lease_time":"12h"}}`, "dhcp.range_start"},
		{"lease too short", `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"192.168.1.100","range_end":"192.168.1.200","lease_time":"30s"}}`, "dhcp.lease_time"},
		{"lease with injection", `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"192.168.1.100","range_end":"192.168.1.200","lease_time":"12h\ndhcp-option=3,6.6.6.6"}}`, "dhcp.lease_time"},
		{"moving the router leaves the old range behind", `{"ip":"10.0.0.1","netmask":"255.255.255.0"}`, "dhcp.range_start"},
	}
	for _, c := range cases {
		e := standard(t)
		rec := e.putJSON(e.api.HandleNetwork, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400: %s", c.name, rec.Code, rec.Body)
			continue
		}
		if !errorFields(t, rec)[c.field] {
			t.Errorf("%s: errors %v, want one on %s", c.name, errorFields(t, rec), c.field)
		}
		if e.stageFileExists() || len(e.staged()) != 0 {
			t.Errorf("%s: an invalid save left a draft", c.name)
		}
	}
}

func TestSeveralProblemsAreReportedTogether(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleNetwork, `{"ip":"nope","netmask":"also nope","dhcp":{"enabled":true,"range_start":"x","range_end":"y","lease_time":"z"}}`)
	got := errorFields(t, rec)
	for _, f := range []string{"ip", "netmask", "dhcp.range_start", "dhcp.range_end", "dhcp.lease_time"} {
		if !got[f] {
			t.Errorf("missing %s in %v", f, got)
		}
	}
}

func TestADisabledDhcpServerNeedsNoRange(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleNetwork, `{"ip":"192.168.1.1","netmask":"255.255.255.0","dhcp":{"enabled":false}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if d := e.mgr.NetworkConfig().DHCP; d == nil || !d.Enabled {
		t.Fatal("the active config must not change")
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.DHCP == nil || staged.DHCP.Enabled {
		t.Errorf("staged dhcp = %+v, want disabled", staged.DHCP)
	}
}

func TestOmittingDhcpLeavesItAlone(t *testing.T) {
	e := standard(t)
	// same /24, only the router's own address moves; the range still fits
	rec := e.putJSON(e.api.HandleNetwork, `{"ip":"192.168.1.2","netmask":"255.255.255.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.LanIP != "192.168.1.2" || staged.DHCP == nil || staged.DHCP.RangeStart != "192.168.1.100" || staged.DHCP.LeaseTime != "12h" {
		t.Errorf("staged = %+v dhcp=%+v", staged, staged.DHCP)
	}
}

func TestMalformedRequestsAreRejected(t *testing.T) {
	e := standard(t)
	cases := map[string]struct{ body, ct string }{
		"unknown field":      {`{"ip":"192.168.1.1","netmask":"255.255.255.0","gateway":"1.2.3.4"}`, "application/json"},
		"not json":           {`ip=192.168.1.1`, "application/json"},
		"wrong content type": {`{"ip":"192.168.1.1","netmask":"255.255.255.0"}`, "application/x-www-form-urlencoded"},
		"no content type":    {`{"ip":"192.168.1.1","netmask":"255.255.255.0"}`, ""},
		"trailing garbage":   {`{"ip":"192.168.1.1","netmask":"255.255.255.0"} {}`, "application/json"},
		"wrong type for ip":  {`{"ip":5,"netmask":"255.255.255.0"}`, "application/json"},
		"oversized body":     {`{"ip":"` + strings.Repeat("a", 70000) + `","netmask":"255.255.255.0"}`, "application/json"},
	}
	for name, c := range cases {
		rec := e.do(e.api.HandleNetwork, http.MethodPut, c.body, c.ct)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	if e.stageFileExists() {
		t.Error("a malformed request left a draft")
	}
}

func TestEditsAreRefusedWhileAChangeIsBeingAppliedOrConfirmed(t *testing.T) {
	e := standard(t)
	e.editable = false

	if resp := decodeInto[networksResponse](t, e.get(e.api.HandleNetworks)); resp.Editable {
		t.Error("the view must say it is not editable")
	}
	rec := e.putJSON(e.api.HandleNetwork, moveLAN)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT = %d, want 409", rec.Code)
	}
	if e.stageFileExists() {
		t.Error("a refused edit left a draft")
	}
	rec = e.putJSON(e.api.HandlePorts, `{"assignments":{"eth0":"lan","eth1":"wan"}}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("ports PUT = %d, want 409", rec.Code)
	}
}

// A half-configured box (no WAN or LAN ports yet) can still have its LAN address edited: only the
// fields this page owns are checked until the draft is committed.
func TestOnlyThePagesOwnFieldsBlockASave(t *testing.T) {
	e := newEnv(t, nil) // nothing configured at all
	rec := e.putJSON(e.api.HandleNetwork, moveLAN)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s (the missing ports must not block saving the address)", rec.Code, rec.Body)
	}
	if staged, _ := e.mgr.StagedNetworkConfig(); staged.LanIP != "10.0.0.1" {
		t.Errorf("staged = %+v", staged)
	}
}

func TestSavingTheLANNeverTouchesThePortRoles(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandleNetwork, moveLAN)
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.WanInterface != "eth0" || len(staged.LanInterfaces) != 1 || staged.LanInterfaces[0] != "eth1" {
		t.Errorf("ports changed: wan=%q lan=%v", staged.WanInterface, staged.LanInterfaces)
	}
}

func TestNetworkMethods(t *testing.T) {
	e := standard(t)
	for _, m := range []string{http.MethodPost, http.MethodPatch} {
		rec := e.do(e.api.HandleNetwork, m, "", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT, DELETE" {
			t.Errorf("%s = %d (Allow %q)", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
	for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := e.do(e.api.HandleNetworks, m, "", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, POST" {
			t.Errorf("list %s = %d (Allow %q)", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if e.stageFileExists() {
		t.Error("a rejected method changed something")
	}
}

// ---------------------------------------------------------------- ports

func portByName(t *testing.T, ports []Port, name string) Port {
	t.Helper()
	for _, p := range ports {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no port %q in %+v", name, ports)
	return Port{}
}

func TestPortsListsHardwareWithRoles(t *testing.T) {
	e := standard(t)
	rec := e.get(e.api.HandlePorts)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	resp := decodeInto[portsResponse](t, rec)
	if !resp.Editable || len(resp.Ports) != 3 {
		t.Fatalf("response = %+v", resp)
	}
	for name, want := range map[string]string{"eth0": "wan", "eth1": "lan", "eth2": "unassigned"} {
		p := portByName(t, resp.Ports, name)
		if p.Role != want || p.ActiveRole != want || p.Pending || !p.Present {
			t.Errorf("%s = %+v, want role %s", name, p, want)
		}
	}
	if p := portByName(t, resp.Ports, "eth0"); p.State != "up" || p.Speed != "1000 Mbps" || p.Driver != "igb" || p.MAC == "" || p.MTU != 1500 {
		t.Errorf("hardware details missing: %+v", p)
	}
}

func TestAConfiguredPortWhoseHardwareIsMissingIsStillListed(t *testing.T) {
	c := baseline()
	c.LanInterfaces = []string{"eth1", "lan9"}
	e := newEnv(t, &c)

	resp := decodeInto[portsResponse](t, e.get(e.api.HandlePorts))
	p := portByName(t, resp.Ports, "lan9")
	if p.Present || p.Role != "lan" {
		t.Errorf("lan9 = %+v, want a missing LAN port that can still be reassigned", p)
	}

	rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"lan9":"unassigned"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if staged, _ := e.mgr.StagedNetworkConfig(); len(staged.LanInterfaces) != 1 || staged.LanInterfaces[0] != "eth1" {
		t.Errorf("staged LAN = %v", staged.LanInterfaces)
	}
}

func TestSwappingTheWANAndLANPortsStagesADraft(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth0":"lan","eth1":"wan","eth2":"unassigned"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}

	resp := decodeInto[portsResponse](t, rec)
	for name, want := range map[string]string{"eth0": "lan", "eth1": "wan", "eth2": "unassigned"} {
		if p := portByName(t, resp.Ports, name); p.Role != want {
			t.Errorf("%s role = %s, want %s", name, p.Role, want)
		}
	}
	if !portByName(t, resp.Ports, "eth0").Pending || !portByName(t, resp.Ports, "eth1").Pending || portByName(t, resp.Ports, "eth2").Pending {
		t.Errorf("pending flags wrong: %+v", resp.Ports)
	}
	if portByName(t, resp.Ports, "eth0").ActiveRole != "wan" {
		t.Error("the active role must still be what is in effect")
	}

	if active := e.mgr.NetworkConfig(); active.WanInterface != "eth0" {
		t.Errorf("the active WAN changed to %q: saving must only stage a draft", active.WanInterface)
	}
	fields := map[string]bool{}
	for _, c := range e.staged() {
		fields[c.Field] = true
	}
	if !fields["wan_interface"] || !fields["lan_ports"] && !fields["lan_interfaces"] {
		t.Errorf("staged changes = %+v", e.staged())
	}
}

func TestAddingASecondLANPortBridgesTheNetworkInStableOrder(t *testing.T) {
	for i := 0; i < 20; i++ { // map iteration order must not leak into the saved order
		e := standard(t)
		rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
		}
		staged, _ := e.mgr.StagedNetworkConfig()
		if strings.Join(staged.LanInterfaces, ",") != "eth1,eth2" {
			t.Fatalf("LAN ports = %v, want [eth1 eth2] in hardware order", staged.LanInterfaces)
		}
	}
	e := standard(t)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"}}`)
	if n := decodeInto[Network](t, e.get(e.api.HandleNetwork)); !n.Bridged || len(n.Ports) != 2 || !n.Pending {
		t.Errorf("network = %+v, want a pending bridged network of 2 ports", n)
	}
}

func TestPortsNotMentionedKeepTheirRoles(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"}}`)
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.WanInterface != "eth0" {
		t.Errorf("WAN = %q: a port that was not mentioned must keep its role", staged.WanInterface)
	}
}

func TestPortRoleChangesMustLeaveOneWANAndAtLeastOneLAN(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"two WANs", `{"assignments":{"eth2":"wan"}}`},
		{"no WAN", `{"assignments":{"eth0":"unassigned"}}`},
		{"no LAN", `{"assignments":{"eth1":"unassigned"}}`},
		{"everything unassigned", `{"assignments":{"eth0":"unassigned","eth1":"unassigned"}}`},
	}
	for _, c := range cases {
		e := standard(t)
		rec := e.putJSON(e.api.HandlePorts, c.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["ports"] {
			t.Errorf("%s: status %d errors %v, want 400 on ports", c.name, rec.Code, errorFields(t, rec))
		}
		if e.stageFileExists() {
			t.Errorf("%s: an invalid assignment left a draft", c.name)
		}
	}
}

func TestBadPortAssignmentsAreRejected(t *testing.T) {
	cases := map[string]struct{ body, field string }{
		"unknown port":     {`{"assignments":{"eth9":"lan"}}`, "assignments.eth9"},
		"invalid role":     {`{"assignments":{"eth2":"dmz"}}`, "assignments.eth2"},
		"role in capitals": {`{"assignments":{"eth2":"LAN"}}`, "assignments.eth2"},
		"empty assignment": {`{"assignments":{}}`, "assignments"},
		"missing":          {`{}`, "assignments"},
	}
	for name, c := range cases {
		e := standard(t)
		rec := e.putJSON(e.api.HandlePorts, c.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[c.field] {
			t.Errorf("%s: status %d errors %v, want one on %s", name, rec.Code, errorFields(t, rec), c.field)
		}
		if e.stageFileExists() {
			t.Errorf("%s: left a draft", name)
		}
	}
}

func TestPortRolesPutBackRemoveTheDraft(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth0":"lan","eth1":"wan"}}`)
	if !e.stageFileExists() {
		t.Fatal("expected a draft")
	}
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth0":"wan","eth1":"lan"}}`)
	if e.stageFileExists() || len(e.staged()) != 0 {
		t.Error("restoring the original roles should clear the draft")
	}
}

func TestAPortNameThatIsNotSafeCannotBecomeARole(t *testing.T) {
	e := standard(t)
	e.ports = append(e.ports, config.SystemInterface{Name: "eth 9", State: "up"})

	rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth 9":"lan"}}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["ports"] {
		t.Errorf("status %d errors %v, want the unsafe name rejected", rec.Code, errorFields(t, rec))
	}
	if e.stageFileExists() {
		t.Error("an unsafe port name was staged")
	}
}

func TestSavingPortRolesNeverTouchesTheAddressOrDhcp(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"}}`)
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.LanIP != "192.168.1.1" || staged.DHCP == nil || staged.DHCP.RangeStart != "192.168.1.100" {
		t.Errorf("the LAN address or DHCP changed: %+v", staged)
	}
}

func TestHardwareErrorsBecomeServerErrors(t *testing.T) {
	e := standard(t)
	e.portsErr = errors.New("cannot read /sys")
	if rec := e.get(e.api.HandlePorts); rec.Code != http.StatusInternalServerError {
		t.Errorf("GET = %d, want 500", rec.Code)
	}
	if rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"}}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("PUT = %d, want 500", rec.Code)
	}
	if e.stageFileExists() {
		t.Error("a failed request left a draft")
	}
}

func TestPortMethods(t *testing.T) {
	e := standard(t)
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		rec := e.do(e.api.HandlePorts, m, "", "")
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, PUT" {
			t.Errorf("%s = %d (Allow %q)", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

func TestAFreshBoxCanHaveItsPortsAssignedFromScratch(t *testing.T) {
	e := newEnv(t, nil)
	rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth0":"wan","eth1":"lan"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.WanInterface != "eth0" || strings.Join(staged.LanInterfaces, ",") != "eth1" {
		t.Errorf("staged = %+v", staged)
	}
}

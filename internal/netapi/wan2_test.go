package netapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/failover"
	"peakroute/internal/speedtest"
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

func TestAConnectionsHealthCheckIsSetPerConnectionAndTheDefaultsAreNotStored(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"wan2"}}`)
	u := decodeInto[Uplink](t, e.uplinkOf("wan", http.MethodGet, ""))
	if !u.Probe.PublicFallbacks || u.Probe.EverySeconds != 5 || u.Probe.DownAfter != 3 || len(u.Probe.Targets) != 0 || len(u.Probe.PublicTargets) != 2 {
		t.Fatalf("defaults: %+v", u.Probe)
	}
	rec := e.uplinkOf("wan2", http.MethodPut, `{"mode":"dhcp","probe":{"targets":[" 8.8.8.8 ","1.0.0.1"],"public_fallbacks":false,"every_seconds":10,"down_after":4}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}
	got := decodeInto[Uplink](t, rec).Probe
	if len(got.Targets) != 2 || got.Targets[0] != "8.8.8.8" || got.PublicFallbacks || got.EverySeconds != 10 || got.DownAfter != 4 {
		t.Fatalf("%+v", got)
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.WanProbe != nil || staged.Wan2.Probe == nil || len(staged.Wan2.Probe.Targets) != 2 || !staged.Wan2.Probe.NoFallbacks {
		t.Fatalf("the settings went to the wrong connection: %+v / %+v", staged.WanProbe, staged.Wan2.Probe)
	}
	fields := map[string]bool{}
	for _, c := range e.staged() {
		fields[c.Field] = true
	}
	if !fields["wan2.probe"] || fields["wan_probe"] {
		t.Errorf("staged: %+v", e.staged())
	}
	// what is left out stays; putting the defaults back leaves nothing stored
	e.uplinkOf("wan2", http.MethodPut, `{"mode":"dhcp","probe":{"public_fallbacks":true}}`)
	if staged, _ = e.mgr.StagedNetworkConfig(); staged.Wan2.Probe == nil || staged.Wan2.Probe.NoFallbacks || len(staged.Wan2.Probe.Targets) != 2 {
		t.Fatalf("%+v", staged.Wan2.Probe)
	}
	e.uplinkOf("wan2", http.MethodPut, `{"mode":"dhcp","probe":{"targets":[],"every_seconds":5,"down_after":3}}`)
	if staged, _ = e.mgr.StagedNetworkConfig(); staged.Wan2.Probe != nil {
		t.Fatalf("defaults were stored: %+v", staged.Wan2.Probe)
	}
	// the first connection's own
	if rec = e.uplinkOf("wan", http.MethodPut, `{"mode":"dhcp","probe":{"targets":["8.8.4.4"]}}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if staged, _ = e.mgr.StagedNetworkConfig(); staged.WanProbe == nil || staged.WanProbe.Targets[0] != "8.8.4.4" {
		t.Fatalf("%+v", staged.WanProbe)
	}
}

func TestMistakesInTheHealthCheckAreReportedOnTheFormsFields(t *testing.T) {
	e := standard(t)
	for body, field := range map[string]string{
		`{"mode":"dhcp","probe":{"targets":["not an address"]}}`:    "probe.targets",
		`{"mode":"dhcp","probe":{"targets":["127.0.0.1"]}}`:         "probe.targets",
		`{"mode":"dhcp","probe":{"targets":["8.8.8.8","8.8.8.8"]}}`: "probe.targets",
		`{"mode":"dhcp","probe":{"every_seconds":1}}`:               "probe.every_seconds",
		`{"mode":"dhcp","probe":{"down_after":99}}`:                 "probe.down_after",
	} {
		rec := e.uplinkOf("wan", http.MethodPut, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"`+field+`"`) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if staged, _ := e.mgr.StagedNetworkConfig(); staged.WanProbe != nil {
		t.Errorf("a refused change was staged: %+v", staged.WanProbe)
	}
}

func TestTheFailoverSettingsAreADraftChangeAndTheStatusIsReportedWhileThereAreTwoConnections(t *testing.T) {
	e := standard(t)
	get := func() failoverResponse { return decodeInto[failoverResponse](t, e.get(e.api.HandleFailover)) }
	if r := get(); r.Available || !r.Failback || r.RecoverySeconds != 120 || r.Status != nil {
		t.Fatalf("one connection: %+v", r)
	}
	// two connections in effect, and a controller that reports
	c := baseline()
	c.Wan2 = &config.WanConfig{Interface: "eth2", Mode: "dhcp"}
	e = newEnv(t, &c)
	e.api.FailoverStatus = func(cfg config.NetworkConfig) (failover.Status, int) {
		return failover.Status{Active: "wan2", Preferred: "wan", FailedOver: true, Reason: "Internet is down"}, 42
	}
	r := get()
	if !r.Available || r.Status == nil || r.Status.Active != "wan2" || !r.Status.FailedOver || r.RecoveringIn != 42 || r.Names["wan2"] != "Internet 2" {
		t.Fatalf("%+v", r)
	}
	rec := e.putJSON(e.api.HandleFailover, `{"failback":false,"recovery_seconds":300}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if r = decodeInto[failoverResponse](t, rec); r.Failback || r.RecoverySeconds != 300 || !r.Pending {
		t.Fatalf("%+v", r)
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if staged.WanFailover == nil || !staged.WanFailover.NoFailback || staged.WanFailover.RecoverySeconds != 300 {
		t.Fatalf("%+v", staged.WanFailover)
	}
	if e.mgr.NetworkConfig().WanFailover != nil {
		t.Error("the change took effect without a commit")
	}
	// the defaults leave nothing stored
	e.putJSON(e.api.HandleFailover, `{"failback":true,"recovery_seconds":120}`)
	if staged, _ = e.mgr.StagedNetworkConfig(); staged.WanFailover != nil {
		t.Fatalf("%+v", staged.WanFailover)
	}
	rec = e.putJSON(e.api.HandleFailover, `{"recovery_seconds":3}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"recovery_seconds"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestEachConnectionHasItsOwnShapingSpeedsAndSpeedTest(t *testing.T) {
	e := standard(t)
	put := func(body string) *httptest.ResponseRecorder {
		return e.do(e.api.HandleSQM, http.MethodPut, body, "application/json")
	}
	view := func() sqmResponse { return decodeInto[sqmResponse](t, e.get(e.api.HandleSQM)) }
	// with one connection there is no second line to speak of, and a request about it changes nothing
	if rec := put(`{"enabled":true,"download_mbit":500,"upload_mbit":40,"second":{"download_mbit":50}}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if v := view(); v.Second != nil || e.draft().SQM.Second != nil {
		t.Fatalf("a second line without a second connection: %+v", v.Second)
	}
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"wan2"}}`)
	rec := put(`{"second":{"download_mbit":50,"upload_mbit":10,"link":"vdsl"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	v := decodeInto[sqmResponse](t, rec)
	if v.Second == nil || v.Second.DownloadMbit != 50 || v.Second.UploadMbit != 10 || v.Second.Link != "vdsl" || v.Second.ShapedDownMbit != 47.5 || v.Second.ShapedUpMbit != 9.5 || v.Second.WanDevice != "eth2" {
		t.Fatalf("%+v", v.Second)
	}
	if v.DownloadMbit != 500 || v.ShapedDownMbit != 475 {
		t.Fatalf("the first line changed: %+v", v)
	}
	if !v.Pending {
		t.Error("a change of the second line is not pending")
	}
	// what is left out stays; emptying it all stores nothing
	put(`{"second":{"upload_mbit":20}}`)
	if l := e.draft().SQM.Second; l == nil || l.DownloadKbit != 50000 || l.UploadKbit != 20000 || l.Link != "vdsl" {
		t.Fatalf("%+v", l)
	}
	put(`{"second":{"download_mbit":0,"upload_mbit":0,"link":""}}`)
	if e.draft().SQM.Second != nil {
		t.Fatalf("%+v", e.draft().SQM.Second)
	}
	rec = put(`{"second":{"download_mbit":99999}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"second.download_mbit"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = put(`{"second":{"link":"carrier-pigeon"}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"second.link"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	// the speed test measures the connection it is asked for, and refuses one that is not there
	var asked []string
	e.api.SpeedTest = func(ctx context.Context, line string, progress func(string)) (speedtest.Result, error) {
		asked = append(asked, line)
		return speedtest.Result{DownloadMbit: 10, UploadMbit: 2}, nil
	}
	if rec := e.do(e.api.HandleSQMSpeedTest, http.MethodPost, ``, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	waitSpeed(t, e)
	// (the second connection is in the draft only, not in effect)
	if rec := e.doQuery(e.api.HandleSQMSpeedTest, http.MethodPost, "line=wan2"); rec.Code != http.StatusBadRequest {
		t.Fatalf("a test of a connection that is not in effect: %d %s", rec.Code, rec.Body)
	}
	if rec := e.doQuery(e.api.HandleSQMSpeedTest, http.MethodPost, "line=wan7"); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	if len(asked) != 1 || asked[0] != "wan" {
		t.Fatalf("%v", asked)
	}
}

func waitSpeed(t *testing.T, e *env) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if v := decodeInto[speedResponse](t, e.get(e.api.HandleSQMSpeedTest)); v.State == "done" || v.State == "error" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the speed test did not finish")
}

func (e *env) doQuery(h http.HandlerFunc, method, query string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/x?"+query, strings.NewReader(""))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestSharingTheTrafficHasWeightsOfItsOwnAndTheSharesFollowTheHealthOfTheConnections(t *testing.T) {
	c := baseline()
	c.Wan2 = &config.WanConfig{Interface: "eth2", Mode: "dhcp"}
	c.WanFailover = &config.FailoverConfig{Balanced: true, WeightFirst: 1, WeightSecond: 3}
	e := newEnv(t, &c)
	states := map[string]string{"wan": "online", "wan2": "online"}
	e.api.FailoverStatus = func(cfg config.NetworkConfig) (failover.Status, int) {
		return failover.Status{Active: "wan", Preferred: "wan", States: states}, 0
	}
	get := func() failoverResponse { return decodeInto[failoverResponse](t, e.get(e.api.HandleFailover)) }
	r := get()
	if !r.Balanced || r.WeightFirst != 1 || r.WeightSecond != 3 || r.Shares["wan"] != 25 || r.Shares["wan2"] != 75 {
		t.Fatalf("%+v", r)
	}
	// a connection that is down has no share, and the other takes it all
	states["wan2"] = "offline"
	if r = get(); r.Shares["wan"] != 100 || r.Shares["wan2"] != 0 {
		t.Fatalf("%+v", r.Shares)
	}
	states["wan2"] = "online"
	// changing them
	rec := e.putJSON(e.api.HandleFailover, `{"weight_first":2,"weight_second":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	staged, _ := e.mgr.StagedNetworkConfig()
	if f := staged.WanFailover; f == nil || !f.Balanced || f.WeightFirst != 2 || f.WeightSecond != 2 {
		t.Fatalf("%+v", f)
	}
	// the shares shown are what is in effect, not the draft
	if r = get(); r.Shares["wan"] != 25 || !r.Pending {
		t.Fatalf("%+v", r)
	}
	// the default weight is not stored, and failover is the default mode
	e.putJSON(e.api.HandleFailover, `{"balanced":false,"weight_first":1,"weight_second":1}`)
	if staged, _ = e.mgr.StagedNetworkConfig(); staged.WanFailover != nil {
		t.Fatalf("%+v", staged.WanFailover)
	}
	rec = e.putJSON(e.api.HandleFailover, `{"weight_second":101}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"weight_second"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

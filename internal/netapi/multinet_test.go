package netapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"peakroute/internal/config"
)

const newGuest = `{"name":"Guest","vlan":20,"ip":"10.20.0.1","netmask":"255.255.255.0","dhcp":{"enabled":true,"range_start":"10.20.0.100","range_end":"10.20.0.200","lease_time":"1h"}}`

func (e *env) net(method, id, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	e.api.HandleNetwork(rec, req)
	return rec
}

func (e *env) create(body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(e.api.HandleNetworks, http.MethodPost, body, "application/json")
}

func (e *env) networks() []Network {
	e.t.Helper()
	return decodeInto[networksResponse](e.t, e.get(e.api.HandleNetworks)).Networks
}

func (e *env) draft() config.NetworkConfig { d, _ := e.mgr.StagedNetworkConfig(); return d }

func (e *env) commit() {
	e.t.Helper()
	if err := e.mgr.CommitNetwork(); err != nil {
		e.t.Fatal(err)
	}
}

func TestTheDefaultNetworkIsListedFirst(t *testing.T) {
	e := standard(t)
	ns := e.networks()
	if len(ns) != 1 {
		t.Fatalf("%+v", ns)
	}
	n := ns[0]
	if n.ID != "lan" || !n.Default || n.Name != "LAN" || n.VLAN != 1 || !n.Internet || n.Isolated ||
		len(n.Ports) != 1 || n.Ports[0] != "eth1" || n.TaggedPorts == nil || len(n.TaggedPorts) != 0 {
		t.Fatalf("%+v", n)
	}
}

func TestCreatingANetworkStagesAnIsolatedOneWithoutInternet(t *testing.T) {
	e := standard(t)
	rec := e.create(newGuest)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	n := decodeInto[Network](t, rec)
	if n.ID != "net2" || n.Name != "Guest" || n.Default || n.VLAN != 20 || n.IP != "10.20.0.1" || !n.Pending ||
		n.Internet || !n.Isolated || n.DHCP == nil || !n.DHCP.Enabled || len(n.Ports) != 0 || len(n.TaggedPorts) != 0 {
		t.Fatalf("%+v", n)
	}
	if len(e.mgr.NetworkConfig().Networks) != 0 {
		t.Fatal("the active configuration changed: saving must only stage")
	}
	d := e.draft()
	if len(d.Networks) != 1 || d.Networks[0].ID != "net2" || d.Networks[0].Internet || !d.Networks[0].Isolated {
		t.Fatalf("%+v", d.Networks)
	}
	if got := e.networks(); len(got) != 2 || got[1].ID != "net2" {
		t.Fatalf("%+v", got)
	}
}

func TestSwitchesGivenAtCreationAreHonoured(t *testing.T) {
	e := standard(t)
	rec := e.create(`{"name":"Lab","vlan":40,"ip":"10.40.0.1","netmask":"255.255.255.0","internet":true,"isolated":false}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	n := decodeInto[Network](t, rec)
	if !n.Internet || n.Isolated || n.DHCP != nil {
		t.Fatalf("%+v", n)
	}
}

func TestIdsAreUniqueAndReusedAfterADelete(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	rec := e.create(`{"name":"IoT","vlan":30,"ip":"10.30.0.1","netmask":"255.255.255.0"}`)
	if n := decodeInto[Network](t, rec); n.ID != "net3" {
		t.Fatalf("%+v", n)
	}
	if rec := e.net(http.MethodDelete, "net2", ""); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = e.create(`{"name":"Cameras","vlan":50,"ip":"10.50.0.1","netmask":"255.255.255.0"}`)
	if n := decodeInto[Network](t, rec); n.ID != "net2" {
		t.Fatalf("%+v", n)
	}
}

func TestCreatingWithBadValuesNamesTheFields(t *testing.T) {
	e := standard(t)
	rec := e.create(`{"name":"","vlan":1,"ip":"nope","netmask":"255.0.255.0","dhcp":{"enabled":true,"range_start":"x","range_end":"y","lease_time":"z"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := errorFields(t, rec)
	for _, want := range []string{"name", "vlan", "ip", "netmask", "dhcp.range_start", "dhcp.range_end", "dhcp.lease_time"} {
		if !f[want] {
			t.Errorf("no error on %s: %v", want, f)
		}
	}
	if e.stageFileExists() {
		t.Fatal("an invalid network left a draft")
	}
}

func TestANetworkThatOverlapsTheLanIsRefused(t *testing.T) {
	e := standard(t)
	rec := e.create(`{"name":"Clash","vlan":20,"ip":"192.168.1.129","netmask":"255.255.255.128"}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["netmask"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestANetworkNeedsTheDefaultDhcpToBeManaged(t *testing.T) {
	c := baseline()
	c.DHCP = nil
	e := newEnv(t, &c)
	rec := e.create(newGuest)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["dhcp.enabled"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestThereIsALimitOnNetworks(t *testing.T) {
	e := standard(t)
	for i := 0; i < config.MaxNetworks; i++ {
		body := `{"name":"Net ` + itoa(i) + `","vlan":` + itoa(100+i) + `,"ip":"10.` + itoa(60+i) + `.0.1","netmask":"255.255.255.0"}`
		if rec := e.create(body); rec.Code != http.StatusCreated {
			t.Fatalf("network %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := e.create(`{"name":"One too many","vlan":300,"ip":"10.99.0.1","netmask":"255.255.255.0"}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["networks"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for ; i > 0; i /= 10 {
		d = append([]byte{byte('0' + i%10)}, d...)
	}
	return string(d)
}

func TestChangingANetwork(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.commit() // stage and commit are separate steps in the real service; here it just fixes the baseline
	e = &env{t: t, mgr: e.mgr, api: e.api, dir: e.dir, ports: e.ports, editable: true}

	// only what is given changes
	rec := e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","internet":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	n := decodeInto[Network](t, rec)
	if !n.Internet || !n.Isolated || n.Name != "Guest" || n.VLAN != 20 || n.DHCP == nil || n.DHCP.RangeStart != "10.20.0.100" || !n.Pending {
		t.Fatalf("%+v", n)
	}
	rec = e.net(http.MethodPut, "net2", `{"name":" Visitors ","vlan":25,"ip":"10.25.0.1","netmask":"255.255.255.0","isolated":false,"dhcp":{"enabled":false}}`)
	n = decodeInto[Network](t, rec)
	if rec.Code != 200 || n.Name != "Visitors" || n.VLAN != 25 || n.IP != "10.25.0.1" || n.Isolated || n.DHCP == nil || n.DHCP.Enabled {
		t.Fatalf("%d %+v", rec.Code, n)
	}
	if got := e.mgr.NetworkConfig().Networks[0]; got.Name != "Guest" || got.VLAN != 20 {
		t.Fatalf("the active network changed: %+v", got)
	}
}

func TestPuttingTheSameValuesBackLeavesNoDraftForANetwork(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.commit()
	if rec := e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0"}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.stageFileExists() {
		t.Fatal("saving what is already in effect created a draft")
	}
	if n := e.networks()[1]; n.Pending {
		t.Fatalf("%+v", n)
	}
}

func TestEditingANetworkIgnoresAnotherNetworksProblems(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	// a second network that is already wrong (a draft written by hand, say)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.Networks = append(c.Networks, config.Network{ID: "net3", Name: "Broken", VLAN: 9999, IP: "x", Netmask: "y", Isolated: true})
		return nil
	})
	if rec := e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","internet":true}`); rec.Code != 200 {
		t.Fatalf("another network's problem blocked this one: %d %s", rec.Code, rec.Body)
	}
}

func TestSharedProblemsAreShownWhicheverNetworkIsEdited(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.create(`{"name":"IoT","vlan":30,"ip":"10.30.0.1","netmask":"255.255.255.0"}`)
	// the first network takes the second one's VLAN
	rec := e.net(http.MethodPut, "net2", `{"vlan":30,"ip":"10.20.0.1","netmask":"255.255.255.0"}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["vlan"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// ... and overlapping the other's subnet
	rec = e.net(http.MethodPut, "net2", `{"ip":"10.30.0.50","netmask":"255.255.255.0"}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["netmask"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := e.draft().Networks[0]; got.VLAN != 20 || got.IP != "10.20.0.1" {
		t.Fatalf("a refused edit changed the draft: %+v", got)
	}
}

func TestChangingTheDefaultNetwork(t *testing.T) {
	e := standard(t)
	rec := e.net(http.MethodPut, "lan", `{"ip":"192.168.1.1","netmask":"255.255.255.0","isolated":true}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if n := decodeInto[Network](t, rec); !n.Isolated || !n.Pending || !n.Internet {
		t.Fatalf("%+v", n)
	}
	if !e.draft().LanIsolated || e.mgr.NetworkConfig().LanIsolated {
		t.Fatal("isolation was not staged")
	}
}

func TestWhatMakesTheDefaultNetworkTheDefaultCannotBeChanged(t *testing.T) {
	e := standard(t)
	for field, body := range map[string]string{
		"name":     `{"name":"Home","ip":"192.168.1.1","netmask":"255.255.255.0"}`,
		"vlan":     `{"vlan":5,"ip":"192.168.1.1","netmask":"255.255.255.0"}`,
		"internet": `{"internet":false,"ip":"192.168.1.1","netmask":"255.255.255.0"}`,
	} {
		rec := e.net(http.MethodPut, "lan", body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[field] {
			t.Errorf("%s: %d %s", field, rec.Code, rec.Body)
		}
	}
	// saying what is already true is fine
	if rec := e.net(http.MethodPut, "lan", `{"name":"LAN","vlan":1,"internet":true,"ip":"192.168.1.1","netmask":"255.255.255.0"}`); rec.Code != 200 {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if e.stageFileExists() {
		t.Error("a rejected edit left a draft")
	}
}

func TestMovingTheDefaultNetworkOntoAnotherNetworksSubnetIsRefused(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	rec := e.net(http.MethodPut, "lan", `{"ip":"10.20.0.77","netmask":"255.255.255.0"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "overlaps") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestDeletingANetwork(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.commit()
	rec := e.net(http.MethodDelete, "net2", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := decodeInto[networksResponse](t, rec).Networks; len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	if len(e.draft().Networks) != 0 || len(e.mgr.NetworkConfig().Networks) != 1 {
		t.Fatal("deleting must stage the removal, not apply it")
	}
	// a network that is only a draft simply disappears
	e2 := standard(t)
	e2.create(newGuest)
	e2.net(http.MethodDelete, "net2", "")
	if e2.stageFileExists() {
		t.Fatal("deleting a network that was never committed left a draft")
	}
}

func TestTheDefaultNetworkCannotBeDeletedAndUnknownOnesAre404(t *testing.T) {
	e := standard(t)
	if rec := e.net(http.MethodDelete, "lan", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("%d", rec.Code)
	}
	if rec := e.net(http.MethodDelete, "nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
	if rec := e.net(http.MethodGet, "nope", ""); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
	if rec := e.net(http.MethodPut, "nope", `{"ip":"1.1.1.1","netmask":"255.255.255.0"}`); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
}

func TestANetworkThatAPortStillCarriesUntaggedCannotBeDeleted(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	if rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"net2"}}}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec := e.net(http.MethodDelete, "net2", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "eth2") || !strings.Contains(rec.Body.String(), "move the port to another network first") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(e.draft().Networks) != 1 {
		t.Fatal("the network was deleted anyway")
	}
}

func TestDeletingATaggedNetworkTakesItOffThePorts(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.create(`{"name":"IoT","vlan":30,"ip":"10.30.0.1","netmask":"255.255.255.0"}`)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth1":{"tagged":["net2"]},"eth2":{"tagged":["net2","net3"]}}}`)
	if rec := e.net(http.MethodDelete, "net2", ""); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := e.draft()
	if _, ok := d.PortNetworks["eth1"]; ok {
		t.Errorf("eth1 carried only the deleted network and should have no entry: %+v", d.PortNetworks)
	}
	if got := d.PortNetworks["eth2"].Tagged; len(got) != 1 || got[0] != "net3" {
		t.Errorf("%+v", d.PortNetworks)
	}
}

func TestNetworkPortListsFollowThePorts(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.create(`{"name":"IoT","vlan":30,"ip":"10.30.0.1","netmask":"255.255.255.0"}`)
	rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth1":{"tagged":["net2","net3"]},"eth2":{"native":"net2","tagged":["net3"]}}}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	ns := e.networks()
	by := map[string]Network{}
	for _, n := range ns {
		by[n.ID] = n
	}
	if got := strings.Join(by["lan"].Ports, ","); got != "eth1" {
		t.Errorf("default network ports: %q", got)
	}
	if got := strings.Join(by["net2"].Ports, ","); got != "eth2" || strings.Join(by["net2"].TaggedPorts, ",") != "eth1" || !by["net2"].Bridged {
		t.Errorf("guest: %+v", by["net2"])
	}
	if strings.Join(by["net3"].TaggedPorts, ",") != "eth1,eth2" || len(by["net3"].Ports) != 0 {
		t.Errorf("iot: %+v", by["net3"])
	}
}

func TestANetworkIsPendingWhenOnlyItsPortsChanged(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.commit()
	if e.networks()[1].Pending {
		t.Fatal("pending right after a commit")
	}
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"net2"}}}`)
	if n := e.networks()[1]; !n.Pending {
		t.Fatalf("%+v", n)
	}
	// tagging counts as well
	e2 := standard(t)
	e2.create(newGuest)
	e2.commit()
	e2.putJSON(e2.api.HandlePorts, `{"assignments":{"eth1":"lan"},"networks":{"eth1":{"tagged":["net2"]}}}`)
	if n := e2.networks()[1]; !n.Pending || len(n.TaggedPorts) != 1 {
		t.Fatalf("%+v", n)
	}
}

func TestNetworkWritesAreRefusedWhileApplying(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.commit()
	e.editable = false
	if rec := e.create(newGuest); rec.Code != http.StatusConflict {
		t.Errorf("POST %d", rec.Code)
	}
	if rec := e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","internet":true}`); rec.Code != http.StatusConflict {
		t.Errorf("PUT %d", rec.Code)
	}
	if rec := e.net(http.MethodDelete, "net2", ""); rec.Code != http.StatusConflict {
		t.Errorf("DELETE %d", rec.Code)
	}
	if e.stageFileExists() {
		t.Error("a refused write left a draft")
	}
}

func TestNetworkRequestsAreStrict(t *testing.T) {
	e := standard(t)
	for name, body := range map[string]string{
		"unknown field": `{"name":"A","vlan":20,"ip":"10.20.0.1","netmask":"255.255.255.0","id":"x"}`,
		"vlan string":   `{"name":"A","vlan":"20","ip":"10.20.0.1","netmask":"255.255.255.0"}`,
		"internet text": `{"name":"A","vlan":20,"ip":"10.20.0.1","netmask":"255.255.255.0","internet":"yes"}`,
		"not json":      `network`,
	} {
		if rec := e.create(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if rec := e.do(e.api.HandleNetworks, http.MethodPost, newGuest, ""); rec.Code == http.StatusCreated {
		t.Error("accepted a body without a JSON content type")
	}
	if e.stageFileExists() {
		t.Error("a rejected request left a draft")
	}
}

// ---- ports -----------------------------------------------------------------------------

func TestPortsListWhatEachLanPortCarriesAndWhatTheyCouldCarry(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"net2"},"eth1":{"tagged":["net2"]}}}`)
	view := decodeInto[portsResponse](t, e.get(e.api.HandlePorts))
	if len(view.Networks) != 2 || view.Networks[0].ID != "lan" || !view.Networks[0].Default || view.Networks[1].ID != "net2" || view.Networks[1].VLAN != 20 {
		t.Fatalf("%+v", view.Networks)
	}
	p1, p2, p0 := portByName(t, view.Ports, "eth1"), portByName(t, view.Ports, "eth2"), portByName(t, view.Ports, "eth0")
	if p1.Native != "lan" || len(p1.Tagged) != 1 || p1.Tagged[0] != "net2" || !p1.Pending {
		t.Errorf("eth1 %+v", p1)
	}
	if p2.Native != "net2" || len(p2.Tagged) != 0 {
		t.Errorf("eth2 %+v", p2)
	}
	if p0.Native != "" || p0.Tagged == nil || len(p0.Tagged) != 0 {
		t.Errorf("a WAN port carries no networks: %+v", p0)
	}
}

func TestAPortNotMentionedKeepsItsNetworks(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"net2"}}}`)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"}}`) // only roles this time
	if got := e.draft().PortNetworks["eth2"].Native; got != "net2" {
		t.Fatalf("native = %q", got)
	}
}

func TestAnEntryThatSaysOnlyTheDefaultIsDropped(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"net2"}}}`)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"lan","tagged":[]}}}`)
	if _, ok := e.draft().PortNetworks["eth2"]; ok {
		t.Fatalf("%+v", e.draft().PortNetworks)
	}
}

func TestAPortThatStopsBeingLanForgetsItsNetworks(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"net2"}}}`)
	if rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"unassigned"}}`); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, ok := e.draft().PortNetworks["eth2"]; ok {
		t.Fatalf("%+v", e.draft().PortNetworks)
	}
}

func TestOnlyLanPortsCarryNetworks(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	rec := e.putJSON(e.api.HandlePorts, `{"assignments":{"eth2":"unassigned"},"networks":{"eth2":{"native":"net2"}}}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["networks.eth2"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = e.putJSON(e.api.HandlePorts, `{"assignments":{"eth0":"wan"},"networks":{"eth0":{"native":"net2"}}}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["networks.eth0"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestPortNetworkErrorsNameThePortAndField(t *testing.T) {
	e := standard(t)
	e.create(newGuest)
	for name, tc := range map[string]struct{ body, field string }{
		"unknown native":    {`{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"nope"}}}`, "port_networks.eth2.native"},
		"unknown tagged":    {`{"assignments":{"eth2":"lan"},"networks":{"eth2":{"tagged":["nope"]}}}`, "port_networks.eth2.tagged"},
		"default tagged":    {`{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"net2","tagged":["lan"]}}}`, "port_networks.eth2.tagged"},
		"native and tagged": {`{"assignments":{"eth2":"lan"},"networks":{"eth2":{"native":"net2","tagged":["net2"]}}}`, "port_networks.eth2.tagged"},
	} {
		rec := e.putJSON(e.api.HandlePorts, tc.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[tc.field] {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(e.draft().PortNetworks) != 0 {
		t.Fatalf("a refused save changed the draft: %+v", e.draft().PortNetworks)
	}
}

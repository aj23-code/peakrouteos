package netapi

import (
	"net/http"
	"strings"
	"testing"

	"peakroute/internal/config"
	"peakroute/internal/network"
)

func kr(dest, gw, iface string, metric int) network.KernelRoute {
	return network.KernelRoute{Destination: dest, Gateway: gw, Interface: iface, Metric: metric}
}

func (e *env) getRoutes() routesResponse {
	e.t.Helper()
	rec := e.get(e.api.HandleRoutes)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	return decodeInto[routesResponse](e.t, rec)
}

const twoRoutes = `{"routes":[
 {"name":"Lab VPN","destination":"10.20.0.0/16","gateway":"192.168.1.50","metric":100,"enabled":true},
 {"name":"","destination":"172.16.0.0/12","gateway":"192.168.1.51","metric":0,"enabled":false}]}`

func TestRoutesStartEmpty(t *testing.T) {
	e := standard(t)
	rec := e.get(e.api.HandleRoutes)
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("not marked no-store")
	}
	r := decodeInto[routesResponse](t, rec)
	if len(r.Routes) != 0 || r.Pending || !r.Editable || r.Table == nil {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(rec.Body.String(), `"routes":[]`) || !strings.Contains(rec.Body.String(), `"table":[]`) {
		t.Errorf("empty lists must be [] not null: %s", rec.Body)
	}
}

func TestSavingRoutesStagesADraftAndLeavesTheSystemAlone(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleRoutes, twoRoutes)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decodeInto[routesResponse](t, rec)
	if !r.Pending || len(r.Routes) != 2 || r.Routes[0].Name != "Lab VPN" || r.Routes[0].Metric != 100 || !r.Routes[0].Enabled || r.Routes[1].Enabled {
		t.Fatalf("%+v", r)
	}
	if len(e.mgr.NetworkConfig().Routes) != 0 {
		t.Fatal("the active configuration changed: saving must only stage")
	}
	draft, _ := e.mgr.StagedNetworkConfig()
	if len(draft.Routes) != 2 || !draft.Routes[1].Disabled || draft.Routes[0].Disabled {
		t.Fatalf("%+v", draft.Routes)
	}
	// a later GET shows the same draft
	if got := e.getRoutes(); !got.Pending || len(got.Routes) != 2 {
		t.Fatalf("%+v", got)
	}
	// and the review sees one change per route
	n := 0
	for _, c := range e.staged() {
		if c.Field == "routes" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("staged route changes: %d (%+v)", n, e.staged())
	}
}

func TestOmittedEnabledMeansOn(t *testing.T) {
	e := standard(t)
	e.putJSON(e.api.HandleRoutes, `{"routes":[{"destination":"10.20.0.0/16","gateway":"192.168.1.50"}]}`)
	if r := e.getRoutes(); len(r.Routes) != 1 || !r.Routes[0].Enabled {
		t.Fatalf("%+v", r.Routes)
	}
}

func TestInputIsTrimmed(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleRoutes, `{"routes":[{"name":"  Lab  ","destination":" 10.20.0.0/16 ","gateway":" 192.168.1.50 "}]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := e.getRoutes().Routes[0]
	if r.Name != "Lab" || r.Destination != "10.20.0.0/16" || r.Gateway != "192.168.1.50" {
		t.Fatalf("%+v", r)
	}
}

func TestErrorsNameTheRowAndField(t *testing.T) {
	e := standard(t)
	rec := e.putJSON(e.api.HandleRoutes, `{"routes":[
	 {"destination":"10.20.0.0/16","gateway":"192.168.1.50"},
	 {"destination":"10.30.0.7/16","gateway":"192.168.1.1","metric":99999,"name":"<b>"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := errorFields(t, rec)
	for _, want := range []string{"routes[1].destination", "routes[1].gateway", "routes[1].metric", "routes[1].name"} {
		if !f[want] {
			t.Errorf("no error on %s: %v", want, f)
		}
	}
	if f["routes[0].destination"] || f["routes[0].gateway"] {
		t.Errorf("the good row was blamed: %v", f)
	}
	if e.stageFileExists() {
		t.Fatal("an invalid save left a draft")
	}
}

func TestOtherPagesProblemsDoNotBlockSavingRoutes(t *testing.T) {
	c := baseline()
	c.LanInterfaces, c.LanIP, c.LanSubnet = nil, "", ""
	e := newEnv(t, nil)
	e.mgr.StageNetwork(func(n *config.NetworkConfig) error { *n = c; return nil })
	// with no LAN a gateway cannot be placed on it, but DHCP uplinks accept any gateway
	rec := e.putJSON(e.api.HandleRoutes, `{"routes":[{"destination":"10.20.0.0/16","gateway":"100.64.0.9"}]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestSavingAnEmptyListRemovesTheRoutesFromTheDraft(t *testing.T) {
	c := baseline()
	c.Routes = []config.Route{{Destination: "10.20.0.0/16", Gateway: "192.168.1.50"}}
	e := newEnv(t, &c)
	rec := e.putJSON(e.api.HandleRoutes, `{"routes":[]}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decodeInto[routesResponse](t, rec)
	if len(r.Routes) != 0 || !r.Pending {
		t.Fatalf("%+v", r)
	}
	if len(e.mgr.NetworkConfig().Routes) != 1 {
		t.Fatal("the active routes were touched")
	}
}

func TestPuttingTheSameRoutesBackLeavesNoDraft(t *testing.T) {
	c := baseline()
	c.Routes = []config.Route{{Name: "Lab", Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: 100}}
	e := newEnv(t, &c)
	e.putJSON(e.api.HandleRoutes, `{"routes":[{"name":"Lab","destination":"10.20.0.0/16","gateway":"192.168.1.50","metric":100,"enabled":true}]}`)
	if e.stageFileExists() || e.getRoutes().Pending {
		t.Fatal("saving what is already in effect created a draft")
	}
}

func TestRoutesRejectUnknownFieldsBadTypesAndWrongMethods(t *testing.T) {
	e := standard(t)
	for name, body := range map[string]string{
		"unknown field": `{"routes":[{"destination":"10.20.0.0/16","gateway":"192.168.1.50","dev":"eth9"}]}`,
		"unknown top":   `{"routes":[],"extra":1}`,
		"metric string": `{"routes":[{"destination":"10.20.0.0/16","gateway":"192.168.1.50","metric":"7"}]}`,
		"not an array":  `{"routes":{}}`,
		"trailing junk": `{"routes":[]} {"routes":[]}`,
		"not json":      `routes`,
	} {
		if rec := e.putJSON(e.api.HandleRoutes, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if rec := e.do(e.api.HandleRoutes, http.MethodPut, `{"routes":[]}`, ""); rec.Code == http.StatusOK {
		t.Error("accepted a body without a JSON content type")
	}
	if rec := e.do(e.api.HandleRoutes, http.MethodDelete, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE %d", rec.Code)
	}
	if e.stageFileExists() {
		t.Fatal("a rejected request left a draft")
	}
}

func TestRouteEditsAreRefusedWhileApplying(t *testing.T) {
	e := standard(t)
	e.editable = false
	if rec := e.putJSON(e.api.HandleRoutes, twoRoutes); rec.Code != http.StatusConflict {
		t.Fatalf("%d", rec.Code)
	}
	if e.stageFileExists() || e.getRoutes().Editable {
		t.Fatal("edits were not refused")
	}
}

func TestTheRoutingTableIsClassifiedAndOurRoutesAreRecognised(t *testing.T) {
	c := baseline()
	c.Routes = []config.Route{
		{Destination: "10.20.0.0/16", Gateway: "192.168.1.50"},
		{Destination: "172.16.0.0/12", Gateway: "192.168.1.51", Disabled: true}, // not in effect
	}
	e := newEnv(t, &c)
	e.api.RoutingTable = func() []LiveRoute {
		return ClassifyRoutes([]network.KernelRoute{
			kr("172.16.0.0/12", "192.168.1.51", "eth1", 0),
			kr("192.168.1.0/24", "", "eth1", 0),
			kr("10.20.0.0/16", "192.168.1.50", "eth1", 0),
			kr("0.0.0.0/0", "203.0.113.1", "eth0", 100),
			kr("203.0.113.0/24", "", "eth0", 100),
		})
	}
	table := e.getRoutes().Table
	var got []string
	for _, r := range table {
		got = append(got, r.Kind+" "+r.Destination)
	}
	want := "default 0.0.0.0/0, connected 192.168.1.0/24, connected 203.0.113.0/24, static 10.20.0.0/16, via 172.16.0.0/12"
	if strings.Join(got, ", ") != want {
		t.Fatalf("%s\nwant %s", strings.Join(got, ", "), want)
	}
}

func TestTheTableComesFromTheActiveConfigNotTheDraft(t *testing.T) {
	e := standard(t)
	e.api.RoutingTable = func() []LiveRoute {
		return ClassifyRoutes([]network.KernelRoute{kr("10.20.0.0/16", "192.168.1.50", "eth1", 0)})
	}
	e.putJSON(e.api.HandleRoutes, twoRoutes) // a draft that is not in effect yet
	if k := e.getRoutes().Table[0].Kind; k != KindVia {
		t.Fatalf("%q: a draft route must not be shown as being in effect", k)
	}
}

func TestClassifyOrder(t *testing.T) {
	got := ClassifyRoutes([]network.KernelRoute{
		kr("192.168.1.0/24", "", "eth1", 0),
		kr("10.0.0.0/8", "192.168.1.9", "eth1", 20),
		kr("10.0.0.0/8", "192.168.1.8", "eth1", 10),
		kr("0.0.0.0/0", "203.0.113.1", "eth0", 100),
		kr("10.0.0.0/16", "192.168.1.7", "eth1", 0),
		kr("169.254.0.0/16", "", "eth0", 1000),
	})
	var order []string
	for _, r := range got {
		order = append(order, r.Destination+"@"+r.Gateway)
	}
	want := "0.0.0.0/0@203.0.113.1, 169.254.0.0/16@, 192.168.1.0/24@, 10.0.0.0/8@192.168.1.8, 10.0.0.0/8@192.168.1.9, 10.0.0.0/16@192.168.1.7"
	if strings.Join(order, ", ") != want {
		t.Fatalf("%s\nwant %s", strings.Join(order, ", "), want)
	}
}

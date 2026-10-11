package netapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func (e *env) fw(h http.HandlerFunc, method, id, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func (e *env) firewall() firewallResponse {
	e.t.Helper()
	return decodeInto[firewallResponse](e.t, e.fw(e.api.HandleFirewall, http.MethodGet, "", ""))
}

func (e *env) addRule(body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleFirewallRules, http.MethodPost, "", body)
}

func (e *env) rule(method, id, body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleFirewallRule, method, id, body)
}

func (e *env) addForward(body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleFirewallForwards, http.MethodPost, "", body)
}

func (e *env) forward(method, id, body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleFirewallForward, method, id, body)
}

// a LAN, and a Guest network (net2) that exists in the draft
func fwEnv(t *testing.T) *env {
	e := standard(t)
	if rec := e.create(newGuest); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return e
}

const allowPrinter = `{"name":"Printer","from":"lan","action":"allow","to":"net2","protocol":"tcp","destination":"10.20.0.20","ports":"9100"}`

func TestTheFirewallStartsEmptyWithItsTabsAndFixedLines(t *testing.T) {
	e := fwEnv(t)
	f := e.firewall()
	if len(f.Rules) != 0 || f.Rules == nil || f.Forwards == nil || len(f.Forwards) != 0 || !f.Editable {
		t.Fatalf("%+v", f)
	}
	if len(f.Tabs) != 3 || f.Tabs[0].ID != "lan" || !f.Tabs[0].Default || f.Tabs[0].Name != "LAN" ||
		f.Tabs[1].ID != "net2" || f.Tabs[1].Name != "Guest" || f.Tabs[2].ID != "wan" || f.Tabs[2].Kind != "internet" {
		t.Fatalf("%+v", f.Tabs)
	}
	lines := func(ls []PolicyLine) string {
		var s []string
		for _, l := range ls {
			s = append(s, l.Kind+": "+l.Text)
		}
		return strings.Join(s, "\n")
	}
	lan := f.Policy["lan"]
	if !strings.Contains(lines(lan.Before), "web interface and SSH are always reachable") || !strings.Contains(lines(lan.After), "allow: Allow everything on this router") ||
		!strings.Contains(lines(lan.After), "allow: Allow to the internet") || !strings.HasSuffix(lines(lan.After), "block: Block everything else") {
		t.Fatalf("lan:\n%s\n--\n%s", lines(lan.Before), lines(lan.After))
	}
	// the default network is not isolated, the guest network is: no line to it
	if strings.Contains(lines(lan.After), "Allow to Guest") {
		t.Errorf("the guest network is isolated:\n%s", lines(lan.After))
	}
	guest := f.Policy["net2"]
	if strings.Contains(lines(guest.Before), "web interface") || !strings.Contains(lines(guest.After), "allow: Allow this router's DNS and ping") ||
		!strings.Contains(lines(guest.After), "off: No access to the internet") || !strings.Contains(lines(guest.After), "off: Isolated from the other networks") {
		t.Fatalf("guest:\n%s", lines(guest.After))
	}
	wan := f.Policy["wan"]
	if len(wan.After) != 1 || wan.After[0].Kind != "block" || strings.Contains(lines(wan.Before), "port forwards") {
		t.Fatalf("wan: %+v", wan)
	}
}

func TestThePolicyLinesFollowTheSwitchesOnLocalNetworks(t *testing.T) {
	e := fwEnv(t)
	// guest: internet on, not isolated
	if rec := e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","internet":true,"isolated":false}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := e.firewall()
	var texts []string
	for _, l := range f.Policy["net2"].After {
		texts = append(texts, l.Text)
	}
	got := strings.Join(texts, "|")
	if !strings.Contains(got, "Allow to the internet") || !strings.Contains(got, "Allow to LAN") || strings.Contains(got, "Isolated") {
		t.Fatalf("%s", got)
	}
	if lan := f.Policy["lan"].After; !strings.Contains(lan[len(lan)-2].Text, "Allow to Guest") {
		t.Fatalf("%+v", lan)
	}
	// forwards add a line to the internet tab
	e.addForward(`{"name":"Web","protocol":"tcp","external_ports":"443","to_ip":"192.168.1.50"}`)
	if before := e.firewall().Policy["wan"].Before; len(before) != 2 || !strings.Contains(before[1].Text, "port forwards") {
		t.Fatalf("%+v", before)
	}
}

func TestAddingARuleStagesItWithItsDefaults(t *testing.T) {
	e := fwEnv(t)
	rec := e.addRule(allowPrinter)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decodeInto[FirewallRule](t, rec)
	if r.ID != "fw1" || r.Name != "Printer" || !r.Enabled || r.From != "lan" || r.Action != "allow" || r.To != "net2" || r.Protocol != "tcp" ||
		r.Destination != "10.20.0.20" || r.Ports != "9100" || r.Source != "" || r.Log || !r.Pending || r.ShadowedBy != "" ||
		r.Text != "Allow TCP 9100, from LAN to Guest (10.20.0.20)" {
		t.Fatalf("%+v", r)
	}
	if e.mgr.NetworkConfig().Firewall != nil {
		t.Fatal("the active configuration changed: saving must only stage")
	}
	var found bool
	for _, c := range e.staged() {
		found = found || (c.Field == "firewall_rules" && c.Label == "Firewall rule Printer")
	}
	if !found {
		t.Fatalf("no review line: %+v", e.staged())
	}
	// the protocol defaults to any
	r = decodeInto[FirewallRule](t, e.addRule(`{"name":"Anything","from":"net2","action":"block","to":"internet"}`))
	if r.ID != "fw2" || r.Protocol != "any" {
		t.Fatalf("%+v", r)
	}
	if got := e.firewall().Rules; len(got) != 2 || got[0].ID != "fw1" || got[1].ID != "fw2" {
		t.Fatalf("%+v", got)
	}
}

func TestRuleErrorsNameTheirFieldsAndStageNothing(t *testing.T) {
	e := fwEnv(t)
	for name, tc := range map[string]struct{ body, field string }{
		"no name":              {`{"from":"lan","action":"allow","to":"net2"}`, "name"},
		"no action":            {`{"name":"x","from":"lan","to":"net2"}`, "action"},
		"bad action":           {`{"name":"x","from":"lan","action":"accept","to":"net2"}`, "action"},
		"no source":            {`{"name":"x","action":"allow","to":"net2"}`, "from"},
		"unknown source":       {`{"name":"x","from":"ghost","action":"allow","to":"net2"}`, "from"},
		"no destination":       {`{"name":"x","from":"lan","action":"allow"}`, "to"},
		"unknown to":           {`{"name":"x","from":"lan","action":"allow","to":"ghost"}`, "to"},
		"to itself":            {`{"name":"x","from":"lan","action":"allow","to":"lan"}`, "to"},
		"internet to internet": {`{"name":"x","from":"wan","action":"allow","to":"internet"}`, "to"},
		"bad protocol":         {`{"name":"x","from":"lan","action":"allow","to":"net2","protocol":"sctp"}`, "protocol"},
		"bad source":           {`{"name":"x","from":"lan","action":"allow","to":"net2","source":"printer"}`, "source"},
		"source elsewhere":     {`{"name":"x","from":"lan","action":"allow","to":"net2","source":"10.20.0.5"}`, "source"},
		"bad destination":      {`{"name":"x","from":"lan","action":"allow","to":"net2","destination":"192.168.1.5"}`, "destination"},
		"ports with icmp":      {`{"name":"x","from":"lan","action":"allow","to":"net2","protocol":"icmp","ports":"80"}`, "ports"},
		"bad ports":            {`{"name":"x","from":"lan","action":"allow","to":"net2","protocol":"tcp","ports":"70000"}`, "ports"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.addRule(tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if f := errorFields(t, rec); !f[tc.field] {
				t.Fatalf("errors %v lack %q", f, tc.field)
			}
		})
	}
	if e.draft().Firewall != nil && len(e.draft().Firewall.Rules) != 0 {
		t.Fatalf("a refused rule was staged: %+v", e.draft().Firewall)
	}
}

func TestPortsAreWrittenWithoutSpacesAndNamesAreTrimmed(t *testing.T) {
	e := fwEnv(t)
	r := decodeInto[FirewallRule](t, e.addRule(`{"name":"  Web  ","from":"lan","action":"allow","to":"internet","protocol":"tcp","ports":"80, 443 ,8000-8100"}`))
	if r.Ports != "80,443,8000-8100" || r.Name != "Web" {
		t.Fatalf("%+v", r)
	}
}

func TestChangingARule(t *testing.T) {
	e := fwEnv(t)
	e.addRule(allowPrinter)
	e.commit()
	if r := e.firewall().Rules[0]; r.Pending {
		t.Fatalf("pending right after the commit: %+v", r)
	}
	rec := e.rule(http.MethodPut, "fw1", `{"action":"block","log":true,"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	r := decodeInto[FirewallRule](t, rec)
	if r.Action != "block" || !r.Log || r.Enabled || !r.Pending || r.Name != "Printer" || r.Ports != "9100" || r.Destination != "10.20.0.20" {
		t.Fatalf("%+v", r)
	}
	// put back: nothing to commit
	r = decodeInto[FirewallRule](t, e.rule(http.MethodPut, "fw1", `{"action":"allow","log":false,"enabled":true}`))
	if r.Pending || e.stageFileExists() {
		t.Fatalf("%+v", r)
	}
	// text fields can be cleared
	r = decodeInto[FirewallRule](t, e.rule(http.MethodPut, "fw1", `{"destination":"","ports":"","protocol":"any"}`))
	if r.Destination != "" || r.Ports != "" || r.Protocol != "any" {
		t.Fatalf("%+v", r)
	}
}

func TestARuleStaysOnItsTab(t *testing.T) {
	e := fwEnv(t)
	e.addRule(allowPrinter)
	rec := e.rule(http.MethodPut, "fw1", `{"from":"net2","to":"lan","destination":""}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["from"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.rule(http.MethodPut, "fw1", `{"from":"lan","name":"Same tab"}`); rec.Code != http.StatusOK {
		t.Fatalf("naming the same tab is fine: %d %s", rec.Code, rec.Body)
	}
}

func TestEditErrorsOfOneRuleIgnoreAnotherRulesProblems(t *testing.T) {
	e := fwEnv(t)
	e.addRule(allowPrinter)
	e.addRule(`{"name":"Other","from":"net2","action":"block","to":"internet"}`)
	// make the second invalid behind the API's back
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error { c.Firewall.Rules[1].Ports = "junk"; return nil })
	if rec := e.rule(http.MethodPut, "fw1", `{"name":"Renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec := e.rule(http.MethodPut, "fw1", `{"ports":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if f := errorFields(t, rec); !f["ports"] || len(f) != 1 {
		t.Fatalf("%v", f)
	}
}

func TestDeletingARule(t *testing.T) {
	e := fwEnv(t)
	e.addRule(allowPrinter)
	e.addRule(`{"name":"Other","from":"net2","action":"block","to":"internet"}`)
	rec := e.rule(http.MethodDelete, "fw1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := decodeInto[firewallResponse](t, rec).Rules; len(got) != 1 || got[0].ID != "fw2" {
		t.Fatalf("%+v", got)
	}
	// the lowest free id is used again
	if r := decodeInto[FirewallRule](t, e.addRule(allowPrinter)); r.ID != "fw1" {
		t.Fatalf("%+v", r)
	}
	// deleting the last ones leaves no empty section and no draft
	e.rule(http.MethodDelete, "fw1", "")
	e.rule(http.MethodDelete, "fw2", "")
	if e.draft().Firewall != nil {
		t.Fatalf("%+v", e.draft().Firewall)
	}
	if rec := e.rule(http.MethodDelete, "fw9", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.rule(http.MethodPut, "fw9", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestARuleThatNeverMatchesIsFlagged(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"Everything","from":"lan","action":"block","to":"any"}`)
	e.addRule(allowPrinter)
	f := e.firewall()
	if f.Rules[0].ShadowedBy != "" || f.Rules[1].ShadowedBy != "fw1" {
		t.Fatalf("%+v", f.Rules)
	}
	// reordering fixes it
	if rec := e.fw(e.api.HandleFirewallOrder, http.MethodPut, "", `{"from":"lan","ids":["fw2","fw1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f = e.firewall()
	if f.Rules[0].ID != "fw2" || f.Rules[0].ShadowedBy != "" || f.Rules[1].ShadowedBy != "" {
		t.Fatalf("%+v", f.Rules)
	}
}

func TestReorderingOneTabLeavesTheOthersWhereTheyAre(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"a","from":"lan","action":"allow","to":"internet","protocol":"tcp","ports":"1"}`)  // fw1
	e.addRule(`{"name":"b","from":"net2","action":"allow","to":"internet","protocol":"tcp","ports":"2"}`) // fw2
	e.addRule(`{"name":"c","from":"lan","action":"allow","to":"internet","protocol":"tcp","ports":"3"}`)  // fw3
	e.addRule(`{"name":"d","from":"net2","action":"allow","to":"internet","protocol":"tcp","ports":"4"}`) // fw4
	e.addRule(`{"name":"e","from":"lan","action":"allow","to":"internet","protocol":"tcp","ports":"5"}`)  // fw5
	e.commit()                                                                                            // the order is only a change for rules that already exist
	order := func() string {
		var ids []string
		for _, r := range e.firewall().Rules {
			ids = append(ids, r.ID)
		}
		return strings.Join(ids, ",")
	}
	if rec := e.fw(e.api.HandleFirewallOrder, http.MethodPut, "", `{"from":"lan","ids":["fw5","fw1","fw3"]}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := order(); got != "fw5,fw2,fw1,fw4,fw3" {
		t.Fatalf("%s", got)
	}
	var changed bool
	for _, c := range e.staged() {
		changed = changed || c.Field == "firewall_order"
	}
	if !changed {
		t.Fatalf("no review line for the new order: %+v", e.staged())
	}
	for name, body := range map[string]string{
		"a missing id":     `{"from":"lan","ids":["fw5","fw1"]}`,
		"an extra id":      `{"from":"lan","ids":["fw5","fw1","fw3","fw2"]}`,
		"a repeated id":    `{"from":"lan","ids":["fw5","fw5","fw1"]}`,
		"another tab's id": `{"from":"lan","ids":["fw5","fw1","fw2"]}`,
		"an unknown tab":   `{"from":"ghost","ids":[]}`,
	} {
		rec := e.fw(e.api.HandleFirewallOrder, http.MethodPut, "", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if got := order(); got != "fw5,fw2,fw1,fw4,fw3" {
		t.Fatalf("a refused order changed the list: %s", got)
	}
}

func TestPortForwards(t *testing.T) {
	e := fwEnv(t)
	rec := e.addForward(`{"name":"Web","protocol":"tcp","external_ports":"443","to_ip":"192.168.1.50","internal_ports":"8443","sources":[" 203.0.113.7 ",""]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	p := decodeInto[PortForward](t, rec)
	if p.ID != "pf1" || !p.Enabled || p.ExternalPorts != "443" || p.InternalPorts != "8443" || len(p.Sources) != 1 || p.Sources[0] != "203.0.113.7" || !p.Pending ||
		p.Text != "TCP 443 from the internet to 192.168.1.50:8443, only from 203.0.113.7" {
		t.Fatalf("%+v", p)
	}
	if e.mgr.NetworkConfig().Firewall != nil {
		t.Fatal("the active configuration changed")
	}
	// a second one, to a device on the guest network
	p = decodeInto[PortForward](t, e.addForward(`{"name":"Game","protocol":"tcp+udp","external_ports":"5000-5010","to_ip":"10.20.0.5"}`))
	if p.ID != "pf2" || p.Sources == nil || len(p.Sources) != 0 {
		t.Fatalf("%+v", p)
	}

	// changing: what is left out stays; sources can be cleared; it can be switched off
	p = decodeInto[PortForward](t, e.forward(http.MethodPut, "pf1", `{"sources":[],"enabled":false,"internal_ports":""}`))
	if p.Enabled || len(p.Sources) != 0 || p.InternalPorts != "" || p.ExternalPorts != "443" || p.ToIP != "192.168.1.50" {
		t.Fatalf("%+v", p)
	}
	// deleting
	rec = e.forward(http.MethodDelete, "pf1", "")
	if rec.Code != http.StatusOK || len(decodeInto[firewallResponse](t, rec).Forwards) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.forward(http.MethodDelete, "pf2", "")
	if e.draft().Firewall != nil {
		t.Fatalf("an empty section is left: %+v", e.draft().Firewall)
	}
	if rec := e.forward(http.MethodDelete, "pf9", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestPortForwardErrorsNameTheirFields(t *testing.T) {
	e := fwEnv(t)
	e.addForward(`{"name":"Web","protocol":"tcp","external_ports":"443","to_ip":"192.168.1.50"}`)
	for name, tc := range map[string]struct{ body, field string }{
		"no name":          {`{"protocol":"tcp","external_ports":"80","to_ip":"192.168.1.50"}`, "name"},
		"icmp":             {`{"name":"x","protocol":"icmp","external_ports":"80","to_ip":"192.168.1.50"}`, "protocol"},
		"no port":          {`{"name":"x","protocol":"tcp","to_ip":"192.168.1.50"}`, "external_ports"},
		"bad port":         {`{"name":"x","protocol":"tcp","external_ports":"99999","to_ip":"192.168.1.50"}`, "external_ports"},
		"overlap":          {`{"name":"x","protocol":"tcp","external_ports":"400-500","to_ip":"192.168.1.50"}`, "external_ports"},
		"bad internal":     {`{"name":"x","protocol":"tcp","external_ports":"80","to_ip":"192.168.1.50","internal_ports":"x"}`, "internal_ports"},
		"range moved":      {`{"name":"x","protocol":"tcp","external_ports":"5000-5010","to_ip":"192.168.1.50","internal_ports":"6000-6010"}`, "internal_ports"},
		"no address":       {`{"name":"x","protocol":"tcp","external_ports":"80"}`, "to_ip"},
		"not on a network": {`{"name":"x","protocol":"tcp","external_ports":"80","to_ip":"172.16.0.5"}`, "to_ip"},
		"the router":       {`{"name":"x","protocol":"tcp","external_ports":"80","to_ip":"192.168.1.1"}`, "to_ip"},
		"bad source":       {`{"name":"x","protocol":"tcp","external_ports":"80","to_ip":"192.168.1.50","sources":["office"]}`, "sources"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := e.addForward(tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if f := errorFields(t, rec); !f[tc.field] {
				t.Fatalf("errors %v lack %q", f, tc.field)
			}
		})
	}
	if got := e.firewall().Forwards; len(got) != 1 {
		t.Fatalf("a refused forward was staged: %+v", got)
	}
}

func TestACommittedFirewallIsNotPending(t *testing.T) {
	e := fwEnv(t)
	e.addRule(allowPrinter)
	e.addForward(`{"name":"Web","protocol":"tcp","external_ports":"443","to_ip":"192.168.1.50"}`)
	e.commit()
	f := e.firewall()
	if f.Rules[0].Pending || f.Forwards[0].Pending {
		t.Fatalf("%+v %+v", f.Rules, f.Forwards)
	}
	e.forward(http.MethodPut, "pf1", `{"external_ports":"8443"}`)
	if f = e.firewall(); !f.Forwards[0].Pending || f.Rules[0].Pending {
		t.Fatalf("%+v %+v", f.Rules, f.Forwards)
	}
}

func TestACommittedRuleSetIsAcceptedByTheWholePipeline(t *testing.T) {
	// the rule set the tests build must also be one the daemon would apply: render and check it
	e := fwEnv(t)
	e.addRule(allowPrinter)
	e.addForward(`{"name":"Web","protocol":"tcp","external_ports":"443","to_ip":"192.168.1.50"}`)
	if err := e.draft().Validate(); err != nil {
		t.Fatalf("the draft does not validate: %v", err)
	}
}

func TestACounterEndpoint(t *testing.T) {
	e := fwEnv(t)
	get := func() (known bool, counters map[string]Counter) {
		var out struct {
			Known    bool               `json:"known"`
			Counters map[string]Counter `json:"counters"`
		}
		rec := e.fw(e.api.HandleFirewallCounters, http.MethodGet, "", "")
		out = decodeInto[struct {
			Known    bool               `json:"known"`
			Counters map[string]Counter `json:"counters"`
		}](t, rec)
		return out.Known, out.Counters
	}
	if known, c := get(); known || c == nil || len(c) != 0 {
		t.Fatalf("not reported: %v %v", known, c)
	}
	e.api.FirewallCounters = func() (map[string]Counter, bool) { return nil, false }
	if known, _ := get(); known {
		t.Fatal("unreadable counters reported as known")
	}
	e.api.FirewallCounters = func() (map[string]Counter, bool) { return map[string]Counter{"fw1": {Packets: 3, Bytes: 180}}, true }
	if known, c := get(); !known || c["fw1"] != (Counter{3, 180}) {
		t.Fatalf("%v %v", known, c)
	}
	if rec := e.fw(e.api.HandleFirewallCounters, http.MethodPost, "", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
}

func TestFirewallEditsAreRefusedWhileApplyingAndRequestsAreStrict(t *testing.T) {
	e := fwEnv(t)
	e.addRule(allowPrinter)
	e.editable = false
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"add rule":    e.addRule(allowPrinter),
		"change rule": e.rule(http.MethodPut, "fw1", `{"name":"x"}`),
		"delete rule": e.rule(http.MethodDelete, "fw1", ""),
		"add forward": e.addForward(`{"name":"x"}`),
		"order":       e.fw(e.api.HandleFirewallOrder, http.MethodPut, "", `{"from":"lan","ids":["fw1"]}`),
	} {
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if e.firewall().Editable {
		t.Fatal("editable while applying")
	}
	e.editable = true
	if rec := e.addRule(`{"name":"x","colour":"red"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := e.addForward(`{"name":"x","vlan":5}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown forward field: %d", rec.Code)
	}
	if rec := e.fw(e.api.HandleFirewall, http.MethodPost, "", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("method: %d", rec.Code)
	}
	if rec := e.fw(e.api.HandleFirewallRules, http.MethodGet, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("method: %d", rec.Code)
	}
}

func TestANetworkThatARuleOrAForwardUsesCannotBeDeleted(t *testing.T) {
	e := fwEnv(t)
	e.addRule(allowPrinter) // lan to net2
	rec := e.net(http.MethodDelete, "net2", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Printer") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	e.rule(http.MethodDelete, "fw1", "")

	e.addRule(`{"name":"From guest","from":"net2","action":"block","to":"internet"}`)
	if rec := e.net(http.MethodDelete, "net2", ""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "From guest") {
		t.Fatalf("a rule from the network: %d %s", rec.Code, rec.Body)
	}
	e.rule(http.MethodDelete, "fw1", "")

	e.addForward(`{"name":"Game","protocol":"tcp","external_ports":"5000","to_ip":"10.20.0.5"}`)
	if rec := e.net(http.MethodDelete, "net2", ""); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Game") {
		t.Fatalf("a forward to a device on the network: %d %s", rec.Code, rec.Body)
	}
	// a forward to another network does not hold it
	e.forward(http.MethodDelete, "pf1", "")
	e.addForward(`{"name":"Web","protocol":"tcp","external_ports":"443","to_ip":"192.168.1.50"}`)
	if rec := e.net(http.MethodDelete, "net2", ""); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestAnIsolatedDefaultNetworkHasNoLinesToTheOthers(t *testing.T) {
	e := fwEnv(t)
	if rec := e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","isolated":false}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	open := e.firewall().Policy["lan"].After
	var hasGuest bool
	for _, l := range open {
		hasGuest = hasGuest || l.Text == "Allow to Guest"
	}
	if !hasGuest {
		t.Fatalf("the open default network should reach the open guest network: %+v", open)
	}
	if rec := e.net(http.MethodPut, "lan", `{"ip":"192.168.1.1","netmask":"255.255.255.0","isolated":true}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := e.firewall()
	var isolated bool
	for _, l := range f.Policy["lan"].After {
		if l.Text == "Allow to Guest" {
			t.Errorf("an isolated default network still reaches Guest: %+v", f.Policy["lan"].After)
		}
		isolated = isolated || strings.HasPrefix(l.Text, "Isolated from the other networks")
	}
	if !isolated {
		t.Errorf("no 'isolated' line: %+v", f.Policy["lan"].After)
	}
	for _, l := range f.Policy["net2"].After {
		if l.Text == "Allow to LAN" {
			t.Errorf("the guest network still reaches an isolated LAN: %+v", f.Policy["net2"].After)
		}
	}
}

func TestTheRuleLimitIsReportedAsAListError(t *testing.T) {
	e := fwEnv(t)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.Firewall = &config.FirewallConfig{}
		for i := 1; i <= config.MaxFirewallRules; i++ {
			c.Firewall.Rules = append(c.Firewall.Rules, config.FirewallRule{ID: "fw" + itoa(i), Name: "r", From: "lan", Action: "allow", Protocol: "tcp", To: "internet", Ports: itoa(i)})
		}
		return nil
	})
	rec := e.addRule(`{"name":"one more","from":"lan","action":"allow","to":"internet","protocol":"tcp","ports":"9999"}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["list"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := len(e.draft().Firewall.Rules); got != config.MaxFirewallRules {
		t.Fatalf("the extra rule was staged: %d", got)
	}
}

func TestWordsAreAcceptedInAnyCase(t *testing.T) {
	e := fwEnv(t)
	r := decodeInto[FirewallRule](t, e.addRule(`{"name":"x","from":"lan","action":" ALLOW ","to":"internet","protocol":"TCP","ports":"80"}`))
	if r.Action != "allow" || r.Protocol != "tcp" {
		t.Fatalf("%+v", r)
	}
	r = decodeInto[FirewallRule](t, e.rule(http.MethodPut, "fw1", `{"action":"Block","protocol":"UDP"}`))
	if r.Action != "block" || r.Protocol != "udp" {
		t.Fatalf("%+v", r)
	}
}

func TestChangingOneRuleLeavesTheOthersAlone(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"first","from":"lan","action":"allow","to":"internet","protocol":"tcp","ports":"1"}`)
	e.addRule(`{"name":"second","from":"lan","action":"allow","to":"internet","protocol":"tcp","ports":"2"}`)
	e.addRule(`{"name":"third","from":"lan","action":"allow","to":"internet","protocol":"tcp","ports":"3"}`)
	e.rule(http.MethodPut, "fw2", `{"name":"changed","action":"block"}`)
	got := e.firewall().Rules
	if got[0].Name != "first" || got[0].Action != "allow" || got[1].Name != "changed" || got[1].Action != "block" || got[2].Name != "third" || got[2].Action != "allow" {
		t.Fatalf("%+v", got)
	}
	e.forward(http.MethodDelete, "pf1", "") // a missing forward is a 404, not a change
	e.addForward(`{"name":"a","protocol":"tcp","external_ports":"80","to_ip":"192.168.1.50"}`)
	e.addForward(`{"name":"b","protocol":"tcp","external_ports":"81","to_ip":"192.168.1.50"}`)
	e.forward(http.MethodPut, "pf2", `{"name":"b2"}`)
	f := e.firewall().Forwards
	if f[0].Name != "a" || f[1].Name != "b2" {
		t.Fatalf("%+v", f)
	}
}

func TestForwardEditsAreRefusedWhileApplying(t *testing.T) {
	e := fwEnv(t)
	e.addForward(`{"name":"a","protocol":"tcp","external_ports":"80","to_ip":"192.168.1.50"}`)
	e.editable = false
	if rec := e.forward(http.MethodPut, "pf1", `{"name":"x"}`); rec.Code != http.StatusConflict {
		t.Fatalf("change: %d", rec.Code)
	}
	if rec := e.forward(http.MethodDelete, "pf1", ""); rec.Code != http.StatusConflict {
		t.Fatalf("delete: %d", rec.Code)
	}
	if got := e.firewall().Forwards; len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("%+v", got)
	}
}

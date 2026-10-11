package netapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"peakroute/internal/network"
)

func (e *env) firewallLog(query string) (*httptest.ResponseRecorder, firewallLogResponse) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.api.HandleFirewallLog(rec, httptest.NewRequest(http.MethodGet, "/x"+query, nil))
	var out firewallLogResponse
	if rec.Code == http.StatusOK {
		out = decodeInto[firewallLogResponse](e.t, rec)
	}
	return rec, out
}

func TestTheFirewallLogIsNotReportedWithoutAProvider(t *testing.T) {
	e := standard(t)
	rec, out := e.firewallLog("")
	if rec.Code != http.StatusOK || out.Known || out.Entries == nil || len(out.Entries) != 0 {
		t.Fatalf("%d %+v", rec.Code, out)
	}
	e.api.FirewallLog = func(int) ([]network.FirewallLogEntry, bool) { return nil, false }
	if _, out := e.firewallLog(""); out.Known {
		t.Fatalf("an unreadable log reported as known: %+v", out)
	}
	e.api.FirewallLog = func(int) ([]network.FirewallLogEntry, bool) { return nil, true }
	if _, out := e.firewallLog(""); !out.Known || len(out.Entries) != 0 {
		t.Fatalf("an empty log: %+v", out)
	}
}

func TestLoggedPacketsAreShownNewestFirstWithTheirRule(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"Camera","from":"net2","action":"block","to":"internet","log":true}`)
	e.commit() // the log belongs to the rules in effect
	var asked int
	e.api.FirewallLog = func(limit int) ([]network.FirewallLogEntry, bool) {
		asked = limit
		return []network.FirewallLogEntry{
			{Time: 1000, Rule: "fw1", In: "br20", Out: "eth0", Src: "10.20.0.40", Dst: "1.1.1.1", Proto: "TCP", SrcPort: 40000, DstPort: 443},
			{Time: 2000, Rule: "fw9", In: "eth1", Src: "192.168.1.5", Dst: "192.168.1.1", Proto: "ICMP"},
			{Time: 3000, Rule: "fw1", In: "br20", Out: "eth0", Src: "10.20.0.41", Dst: "8.8.8.8", Proto: "UDP", SrcPort: 5, DstPort: 53},
		}, true
	}
	rec, out := e.firewallLog("")
	if rec.Code != http.StatusOK || !out.Known || len(out.Entries) != 3 || asked != 200 {
		t.Fatalf("%d asked=%d %+v", rec.Code, asked, out)
	}
	if out.Entries[0].Time != 3000 || out.Entries[1].Time != 2000 || out.Entries[2].Time != 1000 {
		t.Fatalf("not newest first: %+v", out.Entries)
	}
	first := out.Entries[0]
	if first.Name != "Camera" || first.Action != "block" || first.Text != "Block any protocol, from Guest to the internet, logged" ||
		first.Src != "10.20.0.41" || first.DstPort != 53 || first.In != "br20" || first.Out != "eth0" || first.Proto != "UDP" {
		t.Fatalf("%+v", first)
	}
	// a rule that is no longer there is shown by its id
	if gone := out.Entries[1]; gone.Name != "fw9" || gone.Action != "" || gone.Text != "" {
		t.Fatalf("%+v", gone)
	}
}

func TestTheFirewallLogIsLimited(t *testing.T) {
	e := fwEnv(t)
	var all []network.FirewallLogEntry
	for i := 1; i <= 30; i++ {
		all = append(all, network.FirewallLogEntry{Time: int64(i), Rule: "fw1", Src: "1.1.1.1", Dst: "2.2.2.2"})
	}
	var asked int
	e.api.FirewallLog = func(limit int) ([]network.FirewallLogEntry, bool) { asked = limit; return all, true }
	_, out := e.firewallLog("?limit=5")
	if asked != 5 || len(out.Entries) != 5 || out.Entries[0].Time != 30 || out.Entries[4].Time != 26 {
		t.Fatalf("asked %d: %+v", asked, out.Entries)
	}
	for _, bad := range []string{"?limit=0", "?limit=x", "?limit=1001", "?limit=-1"} {
		if rec, _ := e.firewallLog(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec, _ := e.firewallLog("?limit=1000"); rec.Code != http.StatusOK {
		t.Errorf("the largest limit: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	e.api.HandleFirewallLog(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestOnlyRulesInEffectAreLookedUp(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"Running","from":"net2","action":"block","to":"internet","log":true}`) // fw1
	e.commit()
	e.addRule(`{"name":"Draft only","from":"net2","action":"allow","to":"internet","log":true}`) // fw2, not committed
	e.rule(http.MethodPut, "fw1", `{"name":"Renamed in the draft"}`)
	e.api.FirewallLog = func(int) ([]network.FirewallLogEntry, bool) {
		return []network.FirewallLogEntry{
			{Time: 1, Rule: "fw1", Src: "10.20.0.5", Dst: "1.1.1.1"},
			{Time: 2, Rule: "fw2", Src: "10.20.0.5", Dst: "1.1.1.1"},
		}, true
	}
	_, out := e.firewallLog("")
	if out.Entries[0].Name != "fw2" || out.Entries[0].Action != "" {
		t.Errorf("a rule that is only in the draft cannot have logged anything: %+v", out.Entries[0])
	}
	if out.Entries[1].Name != "Running" || out.Entries[1].Action != "block" {
		t.Errorf("the rule as it runs, not as it is edited: %+v", out.Entries[1])
	}
}

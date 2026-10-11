package netapi

import (
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"peakroute/internal/fwlog"
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

// logStore gives the API a real log database and returns it.
func (e *env) logStore() *fwlog.Store {
	e.t.Helper()
	s, err := fwlog.Open(filepath.Join(e.t.TempDir(), "fw.db"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { s.Close() })
	e.api.FirewallLog = s
	return s
}

func logged(e *env, store *fwlog.Store, packets ...network.FirewallLogEntry) {
	e.t.Helper()
	if err := store.Add(packets, ""); err != nil {
		e.t.Fatal(err)
	}
}

func lp(at time.Time, rule, src, dst string, dport int) network.FirewallLogEntry {
	return network.FirewallLogEntry{Time: at.UnixMilli(), Rule: rule, In: "br20", Out: "eth0", Src: src, Dst: dst, Proto: "TCP", SrcPort: 40000, DstPort: dport}
}

func TestTheFirewallLogIsNotReportedWithoutAStore(t *testing.T) {
	e := standard(t)
	rec, out := e.firewallLog("")
	if rec.Code != http.StatusOK || out.Known || out.Entries == nil || len(out.Entries) != 0 {
		t.Fatalf("%d %+v", rec.Code, out)
	}
	e.logStore()
	if _, out := e.firewallLog(""); !out.Known || len(out.Entries) != 0 || out.Next != 0 || out.Total != 0 || out.KeepDays != 7 || out.KeepEntries != 100000 {
		t.Fatalf("an empty log: %+v", out)
	}
}

func TestLoggedPacketsAreShownNewestFirstWithTheirRule(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"Camera","from":"net2","action":"block","to":"internet","log":true}`)
	e.commit() // the log belongs to the rules in effect
	s := e.logStore()
	now := time.Now()
	logged(e, s,
		lp(now.Add(-3*time.Minute), "fw1", "10.20.0.40", "1.1.1.1", 443),
		network.FirewallLogEntry{Time: now.Add(-2 * time.Minute).UnixMilli(), Rule: "fw9", In: "eth1", Src: "192.168.1.5", Dst: "192.168.1.1", Proto: "ICMP"},
		lp(now.Add(-1*time.Minute), "fw1", "10.20.0.41", "8.8.8.8", 53),
		lp(now.Add(-1*time.Minute+10*time.Second), "fw1", "10.20.0.41", "8.8.8.8", 53), // a repeat
	)
	rec, out := e.firewallLog("")
	if rec.Code != http.StatusOK || !out.Known || len(out.Entries) != 3 || out.Total != 3 || out.Oldest != now.Add(-3*time.Minute).UnixMilli() {
		t.Fatalf("%d %+v", rec.Code, out)
	}
	first := out.Entries[0]
	if first.Src != "10.20.0.41" || first.Count != 2 || first.Last-first.Time != 10000 || first.Name != "Camera" || first.Action != "block" ||
		first.Text != "Block any protocol, from Guest to the internet, logged" || first.DstPort != 53 || first.In != "br20" || first.Out != "eth0" || first.Proto != "TCP" {
		t.Fatalf("%+v", first)
	}
	if out.Entries[2].Src != "10.20.0.40" || out.Entries[2].Count != 1 {
		t.Fatalf("not newest first: %+v", out.Entries)
	}
	// a rule that is no longer there is shown by its id
	if gone := out.Entries[1]; gone.Name != "fw9" || gone.Action != "" || gone.Text != "" {
		t.Fatalf("%+v", gone)
	}
}

func TestTheFirewallLogIsPaged(t *testing.T) {
	e := fwEnv(t)
	s := e.logStore()
	var batch []network.FirewallLogEntry
	base := time.Now().Add(-time.Hour)
	for i := 1; i <= 30; i++ {
		batch = append(batch, lp(base.Add(time.Duration(i)*time.Second), "fw1", "1.1.1.1", "2.2.2.2", 1000+i))
	}
	logged(e, s, batch...)
	_, out := e.firewallLog("?limit=5")
	if len(out.Entries) != 5 || out.Entries[0].DstPort != 1030 || out.Entries[4].DstPort != 1026 || out.Next == 0 || out.Total != 30 {
		t.Fatalf("%+v", out)
	}
	_, page2 := e.firewallLog("?limit=5&before=" + strconv.FormatInt(out.Next, 10))
	if len(page2.Entries) != 5 || page2.Entries[0].DstPort != 1025 {
		t.Fatalf("%+v", page2)
	}
	// the last page says there is no more
	_, last := e.firewallLog("?limit=50")
	if len(last.Entries) != 30 || last.Next != 0 {
		t.Fatalf("%d entries, next %d", len(last.Entries), last.Next)
	}
	for _, bad := range []string{"?limit=0", "?limit=x", "?limit=1001", "?limit=-1", "?before=0", "?before=x", "?hours=0", "?hours=169", "?hours=x", "?action=maybe", "?q=" + strings.Repeat("a", 101)} {
		if rec, _ := e.firewallLog(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	for _, ok := range []string{"?limit=1000", "?hours=1", "?hours=168", "?action=allow", "?action=reject", "?q=" + strings.Repeat("a", 100)} {
		if rec, _ := e.firewallLog(ok); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", ok, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	e.api.HandleFirewallLog(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestTheFirewallLogCanBeFiltered(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"Camera","from":"net2","action":"block","to":"internet","log":true}`) // fw1
	e.addRule(`{"name":"Printer","from":"net2","action":"allow","to":"lan","log":true}`)     // fw2
	e.addRule(`{"name":"Web in","from":"lan","action":"reject","to":"internet","log":true}`) // fw3
	e.commit()
	s := e.logStore()
	now := time.Now()
	logged(e, s,
		lp(now.Add(-10*time.Hour), "fw1", "10.20.0.1", "1.1.1.1", 22),
		lp(now.Add(-5*time.Hour), "fw2", "10.20.0.2", "192.168.1.9", 9100),
		lp(now.Add(-2*time.Hour), "fw3", "192.168.1.5", "8.8.8.8", 443),
		lp(now.Add(-30*time.Minute), "fw9", "10.20.0.3", "9.9.9.9", 80), // a rule that has been deleted since
	)
	srcs := func(query string) string {
		t.Helper()
		rec, out := e.firewallLog(query)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", query, rec.Code, rec.Body)
		}
		var s []string
		for _, en := range out.Entries {
			s = append(s, en.Src)
		}
		return strings.Join(s, " ")
	}
	cases := map[string]string{
		"":                               "10.20.0.3 192.168.1.5 10.20.0.2 10.20.0.1",
		"?rule=fw2":                      "10.20.0.2",
		"?rule=fw9":                      "10.20.0.3",
		"?rule=nope":                     "",
		"?action=block":                  "10.20.0.1",
		"?action=allow":                  "10.20.0.2",
		"?action=reject":                 "192.168.1.5",
		"?network=net2":                  "10.20.0.2 10.20.0.1",
		"?network=lan":                   "192.168.1.5",
		"?network=wan":                   "",
		"?network=net2&action=allow":     "10.20.0.2",
		"?network=lan&action=allow":      "",
		"?rule=fw1&action=allow":         "",
		"?rule=fw1&network=net2":         "10.20.0.1",
		"?hours=3":                       "10.20.0.3 192.168.1.5",
		"?hours=6":                       "10.20.0.3 192.168.1.5 10.20.0.2",
		"?q=8.8.8.8":                     "192.168.1.5",
		"?q=9100":                        "10.20.0.2",
		"?q=8.8.8.8&hours=1":             "",
		"?action=allow&q=192.168.1.9":    "10.20.0.2",
		"?q=%25":                         "",
		"?network=net2&hours=6&q=10.20.": "10.20.0.2",
	}
	for q, want := range cases {
		if got := srcs(q); got != want {
			t.Errorf("%q: %q, want %q", q, got, want)
		}
	}
}

func TestTheFirewallLogDownloadsAsCSV(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"=HYPERLINK(\"http://evil\")","from":"net2","action":"block","to":"internet","log":true}`) // fw1
	e.addRule(`{"name":"Printer, with a comma","from":"net2","action":"allow","to":"lan","log":true}`)            // fw2
	e.commit()
	s := e.logStore()
	now := time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC)
	var batch []network.FirewallLogEntry
	batch = append(batch, lp(now, "fw1", "10.20.0.1", "1.1.1.1", 22), lp(now.Add(10*time.Second), "fw1", "10.20.0.1", "1.1.1.1", 22))
	batch = append(batch, network.FirewallLogEntry{Time: now.Add(time.Minute * 5).UnixMilli(), Rule: "fw2", In: "eth1", Src: "192.168.1.5", Dst: "10.20.0.5", Proto: "ICMP"})
	for i := 0; i < 250; i++ { // more than a page
		batch = append(batch, lp(now.Add(time.Duration(10+i)*time.Minute), "fw2", "10.20.0.9", "2.2.2.2", 1000+i))
	}
	logged(e, s, batch...)

	rec := httptest.NewRecorder()
	e.api.HandleFirewallLogCSV(rec, httptest.NewRequest(http.MethodGet, "/x?action=allow", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), `attachment; filename="firewall-log-`) || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	rows, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1+251 { // the header, the ICMP one and all 250 (the page size does not apply)
		t.Fatalf("%d rows", len(rows))
	}
	if strings.Join(rows[0], "|") != "first seen (UTC)|last seen (UTC)|times|rule|action|in|out|source|source port|destination|destination port|protocol" {
		t.Fatalf("%v", rows[0])
	}
	icmp := rows[len(rows)-251]
	_ = icmp
	var found []string
	for _, r := range rows[1:] {
		if r[7] == "192.168.1.5" {
			found = r
		}
	}
	if strings.Join(found, "|") != "2026-10-11 12:35:00|2026-10-11 12:35:00|1|Printer, with a comma|allow|eth1||192.168.1.5||10.20.0.5||ICMP" {
		t.Fatalf("%q", found)
	}
	// a download is everything the filters select: the page size and the page cursor do not apply
	rec = httptest.NewRecorder()
	e.api.HandleFirewallLogCSV(rec, httptest.NewRequest(http.MethodGet, "/x?action=allow&limit=5&before=3", nil))
	if rows, _ := csv.NewReader(rec.Body).ReadAll(); len(rows) != 1+251 {
		t.Fatalf("%d rows with a limit and a cursor", len(rows))
	}
	// a rule name that starts like a formula is not run by a spreadsheet; folded repeats keep their count and both times
	rec = httptest.NewRecorder()
	e.api.HandleFirewallLogCSV(rec, httptest.NewRequest(http.MethodGet, "/x?action=block", nil))
	rows, _ = csv.NewReader(rec.Body).ReadAll()
	if len(rows) != 2 || rows[1][0] != "2026-10-11 12:30:00" || rows[1][1] != "2026-10-11 12:30:10" || rows[1][2] != "2" || !strings.HasPrefix(rows[1][3], "'=HYPERLINK") {
		t.Fatalf("%q", rows)
	}
	for _, c := range []struct{ in, want string }{{"=1", "'=1"}, {"+1", "'+1"}, {"-1", "'-1"}, {"@x", "'@x"}, {"\tx", "'\tx"}, {"\rx", "'\rx"}, {"ok", "ok"}, {"", ""}, {"a=b", "a=b"}} {
		if got := csvCell(c.in); got != c.want {
			t.Errorf("csvCell(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// the filters are checked like for the page; and without a store there is nothing to download
	rec = httptest.NewRecorder()
	e.api.HandleFirewallLogCSV(rec, httptest.NewRequest(http.MethodGet, "/x?action=maybe", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad filter: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	e.api.HandleFirewallLogCSV(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
	e.api.FirewallLog = nil
	rec = httptest.NewRecorder()
	e.api.HandleFirewallLogCSV(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: %d", rec.Code)
	}
}

func TestOnlyRulesInEffectAreLookedUp(t *testing.T) {
	e := fwEnv(t)
	e.addRule(`{"name":"Running","from":"net2","action":"block","to":"internet","log":true}`) // fw1
	e.commit()
	e.addRule(`{"name":"Draft only","from":"net2","action":"allow","to":"internet","log":true}`) // fw2, not committed
	e.rule(http.MethodPut, "fw1", `{"name":"Renamed in the draft"}`)
	s := e.logStore()
	now := time.Now()
	logged(e, s, lp(now.Add(-time.Minute), "fw1", "10.20.0.5", "1.1.1.1", 1), lp(now, "fw2", "10.20.0.6", "1.1.1.1", 1))
	_, out := e.firewallLog("")
	if out.Entries[0].Name != "fw2" || out.Entries[0].Action != "" {
		t.Errorf("a rule that is only in the draft cannot have logged anything: %+v", out.Entries[0])
	}
	if out.Entries[1].Name != "Running" || out.Entries[1].Action != "block" {
		t.Errorf("the rule as it runs, not as it is edited: %+v", out.Entries[1])
	}
	// and the filters use the rules as they run too: the draft rule does not count as "allow"
	if _, out := e.firewallLog("?action=allow"); len(out.Entries) != 0 {
		t.Errorf("%+v", out.Entries)
	}
}

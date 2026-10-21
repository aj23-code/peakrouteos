package fwlog

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/network"
)

var t0 = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func ms(d time.Duration) int64 { return t0.Add(d).UnixMilli() }

func pkt(at time.Duration, rule, src, dst string, dport int) network.FirewallLogEntry {
	return network.FirewallLogEntry{Time: ms(at), Rule: rule, In: "br20", Out: "eth0", Src: src, Dst: dst, Proto: "TCP", SrcPort: 40000, DstPort: dport}
}

// open makes a store whose clock is the given time (the test moves it by assigning to *now).
func open(t *testing.T, now *time.Time) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "fw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.now = func() time.Time { return *now }
	s.lastPrune = time.Time{} // Open pruned with the real clock
	return s
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRepeatsAreFoldedIntoOneEntry(t *testing.T) {
	now := t0
	s := open(t, &now)
	var batch []network.FirewallLogEntry
	for i := 0; i < 5; i++ { // a scan: the same packet every 10 s
		batch = append(batch, pkt(time.Duration(i)*10*time.Second, "fw1", "10.0.0.5", "1.2.3.4", 22))
	}
	must(t, s.Add(batch, ""))
	got, err := s.Query(Query{})
	must(t, err)
	if len(got) != 1 || got[0].Count != 5 || got[0].Time != ms(0) || got[0].Last != ms(40*time.Second) {
		t.Fatalf("%+v", got)
	}
	// the same kind of packet in a later batch (and a later transaction) still counts in the same entry
	must(t, s.Add([]network.FirewallLogEntry{pkt(70*time.Second, "fw1", "10.0.0.5", "1.2.3.4", 22)}, ""))
	got, _ = s.Query(Query{})
	if len(got) != 1 || got[0].Count != 6 || got[0].Last != ms(70*time.Second) {
		t.Fatalf("%+v", got)
	}
	// a pause longer than the window starts a new entry
	must(t, s.Add([]network.FirewallLogEntry{pkt(70*time.Second+Window+time.Second, "fw1", "10.0.0.5", "1.2.3.4", 22)}, ""))
	got, _ = s.Query(Query{})
	if len(got) != 2 || got[0].Count != 1 || got[1].Count != 6 {
		t.Fatalf("%+v", got)
	}
}

func TestOnlyTheSameKindOfPacketIsFolded(t *testing.T) {
	now := t0
	s := open(t, &now)
	base := pkt(0, "fw1", "10.0.0.5", "1.2.3.4", 22)
	vary := []func(*network.FirewallLogEntry){
		func(e *network.FirewallLogEntry) { e.Rule = "fw2" },
		func(e *network.FirewallLogEntry) { e.In = "br30" },
		func(e *network.FirewallLogEntry) { e.Out = "eth1" },
		func(e *network.FirewallLogEntry) { e.Src = "10.0.0.6" },
		func(e *network.FirewallLogEntry) { e.Dst = "1.2.3.5" },
		func(e *network.FirewallLogEntry) { e.Proto = "UDP" },
		func(e *network.FirewallLogEntry) { e.DstPort = 23 },
	}
	batch := []network.FirewallLogEntry{base}
	for _, f := range vary {
		e := base
		e.Time += 1000
		f(&e)
		batch = append(batch, e)
	}
	must(t, s.Add(batch, ""))
	if got, _ := s.Query(Query{}); len(got) != 1+len(vary) {
		t.Fatalf("each difference is its own entry: %d entries", len(got))
	}
	// the source port does not matter (a scan uses a new one every time)
	e := base
	e.Time += 2000
	e.SrcPort = 51234
	must(t, s.Add([]network.FirewallLogEntry{e}, ""))
	got, _ := s.Query(Query{})
	first := got[len(got)-1]
	if first.ID != 1 || first.Count != 2 || first.SrcPort != 40000 || len(got) != 1+len(vary) {
		t.Fatalf("%+v", got)
	}
}

func TestAFailedBatchLeavesNothingBehind(t *testing.T) {
	now := t0
	s := open(t, &now)
	must(t, s.Add([]network.FirewallLogEntry{pkt(0, "fw1", "10.0.0.5", "1.2.3.4", 22)}, "c1"))
	s.db.Exec(`DROP TABLE meta`) // the cursor cannot be saved: the whole batch must fail
	if err := s.Add([]network.FirewallLogEntry{pkt(time.Second, "fw1", "10.0.0.5", "1.2.3.4", 22), pkt(2*time.Second, "fw2", "10.0.0.5", "1.2.3.4", 22)}, "c2"); err == nil {
		t.Fatal("expected an error")
	}
	got, _ := s.Query(Query{})
	if len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("a failed batch changed the log: %+v", got)
	}
	// ...and what it was about to remember was not remembered: after the table is back the same packet is counted once
	s.db.Exec(`CREATE TABLE meta (name TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	must(t, s.Add([]network.FirewallLogEntry{pkt(time.Second, "fw1", "10.0.0.5", "1.2.3.4", 22)}, "c3"))
	got, _ = s.Query(Query{})
	if len(got) != 1 || got[0].Count != 2 || s.Cursor() != "c3" {
		t.Fatalf("%+v %q", got, s.Cursor())
	}
}

func TestTheCursorIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fw.db")
	s, err := Open(path)
	must(t, err)
	if s.Cursor() != "" {
		t.Fatal("a new log has a cursor")
	}
	// Open prunes by the real clock when it reopens the file, so these entries are from now (a fixed date would age out of the log)
	first := pkt(0, "fw1", "10.0.0.5", "1.2.3.4", 22)
	first.Time = time.Now().Add(-time.Minute).UnixMilli()
	second := pkt(0, "fw1", "10.0.0.5", "1.2.3.4", 22)
	second.Time = first.Time + 1000
	must(t, s.Add([]network.FirewallLogEntry{first}, "s=abc;i=1"))
	must(t, s.Add(nil, "")) // an empty cursor leaves the old one
	must(t, s.Add([]network.FirewallLogEntry{second}, "s=abc;i=2"))
	s.Close()
	s, err = Open(path)
	must(t, err)
	defer s.Close()
	if s.Cursor() != "s=abc;i=2" {
		t.Fatalf("%q", s.Cursor())
	}
	if got, _ := s.Query(Query{}); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestOldEntriesAreDropped(t *testing.T) {
	now := t0.Add(10 * 24 * time.Hour)
	s := open(t, &now)
	day := 24 * time.Hour
	must(t, s.Add([]network.FirewallLogEntry{
		pkt(1*day, "fw1", "10.0.0.1", "1.1.1.1", 1), // 9 days old
		pkt(3*day, "fw1", "10.0.0.2", "1.1.1.1", 1), // 7 days old exactly: kept
		pkt(9*day, "fw1", "10.0.0.3", "1.1.1.1", 1), // 1 day old
	}, ""))
	got, _ := s.Query(Query{})
	if len(got) != 2 || got[0].Src != "10.0.0.3" || got[1].Src != "10.0.0.2" {
		t.Fatalf("%+v", got)
	}
	// time passes: the next write drops what became too old, and not before a minute has passed
	now = now.Add(3 * day)
	must(t, s.Add([]network.FirewallLogEntry{pkt(13*day, "fw1", "10.0.0.4", "1.1.1.1", 1)}, ""))
	got, _ = s.Query(Query{})
	if len(got) != 2 || got[0].Src != "10.0.0.4" || got[1].Src != "10.0.0.3" {
		t.Fatalf("%+v", got)
	}
}

func TestTheOldestEntriesGoWhenThereAreTooMany(t *testing.T) {
	now := t0
	s := open(t, &now)
	s.KeepEntries = 50
	var batch []network.FirewallLogEntry
	for i := 0; i < 80; i++ {
		batch = append(batch, pkt(time.Duration(i)*time.Second, "fw1", "10.0.0."+strings.Repeat("1", 1), "1.1.1.1", 1000+i)) // all different: 80 entries
	}
	now = t0.Add(time.Hour)
	must(t, s.Add(batch, ""))
	st, _ := s.Stats()
	if st.Entries != 50 {
		t.Fatalf("%d entries", st.Entries)
	}
	got, _ := s.Query(Query{})
	if got[0].DstPort != 1079 || got[49].DstPort != 1030 || st.Oldest != ms(30*time.Second) {
		t.Fatalf("newest %d, oldest %d, stats %+v", got[0].DstPort, got[49].DstPort, st)
	}
}

func TestQueries(t *testing.T) {
	now := t0
	s := open(t, &now)
	must(t, s.Add([]network.FirewallLogEntry{
		pkt(0, "fw1", "10.0.0.1", "1.1.1.1", 22),
		pkt(1*time.Hour, "fw2", "10.0.0.2", "8.8.8.8", 53),
		{Time: ms(2 * time.Hour), Rule: "fw1", In: "wlan_guest", Out: "", Src: "10.0.0.3", Dst: "192.168.1.1", Proto: "ICMP"},
		pkt(3*time.Hour, "fw3", "10.0.0.4", "1.1.1.1", 443),
	}, ""))
	srcs := func(q Query) string {
		got, err := s.Query(q)
		must(t, err)
		var out []string
		for _, e := range got {
			out = append(out, e.Src[len("10.0.0."):])
		}
		return strings.Join(out, ",")
	}
	cases := []struct {
		name string
		q    Query
		want string
	}{
		{"all, newest first", Query{}, "4,3,2,1"},
		{"limit", Query{Limit: 2}, "4,3"},
		{"before an id", Query{Before: 3}, "2,1"},
		{"one rule", Query{Rules: []string{"fw1"}, RulesSet: true}, "3,1"},
		{"two rules", Query{Rules: []string{"fw2", "fw3"}, RulesSet: true}, "4,2"},
		{"no rule at all matches nothing", Query{Rules: nil, RulesSet: true}, ""},
		{"rules not asked about are not a filter", Query{Rules: []string{"fw1"}}, "4,3,2,1"},
		{"since", Query{Since: ms(90 * time.Minute)}, "4,3"},
		{"since is inclusive", Query{Since: ms(2 * time.Hour)}, "4,3"},
		{"a source port is searchable", Query{Search: "40000"}, "4,2,1"},
		{"a backslash is only text", Query{Search: `\`}, ""},
		{"search an address", Query{Search: "8.8.8"}, "2"},
		{"search a port", Query{Search: "443"}, "4"},
		{"a number is also found inside an address", Query{Search: "192"}, "3"},
		{"search an interface", Query{Search: "guest"}, "3"},
		{"search a protocol, any case", Query{Search: "icmp"}, "3"},
		{"search and rule together", Query{Search: "1.1.1.1", Rules: []string{"fw3"}, RulesSet: true}, "4"},
		{"percent is text, not a wildcard", Query{Search: "%"}, ""},
		{"underscore is text, not a wildcard", Query{Search: "wlan_g"}, "3"},
		{"underscore does not match any character", Query{Search: "wlan-guest"}, ""},
		{"a quote is only text", Query{Search: "' OR 1=1 --"}, ""},
	}
	if none, err := s.Query(Query{RulesSet: true}); err != nil || none == nil || len(none) != 0 {
		t.Errorf("an empty rule set: %v %v", none, err)
	}
	for _, c := range cases {
		if got := srcs(c.q); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestStats(t *testing.T) {
	now := t0
	s := open(t, &now)
	if st, err := s.Stats(); err != nil || st.Entries != 0 || st.Oldest != 0 {
		t.Fatalf("%+v %v", st, err)
	}
	must(t, s.Add([]network.FirewallLogEntry{pkt(5*time.Second, "fw1", "10.0.0.1", "1.1.1.1", 22), pkt(9*time.Second, "fw1", "10.0.0.2", "1.1.1.1", 22)}, ""))
	if st, _ := s.Stats(); st.Entries != 2 || st.Oldest != ms(5*time.Second) {
		t.Fatalf("%+v", st)
	}
}

func TestTheWindowIsInclusiveAndOutOfOrderPacketsStartTheirOwnEntry(t *testing.T) {
	now := t0
	s := open(t, &now)
	must(t, s.Add([]network.FirewallLogEntry{
		pkt(10*time.Second, "fw1", "10.0.0.5", "1.2.3.4", 22),
		pkt(10*time.Second+Window, "fw1", "10.0.0.5", "1.2.3.4", 22), // exactly a window later: still the same entry
		pkt(5*time.Second, "fw1", "10.0.0.5", "1.2.3.4", 22),         // older than the entry's last packet: its own entry
	}, ""))
	got, _ := s.Query(Query{})
	if len(got) != 2 || got[0].Count != 1 || got[0].Time != ms(5*time.Second) || got[1].Count != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestAnEntryThatKeepsBeingHitIsNotOld(t *testing.T) {
	day := 24 * time.Hour
	now := t0
	s := open(t, &now)
	var batch []network.FirewallLogEntry
	for d := 0; d < 10; d++ { // a packet a minute apart for a while, starting 10 days ago: one entry, last seen just now
		batch = append(batch, pkt(time.Duration(d)*10*time.Second, "fw1", "10.0.0.1", "1.1.1.1", 1))
	}
	must(t, s.Add(batch, ""))
	now = t0.Add(10 * day)
	s.db.Exec(`UPDATE events SET last = ?`, ms(9*day)) // seen again yesterday
	must(t, s.prune())
	if got, _ := s.Query(Query{}); len(got) != 1 {
		t.Fatalf("an entry seen yesterday was dropped for its first packet: %+v", got)
	}
}

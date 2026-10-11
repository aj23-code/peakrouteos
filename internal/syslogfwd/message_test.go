package syslogfwd

import (
	"strings"
	"testing"
	"time"

	"peakroute/internal/network"
)

var at = time.Date(2026, 10, 11, 14, 30, 5, 123456000, time.UTC)

func TestAMessageIsWrittenAsRFC5424(t *testing.T) {
	m := Message{Time: at, Facility: 3, Severity: 4, Host: "my-router", App: "dnsmasq", ProcID: "812", MsgID: "ID1", Text: "cannot resolve x"}
	want := "<28>1 2026-10-11T14:30:05.123456Z my-router dnsmasq 812 ID1 - cannot resolve x"
	if got := m.Format(); got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
	// the zone of the time does not matter: it is written in UTC
	local := time.FixedZone("x", 5*3600)
	m.Time = at.In(local)
	if got := m.Format(); got != want {
		t.Fatalf("%q", got)
	}
	// empty fields are a dash
	empty := Message{Time: at, Facility: 1, Severity: 6, Text: "x"}
	if got := empty.Format(); got != "<14>1 2026-10-11T14:30:05.123456Z - - - - - x" {
		t.Fatalf("%q", got)
	}
	// the priority is facility * 8 + severity, and nonsense becomes user / informational
	for _, c := range []struct {
		fac, sev int
		want     string
	}{{0, 0, "<0>"}, {23, 7, "<191>"}, {4, 4, "<36>"}, {24, 3, "<11>"}, {-1, 3, "<11>"}, {3, 8, "<30>"}, {3, -1, "<30>"}} {
		if got := (Message{Time: at, Facility: c.fac, Severity: c.sev, Text: "x"}).Format(); !strings.HasPrefix(got, c.want+"1 ") {
			t.Errorf("facility %d severity %d: %s, want %s", c.fac, c.sev, got, c.want)
		}
	}
}

func TestAMessageCannotBreakItsFraming(t *testing.T) {
	m := Message{Time: at, Facility: 1, Severity: 6, Host: "my router", App: "a b\nc", ProcID: "1 2", MsgID: strings.Repeat("m", 100), Text: "line one\nline two\r\nthree\x00four"}
	got := m.Format()
	if strings.ContainsAny(got, "\n\r\x00") {
		t.Fatalf("%q", got)
	}
	// header fields stay single words: spaces inside become underscores, long ones are cut
	if !strings.Contains(got, " my_router a_bc 1_2 "+strings.Repeat("m", 32)+" - line one line two three four") {
		t.Fatalf("%q", got)
	}
	// non-ASCII in a header field is replaced; in the text it stays
	u := Message{Time: at, Facility: 1, Severity: 6, Host: "räuter", App: "app", Text: "größe ✓"}.Format()
	if !strings.Contains(u, " r_uter app - - - größe ✓") {
		t.Fatalf("%q", u)
	}
	// a long text is cut on a character boundary, never in the middle of one
	long := Message{Time: at, Facility: 1, Severity: 6, Text: strings.Repeat("ü", 2000)}.Format()
	text := long[strings.Index(long, " - - - - - ")+len(" - - - - - "):]
	if len(text) > maxText || !strings.HasPrefix(text, "ü") || strings.ContainsRune(text, '�') || len(text)%2 != 0 {
		t.Fatalf("%d bytes, %q...", len(text), text[:10])
	}
}

func TestJournalLines(t *testing.T) {
	line := `{"MESSAGE":"Started Foo.","PRIORITY":"5","SYSLOG_FACILITY":"3","SYSLOG_IDENTIFIER":"systemd","_COMM":"systemd","_PID":"1","__REALTIME_TIMESTAMP":"1791729005123456","_TRANSPORT":"journal"}`
	m, ok := FromJournalLine([]byte(line), "gw")
	if !ok || m.Text != "Started Foo." || m.Severity != 5 || m.Facility != 3 || m.App != "systemd" || m.ProcID != "1" || m.Host != "gw" || !m.Time.Equal(time.UnixMicro(1791729005123456)) {
		t.Fatalf("%+v %v", m, ok)
	}
	// the command name stands in for a missing identifier; kernel messages are facility 0
	m, ok = FromJournalLine([]byte(`{"MESSAGE":"eth0: link up","PRIORITY":"6","_COMM":"swapper","_TRANSPORT":"kernel","__REALTIME_TIMESTAMP":"1"}`), "")
	if !ok || m.App != "swapper" || m.Facility != 0 {
		t.Fatalf("%+v", m)
	}
	// text that is not UTF-8 comes as a list of bytes
	m, ok = FromJournalLine([]byte(`{"MESSAGE":[104,105,255],"PRIORITY":"6"}`), "")
	if !ok || m.Text != "hi?" {
		t.Fatalf("%+v %v", m, ok)
	}
	// nonsense in the numbers becomes user and informational
	m, _ = FromJournalLine([]byte(`{"MESSAGE":"x","PRIORITY":"9","SYSLOG_FACILITY":"99","__REALTIME_TIMESTAMP":"x"}`), "")
	if m.Severity != 6 || m.Facility != 1 || time.Since(m.Time) > time.Minute {
		t.Fatalf("%+v", m)
	}
	// the edges: 7 and 23 are real, 8 and 24 are not, and nor is anything below zero
	for _, c := range []struct {
		prio, fac string
		sev, f    int
	}{{"7", "23", 7, 23}, {"0", "0", 0, 0}, {"8", "24", 6, 1}, {"-1", "-1", 6, 1}} {
		m, _ := FromJournalLine([]byte(`{"MESSAGE":"x","PRIORITY":"`+c.prio+`","SYSLOG_FACILITY":"`+c.fac+`"}`), "")
		if m.Severity != c.sev || m.Facility != c.f {
			t.Errorf("priority %s facility %s: severity %d facility %d, want %d and %d", c.prio, c.fac, m.Severity, m.Facility, c.sev, c.f)
		}
	}
	for _, bad := range []string{"", "not json", `{"PRIORITY":"6"}`, `{"MESSAGE":""}`, `{"MESSAGE":"   "}`, `{"MESSAGE":{"a":1}}`, `{"MESSAGE":[300]}`} {
		if _, ok := FromJournalLine([]byte(bad), ""); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
	if !IsFirewallLine("fw:fw2 IN=br30 SRC=1.2.3.4") || IsFirewallLine("a fw:x") || IsFirewallLine("") {
		t.Error("IsFirewallLine")
	}
}

func TestAFirewallMessageSaysWhatTheRuleIsAndDoes(t *testing.T) {
	e := network.FirewallLogEntry{Time: at.UnixMilli(), Rule: "fw2", In: "br20", Out: "eth0", Src: "10.20.0.5", Dst: "1.1.1.1", Proto: "TCP", SrcPort: 40000, DstPort: 25}
	m := FirewallMessage(e, "Block mail", "block", "my-router")
	want := `<36>1 2026-10-11T14:30:05.123000Z my-router peakroute-firewall - FIREWALL - rule="Block mail" rule_id=fw2 action=block in=br20 out=eth0 src=10.20.0.5 sport=40000 dst=1.1.1.1 dport=25 proto=TCP`
	if got := m.Format(); got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
	// allowed traffic is informational; stopped traffic is a warning (block and reject alike)
	for action, sev := range map[string]int{"allow": 6, "block": 4, "reject": 4, "": 6} {
		if got := FirewallMessage(e, "x", action, "").Severity; got != sev {
			t.Errorf("%q: severity %d, want %d", action, got, sev)
		}
	}
	// a rule that is gone: its id for a name, no action; to the router itself there is no out interface or port
	icmp := network.FirewallLogEntry{Time: at.UnixMilli(), Rule: "fw9", In: "eth1", Src: "192.168.1.5", Dst: "192.168.1.1", Proto: "ICMP"}
	got := FirewallMessage(icmp, "", "", "").Format()
	if !strings.HasSuffix(got, ` rule=fw9 rule_id=fw9 in=eth1 src=192.168.1.5 dst=192.168.1.1 proto=ICMP`) {
		t.Fatalf("%q", got)
	}
	// a name with quotes, equals signs and line breaks cannot break the line apart
	m = FirewallMessage(e, "say \"hi\" a=b\nx\\y", "block", "")
	if text := m.Format(); strings.Contains(text, "\n") || !strings.Contains(text, `rule="say \"hi\" a=b x\\y" rule_id=fw2`) {
		t.Fatalf("%q", text)
	}
	if m := FirewallMessage(network.FirewallLogEntry{Rule: "fw1", Src: "1.1.1.1", Dst: "2.2.2.2"}, "r", "allow", ""); time.Since(m.Time) > time.Minute {
		t.Errorf("no time: %v", m.Time)
	}
}

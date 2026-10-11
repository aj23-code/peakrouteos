package network

import "testing"

// The kernel prints a packet logged by a rule as its prefix followed by these fields (the format of its LOG target).
const tcpLine = "fw:fw2 IN=br30 OUT=eth0 MAC=02:11:22:33:44:55:02:aa:bb:cc:dd:ee:08:00 SRC=10.30.0.40 DST=93.184.216.34 LEN=60 TOS=0x00 PREC=0x00 TTL=63 ID=21763 DF PROTO=TCP SPT=40000 DPT=443 WINDOW=64240 RES=0x00 SYN URGP=0"

func TestParseFirewallLog(t *testing.T) {
	e, ok := ParseFirewallLog(tcpLine)
	if !ok || e.Rule != "fw2" || e.In != "br30" || e.Out != "eth0" || e.Src != "10.30.0.40" || e.Dst != "93.184.216.34" || e.Proto != "TCP" || e.SrcPort != 40000 || e.DstPort != 443 {
		t.Fatalf("%v %+v", ok, e)
	}
	// traffic to the router has no output interface; ping has no ports
	e, ok = ParseFirewallLog("fw:fw3 IN=eth0 OUT= MAC=aa SRC=203.0.113.2 DST=203.0.113.1 LEN=84 TTL=64 ID=1 PROTO=ICMP TYPE=8 CODE=0 ID=7 SEQ=1")
	if !ok || e.Rule != "fw3" || e.In != "eth0" || e.Out != "" || e.Proto != "ICMP" || e.SrcPort != 0 || e.DstPort != 0 {
		t.Fatalf("%v %+v", ok, e)
	}
	for _, not := range []string{"", "   ", "fw: SRC=1.2.3.4 DST=5.6.7.8", "fw:fw1 IN=a OUT=b", "fw:fw1 SRC=1.2.3.4", "fw:fw1 DST=1.2.3.4",
		"IN=eth0 SRC=1.2.3.4 DST=5.6.7.8 PROTO=TCP", "nf_conntrack: table full, dropping packet", "xfw:fw1 SRC=1.2.3.4 DST=5.6.7.8"} {
		if e, ok := ParseFirewallLog(not); ok {
			t.Errorf("%q accepted: %+v", not, e)
		}
	}
	// a port that is not a number is zero, not a failure
	if e, ok := ParseFirewallLog("fw:fw1 SRC=1.2.3.4 DST=5.6.7.8 PROTO=TCP SPT=x DPT=80"); !ok || e.SrcPort != 0 || e.DstPort != 80 {
		t.Errorf("%v %+v", ok, e)
	}
}

func TestParseFirewallLogJournal(t *testing.T) {
	data := `{"MESSAGE":"` + tcpLine + `","__REALTIME_TIMESTAMP":"1791734400123456","SYSLOG_IDENTIFIER":"kernel"}
{"MESSAGE":"something else entirely","__REALTIME_TIMESTAMP":"1791734401000000"}
not json at all

{"MESSAGE":"fw:fw1 IN=eth1 OUT=br30 SRC=192.168.1.5 DST=10.30.0.20 PROTO=TCP SPT=5 DPT=9100","__REALTIME_TIMESTAMP":"1791734402000000"}
{"MESSAGE":"fw:fw1 IN=eth1 OUT=br30 SRC=192.168.1.5 DST=10.30.0.20 PROTO=TCP SPT=5 DPT=9100","__REALTIME_TIMESTAMP":"junk"}
{"MESSAGE":["a","binary","message"],"__REALTIME_TIMESTAMP":"1"}
`
	got := ParseFirewallLogJournal([]byte(data))
	if len(got) != 3 {
		t.Fatalf("%d: %+v", len(got), got)
	}
	if got[0].Rule != "fw2" || got[0].Time != 1791734400123 || got[1].Rule != "fw1" || got[1].Time != 1791734402000 || got[2].Time != 0 {
		t.Fatalf("%+v", got)
	}
	if got := ParseFirewallLogJournal(nil); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

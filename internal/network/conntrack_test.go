package network

import (
	"os"
	"strconv"
	"testing"
)

func fixtureConns(t *testing.T, name string) []Connection {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return ParseConntrack(string(b))
}

// the real output of `conntrack -L` (conntrack-tools 1.4.8) after a few connections through a router with a
// masquerade rule and one port forward: LAN to the internet (tcp, udp, ping), guest to the internet, and the
// internet to a forwarded port
func TestParseConntrackOfARealTable(t *testing.T) {
	for _, name := range []string{"conntrack-extended.txt", "conntrack-plain.txt"} {
		conns := fixtureConns(t, name)
		if len(conns) != 5 {
			t.Fatalf("%s: %d connections: %+v", name, len(conns), conns)
		}
		by := map[string]Connection{}
		for _, c := range conns {
			by[c.Proto+":"+c.Src+":"+strconv.Itoa(c.DstPort)] = c
		}

		web := by["tcp:192.168.1.50:80"]
		if web.State != "TIME_WAIT" || web.Timeout != 119 || web.Dst != "203.0.113.2" || web.SrcPort != 34652 || web.DstPort != 80 ||
			web.Packets != 4 || web.Bytes != 216 || web.ReplyPackets != 4 || web.ReplyBytes != 218 || !web.HasCounters || !web.Assured || web.Unreplied {
			t.Errorf("%s: %+v", name, web)
		}
		if web.ReplySrc != "203.0.113.2" || web.ReplyDst != "203.0.113.1" || web.ReplySrcPort != 80 || web.ReplyDstPort != 34652 {
			t.Errorf("%s: reply %+v", name, web)
		}
		if web.SourceNAT() != "203.0.113.1" {
			t.Errorf("%s: the masqueraded source is %q", name, web.SourceNAT())
		}
		if addr, _ := web.DestinationNAT(); addr != "" {
			t.Errorf("%s: a masqueraded connection has no destination NAT: %q", name, addr)
		}

		udp := by["udp:192.168.1.50:53"]
		if udp.State != "" || udp.Timeout != 29 || udp.DstPort != 53 || udp.Bytes != 29 {
			t.Errorf("%s: %+v", name, udp)
		}
		icmp := by["icmp:192.168.1.50:0"]
		if icmp.Proto != "icmp" || icmp.Dst != "203.0.113.2" || icmp.Bytes != 84 || icmp.SrcPort != 0 {
			t.Errorf("%s: %+v", name, icmp)
		}

		// the port forward: started from the internet to the router's address, really sent to a device inside
		fwd := by["tcp:203.0.113.2:443"]
		if fwd.Dst != "203.0.113.1" || fwd.DstPort != 443 {
			t.Errorf("%s: %+v", name, fwd)
		}
		if addr, port := fwd.DestinationNAT(); addr != "192.168.1.50" || port != 8443 {
			t.Errorf("%s: the forward goes to %s:%d", name, addr, port)
		}
		if fwd.SourceNAT() != "" {
			t.Errorf("%s: a forwarded connection keeps its source: %q", name, fwd.SourceNAT())
		}
	}
}

func TestParseConntrackLineKinds(t *testing.T) {
	text := `ipv4     2 tcp      6 431999 ESTABLISHED src=10.0.0.5 dst=1.1.1.1 sport=50000 dport=443 packets=10 bytes=1500 src=1.1.1.1 dst=10.0.0.5 sport=443 dport=50000 packets=12 bytes=9000 [ASSURED] mark=0 zone=0 use=2
ipv4     2 udp      17 25 src=10.0.0.5 dst=8.8.8.8 sport=1234 dport=53 [UNREPLIED] src=8.8.8.8 dst=10.0.0.5 sport=53 dport=1234 mark=0 use=1
ipv6     10 tcp     6 100 SYN_SENT src=fd00::5 dst=2001:db8::1 sport=40000 dport=80 [UNREPLIED] src=2001:db8::1 dst=fd00::5 sport=80 dport=40000 use=1
ipv4     2 unknown  2 599 src=10.0.0.5 dst=224.0.0.22 [UNREPLIED] src=224.0.0.22 dst=10.0.0.5 use=1
ipv4     2 tcp      6 notanumber ESTABLISHED src=1.2.3.4 dst=5.6.7.8
ipv4     2 tcp
junk
sctp 132 10 CLOSED
tcp 6
tcp 6 100 ESTABLISHED src=1.2.3.4
tcp 6 100 ESTABLISHED dst=1.2.3.4
udp 17 25 [UNREPLIED] src=10.0.0.9 dst=9.9.9.9 sport=1 dport=53
`
	got := ParseConntrack(text)
	if len(got) != 5 {
		t.Fatalf("%d: %+v", len(got), got)
	}
	tcp, udp, v6, igmp, flagFirst := got[0], got[1], got[2], got[3], got[4]
	if flagFirst.State != "" || !flagFirst.Unreplied || flagFirst.Src != "10.0.0.9" || flagFirst.DstPort != 53 {
		t.Errorf("a flag is not a state: %+v", flagFirst)
	}
	if tcp.State != "ESTABLISHED" || tcp.Timeout != 431999 || !tcp.Assured || tcp.Unreplied || tcp.Bytes != 1500 || tcp.ReplyBytes != 9000 {
		t.Errorf("%+v", tcp)
	}
	if udp.State != "" || !udp.Unreplied || udp.HasCounters || udp.Dst != "8.8.8.8" || udp.DstPort != 53 {
		t.Errorf("%+v", udp)
	}
	if v6.State != "SYN_SENT" || !v6.Unreplied || v6.Src != "fd00::5" || v6.Dst != "2001:db8::1" {
		t.Errorf("%+v", v6)
	}
	if igmp.Proto != "unknown" || igmp.Dst != "224.0.0.22" || igmp.SrcPort != 0 {
		t.Errorf("%+v", igmp)
	}
	if got := ParseConntrack(""); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

func TestNATOfUnrepliedConnectionsIsNotGuessedFrom(t *testing.T) {
	// the reply side is only expected: it still shows the addresses the connection will use
	c := Connection{Src: "10.0.0.5", Dst: "1.1.1.1", DstPort: 443, ReplySrc: "1.1.1.1", ReplyDst: "10.0.0.5", ReplySrcPort: 443}
	if c.SourceNAT() != "" {
		t.Error("no source NAT")
	}
	if a, p := c.DestinationNAT(); a != "" || p != 0 {
		t.Errorf("no destination NAT: %s %d", a, p)
	}
	// only the address is rewritten
	addrOnly := c
	addrOnly.ReplySrc = "192.168.1.60"
	if a, p := addrOnly.DestinationNAT(); a != "192.168.1.60" || p != 443 {
		t.Errorf("an address-only rewrite: %s %d", a, p)
	}
	// only the port is rewritten
	c.ReplySrcPort = 8443
	if a, p := c.DestinationNAT(); a != "1.1.1.1" || p != 8443 {
		t.Errorf("a port-only rewrite: %s %d", a, p)
	}
	// no reply side at all (a table without it)
	if (Connection{Src: "a", Dst: "b"}).SourceNAT() != "" {
		t.Error("an entry without a reply side has no NAT")
	}
}

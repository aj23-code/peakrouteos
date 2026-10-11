package netapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"peakroute/internal/network"
)

func (e *env) connections(query string) (*httptest.ResponseRecorder, ConnectionsReport) {
	e.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x"+query, nil)
	rec := httptest.NewRecorder()
	e.api.HandleConnections(rec, req)
	var rep ConnectionsReport
	if rec.Code == http.StatusOK {
		rep = decodeInto[ConnectionsReport](e.t, rec)
	}
	return rec, rep
}

func conn(src, dst string, dport int, bytes, reply uint64, timeout int) network.Connection {
	return network.Connection{Proto: "tcp", State: "ESTABLISHED", Timeout: timeout, Src: src, Dst: dst, SrcPort: 40000, DstPort: dport,
		ReplySrc: dst, ReplyDst: src, ReplySrcPort: dport, ReplyDstPort: 40000, Bytes: bytes, ReplyBytes: reply, HasCounters: true, Assured: true}
}

func TestConnectionsAreNotReportedWithoutAProvider(t *testing.T) {
	e := standard(t)
	rec, rep := e.connections("")
	if rec.Code != http.StatusOK || rep.Known || rep.Connections == nil || len(rep.Connections) != 0 {
		t.Fatalf("%d %+v", rec.Code, rep)
	}
	e.api.Connections = func() ([]network.Connection, int, int, bool) { return nil, 0, 0, false }
	if _, rep := e.connections(""); rep.Known {
		t.Fatalf("an unreadable table reported as known: %+v", rep)
	}
}

func TestConnectionsAreListedBusiestFirstWithTheirNAT(t *testing.T) {
	e := standard(t)
	masq := conn("192.168.1.50", "93.184.216.34", 443, 100, 900, 100)
	masq.ReplyDst = "203.0.113.10" // the router's internet address: the source was rewritten
	fwd := conn("203.0.113.2", "203.0.113.10", 443, 5000, 5000, 50)
	fwd.ReplySrc, fwd.ReplySrcPort = "192.168.1.60", 8443 // a port forward
	quiet := conn("192.168.1.51", "1.1.1.1", 53, 0, 0, 300)
	quiet.Proto, quiet.State = "udp", ""
	quiet.Unreplied = true
	e.api.Connections = func() ([]network.Connection, int, int, bool) {
		return []network.Connection{quiet, masq, fwd}, 3, 262144, true
	}
	rec, rep := e.connections("")
	if rec.Code != http.StatusOK || !rep.Known || rep.Count != 3 || rep.Max != 262144 || rep.Total != 3 || rep.Shown != 3 || !rep.Accounting {
		t.Fatalf("%d %+v", rec.Code, rep)
	}
	if rep.Connections[0].Src != "203.0.113.2" || rep.Connections[1].Src != "192.168.1.50" || rep.Connections[2].Src != "192.168.1.51" {
		t.Fatalf("not busiest first: %+v", rep.Connections)
	}
	f, m, q := rep.Connections[0], rep.Connections[1], rep.Connections[2]
	if f.DestNAT != "192.168.1.60" || f.DestNATPort != 8443 || f.SourceNAT != "" || f.ExpiresIn != 50 || f.Proto != "tcp" || f.State != "ESTABLISHED" || f.Bytes != 5000 || !f.Assured {
		t.Errorf("forward: %+v", f)
	}
	if m.SourceNAT != "203.0.113.10" || m.DestNAT != "" || m.ReplyBytes != 900 || m.DstPort != 443 {
		t.Errorf("masquerade: %+v", m)
	}
	if q.Proto != "udp" || q.State != "" || !q.Unreplied || q.DestNAT != "" || q.SourceNAT != "" {
		t.Errorf("quiet: %+v", q)
	}
}

func TestWithoutCountersTheLongestLivedComeFirst(t *testing.T) {
	e := standard(t)
	a, b := conn("10.0.0.1", "1.1.1.1", 443, 0, 0, 30), conn("10.0.0.2", "1.1.1.1", 443, 0, 0, 300)
	a.HasCounters, b.HasCounters = false, false
	e.api.Connections = func() ([]network.Connection, int, int, bool) { return []network.Connection{a, b}, 2, 0, true }
	_, rep := e.connections("")
	if rep.Accounting || rep.Connections[0].Src != "10.0.0.2" {
		t.Fatalf("%+v", rep)
	}
}

func TestTheConnectionListIsLimited(t *testing.T) {
	e := standard(t)
	var all []network.Connection
	for i := 1; i <= 30; i++ {
		all = append(all, conn("10.0.0.1", "1.1.1.1", i, uint64(i), 0, 100))
	}
	e.api.Connections = func() ([]network.Connection, int, int, bool) {
		return append([]network.Connection(nil), all...), 30, 0, true
	}
	_, rep := e.connections("?limit=10")
	if rep.Total != 30 || rep.Shown != 10 || len(rep.Connections) != 10 || rep.Connections[0].DstPort != 30 {
		t.Fatalf("%+v", rep)
	}
	if _, rep := e.connections(""); rep.Shown != 30 {
		t.Fatalf("the default limit hides a small table: %+v", rep)
	}
	for _, bad := range []string{"?limit=0", "?limit=-5", "?limit=x", "?limit=5001"} {
		if rec, _ := e.connections(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, rec.Code)
		}
	}
	if rec, _ := e.connections("?limit=5000"); rec.Code != http.StatusOK {
		t.Errorf("the largest limit: %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	e.api.HandleConnections(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func TestBothDirectionsCountWhenSorting(t *testing.T) {
	e := standard(t)
	download := conn("10.0.0.1", "1.1.1.1", 443, 10, 1000, 100) // sent little, received a lot
	upload := conn("10.0.0.2", "1.1.1.1", 443, 100, 0, 100)
	e.api.Connections = func() ([]network.Connection, int, int, bool) {
		return []network.Connection{upload, download}, 2, 0, true
	}
	if _, rep := e.connections(""); rep.Connections[0].Src != "10.0.0.1" {
		t.Fatalf("the connection that moved the most data in total comes first: %+v", rep.Connections)
	}
}

package netapi

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDNS is a tiny DNS server for the lookup test: it answers from a table and records every name it was asked, as asked.
type fakeDNS struct {
	addr  string
	mu    sync.Mutex
	asked []string
}

func (f *fakeDNS) questions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// startFakeDNS: names in a answer with that address, names in ptr answer a reverse lookup with that name, "broken." answers
// SERVFAIL, "silent." is never answered and everything else is NXDOMAIN.
func startFakeDNS(t *testing.T, a, ptr map[string]string) *fakeDNS {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP here: %v", err)
	}
	f := &fakeDNS{addr: conn.LocalAddr().String()}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			q := buf[:n]
			// the question starts after the 12-byte header: labels, then type and class
			i, labels := 12, []string{}
			for i < n && q[i] != 0 {
				l := int(q[i])
				labels = append(labels, string(q[i+1:i+1+l]))
				i += 1 + l
			}
			name := strings.Join(labels, ".") + "."
			qtype := binary.BigEndian.Uint16(q[i+1:])
			end := i + 5
			f.mu.Lock()
			f.asked = append(f.asked, name)
			f.mu.Unlock()
			if name == "silent." {
				continue
			}
			resp := append([]byte(nil), q[:end]...)
			rcode, answer := byte(3), []byte(nil) // NXDOMAIN
			switch {
			case name == "broken.":
				rcode = 2
			case qtype == 1 && a[name] != "":
				rcode = 0
				ip := net.ParseIP(a[name]).To4()
				answer = append([]byte{0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 0, 0, 0, 4}, ip...)
			case qtype == 12 && ptr[name] != "":
				rcode = 0
				var rdata []byte
				for _, l := range strings.Split(strings.TrimSuffix(ptr[name], "."), ".") {
					rdata = append(append(rdata, byte(len(l))), l...)
				}
				rdata = append(rdata, 0)
				answer = append([]byte{0xc0, 0x0c, 0, 12, 0, 1, 0, 0, 0, 0, 0, byte(len(rdata))}, rdata...)
			}
			resp[2], resp[3] = 0x81, 0x80|rcode // response, recursion available
			if len(answer) > 0 {
				resp[6], resp[7] = 0, 1
				resp = append(resp, answer...)
			}
			conn.WriteTo(resp, peer)
		}
	}()
	return f
}

func (e *env) lookup(name string) (int, DNSLookup) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/dns/lookup?name="+name, nil)
	rec := httptest.NewRecorder()
	e.api.HandleDNSLookup(rec, req)
	var out DNSLookup
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestTheLookupTestAsksTheRouterAndSaysWhatCameBack(t *testing.T) {
	e := standard(t)
	dns := startFakeDNS(t,
		map[string]string{"nas.": "192.168.1.50", "www.example.com.": "93.184.216.34"},
		map[string]string{"50.1.168.192.in-addr.arpa.": "nas.lan."})
	e.api.DNSAddr = dns.addr
	old := dnsLookupTimeout
	dnsLookupTimeout = 400 * time.Millisecond
	defer func() { dnsLookupTimeout = old }()

	code, r := e.lookup("NAS")
	if code != 200 || r.Status != "answered" || r.Kind != "name" || r.Query != "nas" || strings.Join(r.Answers, ",") != "192.168.1.50" || r.Server != dns.addr || r.Millis <= 0 {
		t.Fatalf("%d %+v", code, r)
	}
	// asked exactly as typed, as a full name: the router's own search list must not widen the question
	if got := strings.Join(dns.questions(), " "); got != "nas." {
		t.Errorf("the router was asked %q", got)
	}
	// a trailing dot and spaces are tidied away
	if _, r := e.lookup("%20www.example.com."); r.Status != "answered" || r.Query != "www.example.com" || r.Answers[0] != "93.184.216.34" {
		t.Errorf("%+v", r)
	}
	// an address asks for its names
	if _, r := e.lookup("192.168.1.50"); r.Status != "answered" || r.Kind != "address" || len(r.Answers) != 1 || r.Answers[0] != "nas.lan" {
		t.Errorf("reverse: %+v", r)
	}
	if _, r := e.lookup("ghost.lan"); r.Status != "not_found" || len(r.Answers) != 0 || r.Answers == nil || !strings.Contains(r.Message, "no such name") {
		t.Errorf("not found: %+v", r)
	}
	if _, r := e.lookup("10.9.9.9"); r.Status != "not_found" || r.Kind != "address" || r.Answers == nil || len(r.Answers) != 0 {
		t.Errorf("reverse, not found: %+v", r)
	}
	if _, r := e.lookup("broken"); r.Status != "error" || !strings.Contains(r.Message, "error") {
		t.Errorf("servfail: %+v", r)
	}
	start := time.Now()
	if _, r := e.lookup("silent"); r.Status != "timeout" || !strings.Contains(r.Message, "did not answer") || time.Since(start) > 3*time.Second {
		t.Errorf("silence: %+v after %v", r, time.Since(start))
	}
}

func TestTheLookupTestSaysSoWhenNothingListens(t *testing.T) {
	e := standard(t)
	// a port nobody listens on: UDP answers with "connection refused"
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no UDP here")
	}
	e.api.DNSAddr = l.LocalAddr().String()
	l.Close()
	old := dnsLookupTimeout
	dnsLookupTimeout = 400 * time.Millisecond
	defer func() { dnsLookupTimeout = old }()
	_, r := e.lookup("nas")
	if r.Status != "timeout" && r.Status != "error" || r.Message == "" || r.Answers == nil {
		t.Errorf("%+v", r)
	}
}

func TestTheLookupTestRefusesNamesItCannotAsk(t *testing.T) {
	e := standard(t)
	for _, bad := range []string{"", "%20", "a%20b", "a/b", "-x;", "a..b", ".a", "a%0Ab", strings.Repeat("a", 254), "ünï"} {
		if code, _ := e.lookup(bad); code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad, code)
		}
	}
	rec := e.do(e.api.HandleDNSLookup, http.MethodPost, `{}`, "application/json")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("method: %d", rec.Code)
	}
}

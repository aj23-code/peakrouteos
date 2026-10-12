package netapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/diag"
)

type fakeProgram struct {
	lines []string
	hang  bool
	seen  []string
}

func (f *fakeProgram) start(ctx context.Context, name string, args ...string) (io.ReadCloser, func() error, error) {
	f.seen = append([]string{name}, args...)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, l := range f.lines {
			io.WriteString(pw, l+"\n")
		}
		if f.hang {
			<-ctx.Done()
		}
		pw.Close()
	}()
	return pr, func() error { <-done; return nil }, nil
}

func diagEnv(t *testing.T, f *fakeProgram) *env {
	t.Helper()
	b := baseline()
	b.Networks = []config.Network{{ID: "net2", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0"}}
	e := newEnv(t, &b)
	e.api.Diag = &diag.Runner{Start: f.start}
	e.api.Live = func(config.NetworkConfig) *Live { return &Live{Up: true, Address: "203.0.113.10/24"} }
	saved := interfaceAddr
	interfaceAddr = func(name string) (string, bool) { return "", false }
	t.Cleanup(func() { interfaceAddr = saved })
	return e
}

func (e *env) diagPost(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	return e.do(h, http.MethodPost, body, "application/json")
}

func TestTheDiagnosticsListSayWhereATestCanBeSentFrom(t *testing.T) {
	e := diagEnv(t, &fakeProgram{})
	rec := e.get(e.api.HandleDiagnostics)
	r := decodeInto[diagnosticsResponse](t, rec)
	var got []string
	for _, s := range r.Sources {
		got = append(got, s.ID+"="+s.Address)
	}
	if rec.Code != 200 || strings.Join(got, " ") != "auto= wan=203.0.113.10 lan=192.168.1.1 net2=10.20.0.1" || !r.Tools["ping"] || !r.Tools["traceroute"] || r.MaxPings != diag.MaxPings || r.MaxRunning != diag.MaxJobsRunning {
		t.Fatalf("%s %+v", strings.Join(got, " "), r)
	}
	if rec := e.diagPost(e.api.HandleDiagnostics, `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
	// a VPN that is up is a place to send from
	interfaceAddr = func(name string) (string, bool) {
		if name == "wgra" {
			return "10.8.0.1", true
		}
		return "", false
	}
	e.api.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		c.VPN = &config.VPNConfig{WGRemote: &config.WGRemote{PrivateKey: "mLo7zHTRcnYkbu0lHEhpWv5HHe1xvYGk2o1UGfqmBXI=", ListenPort: 51821, Network: "10.8.0.0/24"}}
		return nil
	})
	e.commit()
	r = decodeInto[diagnosticsResponse](t, e.get(e.api.HandleDiagnostics))
	found := false
	for _, s := range r.Sources {
		found = found || (s.ID == "vpn-ra" && s.Address == "10.8.0.1" && s.Name == "Remote access")
	}
	if !found {
		t.Fatalf("%+v", r.Sources)
	}
}

func TestStartingAndFollowingAPing(t *testing.T) {
	f := &fakeProgram{lines: []string{"PING x", "--- x ping statistics ---", "2 packets transmitted, 2 received, 0% packet loss, time 1001ms", "rtt min/avg/max/mdev = 1.0/2.0/3.0/0.5 ms"}}
	e := diagEnv(t, f)
	rec := e.diagPost(e.api.HandleDiagnosticsJobs, `{"tool":"ping","target":" 192.0.2.1 ","count":2,"source":"net2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	v := decodeInto[diag.View](t, rec)
	if v.ID == "" || v.Tool != "ping" || v.Target != "192.0.2.1" || v.Source != "10.20.0.1" || !strings.Contains(v.Command, "-I 10.20.0.1 -- 192.0.2.1") {
		t.Fatalf("%+v", v)
	}
	var end diag.View
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.SetPathValue("id", v.ID)
		rr := httptest.NewRecorder()
		e.api.HandleDiagnosticsJob(rr, req)
		end = decodeInto[diag.View](t, rr)
		if !end.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if end.Running || end.Result != "ok" || end.Summary == nil || end.Summary.Avg != 2 || len(end.Lines) != 4 {
		t.Fatalf("%+v", end)
	}
	// automatic and the internet connection as sources
	e.diagPost(e.api.HandleDiagnosticsJobs, `{"tool":"ping","target":"example.com","count":1}`)
	if strings.Contains(strings.Join(f.seen, " "), "-I") {
		t.Fatalf("%v", f.seen)
	}
	e.diagPost(e.api.HandleDiagnosticsJobs, `{"tool":"ping","target":"example.com","count":1,"source":"wan"}`)
	if !strings.Contains(strings.Join(f.seen, " "), "-I 203.0.113.10") {
		t.Fatalf("%v", f.seen)
	}
}

func TestATestThatIsNotAllowedIsRefusedWithAReason(t *testing.T) {
	e := diagEnv(t, &fakeProgram{})
	for name, c := range map[string]struct {
		body  string
		field string
	}{
		"an option as a target":     {`{"tool":"ping","target":"-f","count":1}`, "target"},
		"a command":                 {`{"tool":"ping","target":"a;b","count":1}`, "target"},
		"too many pings":            {`{"tool":"ping","target":"a.example","count":99}`, "count"},
		"no pings":                  {`{"tool":"ping","target":"a.example"}`, "count"},
		"a source that is none":     {`{"tool":"ping","target":"a.example","count":1,"source":"nowhere"}`, "source"},
		"a source with no address":  {`{"tool":"ping","target":"a.example","count":1,"source":"vpn-s9"}`, "source"},
		"a source for a traceroute": {`{"tool":"traceroute","target":"a.example","source":"lan"}`, "target"},
	} {
		rec := e.diagPost(e.api.HandleDiagnosticsJobs, c.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[c.field] {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	for name, body := range map[string]string{"an unknown tool": `{"tool":"nmap","target":"a.example","count":1}`, "an unknown field": `{"tool":"ping","target":"a.example","count":1,"extra":1}`, "not an object": `[]`} {
		if rec := e.diagPost(e.api.HandleDiagnosticsJobs, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := e.get(e.api.HandleDiagnosticsJobs); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}

func TestTooManyAtOnceAndStoppingOne(t *testing.T) {
	f := &fakeProgram{lines: []string{" 1:  192.168.1.254  0.5ms"}, hang: true}
	e := diagEnv(t, f)
	var ids []string
	for i := 0; i < diag.MaxJobsRunning; i++ {
		rec := e.diagPost(e.api.HandleDiagnosticsJobs, `{"tool":"traceroute","target":"example.com"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		ids = append(ids, decodeInto[diag.View](t, rec).ID)
	}
	if rec := e.diagPost(e.api.HandleDiagnosticsJobs, `{"tool":"traceroute","target":"example.com"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	job := func(method, id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/x", nil)
		req.SetPathValue("id", id)
		rr := httptest.NewRecorder()
		e.api.HandleDiagnosticsJob(rr, req)
		return rr
	}
	rec := job(http.MethodDelete, ids[0])
	v := decodeInto[diag.View](t, rec)
	if rec.Code != 200 || v.Running || v.Result != "stopped" {
		t.Fatalf("%d %+v", rec.Code, v)
	}
	for _, m := range []string{http.MethodGet, http.MethodDelete} {
		if rec := job(m, "nope"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", m, rec.Code)
		}
	}
	if rec := job(http.MethodPost, ids[1]); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
	for _, id := range ids[1:] {
		job(http.MethodDelete, id)
	}
}

func TestTheConnectionTestThroughTheAPI(t *testing.T) {
	e := diagEnv(t, &fakeProgram{})
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	rec := e.diagPost(e.api.HandleDiagnosticsConnect, `{"host":"127.0.0.1","port":`+itoa(port)+`}`)
	c := decodeInto[diag.Connect](t, rec)
	if rec.Code != 200 || c.Status != "open" || c.Address != "127.0.0.1" {
		t.Fatalf("%d %+v", rec.Code, c)
	}
	for name, body := range map[string]string{"a bad host": `{"host":"-x","port":80}`, "no port": `{"host":"127.0.0.1"}`, "port too big": `{"host":"127.0.0.1","port":70000}`, "a bad source": `{"host":"127.0.0.1","port":80,"source":"nowhere"}`} {
		if rec := e.diagPost(e.api.HandleDiagnosticsConnect, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := e.get(e.api.HandleDiagnosticsConnect); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}


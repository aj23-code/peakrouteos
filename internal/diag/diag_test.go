package diag

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fake is a program that prints lines (with a pause between them) and ends, or runs until it is stopped.
type fake struct {
	lines []string
	pause time.Duration
	hang  bool
	err   error
	seen  []string
}

func (f *fake) start(ctx context.Context, name string, args ...string) (io.ReadCloser, func() error, error) {
	f.seen = append([]string{name}, args...)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, l := range f.lines {
			if _, err := io.WriteString(pw, l+"\n"); err != nil {
				return
			}
			time.Sleep(f.pause)
		}
		if f.hang {
			<-ctx.Done()
		}
		pw.Close()
	}()
	return pr, func() error {
		<-done
		if f.hang && ctx.Err() != nil {
			return errors.New("signal: killed")
		}
		return f.err
	}, nil
}

func wait(t *testing.T, r *Runner, id string) View {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v, _ := r.Get(id); !v.Running {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the job did not end")
	return View{}
}

const pingOK = `PING 192.0.2.1 (192.0.2.1) 56(84) bytes of data.
64 bytes from 192.0.2.1: icmp_seq=1 ttl=64 time=0.331 ms
64 bytes from 192.0.2.1: icmp_seq=2 ttl=64 time=0.402 ms

--- 192.0.2.1 ping statistics ---
2 packets transmitted, 2 received, 0% packet loss, time 1001ms
rtt min/avg/max/mdev = 0.331/0.366/0.402/0.035 ms`

func TestAPingRunsAndSaysHowItWent(t *testing.T) {
	f := &fake{lines: strings.Split(pingOK, "\n")}
	r := &Runner{Start: f.start}
	v, err := r.Run(Request{Tool: Ping, Target: "192.0.2.1", Count: 2, Source: "192.168.1.1"})
	if err != nil || !v.Running || v.ID == "" || v.Tool != Ping {
		t.Fatalf("%v %+v", err, v)
	}
	if got := strings.Join(f.seen, " "); got != "ping -4 -n -c 2 -W 2 -w 7 -I 192.168.1.1 -- 192.0.2.1" {
		t.Fatalf("%s", got)
	}
	end := wait(t, r, v.ID)
	if end.Result != "ok" || end.Message != "Finished." || len(end.Lines) != 7 || end.Summary == nil || end.Summary.Sent != 2 || end.Summary.Received != 2 || end.Summary.Loss != 0 ||
		end.Summary.Min != 0.331 || end.Summary.Avg != 0.366 || end.Summary.Max != 0.402 || end.Elapsed < 0 || end.Command != "ping -4 -n -c 2 -W 2 -w 7 -I 192.168.1.1 -- 192.0.2.1" {
		t.Fatalf("%+v", end)
	}
	// without a source: no -I
	f2 := &fake{lines: []string{"x"}}
	r2 := &Runner{Start: f2.start}
	v, _ = r2.Run(Request{Tool: Ping, Target: "example.com", Count: 4})
	wait(t, r2, v.ID)
	if got := strings.Join(f2.seen, " "); strings.Contains(got, "-I") || !strings.HasSuffix(got, "-- example.com") {
		t.Fatalf("%s", got)
	}
}

func TestTheRulesOfWhatMayBeAsked(t *testing.T) {
	r := &Runner{Start: (&fake{lines: []string{"x"}}).start}
	for name, req := range map[string]Request{
		"an option as a target":     {Tool: Ping, Target: "-f", Count: 1},
		"a command as a target":     {Tool: Ping, Target: "a;reboot", Count: 1},
		"a space":                   {Tool: Ping, Target: "a b", Count: 1},
		"no target":                 {Tool: Ping, Count: 1},
		"an IPv6 address":           {Tool: Ping, Target: "::1", Count: 1},
		"too many pings":            {Tool: Ping, Target: "a.example", Count: MaxPings + 1},
		"no pings":                  {Tool: Ping, Target: "a.example", Count: 0},
		"a source that is a name":   {Tool: Ping, Target: "a.example", Count: 1, Source: "eth0; reboot"},
		"an unknown tool":           {Tool: "nmap", Target: "a.example", Count: 1},
		"a source for a traceroute": {Tool: Traceroute, Target: "a.example", Source: "192.168.1.1"},
		"dots in a row":             {Tool: Ping, Target: "a..example", Count: 1},
		"a trailing dash":           {Tool: Ping, Target: "a.example-", Count: 1},
	} {
		if _, err := r.Run(req); err == nil {
			t.Errorf("%s was run", name)
		}
	}
	for _, ok := range []string{"192.0.2.1", "example.com", "nas", "my-host.local", "a_b.example"} {
		if !ValidTarget(ok) {
			t.Errorf("%q refused", ok)
		}
	}
}

func TestATracerouteAndStoppingOne(t *testing.T) {
	f := &fake{lines: []string{" 1?: [LOCALHOST]                      pmtu 1500", " 1:  192.168.1.254  0.512ms"}, pause: 20 * time.Millisecond, hang: true}
	r := &Runner{Start: f.start}
	v, err := r.Run(Request{Tool: Traceroute, Target: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.seen, " "); got != "tracepath -n -4 -m 20 example.com" {
		t.Fatalf("%s", got)
	}
	// lines come while it runs
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cur, _ := r.Get(v.ID); len(cur.Lines) == 2 && cur.Running && cur.Elapsed >= 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no lines while running")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !r.Stop(v.ID) {
		t.Fatal("no such job")
	}
	end := wait(t, r, v.ID)
	if end.Result != "stopped" || end.Message != "Stopped." || end.Summary != nil || len(end.Lines) != 2 {
		t.Fatalf("%+v", end)
	}
	if r.Stop("nope") {
		t.Fatal("a job that is not there was stopped")
	}
	if !r.Stop(v.ID) { // a job that is over stays as it was
		t.Fatal("stopping a finished job")
	}
	if again, _ := r.Get(v.ID); again.Result != "stopped" {
		t.Fatalf("%+v", again)
	}
}

func TestOnlyAFewAtOnceAndOldOnesAreForgotten(t *testing.T) {
	f := &fake{lines: []string{"x"}, hang: true}
	r := &Runner{Start: f.start}
	var ids []string
	for i := 0; i < MaxJobsRunning; i++ {
		v, err := r.Run(Request{Tool: Traceroute, Target: "example.com"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
	}
	if _, err := r.Run(Request{Tool: Traceroute, Target: "example.com"}); err == nil || !strings.Contains(err.Error(), "running already") {
		t.Fatalf("%v", err)
	}
	r.Stop(ids[0])
	wait(t, r, ids[0])
	if _, err := r.Run(Request{Tool: Traceroute, Target: "example.com"}); err != nil {
		t.Fatalf("after one ended: %v", err)
	}
	for _, id := range ids {
		r.Stop(id)
	}
	// old finished jobs go, the newest stay
	g := &fake{lines: []string{"x"}}
	r2 := &Runner{Start: g.start}
	var first, last string
	for i := 0; i < keepJobs+5; i++ {
		v, err := r2.Run(Request{Tool: Traceroute, Target: "example.com"})
		if err != nil {
			t.Fatal(err)
		}
		wait(t, r2, v.ID)
		if i == 0 {
			first = v.ID
		}
		last = v.ID
	}
	if _, ok := r2.Get(first); ok {
		t.Fatal("the oldest job is still there")
	}
	if _, ok := r2.Get(last); !ok {
		t.Fatal("the newest job is gone")
	}
	if _, ok := r2.Get("nope"); ok {
		t.Fatal("a job that never was")
	}
}

func TestOutputIsBoundedAndAFailureIsSaidInWords(t *testing.T) {
	var lines []string
	for i := 0; i < maxLines+50; i++ {
		lines = append(lines, "line")
	}
	lines[0] = strings.Repeat("x", 1000)
	r := &Runner{Start: (&fake{lines: lines}).start}
	v, _ := r.Run(Request{Tool: Traceroute, Target: "example.com"})
	end := wait(t, r, v.ID)
	if len(end.Lines) != maxLines || len(end.Lines[0]) > maxLineLen+4 {
		t.Fatalf("%d lines, the first of %d", len(end.Lines), len(end.Lines[0]))
	}
	// a program that cannot be started
	r3 := &Runner{Start: func(context.Context, string, ...string) (io.ReadCloser, func() error, error) {
		return nil, nil, exec.ErrNotFound
	}}
	if _, err := r3.Run(Request{Tool: Ping, Target: "a.example", Count: 1}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("%v", err)
	}
	// ping that got nothing back: failed, in words, with the summary
	none := `--- 192.0.2.99 ping statistics ---
2 packets transmitted, 0 received, 100% packet loss, time 1013ms`
	r4 := &Runner{Start: (&fake{lines: strings.Split(none, "\n")}).start}
	v, _ = r4.Run(Request{Tool: Ping, Target: "192.0.2.99", Count: 2})
	end = wait(t, r4, v.ID)
	if end.Result != "failed" || end.Message != "No answer came back." || end.Summary == nil || end.Summary.Loss != 100 || end.Summary.Received != 0 {
		t.Fatalf("%+v", end)
	}
	// a program that fails with a message of its own
	r5 := &Runner{Start: (&fake{lines: []string{"", "ping: unknown host nope.invalid"}, err: errors.New("exit status 2")}).start}
	v, _ = r5.Run(Request{Tool: Ping, Target: "nope.invalid", Count: 1})
	end = wait(t, r5, v.ID)
	if end.Result != "failed" || end.Message != "ping: unknown host nope.invalid" {
		t.Fatalf("%+v", end)
	}
	r6 := &Runner{Start: (&fake{err: errors.New("exit status 1")}).start}
	v, _ = r6.Run(Request{Tool: Traceroute, Target: "example.com"})
	if end = wait(t, r6, v.ID); end.Result != "failed" || !strings.HasPrefix(end.Message, "The test failed") {
		t.Fatalf("%+v", end)
	}
}

func TestReadingTheEndOfAPing(t *testing.T) {
	if s := ParsePing(strings.Split(pingOK, "\n")); s == nil || s.Sent != 2 || s.Received != 2 {
		t.Fatalf("%+v", s)
	}
	if ParsePing([]string{"PING x", "64 bytes from"}) != nil {
		t.Fatal("a summary before the end")
	}
	// the older wording "packets received"
	if s := ParsePing([]string{"5 packets transmitted, 4 packets received, 20% packet loss"}); s == nil || s.Received != 4 || s.Loss != 20 {
		t.Fatalf("%+v", s)
	}
	if s := ParsePing([]string{"0 packets transmitted, 0 received, 0% packet loss"}); s == nil || s.Loss != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestTheRealPing(t *testing.T) {
	if _, err := exec.LookPath("ping"); err != nil {
		t.Skip("no ping here")
	}
	r := &Runner{}
	v, err := r.Run(Request{Tool: Ping, Target: "127.0.0.1", Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	end := wait(t, r, v.ID)
	if end.Result != "ok" || end.Summary == nil || end.Summary.Received != 2 {
		t.Skipf("ping is not allowed to run here: %+v", end)
	}
}

func TestTheConnectionTest(t *testing.T) {
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
	ctx := context.Background()
	open := TryConnect(ctx, "127.0.0.1", port, "", 2*time.Second)
	if open.Status != "open" || open.Address != "127.0.0.1" || open.Port != port || open.Millis < 0 {
		t.Fatalf("%+v", open)
	}
	// from a source address of its own
	if got := TryConnect(ctx, "127.0.0.1", port, "127.0.0.1", 2*time.Second); got.Status != "open" {
		t.Fatalf("%+v", got)
	}
	// a port nothing listens on
	l2, _ := net.Listen("tcp4", "127.0.0.1:0")
	closed := l2.Addr().(*net.TCPAddr).Port
	l2.Close()
	if got := TryConnect(ctx, "127.0.0.1", closed, "", 2*time.Second); got.Status != "refused" {
		t.Fatalf("%+v", got)
	}
	// a name that is none, a bad port, a bad name, a bad source
	if got := TryConnect(ctx, "no-such-name.invalid", 80, "", 2*time.Second); got.Status != "no_name" {
		t.Fatalf("%+v", got)
	}
	for _, c := range []struct {
		host string
		port int
		src  string
	}{{"127.0.0.1", 0, ""}, {"127.0.0.1", 70000, ""}, {"-x", 80, ""}, {"127.0.0.1", 80, "not an address"}} {
		if got := TryConnect(ctx, c.host, c.port, c.src, time.Second); got.Status != "error" {
			t.Errorf("%+v: %+v", c, got)
		}
	}
	// an address nobody answers (a documentation range): no answer within the time
	if got := TryConnect(ctx, "192.0.2.77", 80, "", 300*time.Millisecond); got.Status != "timeout" && got.Status != "unreachable" {
		t.Fatalf("%+v", got)
	}
}

package syslogfwd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"peakroute/internal/network"
)

// ---- receivers

type udpServer struct {
	conn net.PacketConn
	mu   sync.Mutex
	got  []string
}

func newUDP(t *testing.T) *udpServer {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP here: %v", err)
	}
	s := &udpServer{conn: c}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.got = append(s.got, string(buf[:n]))
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *udpServer) server() Server {
	a := s.conn.LocalAddr().(*net.UDPAddr)
	return Server{Host: "127.0.0.1", Port: a.Port, Protocol: "udp"}
}

func (s *udpServer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

type tcpServer struct {
	ln       net.Listener
	mu       sync.Mutex
	raw      []byte
	got      []string
	accepted int
	hungUp   int // connections the other side has closed
	conns    []net.Conn
}

func listenTCP(t *testing.T, addr string) *tcpServer {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("no TCP here: %v", err)
	}
	s := &tcpServer{ln: ln}
	t.Cleanup(s.close)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.accepted++
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			go func() {
				buf := make([]byte, 4096)
				var pending []byte
				for {
					n, err := c.Read(buf)
					if n > 0 {
						s.mu.Lock()
						s.raw = append(s.raw, buf[:n]...)
						s.mu.Unlock()
						pending = append(pending, buf[:n]...)
						for {
							i := strings.IndexByte(string(pending), '\n')
							if i < 0 {
								break
							}
							s.mu.Lock()
							s.got = append(s.got, string(pending[:i]))
							s.mu.Unlock()
							pending = pending[i+1:]
						}
					}
					if err != nil {
						s.mu.Lock()
						s.hungUp++
						s.mu.Unlock()
						return
					}
				}
			}()
		}
	}()
	return s
}

func (s *tcpServer) close() {
	s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
}

func (s *tcpServer) server() Server {
	a := s.ln.Addr().(*net.TCPAddr)
	return Server{Host: "127.0.0.1", Port: a.Port, Protocol: "tcp"}
}

func (s *tcpServer) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func (s *tcpServer) hungUpCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hungUp
}

func (s *tcpServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gave up waiting for %s", what)
}

func steady(t *testing.T, what string, n func() int, want int) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	if got := n(); got != want {
		t.Fatalf("%s: %d, want %d", what, got, want)
	}
}

func start(t *testing.T, f *Forwarder) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.Start(ctx)
	t.Cleanup(cancel)
	return cancel
}

func pkt(rule string, port int) network.FirewallLogEntry {
	return network.FirewallLogEntry{Time: at.UnixMilli(), Rule: rule, In: "br20", Out: "eth0", Src: "10.20.0.5", Dst: "1.1.1.1", Proto: "TCP", SrcPort: 40000, DstPort: port}
}

func rules(id string) (string, string, bool) {
	switch id {
	case "fw1":
		return "Block mail", "block", true
	case "fw2":
		return "Allow web", "allow", true
	}
	return "", "", false
}

// ---- tests

func TestFirewallPacketsGoOutOverUDP(t *testing.T) {
	srv := newUDP(t)
	f := &Forwarder{Rule: rules}
	start(t, f)
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true, Hostname: "gw"})
	f.Packet(pkt("fw1", 25))
	waitFor(t, "the datagram", func() bool { return len(srv.lines()) == 1 })
	want := `<36>1 2026-10-11T14:30:05.123000Z gw peakroute-firewall - FIREWALL - rule="Block mail" rule_id=fw1 action=block in=br20 out=eth0 src=10.20.0.5 sport=40000 dst=1.1.1.1 dport=25 proto=TCP`
	if got := srv.lines()[0]; got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
	// a rule that is gone is named by its id
	f.Packet(pkt("fw9", 80))
	waitFor(t, "the second datagram", func() bool { return len(srv.lines()) == 2 })
	if got := srv.lines()[1]; !strings.Contains(got, " rule=fw9 rule_id=fw9 ") || strings.Contains(got, "action=") {
		t.Fatalf("%q", got)
	}
	st := f.Statuses()
	if len(st) != 1 || st[0].Sent != 2 || st[0].Dropped != 0 || st[0].LastError != "" || st[0].Server != srv.server() {
		t.Fatalf("%+v", st)
	}
}

func TestWhatIsNotAskedForIsNotSent(t *testing.T) {
	srv := newUDP(t)
	f := &Forwarder{Rule: rules}
	start(t, f)
	// the firewall log is off
	f.Update(Config{Servers: []Server{srv.server()}, System: true, Firewall: false})
	f.Packet(pkt("fw1", 25))
	// no servers
	f.Update(Config{Firewall: true})
	f.Packet(pkt("fw1", 25))
	// nothing to send
	f.Update(Config{Servers: []Server{srv.server()}})
	f.Packet(pkt("fw1", 25))
	steady(t, "datagrams", func() int { return len(srv.lines()) }, 0)
	if len(f.Statuses()) != 0 {
		t.Fatalf("%+v", f.Statuses())
	}
	// and before anything was set at all
	(&Forwarder{}).Packet(pkt("fw1", 25))
}

func TestTCPSendsOneLinePerMessageOverOneConnection(t *testing.T) {
	srv := newTCP(t)
	f := &Forwarder{Rule: rules}
	start(t, f)
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true})
	for i := 0; i < 5; i++ {
		f.Packet(pkt("fw2", 80+i))
	}
	waitFor(t, "five lines", func() bool { return len(srv.lines()) == 5 })
	for i, l := range srv.lines() {
		if !strings.HasSuffix(l, " dport="+strconv.Itoa(80+i)+" proto=TCP") || !strings.HasPrefix(l, "<38>1 ") {
			t.Fatalf("line %d: %q", i, l)
		}
	}
	// the same configuration again keeps the connection (and the queue)
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true})
	f.Packet(pkt("fw2", 99))
	waitFor(t, "a sixth line", func() bool { return len(srv.lines()) == 6 })
	if srv.connections() != 1 {
		t.Fatalf("%d connections", srv.connections())
	}
	if st := f.Statuses(); len(st) != 1 || !st[0].Connected || st[0].Sent != 6 {
		t.Fatalf("%+v", st)
	}
	// taking the server out of the configuration closes the connection (the sender does not linger)
	f.Update(Config{})
	waitFor(t, "the connection to be closed", func() bool { return srv.hungUpCount() == 1 })
}

func newTCP(t *testing.T) *tcpServer { return listenTCP(t, "127.0.0.1:0") }

func TestAServerThatComesBackGetsWhatWasWaiting(t *testing.T) {
	old := minBackoff
	minBackoff = 10 * time.Millisecond
	defer func() { minBackoff = old }()
	srv := newTCP(t)
	addr := srv.ln.Addr().String()
	f := &Forwarder{Rule: rules}
	start(t, f)
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true})
	f.Packet(pkt("fw2", 1))
	waitFor(t, "the first line", func() bool { return len(srv.lines()) == 1 })
	// the server goes away
	srv.close()
	time.Sleep(50 * time.Millisecond)
	for i := 2; i <= 6; i++ {
		f.Packet(pkt("fw2", i))
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, "an error", func() bool { st := f.Statuses(); return len(st) == 1 && st[0].LastError != "" })
	// and returns on the same address
	back := listenTCP(t, addr)
	waitFor(t, "the newest waiting line", func() bool { return strings.Contains(strings.Join(back.lines(), "\n"), " dport=6 ") })
	got := back.lines()
	// in order, none twice; the ones written into the dying connection may be lost (TCP only says so afterwards), nothing after them
	last := 0
	for _, l := range got {
		i := strings.Index(l, " dport=")
		port, _ := strconv.Atoi(strings.Fields(l[i+len(" dport="):])[0])
		if port <= last {
			t.Fatalf("out of order or repeated: %v", got)
		}
		last = port
	}
	if last != 6 {
		t.Fatalf("the newest message did not arrive: %v", got)
	}
	f.Packet(pkt("fw2", 7))
	waitFor(t, "a message after the return", func() bool { return strings.Contains(strings.Join(back.lines(), "\n"), "dport=7 ") })
	if st := f.Statuses(); st[0].LastError != "" || !st[0].Connected {
		t.Fatalf("%+v", st)
	}
}

func TestAQueueThatIsFullLosesTheOldest(t *testing.T) {
	old := minBackoff
	minBackoff = 10 * time.Millisecond
	defer func() { minBackoff = old }()
	// a port nobody listens on
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	port := addr[strings.LastIndex(addr, ":")+1:]
	n, _ := strconv.Atoi(port)
	f := &Forwarder{Rule: rules}
	start(t, f)
	f.Update(Config{Servers: []Server{{Host: "127.0.0.1", Port: n, Protocol: "tcp"}}, Firewall: true})
	total := queueSize + 500
	for i := 1; i <= total; i++ {
		f.Packet(pkt("fw2", i))
	}
	waitFor(t, "the drops to be counted", func() bool { st := f.Statuses(); return len(st) == 1 && st[0].Dropped >= 400 })
	back := listenTCP(t, addr)
	waitFor(t, "the newest messages", func() bool { return len(back.lines()) >= queueSize-1 })
	time.Sleep(100 * time.Millisecond)
	lines := back.lines()
	portOf := func(l string) int {
		i := strings.Index(l, " dport=")
		p, _ := strconv.Atoi(strings.Fields(l[i+len(" dport="):])[0])
		return p
	}
	// the sender holds the first message while it waits for the server; the queue behind it lost its oldest 500
	if portOf(lines[0]) != 1 || portOf(lines[1]) < 500 || portOf(lines[1]) > 503 {
		t.Fatalf("the first messages to arrive are %d and %d", portOf(lines[0]), portOf(lines[1]))
	}
	if !strings.Contains(lines[len(lines)-1], fmt.Sprintf(" dport=%d ", total)) {
		t.Fatalf("the newest message is not the last: %q", lines[len(lines)-1])
	}
	if st := f.Statuses(); st[0].Dropped < 497 || st[0].Dropped > 501 {
		t.Fatalf("dropped %d", st[0].Dropped)
	}
}

func TestServersComeAndGoWithTheConfiguration(t *testing.T) {
	a, b := newUDP(t), newUDP(t)
	f := &Forwarder{Rule: rules}
	start(t, f)
	f.Update(Config{Servers: []Server{a.server()}, Firewall: true})
	f.Packet(pkt("fw1", 1))
	waitFor(t, "a", func() bool { return len(a.lines()) == 1 })
	// a second server: both get the next message; the first keeps its own count
	f.Update(Config{Servers: []Server{a.server(), b.server()}, Firewall: true})
	f.Packet(pkt("fw1", 2))
	waitFor(t, "both", func() bool { return len(a.lines()) == 2 && len(b.lines()) == 1 })
	// the first is taken out
	f.Update(Config{Servers: []Server{b.server()}, Firewall: true})
	f.Packet(pkt("fw1", 3))
	waitFor(t, "b", func() bool { return len(b.lines()) == 2 })
	steady(t, "a", func() int { return len(a.lines()) }, 2)
	st := f.Statuses()
	if len(st) != 1 || st[0].Server != b.server() || st[0].Sent != 2 {
		t.Fatalf("%+v", st)
	}
	// the same address over TCP is another server
	t1 := newTCP(t)
	same := Server{Host: "127.0.0.1", Port: t1.server().Port, Protocol: "udp"}
	f.Update(Config{Servers: []Server{b.server(), t1.server(), same}, Firewall: true})
	if len(f.Statuses()) != 3 {
		t.Fatalf("%+v", f.Statuses())
	}
	// stopping the forwarder stops everything
	f.Update(Config{})
	if len(f.Statuses()) != 0 {
		t.Fatalf("%+v", f.Statuses())
	}
	f.Packet(pkt("fw1", 4))
	steady(t, "b after it was switched off", func() int { return len(b.lines()) }, 2)
}

func TestEndingTheContextStopsTheSenders(t *testing.T) {
	srv := newUDP(t)
	f := &Forwarder{Rule: rules}
	cancel := start(t, f)
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true})
	f.Packet(pkt("fw1", 1))
	waitFor(t, "the datagram", func() bool { return len(srv.lines()) == 1 })
	cancel()
	waitFor(t, "the senders to stop", func() bool { return len(f.Statuses()) == 0 })
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true}) // too late: nothing starts again
	if len(f.Statuses()) != 0 {
		t.Fatal("a sender started after the end")
	}
}

// ---- the journal

// a journal that hands out lines when told to, and says when it was opened and closed
type fakeJournal struct {
	mu     sync.Mutex
	opened int
	closed int
	errs   []error
	w      *io.PipeWriter
}

func (j *fakeJournal) open(ctx context.Context) (io.ReadCloser, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.opened++
	if j.opened <= len(j.errs) && j.errs[j.opened-1] != nil {
		return nil, j.errs[j.opened-1]
	}
	r, w := io.Pipe()
	j.w = w
	return &closeCounter{ReadCloser: r, j: j}, nil
}

type closeCounter struct {
	io.ReadCloser
	j *fakeJournal
}

func (c *closeCounter) Close() error {
	c.j.mu.Lock()
	c.j.closed++
	c.j.mu.Unlock()
	return c.ReadCloser.Close()
}

func (j *fakeJournal) write(s string) {
	j.mu.Lock()
	w := j.w
	j.mu.Unlock()
	io.WriteString(w, s+"\n")
}

func (j *fakeJournal) counts() (int, int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.opened, j.closed
}

func entry(msg string) string {
	return `{"MESSAGE":` + strconv.Quote(msg) + `,"PRIORITY":"4","SYSLOG_IDENTIFIER":"sshd","_PID":"77","__REALTIME_TIMESTAMP":"1791729005123456"}`
}

func TestTheSystemLogIsForwardedWithoutTheFirewallLines(t *testing.T) {
	srv := newUDP(t)
	j := &fakeJournal{}
	f := &Forwarder{Journal: j.open, Rule: rules, Retry: 20 * time.Millisecond}
	start(t, f)
	f.Update(Config{Servers: []Server{srv.server()}, System: true, Hostname: "gw"})
	waitFor(t, "the journal to be opened", func() bool { o, _ := j.counts(); return o == 1 })
	j.write(entry("Failed password for root"))
	j.write(entry("fw:fw1 IN=br20 OUT=eth0 SRC=10.20.0.5 DST=1.1.1.1 PROTO=TCP SPT=1 DPT=25"))
	j.write("not json")
	j.write(entry("Accepted publickey for admin"))
	waitFor(t, "two datagrams", func() bool { return len(srv.lines()) == 2 })
	steady(t, "datagrams", func() int { return len(srv.lines()) }, 2)
	want0 := "<12>1 2026-10-11T14:30:05.123456Z gw sshd 77 - - Failed password for root"
	if got := srv.lines()[0]; got != want0 {
		t.Fatalf("%q\nwant %q", got, want0)
	}
	if !strings.HasSuffix(srv.lines()[1], "Accepted publickey for admin") {
		t.Fatalf("%q", srv.lines()[1])
	}
}

func TestTheJournalIsFollowedOnlyWhileTheSystemLogIsWanted(t *testing.T) {
	srv := newUDP(t)
	j := &fakeJournal{}
	f := &Forwarder{Journal: j.open, Rule: rules, Retry: 20 * time.Millisecond}
	start(t, f)
	// the firewall log alone does not need the journal
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true})
	steady(t, "journal opens", func() int { o, _ := j.counts(); return o }, 0)
	// the system log does
	f.Update(Config{Servers: []Server{srv.server()}, System: true, Firewall: true})
	waitFor(t, "opened", func() bool { o, _ := j.counts(); return o == 1 })
	// changing something else keeps the same follower
	f.Update(Config{Servers: []Server{srv.server()}, System: true, Firewall: true, Hostname: "x"})
	steady(t, "journal opens", func() int { o, _ := j.counts(); return o }, 1)
	// switched off: closed, and what is written afterwards goes nowhere
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true})
	waitFor(t, "closed", func() bool { _, c := j.counts(); return c >= 1 })
	// no servers at all: not followed either
	f.Update(Config{System: true})
	steady(t, "journal opens", func() int { o, _ := j.counts(); return o }, 1)
}

func TestAJournalThatFailsIsFollowedAgain(t *testing.T) {
	srv := newUDP(t)
	j := &fakeJournal{errs: []error{errors.New("no journalctl"), errors.New("still no")}}
	f := &Forwarder{Journal: j.open, Rule: rules, Retry: 10 * time.Millisecond}
	start(t, f)
	f.Update(Config{Servers: []Server{srv.server()}, System: true})
	waitFor(t, "the third try", func() bool { o, _ := j.counts(); return o >= 3 })
	j.write(entry("hello"))
	waitFor(t, "the message", func() bool { return len(srv.lines()) == 1 })
	// a journal that ends is followed again too
	j.mu.Lock()
	j.w.Close()
	j.mu.Unlock()
	waitFor(t, "a fourth open", func() bool { o, _ := j.counts(); return o >= 4 })
	j.write(entry("again"))
	waitFor(t, "the next message", func() bool { return len(srv.lines()) == 2 })
}

func TestATestMessageSaysWhatHappenedToEachServer(t *testing.T) {
	u, tc := newUDP(t), newTCP(t)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := dead.Addr().(*net.TCPAddr).Port
	dead.Close()
	f := &Forwarder{}
	start(t, f)
	srvs := []Server{u.server(), tc.server(), {Host: "127.0.0.1", Port: deadPort, Protocol: "tcp"}}
	f.Update(Config{Servers: srvs, Firewall: true, Hostname: "gw"})
	res := f.SendTest(context.Background(), "PeakRoute test message")
	if len(res) != 3 || !res[0].OK || !strings.Contains(res[0].Message, "UDP is not acknowledged") || !res[1].OK || res[1].Message != "connected and sent" ||
		res[2].OK || !strings.Contains(res[2].Message, "could not connect") || res[2].Server != srvs[2] {
		t.Fatalf("%+v", res)
	}
	waitFor(t, "the two test messages", func() bool { return len(u.lines()) == 1 && len(tc.lines()) == 1 })
	for _, l := range []string{u.lines()[0], tc.lines()[0]} {
		if !strings.HasPrefix(l, "<14>1 ") || !strings.Contains(l, " gw peakroute - TEST - PeakRoute test message") {
			t.Fatalf("%q", l)
		}
	}
	// a TCP message ends with a line break, so that the server can tell where it stops; a UDP datagram is the message
	tc.mu.Lock()
	raw := string(tc.raw)
	tc.mu.Unlock()
	if !strings.HasSuffix(raw, "PeakRoute test message\n") {
		t.Fatalf("%q", raw)
	}
	if got := u.lines()[0]; strings.HasSuffix(got, "\n") {
		t.Fatalf("%q", got)
	}
	// with nothing configured there is nothing to say
	g := &Forwarder{}
	if got := g.SendTest(context.Background(), "x"); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestFollowJournalAsksJournalctlForNewEntriesOnly(t *testing.T) {
	dir := t.TempDir()
	args := dir + "/args"
	script := "#!/bin/sh\necho \"$@\" > " + args + "\necho '" + entry("from the fake journal") + "'\nsleep 5\n"
	if err := writeExecutable(dir+"/journalctl", script); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+pathEnv())
	r, err := FollowJournal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil || !strings.Contains(line, "from the fake journal") {
		t.Fatalf("%q %v", line, err)
	}
	got, _ := readFile(args)
	if strings.TrimSpace(got) != "-f -n 0 --output=json --no-pager" {
		t.Fatalf("journalctl %s", got)
	}
	start := time.Now()
	r.Close() // stops it without waiting for its sleep
	if time.Since(start) > 2*time.Second {
		t.Fatal("closing waited for journalctl")
	}
}

func TestOldPacketsAreNotForwarded(t *testing.T) {
	saved := maxAge
	maxAge = 2 * time.Minute
	defer func() { maxAge = saved }()
	srv := newUDP(t)
	f := &Forwarder{Rule: rules}
	start(t, f)
	f.Update(Config{Servers: []Server{srv.server()}, Firewall: true})
	old := pkt("fw1", 1)
	old.Time = time.Now().Add(-3 * time.Minute).UnixMilli() // read back from the journal after a restart
	f.Packet(old)
	recent := pkt("fw1", 2)
	recent.Time = time.Now().Add(-30 * time.Second).UnixMilli()
	f.Packet(recent)
	untimed := pkt("fw1", 3)
	untimed.Time = 0
	f.Packet(untimed)
	waitFor(t, "two datagrams", func() bool { return len(srv.lines()) == 2 })
	steady(t, "datagrams", func() int { return len(srv.lines()) }, 2)
	if l := srv.lines(); !strings.Contains(l[0], "dport=2 ") || !strings.Contains(l[1], "dport=3 ") {
		t.Fatalf("%v", l)
	}
}

func TestTheWaitBetweenConnectionAttemptsDoublesUpToTheLongest(t *testing.T) {
	var got []time.Duration
	b := time.Second // the real values: nothing in the test changes them
	for i := 0; i < 8; i++ {
		got = append(got, b)
		b = nextBackoff(b)
	}
	if maxBackoff != 30*time.Second {
		t.Skip("another test is changing the longest wait")
	}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("%v, want %v seconds", got, want)
		}
	}
}

// The journal follower is closed from two places at once when the system log is switched off (the one that reads it and the one that
// notices the context has ended). Closing must be safe to do twice at the same time: exec's Wait may only be called once, and a second
// call used to wait forever, which hung Update and with it the commit that switched the log off.
func TestAJournalProcessCanBeClosedFromTwoPlacesAtOnce(t *testing.T) {
	for i := 0; i < 40; i++ {
		// with a context, as FollowJournal starts journalctl: that is what makes a second Wait block
		cmd := exec.CommandContext(context.Background(), "sleep", "30")
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Skip("cannot start sleep: " + err.Error())
		}
		p := &journalProcess{ReadCloser: out, cmd: cmd}
		done := make(chan struct{}, 2)
		for j := 0; j < 2; j++ {
			go func() { p.Close(); done <- struct{}{} }()
		}
		for j := 0; j < 2; j++ {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("round %d: Close did not return", i)
			}
		}
	}
}

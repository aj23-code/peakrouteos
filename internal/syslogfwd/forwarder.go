package syslogfwd

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"os/exec"
	"sort"
	"sync"
	"time"

	"peakroute/internal/network"
)

// Config is what is sent where.
type Config struct {
	Servers  []Server
	System   bool   // the system journal
	Firewall bool   // the packets that rules with logging matched
	Hostname string // the name the messages carry as their origin ("" when the router has none set)
}

func (c Config) active() bool { return len(c.Servers) > 0 && (c.System || c.Firewall) }

// JournalSource follows the entries the system journal gets from now on, one JSON object per line.
type JournalSource func(ctx context.Context) (io.ReadCloser, error)

// FollowJournal follows the system journal with journalctl.
func FollowJournal(ctx context.Context) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, "journalctl", "-f", "-n", "0", "--output=json", "--no-pager")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &journalProcess{ReadCloser: out, cmd: cmd}, nil
}

type journalProcess struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (p *journalProcess) Close() error {
	p.cmd.Process.Kill()
	p.ReadCloser.Close()
	p.cmd.Wait()
	return nil
}

// Forwarder sends the logs to the servers it was last given with Update.
type Forwarder struct {
	// Journal follows the system journal; nil uses journalctl.
	Journal JournalSource
	// Rule says what the rules in effect call a rule and what it does (ok false: the rule is gone).
	Rule func(id string) (name, action string, ok bool)
	// Retry is how long to wait before following the journal again after it stopped (default 10 s).
	Retry time.Duration

	mu      sync.Mutex
	ctx     context.Context
	cfg     Config
	senders map[string]*sender
	stopJ   context.CancelFunc // stops the journal follower
	jdone   chan struct{}
}

// Start gives the forwarder the context that ends everything it does.
func (f *Forwarder) Start(ctx context.Context) {
	f.mu.Lock()
	f.ctx = ctx
	f.senders = map[string]*sender{}
	cfg := f.cfg
	f.cfg = Config{}
	f.mu.Unlock()
	f.Update(cfg)
	go func() {
		<-ctx.Done()
		f.Update(Config{})
	}()
}

// Update sets what is sent where. Servers that stay keep their connection and queue; the others are started or stopped.
func (f *Forwarder) Update(cfg Config) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ctx == nil {
		f.cfg = cfg
		return
	}
	if f.ctx.Err() != nil {
		cfg = Config{}
	}
	want := map[string]Server{}
	if cfg.active() {
		for _, s := range cfg.Servers {
			want[s.key()] = s
		}
	}
	for k, s := range f.senders {
		if _, ok := want[k]; !ok {
			s.stop()
			delete(f.senders, k)
		}
	}
	for k, srv := range want {
		if _, ok := f.senders[k]; !ok {
			f.senders[k] = newSender(f.ctx, srv)
		}
	}
	f.cfg = cfg
	// the journal is followed only while the system log is wanted
	wantJournal := cfg.active() && cfg.System
	switch {
	case wantJournal && f.stopJ == nil:
		ctx, cancel := context.WithCancel(f.ctx)
		f.stopJ, f.jdone = cancel, make(chan struct{})
		go f.followJournal(ctx, f.jdone)
	case !wantJournal && f.stopJ != nil:
		f.stopJ()
		done := f.jdone
		f.stopJ, f.jdone = nil, nil
		f.mu.Unlock()
		<-done
		f.mu.Lock()
	}
}

// Statuses says how sending to each server is going, in a stable order.
func (f *Forwarder) Statuses() []ServerStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ServerStatus
	for _, s := range f.senders {
		out = append(out, s.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Server.key() < out[j].Server.key() })
	return out
}

func (f *Forwarder) dispatch(m Message) {
	f.mu.Lock()
	line := m.Format()
	for _, s := range f.senders {
		s.enqueue(line)
	}
	f.mu.Unlock()
}

// maxAge is how old a logged packet may be and still be forwarded (a variable for the tests).
var maxAge = 2 * time.Minute

// Packet is called with every packet a rule with logging matched.
func (f *Forwarder) Packet(e network.FirewallLogEntry) {
	// forwarding is live: what the router reads back out of the journal after a restart is old news
	if e.Time > 0 && time.Since(time.UnixMilli(e.Time)) > maxAge {
		return
	}
	f.mu.Lock()
	cfg := f.cfg
	f.mu.Unlock()
	if !cfg.active() || !cfg.Firewall {
		return
	}
	name, action := e.Rule, ""
	if f.Rule != nil {
		if n, a, ok := f.Rule(e.Rule); ok {
			name, action = n, a
		}
	}
	f.dispatch(FirewallMessage(e, name, action, cfg.Hostname))
}

func (f *Forwarder) followJournal(ctx context.Context, done chan struct{}) {
	defer close(done)
	open := f.Journal
	if open == nil {
		open = FollowJournal
	}
	retry := f.Retry
	if retry <= 0 {
		retry = 10 * time.Second
	}
	warned := false
	for ctx.Err() == nil {
		r, err := open(ctx)
		if err == nil {
			sc := bufio.NewScanner(r)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			closed := make(chan struct{})
			go func() { // closing the reader is what stops a blocked read
				select {
				case <-ctx.Done():
					r.Close()
				case <-closed:
				}
			}()
			for sc.Scan() {
				f.mu.Lock()
				host := f.cfg.Hostname
				f.mu.Unlock()
				m, ok := FromJournalLine(sc.Bytes(), host)
				if !ok || IsFirewallLine(m.Text) {
					continue
				}
				f.dispatch(m)
			}
			close(closed)
			r.Close()
			err = sc.Err()
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil && !warned {
			log.Printf("[!] Syslog forwarding: cannot follow the journal (%v); trying again every %v", err, retry)
			warned = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// TestResult is what happened to a test message sent to one server.
type TestResult struct {
	Server  Server
	OK      bool
	Message string
}

// SendTest sends one message to every server in the configuration, over a connection of its own, and says what happened: a
// TCP server is reached or it is not; a UDP datagram is sent and nobody can say whether it arrived.
func (f *Forwarder) SendTest(ctx context.Context, text string) []TestResult {
	f.mu.Lock()
	cfg := f.cfg
	f.mu.Unlock()
	line := Message{Time: time.Now(), Facility: 1, Severity: 6, Host: cfg.Hostname, App: "peakroute", MsgID: "TEST", Text: text}.Format()
	var out []TestResult
	for _, srv := range cfg.Servers {
		res := TestResult{Server: srv}
		d := net.Dialer{Timeout: 3 * time.Second}
		c, err := d.DialContext(ctx, srv.Protocol, srv.addr())
		if err != nil {
			res.Message = "could not connect: " + err.Error()
			out = append(out, res)
			continue
		}
		payload := line
		if srv.Protocol == "tcp" {
			payload += "\n"
		}
		c.SetWriteDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte(payload)); err != nil {
			res.Message = "could not send: " + err.Error()
		} else if srv.Protocol == "tcp" {
			res.OK, res.Message = true, "connected and sent"
		} else {
			res.OK, res.Message = true, "sent (UDP is not acknowledged, so check the server to be sure)"
		}
		c.Close()
		out = append(out, res)
	}
	return out
}

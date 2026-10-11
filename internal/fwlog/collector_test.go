package fwlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"peakroute/internal/network"
)

func journalLine(cursor string, micros int64, msg string) string {
	return fmt.Sprintf(`{"__CURSOR":%q,"__REALTIME_TIMESTAMP":"%d","MESSAGE":%q,"_TRANSPORT":"kernel"}`, cursor, micros, msg) + "\n"
}

const fwMsg = "fw:fw2 IN=br30 OUT=eth0 MAC=aa SRC=10.30.0.40 DST=1.2.3.4 LEN=60 PROTO=TCP SPT=40000 DPT=443 WINDOW=64240"

func TestParseJournalLine(t *testing.T) {
	e, cur, ok := ParseJournalLine([]byte(journalLine("s=1;i=7", 1791720000123456, fwMsg)))
	if !ok || cur != "s=1;i=7" || e.Time != 1791720000123 || e.Rule != "fw2" || e.Src != "10.30.0.40" || e.DstPort != 443 || e.In != "br30" {
		t.Fatalf("%+v %q %v", e, cur, ok)
	}
	// a timestamp that is not a number leaves the time empty; the line is still the firewall's
	if e, _, ok := ParseJournalLine([]byte(`{"MESSAGE":"fw:fw1 IN=eth1 OUT=br30 SRC=192.168.1.5 DST=10.30.0.20 PROTO=TCP SPT=5 DPT=9100","__REALTIME_TIMESTAMP":"junk"}`)); !ok || e.Time != 0 || e.Rule != "fw1" {
		t.Errorf("%+v %v", e, ok)
	}
	for _, bad := range []string{"", "not json", `{"MESSAGE":"something else"}`, `{"MESSAGE":["a","b"]}`, `{"MESSAGE":"fw:fw1 SRC=1.1.1.1"}`} {
		if _, _, ok := ParseJournalLine([]byte(bad)); ok {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// a source that hands out the given texts, one per call, and records what it was asked
type fakeSource struct {
	mu      sync.Mutex
	texts   []string
	errs    []error
	readErr []error // how the reader of the nth call ends (nil: a plain end)
	asked   []string
	block   chan struct{} // when set, the reader stays open (like a journal being followed) until it is closed
	started chan struct{}
}

func (f *fakeSource) source(ctx context.Context, after string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, after)
	n := len(f.asked) - 1
	if n < len(f.errs) && f.errs[n] != nil {
		return nil, f.errs[n]
	}
	text := ""
	if n < len(f.texts) {
		text = f.texts[n]
	}
	pr, pw := io.Pipe()
	var endErr error
	if n < len(f.readErr) {
		endErr = f.readErr[n]
	}
	go func() {
		io.WriteString(pw, text)
		if n == 0 && f.block != nil {
			if f.started != nil {
				close(f.started)
			}
			select {
			case <-f.block:
			case <-ctx.Done():
			}
		}
		pw.CloseWithError(endErr)
	}()
	return pr, nil
}

func (f *fakeSource) askedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gave up waiting for %s", what)
}

func TestTheCollectorSavesWhatTheJournalSaysAndCarriesOnAfterIt(t *testing.T) {
	now := t0
	s := open(t, &now)
	src := &fakeSource{texts: []string{
		journalLine("c1", 1791720000000000, fwMsg) +
			journalLine("c2", 1791720001000000, "fw:fw2 IN=br30 OUT=eth0 SRC=10.30.0.40 DST=1.2.3.4 PROTO=TCP SPT=40001 DPT=443") + // a repeat
			journalLine("c3", 1791720002000000, "kernel: something that is not the firewall") +
			"not even json\n" +
			journalLine("c4", 1791720003000000, "fw:fw3 IN=br30 OUT= SRC=10.30.0.41 DST=10.30.0.1 PROTO=ICMP"),
		journalLine("c5", 1791720100000000, fwMsg), // what the second reader brings
	}}
	c := &Collector{Store: s, Source: src.source, Every: 20 * time.Millisecond, Retry: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	// the first reader ends after its text: the collector reads again, after the last cursor it saved
	waitFor(t, "the second read", func() bool { return len(src.askedList()) >= 2 && s.Cursor() == "c5" })
	cancel()
	<-done
	asked := src.askedList()
	if asked[0] != "" || asked[1] != "c4" {
		t.Fatalf("asked after %q", asked)
	}
	got, _ := s.Query(Query{})
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	// newest first: the later fw2 packet (a new entry: more than a minute later), the ICMP one, the folded pair
	if got[0].Rule != "fw2" || got[0].Count != 1 || got[1].Rule != "fw3" || got[2].Rule != "fw2" || got[2].Count != 2 || got[2].Last-got[2].Time != 1000 {
		t.Fatalf("%+v", got)
	}
}

func TestTheCollectorTriesAgainAfterAFailure(t *testing.T) {
	now := t0
	s := open(t, &now)
	src := &fakeSource{errs: []error{errors.New("no journalctl"), errors.New("still no")}, texts: []string{"", "", journalLine("c1", 1791720000000000, fwMsg)}}
	c := &Collector{Store: s, Source: src.source, Every: 20 * time.Millisecond, Retry: 20 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, "the entry", func() bool { st, _ := s.Stats(); return st.Entries == 1 })
	cancel()
	<-done
}

func TestStoppingTheCollectorSavesWhatWasRead(t *testing.T) {
	now := t0
	s := open(t, &now)
	src := &fakeSource{texts: []string{journalLine("c1", 1791720000000000, fwMsg)}, block: make(chan struct{}), started: make(chan struct{})}
	// a long interval: nothing is saved until the collector is stopped
	c := &Collector{Store: s, Source: src.source, Every: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	<-src.started
	time.Sleep(100 * time.Millisecond)
	if st, _ := s.Stats(); st.Entries != 0 {
		t.Fatal("saved before the interval")
	}
	cancel()
	<-done
	if st, _ := s.Stats(); st.Entries != 1 || s.Cursor() != "c1" {
		t.Fatalf("%+v %q", st, s.Cursor())
	}
	close(src.block)
}

func TestTheCollectorSkipsLinesWithoutTheTagWithoutParsingThem(t *testing.T) {
	now := t0
	s := open(t, &now)
	huge := `{"MESSAGE":"` + strings.Repeat("x", 100000) + `"}` + "\n" // longer than a default scanner line
	src := &fakeSource{texts: []string{huge + journalLine("c1", 1791720000000000, fwMsg)}}
	c := &Collector{Store: s, Source: src.source, Every: 20 * time.Millisecond, Retry: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, "the entry", func() bool { st, _ := s.Stats(); return st.Entries == 1 })
	cancel()
	<-done
}

func TestTheCollectorWaitsBetweenTries(t *testing.T) {
	now := t0
	s := open(t, &now)
	var mu sync.Mutex
	tries := 0
	c := &Collector{Store: s, Every: time.Hour, Retry: 100 * time.Millisecond, Source: func(context.Context, string) (io.ReadCloser, error) {
		mu.Lock()
		tries++
		mu.Unlock()
		return nil, errors.New("no")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if tries < 2 || tries > 5 {
		t.Fatalf("%d tries in 350 ms with a 100 ms wait", tries)
	}
}

// journalctl is run with the right arguments: after the saved cursor, or from a week back when there is none
func TestJournalSourceAsksJournalctlForTheTaggedKernelLines(t *testing.T) {
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	script := "#!/bin/sh\necho \"$@\" > " + args + "\nsleep 5\n"
	if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct{ after, want string }{
		{"s=abc;i=9", "-k -f --output=json --no-pager --grep=fw: --after-cursor=s=abc;i=9"},
		{"", "-k -f --output=json --no-pager --grep=fw: --since=-7d"},
	} {
		os.Remove(args)
		r, err := JournalSource(context.Background(), tc.after)
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "journalctl to run", func() bool { _, err := os.Stat(args); return err == nil })
		time.Sleep(20 * time.Millisecond)
		got, _ := os.ReadFile(args)
		if strings.TrimSpace(string(got)) != tc.want {
			t.Errorf("after %q: journalctl %s, want %s", tc.after, got, tc.want)
		}
		start := time.Now()
		r.Close() // stops the process without waiting for its sleep
		if time.Since(start) > 2*time.Second {
			t.Error("closing the source waited for journalctl")
		}
	}
}

func TestAPositionTheJournalRefusesIsGivenUp(t *testing.T) {
	now := t0
	s := open(t, &now)
	must(t, s.Add([]network.FirewallLogEntry{pkt(0, "fw1", "10.0.0.5", "1.2.3.4", 22)}, "garbled"))
	src := &fakeSource{
		texts:   []string{"", journalLine("c1", 1791720100000000, fwMsg)},
		readErr: []error{fmt.Errorf("%w (Failed to seek to cursor)", ErrBadCursor)},
	}
	c := &Collector{Store: s, Source: src.source, Every: 20 * time.Millisecond, Retry: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, "the cursor to be replaced", func() bool { return s.Cursor() == "c1" })
	cancel()
	<-done
	if asked := src.askedList(); len(asked) < 2 || asked[0] != "garbled" || asked[1] != "" {
		t.Fatalf("asked after %q", asked)
	}
}

func TestOtherFailuresKeepThePosition(t *testing.T) {
	now := t0
	s := open(t, &now)
	must(t, s.Add([]network.FirewallLogEntry{pkt(0, "fw1", "10.0.0.5", "1.2.3.4", 22)}, "c0"))
	src := &fakeSource{texts: []string{"", "", "", ""}, readErr: []error{errors.New("journalctl: exit status 1"), nil, errors.New("boom")}}
	c := &Collector{Store: s, Source: src.source, Every: 20 * time.Millisecond, Retry: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	if s.Cursor() != "c0" {
		t.Fatalf("the cursor was dropped: %q", s.Cursor())
	}
}

// a journalctl that fails says why: a refused position is told apart from any other failure
func TestJournalSourceReportsWhyJournalctlStopped(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "journalctl"), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	read := func() error {
		r, err := JournalSource(context.Background(), "x")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		_, err = io.ReadAll(r)
		return err
	}
	write(`echo "Failed to seek to cursor: Invalid argument" >&2; exit 1`)
	if err := read(); !errors.Is(err, ErrBadCursor) {
		t.Errorf("a refused position: %v", err)
	}
	write(`echo "Failed to open journal" >&2; exit 1`)
	if err := read(); err == nil || errors.Is(err, ErrBadCursor) || !strings.Contains(err.Error(), "Failed to open journal") {
		t.Errorf("another failure: %v", err)
	}
	write(`echo hello; exit 0`)
	if err := read(); err != nil {
		t.Errorf("a plain end: %v", err)
	}
}

func TestEveryPacketIsHandedOnAsItIsRead(t *testing.T) {
	now := t0
	s := open(t, &now)
	src := &fakeSource{texts: []string{
		journalLine("c1", 1791720000000000, fwMsg) + journalLine("c2", 1791720001000000, fwMsg) + // the same kind: folded in the log, two packets here
			journalLine("c3", 1791720002000000, "kernel: not the firewall"),
	}}
	var mu sync.Mutex
	var seen []string
	c := &Collector{Store: s, Source: src.source, Every: 20 * time.Millisecond, Retry: time.Hour, OnPacket: func(e network.FirewallLogEntry) {
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s:%d", e.Rule, e.Time))
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, "the entry", func() bool { st, _ := s.Stats(); return st.Entries == 1 })
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "fw2:1791720000000" || seen[1] != "fw2:1791720001000" {
		t.Fatalf("%v", seen)
	}
	if got, _ := s.Query(Query{}); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("%+v", got)
	}
}

package fwlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"peakroute/internal/network"
)

// Source starts following the kernel messages that carry a firewall rule's tag, one journal JSON object per line,
// beginning after the given journal cursor (empty: from the start of what is kept). Closing the reader stops it.
type Source func(ctx context.Context, afterCursor string) (io.ReadCloser, error)

// JournalSource follows the system journal with journalctl.
func JournalSource(ctx context.Context, afterCursor string) (io.ReadCloser, error) {
	args := []string{"-k", "-f", "--output=json", "--no-pager", "--grep=fw:"}
	if afterCursor != "" {
		args = append(args, "--after-cursor="+afterCursor)
	} else {
		args = append(args, "--since=-"+strconv.Itoa(DefaultKeepDays)+"d")
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	cmd.WaitDelay = 500 * time.Millisecond // do not wait on a helper that kept the pipe open after journalctl was killed
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &process{ReadCloser: out, cmd: cmd, stderr: stderr}, nil
}

// ErrBadCursor is what reading says when journalctl refuses the saved position (a damaged cursor); the collector then
// starts again from the start of what the journal keeps.
var ErrBadCursor = errors.New("the journal does not accept the saved position")

type process struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	once   sync.Once
	waited error
}

func (p *process) wait() error {
	p.once.Do(func() { p.waited = p.cmd.Wait() })
	return p.waited
}

// Read passes journalctl's output on; when it ends, a journalctl that failed says why.
func (p *process) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if err == io.EOF {
		if werr := p.wait(); werr != nil {
			msg := strings.TrimSpace(p.stderr.String())
			if strings.Contains(msg, "seek to cursor") {
				return n, fmt.Errorf("%w (%s)", ErrBadCursor, msg)
			}
			return n, fmt.Errorf("journalctl: %v: %s", werr, msg)
		}
	}
	return n, err
}

func (p *process) Close() error {
	p.cmd.Process.Kill()
	p.ReadCloser.Close()
	p.wait()
	return nil
}

// ParseJournalLine reads one line of `journalctl --output=json` and returns the firewall packet in it, with its time and
// the journal cursor. ok is false for any other line.
func ParseJournalLine(line []byte) (e network.FirewallLogEntry, cursor string, ok bool) {
	var raw struct {
		Message   string `json:"MESSAGE"`
		Timestamp string `json:"__REALTIME_TIMESTAMP"` // microseconds
		Cursor    string `json:"__CURSOR"`
	}
	if json.Unmarshal(line, &raw) != nil {
		return e, "", false
	}
	e, ok = network.ParseFirewallLog(raw.Message)
	if !ok {
		return e, "", false
	}
	if us, err := strconv.ParseInt(raw.Timestamp, 10, 64); err == nil {
		e.Time = us / 1000
	}
	return e, raw.Cursor, true
}

// Collector copies the firewall lines of the journal into the Store, a batch every Every, and when it is stopped or the
// journal reader ends, starts again after the last line it saved (so that nothing is lost, and nothing is saved twice).
type Collector struct {
	Store  *Store
	Source Source
	Every  time.Duration // how often the packets read so far are saved (default 2 s: one write, not one per packet)
	Retry  time.Duration // how long to wait before reading again after a failure (default 30 s)
}

func (c *Collector) Run(ctx context.Context) {
	every, retry := c.Every, c.Retry
	if every <= 0 {
		every = 2 * time.Second
	}
	if retry <= 0 {
		retry = 30 * time.Second
	}
	warned := false
	for ctx.Err() == nil {
		err := c.follow(ctx, every)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrBadCursor) {
			log.Printf("[!] Firewall log: the journal does not accept the saved position; reading from the start again")
			c.Store.ResetCursor()
		} else if err != nil && !warned {
			log.Printf("[!] Firewall log: cannot read the journal (%v); trying again every %v", err, retry)
			warned = true
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

// follow reads until the source ends or fails.
func (c *Collector) follow(ctx context.Context, every time.Duration) error {
	r, err := c.Source(ctx, c.Store.Cursor())
	if err != nil {
		return err
	}
	defer r.Close()

	type item struct {
		e      network.FirewallLogEntry
		cursor string
	}
	lines := make(chan item)
	done := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			text := sc.Bytes()
			if !strings.Contains(string(text), "fw:") {
				continue
			}
			if e, cur, ok := ParseJournalLine(text); ok {
				select {
				case lines <- item{e, cur}:
				case <-ctx.Done():
					done <- nil
					return
				}
			}
		}
		done <- sc.Err()
	}()

	var batch []network.FirewallLogEntry
	var cursor string
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := c.Store.Add(batch, cursor); err != nil {
			log.Printf("[!] Firewall log: cannot save %d entries: %v", len(batch), err)
		}
		batch = batch[:0]
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case it := <-lines:
			batch = append(batch, it.e)
			if it.cursor != "" {
				cursor = it.cursor
			}
		case <-tick.C:
			flush()
		case err := <-done:
			flush()
			return err
		case <-ctx.Done():
			flush()
			return nil
		}
	}
}

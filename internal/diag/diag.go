// Package diag runs the diagnostic tools that take a while (ping, traceroute) as jobs the page can follow: it starts the program, keeps
// what it prints line by line, and lets the page look at it every second and stop it. Nothing is run through a shell: the program and
// its arguments are fixed here, and the only things that come from the page are a validated target, a count and a source address.
package diag

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxJobsRunning = 3
	keepJobs       = 20
	maxLines       = 500
	maxLineLen     = 300
	MaxPings       = 20
	traceHops      = 20
)

// Tool names.
const (
	Ping       = "ping"
	Traceroute = "traceroute"
)

// Request is what the page asks for.
type Request struct {
	Tool   string
	Target string // a host name or an IPv4 address
	Count  int    // ping: how many (1 to 20)
	Source string // an IPv4 address of this router to send from, or "" for whatever the routing table says
}

// Summary is what a ping says at the end.
type Summary struct {
	Sent     int     `json:"sent"`
	Received int     `json:"received"`
	Loss     float64 `json:"loss"` // percent
	Min      float64 `json:"min"`  // milliseconds; 0 when nothing came back
	Avg      float64 `json:"avg"`
	Max      float64 `json:"max"`
}

// View is a job as the page sees it.
type View struct {
	ID      string   `json:"id"`
	Tool    string   `json:"tool"`
	Target  string   `json:"target"`
	Source  string   `json:"source"`
	Command string   `json:"command"`
	Running bool     `json:"running"`
	Started int64    `json:"started"` // Unix milliseconds
	Elapsed int64    `json:"elapsed"` // milliseconds
	Lines   []string `json:"lines"`
	Result  string   `json:"result"` // "" while running, then "ok", "failed", "stopped" or "timeout"
	Message string   `json:"message"`
	Summary *Summary `json:"summary"` // ping only, and only once it has said
}

type job struct {
	view   View
	cancel context.CancelFunc
	// stopped says that the page asked to stop it (so that a killed program is "stopped", not "failed")
	stopped bool
}

// Runner keeps the jobs.
type Runner struct {
	// Start runs a program and gives its output (standard output and error together). Nil means the real thing. Tests put their own here.
	Start func(ctx context.Context, name string, args ...string) (io.ReadCloser, func() error, error)

	mu    sync.Mutex
	jobs  map[string]*job
	order []string
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// ValidTarget accepts a host name or an IPv4 address and nothing that a program could take for an option.
func ValidTarget(s string) bool {
	if s == "" || len(s) > 253 || strings.HasPrefix(s, "-") {
		return false
	}
	if ip, err := netip.ParseAddr(s); err == nil {
		return ip.Is4()
	}
	return hostPattern.MatchString(s) && !strings.Contains(s, "..")
}

func (r *Runner) start(ctx context.Context, name string, args ...string) (io.ReadCloser, func() error, error) {
	if r.Start != nil {
		return r.Start(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	// the program is waited for as soon as it ends, and only then is the pipe closed, so that the reader sees the end of the output
	ended := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		pw.Close()
		ended <- err
	}()
	return pr, func() error { return <-ended }, nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// command is the program and arguments for a request, which are fixed here.
func command(req Request) (string, []string, time.Duration, error) {
	if !ValidTarget(req.Target) {
		return "", nil, 0, errors.New("that is not a name or an IPv4 address")
	}
	if req.Source != "" {
		if ip, err := netip.ParseAddr(req.Source); err != nil || !ip.Is4() {
			return "", nil, 0, errors.New("the source is not an IPv4 address")
		}
	}
	switch req.Tool {
	case Ping:
		n := req.Count
		if n < 1 || n > MaxPings {
			return "", nil, 0, fmt.Errorf("the number of pings is from 1 to %d", MaxPings)
		}
		args := []string{"-4", "-n", "-c", strconv.Itoa(n), "-W", "2", "-w", strconv.Itoa(n + 5)}
		if req.Source != "" {
			args = append(args, "-I", req.Source)
		}
		return "ping", append(args, "--", req.Target), time.Duration(n+10) * time.Second, nil
	case Traceroute:
		// tracepath (package iputils-tracepath) works without special rights; it takes no source address, the routing table decides
		if req.Source != "" {
			return "", nil, 0, errors.New("a traceroute leaves the way the routing table says; it cannot be sent from one network's address")
		}
		return "tracepath", []string{"-n", "-4", "-m", strconv.Itoa(traceHops), req.Target}, 90 * time.Second, nil
	}
	return "", nil, 0, errors.New("unknown tool")
}

// Run starts a job.
func (r *Runner) Run(req Request) (View, error) {
	name, args, limit, err := command(req)
	if err != nil {
		return View{}, err
	}
	r.mu.Lock()
	running := 0
	for _, j := range r.jobs {
		if j.view.Running {
			running++
		}
	}
	if running >= MaxJobsRunning {
		r.mu.Unlock()
		return View{}, fmt.Errorf("%d tests are running already: wait for one to finish or stop it", running)
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	out, wait, err := r.start(ctx, name, args...)
	if err != nil {
		r.mu.Unlock()
		cancel()
		if errors.Is(err, exec.ErrNotFound) {
			return View{}, fmt.Errorf("%s is not installed on this router", name)
		}
		return View{}, fmt.Errorf("%s could not be started: %v", name, err)
	}
	j := &job{cancel: cancel}
	j.view = View{ID: newID(), Tool: req.Tool, Target: req.Target, Source: req.Source, Command: strings.Join(append([]string{name}, args...), " "),
		Running: true, Started: time.Now().UnixMilli(), Lines: []string{}}
	if r.jobs == nil {
		r.jobs = map[string]*job{}
	}
	r.jobs[j.view.ID] = j
	r.order = append(r.order, j.view.ID)
	for len(r.order) > keepJobs { // the oldest that are over go; one that is running stays
		removed := false
		for i, id := range r.order {
			if !r.jobs[id].view.Running {
				delete(r.jobs, id)
				r.order = append(r.order[:i], r.order[i+1:]...)
				removed = true
				break
			}
		}
		if !removed {
			break
		}
	}
	view := j.view
	r.mu.Unlock()

	go r.collect(ctx, j, out, wait, req, limit)
	return view, nil
}

func (r *Runner) collect(ctx context.Context, j *job, out io.ReadCloser, wait func() error, req Request, limit time.Duration) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if len(line) > maxLineLen {
			line = line[:maxLineLen] + "…"
		}
		r.mu.Lock()
		if len(j.view.Lines) < maxLines {
			j.view.Lines = append(j.view.Lines, line)
		}
		r.mu.Unlock()
	}
	out.Close()
	err := wait()
	timedOut := errors.Is(ctx.Err(), context.DeadlineExceeded)
	j.cancel()

	r.mu.Lock()
	defer r.mu.Unlock()
	j.view.Running = false
	j.view.Elapsed = time.Now().UnixMilli() - j.view.Started
	if req.Tool == Ping {
		j.view.Summary = ParsePing(j.view.Lines)
	}
	switch {
	case j.stopped:
		j.view.Result, j.view.Message = "stopped", "Stopped."
	case timedOut:
		j.view.Result, j.view.Message = "timeout", fmt.Sprintf("Stopped after %d seconds.", int(limit.Seconds()))
	case err == nil:
		j.view.Result, j.view.Message = "ok", "Finished."
		if s := j.view.Summary; s != nil && s.Received == 0 {
			j.view.Result, j.view.Message = "failed", "No answer came back."
		}
	default:
		j.view.Result = "failed"
		j.view.Message = failureWords(req, err, j.view.Lines, j.view.Summary)
	}
}

// failureWords says in words why a program failed: ping exits with 1 when nothing answered and with 2 when it could not even start.
func failureWords(req Request, err error, lines []string, s *Summary) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) && req.Tool == Ping {
		if ee.ExitCode() == 1 {
			return "No answer came back."
		}
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				return l
			}
		}
	}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return "The test failed: " + err.Error()
}

// Get returns a job as it is now.
func (r *Runner) Get(id string) (View, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return View{}, false
	}
	v := j.view
	v.Lines = append([]string{}, v.Lines...)
	if v.Running {
		v.Elapsed = time.Now().UnixMilli() - v.Started
	}
	return v, true
}

// Stop asks a job to stop (false: there is no such job). A job that is over stays as it was.
func (r *Runner) Stop(id string) bool {
	r.mu.Lock()
	j, ok := r.jobs[id]
	if ok && j.view.Running {
		j.stopped = true
	}
	r.mu.Unlock()
	if ok {
		j.cancel()
	}
	return ok
}

var (
	pingTotals = regexp.MustCompile(`(\d+) packets transmitted, (\d+) (?:packets )?received`)
	pingRTT    = regexp.MustCompile(`= ([\d.]+)/([\d.]+)/([\d.]+)`)
)

// ParsePing reads the closing lines of ping. It returns nil when ping did not get as far as saying.
func ParsePing(lines []string) *Summary {
	var s *Summary
	for _, l := range lines {
		if m := pingTotals.FindStringSubmatch(l); m != nil {
			s = &Summary{}
			s.Sent, _ = strconv.Atoi(m[1])
			s.Received, _ = strconv.Atoi(m[2])
			if s.Sent > 0 {
				s.Loss = float64(s.Sent-s.Received) * 100 / float64(s.Sent)
			}
		}
		if m := pingRTT.FindStringSubmatch(l); m != nil && s != nil {
			s.Min, _ = strconv.ParseFloat(m[1], 64)
			s.Avg, _ = strconv.ParseFloat(m[2], 64)
			s.Max, _ = strconv.ParseFloat(m[3], 64)
		}
	}
	return s
}

// ---- the connection test --------------------------------------------------------------------------------------------

// Connect is the result of trying to open a TCP connection.
type Connect struct {
	Host    string  `json:"host"`
	Port    int     `json:"port"`
	Address string  `json:"address"` // what the name came to
	Status  string  `json:"status"`  // "open", "refused", "timeout", "unreachable", "no_name" or "error"
	Millis  float64 `json:"millis"`
	Message string  `json:"message"`
}

// TryConnect opens a TCP connection to host:port (from source when it is given) and closes it again, and says what happened.
func TryConnect(ctx context.Context, host string, port int, source string, timeout time.Duration) Connect {
	res := Connect{Host: host, Port: port}
	if !ValidTarget(host) || port < 1 || port > 65535 {
		res.Status, res.Message = "error", "that is not a name or an address and a port"
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := &net.Dialer{}
	if source != "" {
		ip := net.ParseIP(source)
		if ip == nil {
			res.Status, res.Message = "error", "the source is not an address"
			return res
		}
		d.LocalAddr = &net.TCPAddr{IP: ip}
	}
	start := time.Now()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		res.Millis = float64(time.Since(start).Microseconds()) / 1000
		res.Status, res.Message = "no_name", "the name could not be turned into an address"
		return res
	}
	res.Address = ips[0].String()
	start = time.Now()
	conn, err := d.DialContext(ctx, "tcp4", net.JoinHostPort(res.Address, strconv.Itoa(port)))
	res.Millis = float64(time.Since(start).Microseconds()) / 1000
	switch {
	case err == nil:
		conn.Close()
		res.Status, res.Message = "open", "The connection was accepted."
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		res.Status, res.Message = "timeout", "No answer: the port is closed by a firewall, or the address is not there."
	case strings.Contains(err.Error(), "refused"):
		res.Status, res.Message = "refused", "The address answered and refused: nothing is listening on that port."
	case strings.Contains(err.Error(), "unreachable") || strings.Contains(err.Error(), "no route"):
		res.Status, res.Message = "unreachable", "There is no way to reach that address from here."
	default:
		res.Status, res.Message = "error", err.Error()
	}
	return res
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Package speedtest measures the internet connection's speed from the router, so that the smart queue management card can fill in the
// line's speeds for a person who does not know them. It talks to Cloudflare's public speed test endpoints (speed.cloudflare.com): a
// download of random bytes and an upload that is thrown away. It is an estimate: a few parallel streams, a few seconds each, a cap on the
// data used, and the first second left out while the connections warm up.
package speedtest

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults: enough to fill a gigabit line for a few seconds and no more.
const (
	DefaultBase     = "https://speed.cloudflare.com"
	DefaultStreams  = 4
	DefaultDuration = 8 * time.Second
	DefaultWarmup   = time.Second
	// DefaultMaxBytes caps the data used in each direction (a line that is faster than this finishes early and is still measured).
	DefaultMaxBytes = 400 << 20
	chunk           = 25 << 20 // bytes asked for at a time
)

// Result is what a test measured.
type Result struct {
	DownloadMbit float64   `json:"download_mbit"`
	UploadMbit   float64   `json:"upload_mbit"`
	Server       string    `json:"server"`
	BytesDown    int64     `json:"bytes_down"` // the data the test used
	BytesUp      int64     `json:"bytes_up"`
	At           time.Time `json:"at"`
}

// Tester runs a test. The zero value is ready to use (with the defaults above).
type Tester struct {
	Base     string
	Streams  int
	Duration time.Duration
	Warmup   time.Duration
	MaxBytes int64
	Client   *http.Client // a client that speaks plain HTTP/1.1 over its own connections when nil
	// Device, when set, is the interface the test's connections go out of: with two internet connections, the one to measure.
	Device string
}

func (t *Tester) base() string {
	if t.Base != "" {
		return t.Base
	}
	return DefaultBase
}

func (t *Tester) streams() int {
	if t.Streams > 0 {
		return t.Streams
	}
	return DefaultStreams
}

func (t *Tester) duration() time.Duration {
	if t.Duration > 0 {
		return t.Duration
	}
	return DefaultDuration
}

func (t *Tester) warmup() time.Duration {
	if t.Warmup > 0 {
		return t.Warmup
	}
	if t.Duration > 0 && t.Duration < 3*DefaultWarmup {
		return t.Duration / 4
	}
	return DefaultWarmup
}

func (t *Tester) maxBytes() int64 {
	if t.MaxBytes > 0 {
		return t.MaxBytes
	}
	return DefaultMaxBytes
}

// tracker remembers the TCP connections a test opens, so that what the other end has acknowledged can be read from them.
type tracker struct {
	dev   string // the interface the connections are bound to ("": none)
	mu    sync.Mutex
	conns []*net.TCPConn
	last  []uint64 // the most each connection was seen to have acknowledged (a closed connection can no longer be asked)
}

func (tr *tracker) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: 10 * time.Second, Control: dialControl(tr.dev)}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if tc, ok := c.(*net.TCPConn); ok {
		tr.mu.Lock()
		tr.conns = append(tr.conns, tc)
		tr.last = append(tr.last, 0)
		tr.mu.Unlock()
	}
	return c, nil
}

// acked is the total acknowledged over every connection so far; ok is false when the system cannot say.
func (tr *tracker) acked() (uint64, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	var sum uint64
	any := false
	for i, c := range tr.conns {
		if n, ok := ackedBytes(c); ok {
			any = true
			if n > tr.last[i] {
				tr.last[i] = n
			}
		}
		sum += tr.last[i]
	}
	return sum, any
}

// client: several connections, not one multiplexed one (HTTP/2 would put every stream on a single TCP connection, which does not fill a
// fast line), and no timeout of its own (the test's time limit stops it).
func (t *Tester) client(tr *tracker) *http.Client {
	if t.Client != nil {
		return t.Client
	}
	return &http.Client{Transport: &http.Transport{
		DialContext:         tr.dial,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConnsPerHost: t.streams(),
		DisableCompression:  true,
	}}
}

// counter counts bytes, and says when the warm-up is over: only what moves after it is counted into the rate.
type counter struct {
	total   atomic.Int64
	warm    atomic.Int64 // bytes seen when the warm-up ended
	warmAt  atomic.Int64 // unix nanoseconds
	lastAt  atomic.Int64 // unix nanoseconds of the last data
	started time.Time
	warmup  time.Duration
	cap     int64 // the byte cap: a line so fast that a tenth of it passes before the warm-up time is over is warm by then
}

func (c *counter) add(n int) {
	total := c.total.Add(int64(n))
	now := time.Now()
	c.lastAt.Store(now.UnixNano())
	if c.warmAt.Load() == 0 && (now.Sub(c.started) >= c.warmup || (c.cap > 0 && total >= c.cap/10)) {
		if c.warmAt.CompareAndSwap(0, now.UnixNano()) {
			c.warm.Store(total)
		}
	}
}

// rate is the speed over the part of the test after the warm-up up to the last data, in Mbit/s.
func (c *counter) rate() (float64, error) {
	at := c.warmAt.Load()
	if at == 0 {
		return 0, errors.New("the test ended before it had any data to measure")
	}
	secs := time.Duration(c.lastAt.Load() - at).Seconds()
	bytes := c.total.Load() - c.warm.Load()
	if secs <= 0.01 || bytes <= 0 {
		return 0, errors.New("the test was too short to measure")
	}
	return float64(bytes) * 8 / secs / 1e6, nil
}

type countingReader struct {
	remaining int64
	c         *counter
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 0
	}
	r.remaining -= int64(len(p))
	r.c.add(len(p))
	return len(p), nil
}

// run starts the streams, which call one(ctx) over and over until ctx ends or the byte cap is reached, and measures c.
func (t *Tester) run(ctx context.Context, c *counter, one func(ctx context.Context) error) (float64, error) {
	rctx, cancel := context.WithTimeout(ctx, t.duration())
	defer cancel()
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for i := 0; i < t.streams(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rctx.Err() == nil && c.total.Load() < t.maxBytes() {
				if err := one(rctx); err != nil && rctx.Err() == nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	rate, err := c.rate()
	if err != nil {
		if e, ok := firstErr.Load().(error); ok {
			return 0, e
		}
		return 0, err
	}
	return rate, nil
}

// Download measures what comes in.
func (t *Tester) Download(ctx context.Context) (mbit float64, bytes int64, err error) {
	client := t.client(&tracker{dev: t.Device})
	c := &counter{started: time.Now(), warmup: t.warmup(), cap: t.maxBytes()}
	rate, err := t.run(ctx, c, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/__down?bytes=%d", t.base(), chunk), nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("the test server answered %s", res.Status)
		}
		b := make([]byte, 64<<10)
		for {
			n, err := res.Body.Read(b)
			c.add(n)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if c.total.Load() >= t.maxBytes() {
				return nil
			}
		}
	})
	return rate, c.total.Load(), err
}

// Upload measures what goes out. The rate is what the other end acknowledged (TCP's own count), so a send buffer that swallows the first
// seconds of a slow upload does not pass for speed; where the system cannot say, it is the bytes handed to the connection.
func (t *Tester) Upload(ctx context.Context) (mbit float64, bytes int64, err error) {
	tr := &tracker{dev: t.Device}
	client := t.client(tr)
	c := &counter{started: time.Now(), warmup: t.warmup(), cap: t.maxBytes()}

	rctx, cancel := context.WithTimeout(ctx, t.duration())
	defer cancel()
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for i := 0; i < t.streams(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rctx.Err() == nil && c.total.Load() < t.maxBytes() {
				body := &countingReader{remaining: chunk / 2, c: c}
				req, err := http.NewRequestWithContext(rctx, http.MethodPost, t.base()+"/__up", body)
				if err == nil {
					req.ContentLength = chunk / 2
					req.Header.Set("Content-Type", "application/octet-stream")
					var res *http.Response
					if res, err = client.Do(req); err == nil {
						io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
						res.Body.Close()
						if res.StatusCode != http.StatusOK {
							err = fmt.Errorf("the test server answered %s", res.Status)
						}
					}
				}
				if err != nil && rctx.Err() == nil {
					firstErr.CompareAndSwap(nil, err)
					return
				}
			}
		}()
	}

	// the acknowledged bytes at the end of the warm-up and at the end (or when the cap is reached)
	var a0, a1 uint64
	var t0, t1 time.Time
	haveAck := false
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
loop:
	for {
		select {
		case <-rctx.Done():
			break loop
		case <-poll.C:
			sum, ok := tr.acked()
			if !ok {
				continue
			}
			haveAck = true
			now := time.Now()
			if t0.IsZero() && (now.Sub(c.started) >= t.warmup() || sum >= uint64(t.maxBytes())/10) {
				a0, t0 = sum, now
			}
			a1, t1 = sum, now
			if sum >= uint64(t.maxBytes()) {
				break loop
			}
		}
	}
	cancel()
	wg.Wait()
	if ctx.Err() != nil {
		return 0, 0, ctx.Err()
	}
	if haveAck && !t0.IsZero() && t1.Sub(t0) > 20*time.Millisecond && a1 > a0 {
		return float64(a1-a0) * 8 / t1.Sub(t0).Seconds() / 1e6, int64(a1), nil
	}
	if e, ok := firstErr.Load().(error); ok {
		return 0, 0, e
	}
	rate, err := c.rate()
	return rate, c.total.Load(), err
}

// Run measures the download, then the upload. progress is told which part is running (may be nil).
func (t *Tester) Run(ctx context.Context, progress func(phase string)) (Result, error) {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	say("download")
	down, bd, err := t.Download(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("the download test failed: %w", err)
	}
	say("upload")
	up, bu, err := t.Upload(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("the upload test failed: %w", err)
	}
	return Result{DownloadMbit: down, UploadMbit: up, Server: t.base(), BytesDown: bd, BytesUp: bu, At: time.Now()}, nil
}

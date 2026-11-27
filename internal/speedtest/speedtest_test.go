package speedtest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// a test server whose every connection moves about perConn bytes a second, in both directions
func slowServer(t *testing.T, perConn int64) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var down, up atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/__down", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.ParseInt(r.URL.Query().Get("bytes"), 10, 64)
		piece := make([]byte, 16<<10)
		every := time.Duration(float64(len(piece)) / float64(perConn) * float64(time.Second))
		for n > 0 {
			k := int64(len(piece))
			if k > n {
				k = n
			}
			if _, err := w.Write(piece[:k]); err != nil {
				return
			}
			down.Add(k)
			n -= k
			time.Sleep(every)
		}
	})
	mux.HandleFunc("/__up", func(w http.ResponseWriter, r *http.Request) {
		piece := make([]byte, 16<<10)
		every := time.Duration(float64(len(piece)) / float64(perConn) * float64(time.Second))
		for {
			n, err := r.Body.Read(piece)
			up.Add(int64(n))
			if err != nil {
				break
			}
			time.Sleep(every)
		}
		io.WriteString(w, "ok")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv, &down, &up
}

func quick(base string) *Tester {
	return &Tester{Base: base, Streams: 4, Duration: 1500 * time.Millisecond, Warmup: 300 * time.Millisecond, MaxBytes: 100 << 20}
}

func within(got, want, frac float64) bool { return got > want*(1-frac) && got < want*(1+frac) }

func TestTheDownloadAndUploadAreMeasuredNearWhatTheConnectionsMove(t *testing.T) {
	// 4 streams of 500 kB/s each: 2 MB/s = 16 Mbit/s
	srv, _, _ := slowServer(t, 500_000)
	var phases []string
	res, err := quick(srv.URL).Run(context.Background(), func(p string) { phases = append(phases, p) })
	if err != nil {
		t.Fatal(err)
	}
	if !within(res.DownloadMbit, 16, 0.3) || !within(res.UploadMbit, 16, 0.3) {
		t.Errorf("measured %.1f down %.1f up, want about 16", res.DownloadMbit, res.UploadMbit)
	}
	if strings.Join(phases, ",") != "download,upload" || res.Server != srv.URL || res.BytesDown == 0 || res.BytesUp == 0 || res.At.IsZero() {
		t.Errorf("%v %+v", phases, res)
	}
}

func TestTheDataUsedIsCapped(t *testing.T) {
	srv, down, up := slowServer(t, 50_000_000) // fast enough to hit the cap
	tester := &Tester{Base: srv.URL, Streams: 4, Duration: 3 * time.Second, Warmup: 20 * time.Millisecond, MaxBytes: 8 << 20}
	if _, err := tester.Run(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	// a stream may be mid-chunk when the cap is reached: it stops at the next read, so the overshoot is small
	if down.Load() > 8<<20+4*(25<<20) || up.Load() > 8<<20+4*(25<<20) {
		t.Errorf("down %d up %d", down.Load(), up.Load())
	}
	if down.Load() >= 4*(25<<20) {
		t.Errorf("the whole chunks were downloaded: %d", down.Load())
	}
}

func TestAServerThatFailsIsReportedNotMeasured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusServiceUnavailable) }))
	defer srv.Close()
	_, err := quick(srv.URL).Run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "download test failed") || !strings.Contains(err.Error(), "503") {
		t.Errorf("%v", err)
	}
	if _, err := quick("http://127.0.0.1:1").Run(context.Background(), nil); err == nil {
		t.Error("a closed port was measured")
	}
}

func TestACancelledTestStopsAtOnce(t *testing.T) {
	srv, _, _ := slowServer(t, 100_000)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	tester := &Tester{Base: srv.URL, Duration: 10 * time.Second}
	if _, err := tester.Run(ctx, nil); err == nil {
		t.Error("a cancelled test returned a result")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v to stop", time.Since(start))
	}
}

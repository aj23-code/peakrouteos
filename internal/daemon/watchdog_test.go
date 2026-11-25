package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestTheWatchdogIsFedAThirdOfTheTimeSystemdAllows(t *testing.T) {
	every, ok := watchdogInterval(env(map[string]string{"WATCHDOG_USEC": "90000000", "NOTIFY_SOCKET": "/run/systemd/notify"}), 100)
	if !ok || every != 30*time.Second {
		t.Errorf("%v %v", every, ok)
	}
	// meant for another process, no watchdog, no socket, nonsense: nothing to feed
	for name, m := range map[string]map[string]string{
		"another pid": {"WATCHDOG_USEC": "90000000", "NOTIFY_SOCKET": "/x", "WATCHDOG_PID": "7"},
		"none":        {"NOTIFY_SOCKET": "/x"},
		"no socket":   {"WATCHDOG_USEC": "90000000"},
		"nonsense":    {"WATCHDOG_USEC": "soon", "NOTIFY_SOCKET": "/x"},
		"zero":        {"WATCHDOG_USEC": "0", "NOTIFY_SOCKET": "/x"},
	} {
		if _, ok := watchdogInterval(env(m), 100); ok {
			t.Errorf("%s: a watchdog was started", name)
		}
	}
	if _, ok := watchdogInterval(env(map[string]string{"WATCHDOG_USEC": "90000000", "NOTIFY_SOCKET": "/x", "WATCHDOG_PID": strconv.Itoa(100)}), 100); !ok {
		t.Error("its own pid was refused")
	}
	if every, _ := watchdogInterval(env(map[string]string{"WATCHDOG_USEC": "500000", "NOTIFY_SOCKET": "/x"}), 1); every != time.Second {
		t.Errorf("%v: never faster than a second", every)
	}
}

func TestNotifyReachesTheSocketSystemdListensOn(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "notify")
	l, err := net.ListenPacket("unixgram", sock)
	if err != nil {
		t.Skipf("no unix datagram sockets here: %v", err)
	}
	defer l.Close()
	if err := notifyState(sock, "WATCHDOG=1"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	l.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := l.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "WATCHDOG=1" {
		t.Errorf("%q %v", buf[:n], err)
	}
	if err := notifyState(filepath.Join(t.TempDir(), "nobody"), "WATCHDOG=1"); err == nil {
		t.Error("a missing socket is not an error")
	}
}

type feed struct {
	mu    sync.Mutex
	sent  int
	fails bool
}

func (f *feed) notify(string) error { f.mu.Lock(); defer f.mu.Unlock(); f.sent++; return nil }
func (f *feed) count() int          { f.mu.Lock(); defer f.mu.Unlock(); return f.sent }

func TestTheWatchdogIsFedWhileTheDaemonAnswersAndStopsWhenItDoesNot(t *testing.T) {
	f := &feed{}
	var mu sync.Mutex
	healthy := true
	probe := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if !healthy {
			return errors.New("no answer")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunWatchdog(ctx, 10*time.Millisecond, 0, probe, f.notify, func(string, ...any) {})
		close(done)
	}()
	defer func() { cancel(); <-done }()

	time.Sleep(80 * time.Millisecond)
	if f.count() < 3 {
		t.Fatalf("fed only %d times while well", f.count())
	}
	mu.Lock()
	healthy = false
	mu.Unlock()
	time.Sleep(30 * time.Millisecond) // a round in flight
	at := f.count()
	time.Sleep(100 * time.Millisecond)
	if f.count() != at {
		t.Errorf("the watchdog was fed (%d -> %d) while the daemon did not answer", at, f.count())
	}
	mu.Lock()
	healthy = true
	mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	if f.count() <= at {
		t.Error("it did not start feeding again when the daemon answered")
	}
}

func TestTheFirstMinutesAreFedWhateverTheProbeSays(t *testing.T) {
	f := &feed{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunWatchdog(ctx, 10*time.Millisecond, time.Hour, func(context.Context) error { return errors.New("still starting") }, f.notify, func(string, ...any) {})
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	if f.count() < 3 {
		t.Errorf("the starting daemon was not fed: %d", f.count())
	}
}

func TestTheProbeAsksTheRealHTTPSServerForHealthz(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	port := ":" + srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	if err := healthProbe(port)(context.Background()); err != nil {
		t.Errorf("a healthy server: %v", err)
	}
	srv.Config.Handler = http.NotFoundHandler()
	if err := healthProbe(port)(context.Background()); err == nil {
		t.Error("a 404 counted as well")
	}
	srv.Close()
	if err := healthProbe(port)(context.Background()); err == nil {
		t.Error("a closed server counted as well")
	}
}

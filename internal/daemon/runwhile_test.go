package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond for up to two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunWhileStartsAndStopsWithTheCondition(t *testing.T) {
	var want, running atomic.Bool
	var starts atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		RunWhile(ctx, time.Millisecond, want.Load, func(ctx context.Context) {
			starts.Add(1)
			running.Store(true)
			<-ctx.Done()
			running.Store(false)
		})
	}()

	time.Sleep(20 * time.Millisecond)
	if starts.Load() != 0 {
		t.Fatal("work started while it was not wanted")
	}
	want.Store(true)
	waitFor(t, "work to start", running.Load)
	want.Store(false)
	waitFor(t, "work to stop when no longer wanted", func() bool { return !running.Load() })
	want.Store(true)
	waitFor(t, "work to start again", func() bool { return running.Load() && starts.Load() == 2 })

	cancel()
	<-finished
	if running.Load() {
		t.Fatal("RunWhile returned before its work had stopped")
	}
	if n := starts.Load(); n != 2 {
		t.Fatalf("work started %d times, want 2 (once per time it became wanted)", n)
	}
}

func TestRunWhileRestartsWorkThatReturned(t *testing.T) {
	var mu sync.Mutex
	starts := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunWhile(ctx, time.Millisecond, func() bool { return true }, func(context.Context) {
		mu.Lock()
		starts++
		mu.Unlock()
	})
	waitFor(t, "work that returned to be started again", func() bool { mu.Lock(); defer mu.Unlock(); return starts >= 3 })
}

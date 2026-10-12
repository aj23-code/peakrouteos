package wg

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// Reader reads the running WireGuard interfaces with "wg show all dump". The page asks every few seconds, so an answer is kept for a
// moment. Where the tools are not installed (or nothing is running) it answers with nothing.
type Reader struct {
	mu    sync.Mutex
	at    time.Time
	value []Interface
	Run   func(ctx context.Context) ([]byte, error) // for tests; default runs wg
	Keep  time.Duration                             // how long an answer stands; default 2 seconds
}

func (r *Reader) run(ctx context.Context) ([]byte, error) {
	if r.Run != nil {
		return r.Run(ctx)
	}
	return exec.CommandContext(ctx, "wg", "show", "all", "dump").Output()
}

// Interfaces returns the running interfaces.
func (r *Reader) Interfaces() []Interface {
	keep := r.Keep
	if keep == 0 {
		keep = 2 * time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.at.IsZero() && time.Since(r.at) < keep {
		return r.value
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := r.run(ctx)
	r.at = time.Now()
	if err != nil {
		r.value = nil
	} else {
		r.value = ParseDump(string(out))
	}
	return r.value
}

package daemon

import (
	"context"
	"time"
)

// RunWhile runs work while want says so, asked every `every`: work starts when want becomes true and is stopped (its context
// cancelled, and waited for) when want becomes false or ctx ends; work that returns by itself is started again at the next look.
// The router's own background work (usage counting, the internet health check, Dynamic DNS, the firewall log, presence) rests
// with it while the device is an access point, so that an access point spends no memory or processes on it.
func RunWhile(ctx context.Context, every time.Duration, want func() bool, work func(context.Context)) {
	type running struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	var cur *running
	halt := func() {
		if cur != nil {
			cur.cancel()
			<-cur.done
			cur = nil
		}
	}
	defer halt()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if cur != nil {
			select {
			case <-cur.done: // it returned by itself
				halt()
			default:
			}
		}
		if on := want(); on && cur == nil {
			wctx, cancel := context.WithCancel(ctx)
			cur = &running{cancel: cancel, done: make(chan struct{})}
			go func(done chan struct{}) {
				defer close(done)
				work(wctx)
			}(cur.done)
		} else if !on {
			halt()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

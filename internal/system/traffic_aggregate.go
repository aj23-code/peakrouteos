package system

import "time"

// Every history chart works from buckets of 60 seconds or coarser, so storing one row per
// second per interface is wasted writes. The aggregator collapses 1-second rate samples into
// one average row per interface per minute in RAM before they are persisted.

const aggregateBucketSeconds = 60

type minuteKey struct {
	iface  string
	minute int64 // Unix seconds, start of the minute
}

type minuteTotals struct {
	rxSum, txSum, n int64
	dirty           bool // changed since it was last handed out for writing
}

type minuteAggregator struct {
	totals map[minuteKey]*minuteTotals
}

func newMinuteAggregator() *minuteAggregator {
	return &minuteAggregator{totals: make(map[minuteKey]*minuteTotals)}
}

// add folds one sample into its minute.
func (a *minuteAggregator) add(s MetricSample) {
	key := minuteKey{iface: s.Iface, minute: s.Timestamp - s.Timestamp%aggregateBucketSeconds}

	t := a.totals[key]
	if t == nil {
		t = &minuteTotals{}
		a.totals[key] = t
	}
	t.rxSum += s.RxBytes
	t.txSum += s.TxBytes
	t.n++
	t.dirty = true
}

// changed returns the per-minute averages (timestamped at the start of the minute) that have
// changed since the last call. A minute that is still in progress is returned each time it
// gains samples; persisting it with INSERT OR REPLACE keeps the stored row current.
func (a *minuteAggregator) changed() []MetricSample {
	var out []MetricSample
	for key, t := range a.totals {
		if !t.dirty {
			continue
		}
		out = append(out, MetricSample{
			Iface:     key.iface,
			Timestamp: key.minute,
			RxBytes:   t.rxSum / t.n,
			TxBytes:   t.txSum / t.n,
		})
		t.dirty = false
	}
	return out
}

// expire forgets minutes that ended more than a minute ago. The extra minute lets a late
// sample still merge into its minute's average instead of replacing it with a single value.
func (a *minuteAggregator) expire(now time.Time) {
	cutoff := now.Unix()/aggregateBucketSeconds*aggregateBucketSeconds - aggregateBucketSeconds
	for key := range a.totals {
		if key.minute < cutoff {
			delete(a.totals, key)
		}
	}
}

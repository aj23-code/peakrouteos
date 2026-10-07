package system

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func sortedByTime(samples []MetricSample) []MetricSample {
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Timestamp != samples[j].Timestamp {
			return samples[i].Timestamp < samples[j].Timestamp
		}
		return samples[i].Iface < samples[j].Iface
	})
	return samples
}

func TestAggregatorAveragesSamplesIntoMinutes(t *testing.T) {
	const m = int64(1_700_000_040) // a multiple of 60
	agg := newMinuteAggregator()

	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 1, RxBytes: 100, TxBytes: 10})
	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 30, RxBytes: 200, TxBytes: 20})
	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 59, RxBytes: 300, TxBytes: 30})
	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 60, RxBytes: 50, TxBytes: 5})
	agg.add(MetricSample{Iface: "eth1", Timestamp: m + 5, RxBytes: 7, TxBytes: 9})

	got := sortedByTime(agg.changed())
	want := []MetricSample{
		{Iface: "eth0", Timestamp: m, RxBytes: 200, TxBytes: 20},
		{Iface: "eth1", Timestamp: m, RxBytes: 7, TxBytes: 9},
		{Iface: "eth0", Timestamp: m + 60, RxBytes: 50, TxBytes: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changed() = %+v, want %+v", got, want)
	}
}

func TestAggregatorOnlyReturnsWhatChanged(t *testing.T) {
	const m = int64(1_700_000_040)
	agg := newMinuteAggregator()

	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 1, RxBytes: 100})
	agg.changed()

	if again := agg.changed(); len(again) != 0 {
		t.Errorf("nothing changed, but changed() returned %+v", again)
	}

	// A later sample in the same (still open) minute updates its average
	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 2, RxBytes: 300})
	got := agg.changed()
	if len(got) != 1 || got[0].RxBytes != 200 || got[0].Timestamp != m {
		t.Errorf("after a second sample changed() = %+v, want one row at %d averaging 200", got, m)
	}
}

func TestAggregatorKeepsLastMinuteSoLateSamplesMerge(t *testing.T) {
	const m = int64(1_700_000_040)
	agg := newMinuteAggregator()
	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 10, RxBytes: 100})
	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 20, RxBytes: 300})
	agg.changed()

	// Now is inside the next minute: the previous minute must still be remembered
	agg.expire(time.Unix(m+65, 0))
	agg.add(MetricSample{Iface: "eth0", Timestamp: m + 59, RxBytes: 400}) // late straggler
	got := agg.changed()
	// merged: (100 + 300 + 400) / 3 = 266; a lone replacement row would read 400
	if len(got) != 1 || got[0].RxBytes != 266 {
		t.Errorf("late sample = %+v, want it merged into the minute's average (266), not replace it", got)
	}

	// Two minutes later it is forgotten
	agg.expire(time.Unix(m+130, 0))
	if len(agg.totals) != 0 {
		t.Errorf("minutes older than the last one should be expired, still tracking %d", len(agg.totals))
	}
}

// The point of aggregating: same charts, far fewer rows. Store identical traffic the old way
// (a row per second) and the new way (a row per minute) and compare what the API returns.
func TestPerMinuteRowsGiveTheSameChartsAsPerSecondRows(t *testing.T) {
	now := time.Now().Unix()
	start := (now-3600)/60*60 + 60 // an aligned minute inside the 1h window
	const minutes = 45             // spans several 15-minute buckets for the 24h view

	perSecond := newTestTrafficDB(t, filepath.Join(t.TempDir(), "sec.db"), time.Hour)
	defer perSecond.Close()
	perMinute := newTestTrafficDB(t, filepath.Join(t.TempDir(), "min.db"), time.Hour)
	defer perMinute.Close()

	agg := newMinuteAggregator()
	for k := int64(0); k < minutes; k++ {
		rx, tx := 1000*(k+1), 400*(k+1)
		for sec := int64(0); sec < 60; sec++ {
			ts := start + k*60 + sec
			if _, err := perSecond.db.Exec(`INSERT INTO interface_metrics VALUES ('eth0', ?, ?, ?)`, ts, rx, tx); err != nil {
				t.Fatal(err)
			}
			agg.add(MetricSample{Iface: "eth0", Timestamp: ts, RxBytes: rx, TxBytes: tx})
		}
	}
	perMinute.flush(agg.changed())

	if a, b := rowCount(t, perSecond), rowCount(t, perMinute); a != minutes*60 || b != minutes {
		t.Fatalf("rows: per-second store has %d (want %d), per-minute store has %d (want %d)", a, minutes*60, b, minutes)
	}

	for _, rng := range []string{"1h", "24h", "30d"} {
		want, err := perSecond.GetHistory("eth0", rng)
		if err != nil {
			t.Fatal(err)
		}
		got, err := perMinute.GetHistory("eth0", rng)
		if err != nil {
			t.Fatal(err)
		}
		if len(want.Rx) == 0 {
			t.Fatalf("%s: reference history is empty", rng)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s history differs\n per-minute: %v\n per-second: %v", rng, fmt.Sprint(got), fmt.Sprint(want))
		}
	}
}

func TestManySamplesCollapseIntoAtMostTwoRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")

	tdb := newTestTrafficDB(t, path, time.Hour)
	for i := 0; i < 100; i++ {
		tdb.RecordSample("eth0", int64(i), int64(i))
	}
	if err := tdb.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := newTestTrafficDB(t, path, time.Hour)
	defer reopened.Close()

	// 100 samples taken within moments are one minute's worth (two only if the clock crossed a minute)
	if n := rowCount(t, reopened); n < 1 || n > 2 {
		t.Errorf("100 samples produced %d rows, want 1 (or 2 across a minute boundary)", n)
	}
}

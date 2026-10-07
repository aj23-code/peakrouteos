package system

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"peakroute/internal/config"
)

type countingRecorder struct {
	mu      sync.Mutex
	samples int
}

func (c *countingRecorder) RecordSample(iface string, rxBytes, txBytes int64) {
	c.mu.Lock()
	c.samples++
	c.mu.Unlock()
}

func (c *countingRecorder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.samples
}

// resetSamplerState gives a test a clean sampler and restores the globals afterwards
func resetSamplerState(t *testing.T) *countingRecorder {
	t.Helper()

	rec := &countingRecorder{}

	sampleMu.Lock()
	lastIfaceStats = make(map[string]ifaceSample)
	latestIfaces = nil
	haveLastCPU = false
	latestCPUPct = 0
	globalTrafficDB = rec
	sampleMu.Unlock()

	t.Cleanup(func() {
		sampleMu.Lock()
		lastIfaceStats = make(map[string]ifaceSample)
		latestIfaces = nil
		haveLastCPU = false
		latestCPUPct = 0
		globalTrafficDB = nil
		sampleMu.Unlock()
	})

	return rec
}

func TestBytesPerSec(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur uint64
		seconds   float64
		want      uint64
		wantOK    bool
	}{
		{"steady traffic", 1000, 3000, 2, 1000, true},
		{"no traffic", 500, 500, 1, 0, true},
		{"fractional interval", 0, 1500, 1.5, 1000, true},
		{"counter reset", 5000, 100, 1, 0, false},
		{"no elapsed time", 100, 200, 0, 0, false},
		{"negative elapsed time", 100, 200, -1, 0, false},
	}
	for _, c := range cases {
		got, ok := bytesPerSec(c.prev, c.cur, c.seconds)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: bytesPerSec(%d, %d, %v) = (%d, %v), want (%d, %v)",
				c.name, c.prev, c.cur, c.seconds, got, ok, c.want, c.wantOK)
		}
	}
}

func TestFirstPollRecordsNothing(t *testing.T) {
	rec := resetSamplerState(t)

	if err := PollSystemSample(); err != nil {
		t.Fatalf("PollSystemSample: %v", err)
	}

	if n := rec.count(); n != 0 {
		t.Errorf("first poll recorded %d samples, want 0 (no previous reading to compute a rate from)", n)
	}
}

func TestPollRecordsOneSamplePerInterface(t *testing.T) {
	rec := resetSamplerState(t)

	if err := PollSystemSample(); err != nil {
		t.Fatalf("first PollSystemSample: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // make sure some time elapses between readings
	if err := PollSystemSample(); err != nil {
		t.Fatalf("second PollSystemSample: %v", err)
	}

	sampleMu.Lock()
	ifaces := len(latestIfaces)
	sampleMu.Unlock()

	if ifaces == 0 {
		t.Skip("no network interfaces found in /proc/net/dev")
	}
	if n := rec.count(); n != ifaces {
		t.Errorf("second poll recorded %d samples for %d interfaces, want one each", n, ifaces)
	}
}

// Regression test: serving the stats endpoint used to resample /proc and record extra
// history samples on every dashboard refresh, overwriting the poller's real values.
func TestReadingStatsDoesNotRecordSamples(t *testing.T) {
	rec := resetSamplerState(t)

	sysPath := filepath.Join(t.TempDir(), "sys.json")
	if err := config.SaveSysConfig(sysPath, &config.SysConfig{Hostname: "test", OperationMode: "ROUTER"}); err != nil {
		t.Fatalf("SaveSysConfig: %v", err)
	}

	if err := PollSystemSample(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := PollSystemSample(); err != nil {
		t.Fatal(err)
	}
	recordedByPoller := rec.count()

	for i := 0; i < 10; i++ {
		stats, err := GetSystemStats(sysPath)
		if err != nil {
			t.Fatalf("GetSystemStats: %v", err)
		}
		if len(stats.Interfaces) == 0 {
			t.Fatal("GetSystemStats returned no interfaces")
		}
	}

	if n := rec.count(); n != recordedByPoller {
		t.Errorf("reading stats recorded %d extra samples, want 0", n-recordedByPoller)
	}
}

func TestStatsRequestBeforeFirstPollStillWorks(t *testing.T) {
	rec := resetSamplerState(t)

	sysPath := filepath.Join(t.TempDir(), "sys.json")
	if err := config.SaveSysConfig(sysPath, &config.SysConfig{Hostname: "test", OperationMode: "ROUTER"}); err != nil {
		t.Fatalf("SaveSysConfig: %v", err)
	}

	stats, err := GetSystemStats(sysPath)
	if err != nil {
		t.Fatalf("GetSystemStats before any poll: %v", err)
	}
	if len(stats.Interfaces) == 0 {
		t.Error("expected interfaces from the fallback sample")
	}
	if n := rec.count(); n != 0 {
		t.Errorf("fallback sample recorded %d history samples, want 0", n)
	}
}

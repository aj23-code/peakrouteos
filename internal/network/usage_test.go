package network

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner answers nft and ip the way the tools print for the sets and the neighbour table.
type fakeRunner struct {
	sets  string
	neigh string
	err   error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	switch name {
	case "nft":
		return f.sets, f.err
	case "ip":
		return f.neigh, nil
	}
	return "", errors.New("unexpected command " + name)
}

// sets builds the JSON `nft -j list table` prints: the elements of each set with their byte counters.
func sets(elems map[string]map[string]int) string {
	var b strings.Builder
	b.WriteString(`{"nftables":[{"metainfo":{"version":"1.0.9"}}`)
	for name, m := range elems {
		b.WriteString(`,{"set":{"family":"inet","name":"` + name + `","table":"peakroute_acct","type":"ipv4_addr","elem":[`)
		first := true
		for ip, bytes := range m {
			if !first {
				b.WriteString(",")
			}
			first = false
			b.WriteString(`{"elem":{"val":"` + ip + `","expires":599,"counter":{"packets":1,"bytes":` + itoa(bytes) + `}}}`)
		}
		b.WriteString(`]}}`)
	}
	b.WriteString(`,{"chain":{"name":"forward"}}]}`)
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

const neigh = `[{"dst":"192.168.1.50","dev":"br0","lladdr":"F8:94:C2:A4:13:A7","state":["REACHABLE"]},
 {"dst":"fd00::50","dev":"br0","lladdr":"f8:94:c2:a4:13:a7","state":["STALE"]},
 {"dst":"192.168.1.60","dev":"br0","lladdr":"48:b0:2d:f1:fe:ca","state":["REACHABLE"]},
 {"dst":"192.168.1.70","dev":"br0","state":["FAILED"]}]`

func TestTheCountersOfADevicesAddressesAreAddedUpUnderItsHardwareAddress(t *testing.T) {
	f := &fakeRunner{neigh: neigh, sets: sets(map[string]map[string]int{
		"up4":   {"192.168.1.50": 1000, "192.168.1.60": 200},
		"down4": {"192.168.1.50": 9000},
		"up6":   {"fd00::50": 500},
	})}
	tr := NewUsageTracker(f, "")
	tr.Poll(t.Context())
	if got := tr.GetBytesForMAC("f8:94:c2:a4:13:a7"); got != 10500 {
		t.Errorf("laptop: %d, want 10500 (v4 up and down and v6 up)", got)
	}
	if got := tr.GetBytesForMAC("48:B0:2D:F1:FE:CA"); got != 200 {
		t.Errorf("tv: %d, want 200 (the lookup ignores case)", got)
	}
	if tr.GetBytesForMAC("02:00:00:00:00:99") != 0 {
		t.Error("a device nobody saw has bytes")
	}
}

func TestOnlyWhatACounterGrewByIsAddedAndAStartedOverCounterCountsFresh(t *testing.T) {
	f := &fakeRunner{neigh: neigh, sets: sets(map[string]map[string]int{"up4": {"192.168.1.50": 1000}})}
	tr := NewUsageTracker(f, "")
	tr.Poll(t.Context())
	f.sets = sets(map[string]map[string]int{"up4": {"192.168.1.50": 1600}})
	tr.Poll(t.Context())
	if got := tr.GetBytesForMAC("f8:94:c2:a4:13:a7"); got != 1600 {
		t.Errorf("after growing: %d, want 1600", got)
	}
	// the firewall was loaded again: the counter starts from zero
	f.sets = sets(map[string]map[string]int{"up4": {"192.168.1.50": 300}})
	tr.Poll(t.Context())
	if got := tr.GetBytesForMAC("f8:94:c2:a4:13:a7"); got != 1900 {
		t.Errorf("after a restart of the counter: %d, want 1900", got)
	}
	// the element expired and came back
	f.sets = sets(map[string]map[string]int{"up4": {}})
	tr.Poll(t.Context())
	f.sets = sets(map[string]map[string]int{"up4": {"192.168.1.50": 50}})
	tr.Poll(t.Context())
	if got := tr.GetBytesForMAC("f8:94:c2:a4:13:a7"); got != 1950 {
		t.Errorf("after expiring: %d, want 1950", got)
	}
}

func TestADeviceNobodyKnowsYetIsCountedOnceItIsKnown(t *testing.T) {
	f := &fakeRunner{neigh: `[]`, sets: sets(map[string]map[string]int{"up4": {"192.168.1.50": 700}})}
	tr := NewUsageTracker(f, "")
	tr.Poll(t.Context())
	if tr.GetBytesForMAC("f8:94:c2:a4:13:a7") != 0 {
		t.Fatal("counted for nobody")
	}
	f.neigh = neigh
	tr.Poll(t.Context())
	if got := tr.GetBytesForMAC("f8:94:c2:a4:13:a7"); got != 700 {
		t.Errorf("%d, want the 700 that were waiting", got)
	}
}

func TestTotalsSurviveARestartWithoutCountingTheKernelsCountersTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_usage.json")
	f := &fakeRunner{neigh: neigh, sets: sets(map[string]map[string]int{"up4": {"192.168.1.50": 1000}})}
	a := NewUsageTracker(f, path)
	a.Poll(t.Context())
	a.Save()
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("the file: %v %v", info, err)
	}

	// the daemon starts again: the kernel still holds the same counter, grown a little
	f.sets = sets(map[string]map[string]int{"up4": {"192.168.1.50": 1250}})
	b := NewUsageTracker(f, path)
	b.Load()
	if got := b.GetBytesForMAC("f8:94:c2:a4:13:a7"); got != 1000 {
		t.Fatalf("loaded %d, want 1000", got)
	}
	b.Poll(t.Context())
	if got := b.GetBytesForMAC("f8:94:c2:a4:13:a7"); got != 1250 {
		t.Errorf("after the restart: %d, want 1250 (not 2250)", got)
	}
}

func TestNoTableOrNoToolCountsNothingAndSaysNothing(t *testing.T) {
	tr := NewUsageTracker(&fakeRunner{err: errors.New("no such table")}, "")
	tr.Poll(t.Context())
	var nilTracker *UsageTracker
	nilTracker.Poll(t.Context())
	if nilTracker.GetBytesForMAC("f8:94:c2:a4:13:a7") != 0 || tr.GetBytesForMAC("f8:94:c2:a4:13:a7") != 0 {
		t.Error("bytes from nowhere")
	}
	// something that is not JSON is survived too
	NewUsageTracker(&fakeRunner{sets: "not json"}, "").Poll(t.Context())
}

// a runner whose table is missing until Ensure loads it
type lateTable struct {
	loaded bool
	sets   string
	neigh  string
	lists  int
}

func (l *lateTable) Run(_ context.Context, name string, args ...string) (string, error) {
	if name == "ip" {
		return l.neigh, nil
	}
	l.lists++
	if !l.loaded {
		return "", errors.New("Error: No such file or directory")
	}
	return l.sets, nil
}

func TestAMissingTableIsLoadedByTheTrackerAndCountedFromThen(t *testing.T) {
	r := &lateTable{neigh: neigh, sets: sets(map[string]map[string]int{"up4": {"192.168.1.50": 900}})}
	tr := NewUsageTracker(r, "")
	calls := 0
	tr.Ensure = func(context.Context) error { calls++; r.loaded = true; return nil }
	tr.Poll(t.Context())
	if calls != 1 {
		t.Fatalf("Ensure called %d times, want 1", calls)
	}
	if got := tr.GetBytesForMAC("f8:94:c2:a4:13:a7"); got != 900 {
		t.Errorf("%d bytes counted right after the table was loaded, want 900", got)
	}
	// loaded: it is not asked again
	tr.Poll(t.Context())
	if calls != 1 {
		t.Errorf("Ensure called again with the table there (%d)", calls)
	}
}

func TestATableThatCannotBeLoadedIsNotTriedEveryPoll(t *testing.T) {
	r := &lateTable{}
	tr := NewUsageTracker(r, "")
	calls := 0
	tr.Ensure = func(context.Context) error { calls++; return errors.New("no nft here") }
	for i := 0; i < 5; i++ {
		tr.Poll(t.Context())
	}
	if calls != 1 {
		t.Errorf("Ensure called %d times in five polls, want once a minute", calls)
	}
}

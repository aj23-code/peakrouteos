package presence

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"peakroute/internal/network"
)

type rig struct {
	t      *testing.T
	tr     *Tracker
	now    time.Time
	wifi   map[string]Link
	known  bool
	online map[string]bool // who answers ARP
	probe  error
	asked  [][]Target
}

const (
	macWired = "aa:bb:cc:00:00:01"
	macWifi  = "aa:bb:cc:00:00:02"
	macGone  = "aa:bb:cc:00:00:03"
)

func newRig(t *testing.T) *rig {
	r := &rig{t: t, now: time.Unix(1_000_000, 0), known: true, wifi: map[string]Link{}, online: map[string]bool{}}
	r.tr = &Tracker{
		Path: filepath.Join(t.TempDir(), "presence.json"),
		Now:  func() time.Time { return r.now },
		Wifi: func(context.Context) (map[string]Link, bool) { return r.wifi, r.known },
		Probe: func(_ context.Context, ts []Target) (map[string]bool, error) {
			r.asked = append(r.asked, ts)
			if r.probe != nil {
				return nil, r.probe
			}
			out := map[string]bool{}
			for _, tg := range ts {
				if r.online[tg.MAC] {
					out[tg.MAC] = true
				}
			}
			return out, nil
		},
		Targets: func() []Target {
			return []Target{
				{MAC: macWired, IP: netip.MustParseAddr("192.168.1.10")},
				{MAC: macWifi, IP: netip.MustParseAddr("192.168.1.11")},
				{MAC: macGone, IP: netip.MustParseAddr("192.168.1.12")},
			}
		},
	}
	r.tr.Load()
	return r
}

func (r *rig) leases() map[string]network.ClientLease {
	ls := []network.ClientLease{{MACAddress: macWired}, {MACAddress: macWifi}, {MACAddress: macGone}}
	r.tr.Annotate(context.Background(), ls)
	out := map[string]network.ClientLease{}
	for _, l := range ls {
		out[l.MACAddress] = l
	}
	return out
}

func (r *rig) advance(d time.Duration) { r.now = r.now.Add(d) }

func TestWifiAndWiredDevicesAreOnlineAndTheOneThatLeftIsOffline(t *testing.T) {
	r := newRig(t)
	r.wifi[macWifi] = Link{SSID: "Home", Band: "5 GHz"}
	r.online[macWired] = true
	r.tr.Round(context.Background())
	got := r.leases()

	if l := got[macWifi]; l.Status != "online" || l.Connection != "wifi" || l.SSID != "Home" || l.Band != "5 GHz" || l.LastSeen != r.now.Unix() {
		t.Fatalf("wifi: %+v", l)
	}
	if l := got[macWired]; l.Status != "online" || l.Connection != "wired" || l.SSID != "" || l.LastSeen != r.now.Unix() {
		t.Fatalf("wired: %+v", l)
	}
	if l := got[macGone]; l.Status != "offline" || l.Connection != "unknown" || l.LastSeen != 0 {
		t.Fatalf("never seen: %+v", l)
	}
}

func TestWifiDevicesAreNotAskedWithARP(t *testing.T) {
	r := newRig(t)
	r.wifi[macWifi] = Link{SSID: "Home"}
	r.tr.Round(context.Background())
	for _, tg := range r.asked[0] {
		if tg.MAC == macWifi {
			t.Fatalf("asked a Wi-Fi device: %+v", r.asked[0])
		}
	}
	if len(r.asked[0]) != 2 {
		t.Fatalf("%+v", r.asked[0])
	}
}

func TestADeviceStaysOnlineForTheWindowThenGoesOfflineKeepingWhatIsKnown(t *testing.T) {
	r := newRig(t)
	r.wifi[macWifi] = Link{SSID: "Home", Band: "2.4 GHz"}
	r.online[macWired] = true
	r.tr.Round(context.Background())
	seen := r.now.Unix()

	// both leave
	delete(r.wifi, macWifi)
	r.online[macWired] = false
	r.advance(75 * time.Second) // exactly the window: still counts
	if l := r.leases()[macWired]; l.Status != "online" {
		t.Fatalf("at the window: %+v", l)
	}
	r.advance(time.Second)
	got := r.leases()
	if l := got[macWired]; l.Status != "offline" || l.Connection != "wired" || l.LastSeen != seen {
		t.Fatalf("wired, gone: %+v", l)
	}
	// the Wi-Fi device is remembered as a Wi-Fi device, with where it was
	if l := got[macWifi]; l.Status != "offline" || l.Connection != "wifi" || l.SSID != "Home" || l.Band != "2.4 GHz" || l.LastSeen != seen {
		t.Fatalf("wifi, gone: %+v", l)
	}
}

func TestADeviceThatMovesFromWifiToACableIsWiredAfterItsNextAnswer(t *testing.T) {
	r := newRig(t)
	r.wifi[macWifi] = Link{SSID: "Home", Band: "5 GHz"}
	r.tr.Round(context.Background())
	delete(r.wifi, macWifi)
	r.online[macWifi] = true
	r.advance(20 * time.Second)
	r.tr.Round(context.Background())
	if l := r.leases()[macWifi]; l.Status != "online" || l.Connection != "wired" || l.SSID != "" || l.Band != "" {
		t.Fatalf("%+v", l)
	}
}

func TestWhenTheWifiSideCannotBeReadAnAnswerDoesNotMakeADeviceWired(t *testing.T) {
	r := newRig(t)
	r.known = false
	r.online[macWired] = true
	r.tr.Round(context.Background())
	got := r.leases()
	if l := got[macWired]; l.Status != "online" || l.Connection != "unknown" {
		t.Fatalf("%+v", l)
	}
	if l := got[macGone]; l.Status != "offline" || l.Connection != "unknown" {
		t.Fatalf("%+v", l)
	}
}

func TestWithoutAbilityToProbeStatusIsUnknownNotOffline(t *testing.T) {
	r := newRig(t)
	r.probe = errors.New("operation not permitted")
	r.wifi[macWifi] = Link{SSID: "Home"}
	r.tr.Round(context.Background())
	got := r.leases()
	if l := got[macWifi]; l.Status != "online" || l.Connection != "wifi" {
		t.Fatalf("Wi-Fi is still known: %+v", l)
	}
	for _, mac := range []string{macWired, macGone} {
		if l := got[mac]; l.Status != "unknown" || l.Connection != "wired" {
			t.Fatalf("%s: %+v", mac, l)
		}
	}
	// and when it is not known whether they are wired either
	r.known = false
	if l := r.leases()[macWired]; l.Status != "unknown" || l.Connection != "unknown" {
		t.Fatalf("%+v", l)
	}
	// probing starts to work: offline is now a real answer
	r.probe = nil
	r.tr.Round(context.Background())
	r.known = true
	if l := r.leases()[macGone]; l.Status != "offline" {
		t.Fatalf("%+v", l)
	}
}

func TestBeforeTheFirstRoundNothingIsClaimedOffline(t *testing.T) {
	r := newRig(t)
	if l := r.leases()[macWired]; l.Status != "unknown" {
		t.Fatalf("%+v", l)
	}
}

func TestWifiStationsAreOnlineEvenBeforeAnyRound(t *testing.T) {
	r := newRig(t)
	r.wifi[macWifi] = Link{SSID: "Home", Band: "5 GHz"}
	if l := r.leases()[macWifi]; l.Status != "online" || l.Connection != "wifi" {
		t.Fatalf("%+v", l)
	}
}

func TestWhatIsSeenIsRemembered(t *testing.T) {
	r := newRig(t)
	r.wifi[macWifi] = Link{SSID: "Home", Band: "5 GHz"}
	r.online[macWired] = true
	r.tr.Round(context.Background())
	r.tr.Save(false)
	seen := r.now.Unix()

	// a new run of the program, a day later, with nobody there
	r2 := newRig(t)
	r2.tr.Path = r.tr.Path
	r2.now = r.now.Add(24 * time.Hour)
	r2.tr.Load()
	r2.probe = nil
	r2.tr.Round(context.Background())
	got := r2.leases()
	if l := got[macWifi]; l.Status != "offline" || l.Connection != "wifi" || l.SSID != "Home" || l.LastSeen != seen {
		t.Fatalf("%+v", l)
	}
	if l := got[macWired]; l.Status != "offline" || l.Connection != "wired" || l.LastSeen != seen {
		t.Fatalf("%+v", l)
	}
	if b, _ := os.ReadFile(r.tr.Path); len(b) == 0 {
		t.Fatal("nothing written")
	}
	if fi, _ := os.Stat(r.tr.Path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestOnlyMovingTimesAreSavedRarelyButNewFactsAtOnce(t *testing.T) {
	r := newRig(t)
	r.online[macWired] = true
	r.tr.Round(context.Background())
	r.tr.Save(false)

	r.advance(20 * time.Second)
	r.tr.Round(context.Background()) // only the time moved
	os.Chtimes(r.tr.Path, time.Unix(5, 0), time.Unix(5, 0))
	r.tr.Save(false)
	if fi, _ := os.Stat(r.tr.Path); fi.ModTime().Unix() != 5 {
		t.Fatal("rewritten for a time change only")
	}
	r.advance(6 * time.Minute)
	r.tr.Round(context.Background())
	r.tr.Save(false)
	if fi, _ := os.Stat(r.tr.Path); fi.ModTime().Unix() == 5 {
		t.Fatal("not written after the interval")
	}

	// a device never seen before is saved at once
	os.Chtimes(r.tr.Path, time.Unix(5, 0), time.Unix(5, 0))
	r.online[macGone] = true
	r.advance(20 * time.Second)
	r.tr.Round(context.Background())
	r.tr.Save(false)
	if fi, _ := os.Stat(r.tr.Path); fi.ModTime().Unix() == 5 {
		t.Fatal("a new device was not saved at once")
	}
}

func TestOldRecordsAreForgotten(t *testing.T) {
	r := newRig(t)
	r.online[macWired] = true
	r.tr.Round(context.Background())
	r.advance(61 * 24 * time.Hour)
	r.tr.Save(true)
	r2 := newRig(t)
	r2.tr.Path = r.tr.Path
	r2.tr.Load()
	if len(r2.tr.seen) != 0 {
		t.Fatalf("%+v", r2.tr.seen)
	}
}

func TestAnUnreadableMemoryStartsEmpty(t *testing.T) {
	r := newRig(t)
	os.WriteFile(r.tr.Path, []byte("{not json"), 0o600)
	r.tr.Load()
	r.online[macWired] = true
	r.tr.Round(context.Background())
	if l := r.leases()[macWired]; l.Status != "online" {
		t.Fatalf("%+v", l)
	}
}

func TestRunRoundsUntilStoppedThenSaves(t *testing.T) {
	r := newRig(t)
	r.online[macWired] = true
	r.tr.Now = nil // real time
	r.tr.Every = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.tr.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
	if len(r.asked) < 3 {
		t.Fatalf("only %d rounds", len(r.asked))
	}
	if _, err := os.Stat(r.tr.Path); err != nil {
		t.Fatalf("not saved: %v", err)
	}
}

func TestADeviceThatJustLeftWifiIsNotCalledOnlineWhileProbingIsImpossible(t *testing.T) {
	r := newRig(t)
	r.wifi[macWifi] = Link{SSID: "Home"}
	r.tr.Round(context.Background())
	delete(r.wifi, macWifi)
	r.probe = errors.New("operation not permitted")
	r.advance(10 * time.Second) // well inside the window
	r.tr.Round(context.Background())
	if l := r.leases()[macWifi]; l.Status != "unknown" {
		t.Fatalf("%+v", l)
	}
}

func TestNoDevicesToLookForMeansNoProbe(t *testing.T) {
	r := newRig(t)
	r.tr.Targets = func() []Target { return nil }
	r.tr.Round(context.Background())
	if len(r.asked) != 0 {
		t.Fatalf("probed with nothing to look for: %v", r.asked)
	}
	// and a round where everything is on Wi-Fi has nobody left to ask either
	r2 := newRig(t)
	r2.wifi[macWired], r2.wifi[macWifi], r2.wifi[macGone] = Link{}, Link{}, Link{}
	r2.tr.Round(context.Background())
	if len(r2.asked) != 0 {
		t.Fatalf("%v", r2.asked)
	}
}

func TestTheFinalTimesAreSavedWhenTheTrackerStops(t *testing.T) {
	r := newRig(t)
	r.online[macWired] = true
	probe := r.tr.Probe
	r.tr.Probe = func(ctx context.Context, ts []Target) (map[string]bool, error) {
		r.now = r.now.Add(time.Second) // only the time moves from here on
		return probe(ctx, ts)
	}
	r.tr.Every = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.tr.Run(ctx); close(done) }()
	time.Sleep(80 * time.Millisecond)
	cancel()
	<-done
	r.tr.mu.Lock()
	last := r.tr.seen[macWired].LastSeen
	r.tr.mu.Unlock()

	r2 := newRig(t)
	r2.tr.Path = r.tr.Path
	r2.tr.Load()
	if got := r2.tr.seen[macWired].LastSeen; got != last {
		t.Fatalf("saved %d, the last time seen was %d", got, last)
	}
}

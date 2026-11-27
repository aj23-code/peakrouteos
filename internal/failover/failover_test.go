package failover

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"peakroute/internal/config"
)

var t0 = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func settings(failback bool, recovery time.Duration) config.FailoverSettings {
	return config.FailoverSettings{Failback: failback, Recovery: recovery}
}

func TestTheTrafficMovesOnlyWhenTheCarryingConnectionIsDownAndTheOtherIsNot(t *testing.T) {
	set := settings(true, 2*time.Minute)
	for name, tc := range map[string]struct {
		active string
		state  map[string]string
		want   string
	}{
		"the start, both up":               {"", map[string]string{"wan": "online", "wan2": "online"}, "wan"},
		"the start, the preferred is down": {"", map[string]string{"wan": "offline", "wan2": "online"}, "wan2"},
		"the start, nothing known yet":     {"", map[string]string{}, "wan"},
		"preferred fine":                   {"wan", map[string]string{"wan": "online", "wan2": "online"}, "wan"},
		"preferred not judged yet":         {"wan", map[string]string{"wan": "unknown", "wan2": "online"}, "wan"},
		"preferred down":                   {"wan", map[string]string{"wan": "offline", "wan2": "online"}, "wan2"},
		"preferred down, other not judged": {"wan", map[string]string{"wan": "offline", "wan2": "unknown"}, "wan2"},
		"both down: stay":                  {"wan", map[string]string{"wan": "offline", "wan2": "offline"}, "wan"},
		"both down on the other: stay":     {"wan2", map[string]string{"wan": "offline", "wan2": "offline"}, "wan2"},
		"the other fails, preferred is up": {"wan2", map[string]string{"wan": "online", "wan2": "offline"}, "wan"},
		"the other fails, preferred down":  {"wan2", map[string]string{"wan": "offline", "wan2": "offline"}, "wan2"},
	} {
		got, _ := decide(tc.active, "wan", tc.state, map[string]time.Time{}, t0, set)
		if got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	// the other connection preferred: the same with the names swapped
	if got, _ := decide("wan2", "wan2", map[string]string{"wan": "online", "wan2": "offline"}, nil, t0, set); got != "wan" {
		t.Errorf("%s", got)
	}
}

func TestTheTrafficGoesBackOnlyAfterThePreferredConnectionHasBeenUpForTheRecoveryTime(t *testing.T) {
	set := settings(true, 2*time.Minute)
	up := map[string]string{"wan": "online", "wan2": "online"}
	since := map[string]time.Time{"wan": t0}
	if got, _ := decide("wan2", "wan", up, since, t0.Add(119*time.Second), set); got != "wan2" {
		t.Errorf("went back after 119 s: %s", got)
	}
	if got, why := decide("wan2", "wan", up, since, t0.Add(120*time.Second), set); got != "wan" || !strings.Contains(why, "120 s") {
		t.Errorf("did not go back after 120 s: %s %q", got, why)
	}
	// without failback it stays on the other one while that one works...
	nofb := settings(false, 2*time.Minute)
	if got, _ := decide("wan2", "wan", up, since, t0.Add(time.Hour), nofb); got != "wan2" {
		t.Errorf("went back with failback off: %s", got)
	}
	// ... and goes back when that one fails
	if got, _ := decide("wan2", "wan", map[string]string{"wan": "online", "wan2": "offline"}, since, t0.Add(time.Second), nofb); got != "wan" {
		t.Errorf("did not leave a dead connection: %s", got)
	}
	// a preferred connection that was not up for the time (flapping) does not count
	if got, _ := decide("wan2", "wan", up, map[string]time.Time{}, t0.Add(time.Hour), set); got != "wan2" {
		t.Errorf("%s", got)
	}
}

// a fake machine: a routing table and the programs that read and change it
type machine struct {
	mu     sync.Mutex
	routes []route
	calls  []string
	addrs  map[string]string // device -> address
}

func (m *machine) Run(ctx context.Context, name string, args ...string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	line := name + " " + strings.Join(args, " ")
	m.calls = append(m.calls, line)
	switch {
	case line == "ip -4 -o route show default":
		var b strings.Builder
		for _, r := range m.routes {
			if r.via != "" {
				fmt.Fprintf(&b, "default via %s dev %s metric %d \n", r.via, r.dev, r.metric)
			} else {
				fmt.Fprintf(&b, "default dev %s scope link metric %d \n", r.dev, r.metric)
			}
		}
		return b.String(), nil
	case strings.HasPrefix(line, "ip -4 route replace default"), strings.HasPrefix(line, "ip -4 route del default"):
		r := parseRoutes(strings.Replace(line, "ip -4 route replace ", "", 1))
		if strings.HasPrefix(line, "ip -4 route del") {
			r = parseRoutes(strings.Replace(line, "ip -4 route del ", "", 1))
		}
		if len(r) != 1 {
			return "", fmt.Errorf("bad route %q", line)
		}
		var kept []route
		for _, x := range m.routes {
			if !(x.dev == r[0].dev && x.metric == r[0].metric) {
				kept = append(kept, x)
			}
		}
		m.routes = kept
		if strings.HasPrefix(line, "ip -4 route replace") {
			m.routes = append(m.routes, r[0])
		}
		return "", nil
	case strings.HasPrefix(line, "ip -4 -o addr show dev "):
		dev := strings.TrimPrefix(line, "ip -4 -o addr show dev ")
		return fmt.Sprintf("2: %s    inet %s/24 brd 0.0.0.0 scope global %s\n", dev, m.addrs[dev], dev), nil
	}
	return "", nil
}

func (m *machine) metrics() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.routes {
		out = append(out, fmt.Sprintf("%s=%d", r.dev, r.metric))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func (m *machine) called(prefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

type rig struct {
	m     *machine
	c     *Controller
	cfg   config.NetworkConfig
	state map[string]string
	now   time.Time
}

func newRig(t *testing.T) *rig {
	r := &rig{m: &machine{routes: []route{{"203.0.113.1", "eth0", 100}, {"198.51.100.1", "eth2", 200}},
		addrs: map[string]string{"eth0": "203.0.113.10", "eth2": "198.51.100.7"}},
		state: map[string]string{"wan": "online", "wan2": "online"}, now: t0}
	r.cfg = config.NetworkConfig{WanInterface: "eth0", WanMode: "dhcp", LanInterfaces: []string{"eth1"},
		Wan2: &config.WanConfig{Interface: "eth2", Mode: "dhcp"}}
	r.c = &Controller{Config: func() config.NetworkConfig { return r.cfg }, State: func(id string) string { return r.state[id] }, Runner: r.m,
		Now: func() time.Time { return r.now }, RunDir: t.TempDir()}
	return r
}

func (r *rig) step(after time.Duration) {
	r.now = r.now.Add(after)
	r.c.Step(context.Background())
}

func TestARouterWithOneConnectionIsLeftAlone(t *testing.T) {
	r := newRig(t)
	r.cfg.Wan2 = nil
	r.state["wan"] = "offline"
	r.step(0)
	if len(r.m.calls) != 0 {
		t.Fatalf("%v", r.m.calls)
	}
}

func TestAFailoverRaisesTheDeadRoutesMetricAndForgetsItsConnectionsAndFailbackWaitsForTheRecoveryTime(t *testing.T) {
	r := newRig(t)
	r.step(0)
	if r.m.metrics() != "eth0=100 eth2=200" || r.m.called("ip -4 route replace default")+r.m.called("ip -4 route del default") != 0 {
		t.Fatalf("a healthy pair is left as it is: %s %v", r.m.metrics(), r.m.calls)
	}
	// the preferred connection stops
	r.state["wan"] = "offline"
	r.step(5 * time.Second)
	if r.m.metrics() != "eth0=1000 eth2=100" {
		t.Fatalf("after the failure: %s", r.m.metrics())
	}
	st := r.c.Status()
	if st.Active != "wan2" || !st.FailedOver || st.Since == nil || len(st.Events) != 1 || st.Events[0].From != "wan" || st.Events[0].To != "wan2" || !strings.Contains(st.Events[0].Reason, "Internet is down") {
		t.Fatalf("%+v", st)
	}
	if r.m.called("conntrack -D --reply-dst 203.0.113.10") != 1 {
		t.Fatalf("the connections of the line that was left were not forgotten: %v", r.m.calls)
	}
	// a DHCP renewal puts a route with the old metric back: it is taken away again at the next look
	r.m.routes = append(r.m.routes, route{"203.0.113.1", "eth0", 100})
	r.step(2 * time.Second)
	if r.m.metrics() != "eth0=1000 eth2=100" {
		t.Fatalf("a route that came back: %s", r.m.metrics())
	}
	// it is up again, but not for long enough: it waits (at the standby metric, so its checks can use it)
	r.state["wan"] = "online"
	r.step(2 * time.Second)
	if r.m.metrics() != "eth0=200 eth2=100" || r.c.Status().Active != "wan2" {
		t.Fatalf("recovering: %s %+v", r.m.metrics(), r.c.Status())
	}
	if left := r.c.RecoveringIn(r.cfg); left < 100 || left > 121 {
		t.Fatalf("recovering in %d s", left)
	}
	r.step(100 * time.Second)
	if r.c.Status().Active != "wan2" {
		t.Fatal("went back too early")
	}
	r.step(30 * time.Second)
	if r.m.metrics() != "eth0=100 eth2=200" || r.c.Status().Active != "wan" || r.c.Status().FailedOver {
		t.Fatalf("failback: %s %+v", r.m.metrics(), r.c.Status())
	}
	if r.m.called("conntrack -D --reply-dst 198.51.100.7") != 1 {
		t.Fatalf("%v", r.m.calls)
	}
	if ev := r.c.Status().Events; len(ev) != 2 || ev[0].To != "wan" || ev[1].To != "wan2" {
		t.Fatalf("events newest first: %+v", ev)
	}
	// a flapping preferred connection resets the wait
	r.state["wan"] = "offline"
	r.step(time.Second)
	r.state["wan"] = "online"
	r.step(time.Second)
	r.state["wan"] = "offline"
	r.step(time.Second)
	r.state["wan"] = "online"
	r.step(60 * time.Second)
	if r.c.Status().Active != "wan2" {
		t.Fatalf("%+v", r.c.Status())
	}
}

func TestPreferringTheOtherConnectionMovesTheTrafficAtOnceIfItIsUp(t *testing.T) {
	r := newRig(t)
	r.step(0)
	r.cfg.WanPreferred = "wan2"
	r.step(time.Second)
	if r.c.Status().Active != "wan2" || r.m.metrics() != "eth0=200 eth2=100" {
		t.Fatalf("%s %+v", r.m.metrics(), r.c.Status())
	}
	if ev := r.c.Status().Events; len(ev) != 1 || !strings.Contains(ev[0].Reason, "changed") {
		t.Fatalf("%+v", ev)
	}
	// preferring a connection that is down: the traffic stays where it works
	r.cfg.WanPreferred = ""
	r.state["wan"] = "offline"
	r.step(time.Second)
	if r.c.Status().Active != "wan2" {
		t.Fatalf("%+v", r.c.Status())
	}
}

func TestAConnectionWithoutADefaultRouteYetIsLeftAlone(t *testing.T) {
	r := newRig(t)
	r.m.routes = []route{{"203.0.113.1", "eth0", 100}} // the second has no lease yet
	r.step(0)
	r.state["wan"] = "offline"
	r.step(time.Second)
	if r.m.metrics() != "eth0=1000" {
		t.Fatalf("%s", r.m.metrics())
	}
	for _, c := range r.m.calls {
		if strings.Contains(c, "eth2") && strings.Contains(c, "route replace") {
			t.Errorf("a route was made up for a connection that has none: %s", c)
		}
	}
}

func TestAPointToPointRouteHasNoGatewayAndKeepsNone(t *testing.T) {
	r := newRig(t)
	r.m.routes = []route{{"", "ppp0", 100}, {"", "ppp1", 200}}
	r.cfg.WanMode, r.cfg.WanPPPoE = "pppoe", &config.PPPoEConfig{Username: "u", Password: "p"}
	r.cfg.Wan2.Mode, r.cfg.Wan2.PPPoE = "pppoe", &config.PPPoEConfig{Username: "u", Password: "p"}
	r.step(0)
	r.state["wan"] = "offline"
	r.step(time.Second)
	if r.m.metrics() != "ppp0=1000 ppp1=100" {
		t.Fatalf("%s", r.m.metrics())
	}
	for _, c := range r.m.calls {
		if strings.Contains(c, "via") && strings.Contains(c, "route replace") {
			t.Errorf("a gateway was made up: %s", c)
		}
	}
}

func TestTheRoutesAreParsedFromWhatIpPrints(t *testing.T) {
	got := parseRoutes("default via 10.0.0.1 dev eth0 proto dhcp src 10.0.0.5 metric 100 \ndefault dev ppp0 scope link \n10.0.0.0/24 dev eth0 scope link\ndefault via fe80::1 dev eth0 metric 5\n")
	if len(got) != 3 || got[0] != (route{"10.0.0.1", "eth0", 100}) || got[1] != (route{"", "ppp0", 0}) {
		t.Fatalf("%+v", got)
	}
	if as := addrsOf("2: eth0    inet 10.0.0.5/24 brd 10.0.0.255 scope global eth0\n3: ppp0    inet 10.64.0.2 peer 10.64.64.64/32 scope global ppp0\n"); len(as) != 2 || as[0].String() != "10.0.0.5" || as[1].String() != "10.64.0.2" {
		t.Fatalf("%v", as)
	}
}

func TestTheServicesAreToldOnceForEachMoveAndNeverForAnythingElse(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	var told []Event
	r.c.OnChange = func(e Event) { mu.Lock(); told = append(told, e); mu.Unlock() }
	count := func() int {
		time.Sleep(50 * time.Millisecond) // (it is called in a goroutine of its own)
		mu.Lock()
		defer mu.Unlock()
		return len(told)
	}
	r.step(0)
	r.step(time.Second)
	if count() != 0 {
		t.Fatalf("told with nothing moved: %+v", told)
	}
	r.state["wan"] = "offline"
	r.step(time.Second)
	r.step(time.Second) // (and again: still the same move)
	if count() != 1 || told[0].From != "wan" || told[0].To != "wan2" {
		t.Fatalf("%+v", told)
	}
	// it is told after the routes were changed, not before
	if r.m.metrics() != "eth0=1000 eth2=100" {
		t.Fatalf("%s", r.m.metrics())
	}
	r.state["wan"] = "online"
	r.step(time.Second)
	r.step(200 * time.Second)
	if count() != 2 || told[1].To != "wan" {
		t.Fatalf("%+v", told)
	}
}

func TestTheAdministratorCanSwitchTheTrafficByHandAndBackToAutomatic(t *testing.T) {
	r := newRig(t)
	r.step(0)
	// to the second connection, at once, though the first is fine
	r.c.Pin("wan2")
	r.step(time.Second)
	st := r.c.Status()
	if st.Active != "wan2" || st.Pinned != "wan2" || !st.FailedOver && st.Preferred != "wan2" || r.m.metrics() != "eth0=200 eth2=100" {
		t.Fatalf("%s %+v", r.m.metrics(), st)
	}
	if ev := st.Events; len(ev) != 1 || ev[0].Reason != "switched by hand" || ev[0].To != "wan2" {
		t.Fatalf("%+v", ev)
	}
	// it stays there as long as the pinned connection works, however long the first has been up
	r.step(time.Hour)
	if r.c.Status().Active != "wan2" {
		t.Fatalf("%+v", r.c.Status())
	}
	// the pinned connection fails: the traffic still goes to the other
	r.state["wan2"] = "offline"
	r.step(time.Second)
	if r.c.Status().Active != "wan" || r.m.metrics() != "eth0=100 eth2=1000" {
		t.Fatalf("%s %+v", r.m.metrics(), r.c.Status())
	}
	r.state["wan2"] = "online"
	r.step(time.Second)
	r.step(300 * time.Second)
	if r.c.Status().Active != "wan2" {
		t.Fatalf("the pinned connection is back and is the one chosen: %+v", r.c.Status())
	}
	// back to automatic: the preferred connection at once (no recovery time to wait: nothing failed)
	r.c.Pin("")
	r.step(time.Second)
	st = r.c.Status()
	if st.Active != "wan" || st.Pinned != "" || r.m.metrics() != "eth0=100 eth2=200" {
		t.Fatalf("%s %+v", r.m.metrics(), st)
	}
	if ev := st.Events; ev[0].Reason != "back to automatic" {
		t.Fatalf("%+v", ev[0])
	}
	// pinning the connection that already carries the traffic changes nothing
	n := len(st.Events)
	r.c.Pin("wan")
	r.step(time.Second)
	if len(r.c.Status().Events) != n {
		t.Fatalf("%+v", r.c.Status().Events)
	}
}

func TestWhileSharingTheRoutersOwnTrafficStillMovesWhenThePreferredConnectionFails(t *testing.T) {
	r := newRig(t)
	r.cfg.WanFailover = &config.FailoverConfig{Balanced: true}
	r.step(0)
	r.state["wan"] = "offline"
	r.step(time.Second)
	if r.c.Status().Active != "wan2" || r.m.metrics() != "eth0=1000 eth2=100" {
		t.Fatalf("%s %+v", r.m.metrics(), r.c.Status())
	}
}

func TestTheLastChangesSurviveARestartOfTheDaemon(t *testing.T) {
	path := t.TempDir() + "/sub/failover_events.json"
	r := newRig(t)
	r.c.EventsPath = path
	r.step(0)
	r.state["wan"] = "offline"
	r.step(time.Second)
	// a new controller (the daemon restarted) shows what happened before
	r2 := newRig(t)
	r2.c.EventsPath = path
	if ev := r2.c.Status().Events; len(ev) != 1 || ev[0].To != "wan2" || !strings.Contains(ev[0].Reason, "Internet is down") {
		t.Fatalf("%+v", ev)
	}
	// and the new changes come before the old ones (a restarted controller starts on the preferred connection, so nothing moved yet)
	r2.step(0)
	r2.state["wan"] = "offline"
	r2.step(time.Second)
	if ev := r2.c.Status().Events; len(ev) != 2 || !ev[0].Time.After(ev[1].Time) && !ev[0].Time.Equal(ev[1].Time) {
		t.Fatalf("%+v", ev)
	}
	// a file that is not what it should be is ignored
	os.WriteFile(path, []byte("not json"), 0600)
	r3 := newRig(t)
	r3.c.EventsPath = path
	if ev := r3.c.Status().Events; len(ev) != 0 {
		t.Fatalf("%+v", ev)
	}
}

package failover

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/wanhealth"
)

// With the real kernel: two connections to two upstreams that both own 9.9.9.9. The real health checks probe it through each connection and
// the controller moves the default routes. The upstream of the first connection stops answering pings: the traffic goes through the second;
// it answers again: the traffic goes back after the recovery time. The test runs itself again inside a user and network namespace (the
// router), which makes two more (the upstreams).
func TestADeadUpstreamMovesTheTrafficAndItComesBackInTheRealKernel(t *testing.T) {
	if os.Getenv("FAILOVER_NS") == "child" {
		kernelChild(t)
		return
	}
	for _, tool := range []string{"unshare", "ip", "nsenter", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-rn", exe, "-test.run=^TestADeadUpstreamMovesTheTrafficAndItComesBackInTheRealKernel$", "-test.v")
	cmd.Env = append(os.Environ(), "FAILOVER_NS=child")
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "NAMESPACE-SKIP") {
		t.Skipf("cannot make namespaces here: %s", out)
	}
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

func sh(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
			fmt.Println("NAMESPACE-SKIP", string(out))
			os.Exit(0)
		}
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

func kernelChild(t *testing.T) {
	var pids []string
	for i := 0; i < 2; i++ {
		cmd := exec.Command("unshare", "-n", "sleep", "120")
		if err := cmd.Start(); err != nil {
			fmt.Println("NAMESPACE-SKIP", err)
			os.Exit(0)
		}
		defer cmd.Process.Kill()
		pids = append(pids, fmt.Sprint(cmd.Process.Pid))
	}
	time.Sleep(200 * time.Millisecond)
	a, b := pids[0], pids[1]
	ns := func(pid, cmd string) string { return sh(t, "nsenter -t "+pid+" -n sh -c '"+cmd+"'") }
	sh(t, "ip link set lo up; ip link add w1 type veth peer name p1; ip link add w2 type veth peer name p2; ip link set p1 netns "+a+"; ip link set p2 netns "+b)
	sh(t, "ip addr add 10.1.0.2/24 dev w1; ip addr add 10.2.0.2/24 dev w2; ip link set w1 up; ip link set w2 up")
	ns(a, "ip link set lo up; ip addr add 10.1.0.1/24 dev p1; ip link set p1 up; ip addr add 9.9.9.9/32 dev lo")
	ns(b, "ip link set lo up; ip addr add 10.2.0.1/24 dev p2; ip link set p2 up; ip addr add 9.9.9.9/32 dev lo")
	sh(t, "ip route add default via 10.1.0.1 dev w1 metric 100; ip route add default via 10.2.0.1 dev w2 metric 200")

	cfg := config.NetworkConfig{WanInterface: "w1", WanMode: "dhcp", LanInterfaces: []string{"eth1"},
		Wan2:        &config.WanConfig{Interface: "w2", Mode: "dhcp"},
		WanFailover: &config.FailoverConfig{RecoverySeconds: 2}}

	// the real health checks: each through its own connection, to 9.9.9.9
	store, err := wanhealth.Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mk := func(id, dev string) *wanhealth.Prober {
		return &wanhealth.Prober{Store: store, ID: id, Every: 150 * time.Millisecond, Timeout: 300 * time.Millisecond,
			Pinger:    &wanhealth.AutoPinger{Device: func() string { return dev }},
			DownAfter: func() int { return 2 },
			Target:    func() (wanhealth.Target, bool) { return wanhealth.Target{}, false },
			Extra:     func() []wanhealth.Target { return []wanhealth.Target{{Addr: netip.MustParseAddr("9.9.9.9")}} }}
	}
	one, two := mk("wan", "w1"), mk("wan2", "w2")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go one.Run(ctx)
	go two.Run(ctx)
	states := map[string]*wanhealth.Prober{"wan": one, "wan2": two}
	ctl := &Controller{Config: func() config.NetworkConfig { return cfg }, Every: 100 * time.Millisecond, Runner: execRunner{},
		State: func(id string) string { return states[id].Status().State }}
	go ctl.Run(ctx)

	via := func() string {
		out := sh(t, "ip -4 route get 9.9.9.9 | head -1")
		for _, f := range strings.Fields(out) {
			if f == "w1" || f == "w2" {
				return f
			}
		}
		return out
	}
	waitFor := func(what string, limit time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: not within %v\nroutes: %s\nstatus: %+v\nfirst: %+v\nsecond: %+v", what, limit, sh(t, "ip -4 route show default"), ctl.Status(), one.Status(), two.Status())
	}

	waitFor("both connections judged up", 10*time.Second, func() bool {
		return one.Status().State == "online" && two.Status().State == "online"
	})
	if got := via(); got != "w1" {
		t.Fatalf("the preferred connection carries the traffic: %s", got)
	}

	// the first upstream stops answering
	ns(a, "echo 1 > /proc/sys/net/ipv4/icmp_echo_ignore_all")
	waitFor("the traffic moves to the second connection", 10*time.Second, func() bool { return via() == "w2" })
	if st := ctl.Status(); st.Active != "wan2" || !st.FailedOver || len(st.Events) != 1 {
		t.Fatalf("%+v", st)
	}
	routes := sh(t, "ip -4 route show default")
	if !strings.Contains(routes, "dev w1 metric 1000") || !strings.Contains(routes, "dev w2 metric 100") {
		t.Fatalf("the routes after the failover:\n%s", routes)
	}

	// it answers again: it is seen coming back through its kept route, and the traffic waits for the recovery time before going back
	ns(a, "echo 0 > /proc/sys/net/ipv4/icmp_echo_ignore_all")
	waitFor("the first connection is seen up again", 10*time.Second, func() bool { return one.Status().State == "online" })
	if got := via(); got != "w2" {
		t.Fatalf("went back before the recovery time: %s", got)
	}
	waitFor("the traffic goes back to the first connection", 15*time.Second, func() bool { return via() == "w1" })
	if st := ctl.Status(); st.Active != "wan" || st.FailedOver || len(st.Events) != 2 {
		t.Fatalf("%+v", st)
	}

	// both stop: nothing moves (there is nowhere better to go)
	ns(a, "echo 1 > /proc/sys/net/ipv4/icmp_echo_ignore_all")
	ns(b, "echo 1 > /proc/sys/net/ipv4/icmp_echo_ignore_all")
	waitFor("both are found down", 10*time.Second, func() bool { return one.Status().State == "offline" && two.Status().State == "offline" })
	time.Sleep(600 * time.Millisecond)
	if got := via(); got != "w1" || len(ctl.Status().Events) != 2 {
		t.Fatalf("with both down the traffic moved: %s %+v", got, ctl.Status())
	}
}

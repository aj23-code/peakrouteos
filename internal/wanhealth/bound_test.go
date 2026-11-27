package wanhealth

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// With the real kernel (docs/wan-failover.md, section 4): two connections, each to an upstream that owns the address 9.9.9.9 on its
// own. Only the first upstream answers a ping. A probe bound to a connection goes out of that connection and no other: through the
// first it is answered, through the second it is not, whichever the routing table prefers. The test runs itself again inside a user and
// network namespace (the router), which makes two more (the upstreams).
func TestProbesGoOutOfTheConnectionTheyAreBoundToInTheRealKernel(t *testing.T) {
	if os.Getenv("WANHEALTH_NS") == "child" {
		boundChild(t)
		return
	}
	for _, tool := range []string{"unshare", "ip", "nsenter", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	// (some machines, such as GitHub's runners, do not let a program make a user and network namespace)
	if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
		t.Skipf("no user and network namespaces here: %v %s", err, out)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-rn", exe, "-test.run=^TestProbesGoOutOfTheConnectionTheyAreBoundToInTheRealKernel$", "-test.v")
	cmd.Env = append(os.Environ(), "WANHEALTH_NS=child")
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "NAMESPACE-SKIP") {
		t.Skipf("cannot make namespaces here: %s", out)
	}
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
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

func boundChild(t *testing.T) {
	var pids []string
	for i := 0; i < 2; i++ {
		cmd := exec.Command("unshare", "-n", "sleep", "60")
		if err := cmd.Start(); err != nil {
			fmt.Println("NAMESPACE-SKIP", err)
			os.Exit(0)
		}
		defer cmd.Process.Kill()
		pids = append(pids, fmt.Sprint(cmd.Process.Pid))
	}
	time.Sleep(200 * time.Millisecond) // the new namespaces exist once unshare has run
	a, b := pids[0], pids[1]
	ns := func(pid, cmd string) string { return sh(t, "nsenter -t "+pid+" -n sh -c '"+cmd+"'") }
	sh(t, "ip link set lo up; ip link add w1 type veth peer name p1; ip link add w2 type veth peer name p2; ip link set p1 netns "+a+"; ip link set p2 netns "+b)
	sh(t, "ip addr add 10.1.0.2/24 dev w1; ip addr add 10.2.0.2/24 dev w2; ip link set w1 up; ip link set w2 up")
	ns(a, "ip link set lo up; ip addr add 10.1.0.1/24 dev p1; ip link set p1 up; ip addr add 9.9.9.9/32 dev lo")
	ns(b, "ip link set lo up; ip addr add 10.2.0.1/24 dev p2; ip link set p2 up; ip addr add 9.9.9.9/32 dev lo; echo 1 > /proc/sys/net/ipv4/icmp_echo_ignore_all")
	sh(t, "ip route add default via 10.1.0.1 dev w1 metric 100; ip route add default via 10.2.0.1 dev w2 metric 200")

	via := func(dev string) error {
		p := &AutoPinger{Device: func() string { return dev }}
		_, err := p.Ping(context.Background(), addr("9.9.9.9"), 600*time.Millisecond)
		return err
	}
	if err := via("w1"); err != nil {
		t.Errorf("through the first connection, whose upstream answers: %v", err)
	}
	if err := via("w2"); err == nil {
		t.Error("through the second connection, whose upstream does not answer, the probe was answered: it did not go out of that connection")
	}
	if err := via(""); err != nil {
		t.Errorf("unbound, the routing table sends it through the first connection: %v", err)
	}
	if err := via("nonexistent9"); err == nil {
		t.Error("a probe bound to an interface that is not there succeeded")
	}

	// whole probers, one per connection, with the target 9.9.9.9 added: the first is up, the second is down
	mk := func(id, dev string) *Prober {
		p := &Prober{Store: memStore(t), Pinger: &AutoPinger{Device: func() string { return dev }}, ID: id, Every: time.Second, Timeout: 500 * time.Millisecond,
			Target: func() (Target, bool) { return Target{}, false },
			Extra:  func() []Target { return []Target{{Addr: addr("9.9.9.9"), Source: "added by you"}} }}
		return p
	}
	one, two := mk("wan", "w1"), mk("wan2", "w2")
	rounds(one, 3)
	rounds(two, 3)
	if st := one.Status(); st.State != StateOnline {
		t.Errorf("first: %+v", st)
	}
	if st := two.Status(); st.State != StateOffline {
		t.Errorf("second: %+v", st)
	}
}

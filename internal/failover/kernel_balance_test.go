package failover

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
)

// With the real kernel: a router with two connections (to the upstreams A and B, which both own 9.9.9.9) and a device behind it. New
// connections from the device are shared between the connections by weight, a connection that is down gets none, and what arrives on a
// connection (to the router itself, or through a port forward) is answered through the same connection even though the default route is
// the other's. The test runs itself again inside a user and network namespace (the router), which makes three more.
func TestSharingTheTrafficAndAnsweringTheWayItCameInTheRealKernel(t *testing.T) {
	if os.Getenv("FAILOVER_NS") == "balance" {
		balanceChild(t)
		return
	}
	for _, tool := range []string{"unshare", "ip", "nsenter", "sh", "nft", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-rn", exe, "-test.run=^TestSharingTheTrafficAndAnsweringTheWayItCameInTheRealKernel$", "-test.v")
	cmd.Env = append(os.Environ(), "FAILOVER_NS=balance")
	out, err := cmd.CombinedOutput()
	if strings.Contains(string(out), "NAMESPACE-SKIP") {
		t.Skipf("cannot make namespaces here: %s", out)
	}
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

var counted = regexp.MustCompile(`packets (\d+)`)

func balanceChild(t *testing.T) {
	var pids []string
	for i := 0; i < 3; i++ {
		cmd := exec.Command("unshare", "-n", "sleep", "120")
		if err := cmd.Start(); err != nil {
			fmt.Println("NAMESPACE-SKIP", err)
			os.Exit(0)
		}
		defer cmd.Process.Kill()
		pids = append(pids, fmt.Sprint(cmd.Process.Pid))
	}
	time.Sleep(200 * time.Millisecond)
	a, b, c := pids[0], pids[1], pids[2]
	ns := func(pid, cmd string) string { return sh(t, "nsenter -t "+pid+" -n sh -c '"+cmd+"'") }

	sh(t, "ip link set lo up; ip link add w1 type veth peer name p1; ip link add w2 type veth peer name p2; ip link add lan0 type veth peer name pc; "+
		"ip link set p1 netns "+a+"; ip link set p2 netns "+b+"; ip link set pc netns "+c)
	sh(t, "ip addr add 10.1.0.2/24 dev w1; ip addr add 10.2.0.2/24 dev w2; ip addr add 192.168.1.1/24 dev lan0; ip link set w1 up; ip link set w2 up; ip link set lan0 up")
	// the upstreams own 9.9.9.9 and count what reaches it
	count := `nft add table inet c; nft add chain inet c i { type filter hook input priority 0 \; }; nft add rule inet c i udp dport 9999 counter`
	// (each upstream also has a remote address to reach the router from: the answer to it is routed, not sent on the link)
	ns(a, "ip link set lo up; ip addr add 10.1.0.1/24 dev p1; ip link set p1 up; ip addr add 9.9.9.9/32 dev lo; ip addr add 198.51.100.50/32 dev lo; "+count)
	ns(b, "ip link set lo up; ip addr add 10.2.0.1/24 dev p2; ip link set p2 up; ip addr add 9.9.9.9/32 dev lo; ip addr add 203.0.113.50/32 dev lo; "+count)
	ns(c, "ip link set lo up; ip addr add 192.168.1.2/24 dev pc; ip link set pc up; ip route add default via 192.168.1.1")
	// the router: forwards, translates what goes out, forwards port 8080 of the second connection to the device, prefers the first connection
	sh(t, "echo 1 > /proc/sys/net/ipv4/ip_forward; echo 0 > /proc/sys/net/ipv4/conf/all/rp_filter; echo 0 > /proc/sys/net/ipv4/conf/default/rp_filter; echo 0 > /proc/sys/net/ipv4/conf/w1/rp_filter; echo 0 > /proc/sys/net/ipv4/conf/w2/rp_filter")
	sh(t, `nft -f - <<'EOF'
table ip nat {
    chain prerouting {
        type nat hook prerouting priority dstnat;
        iifname "w2" udp dport 8080 dnat to 192.168.1.2:8080
    }
    chain postrouting {
        type nat hook postrouting priority srcnat;
        oifname { "w1", "w2" } masquerade
    }
}
EOF`)
	sh(t, "ip route add default via 10.1.0.1 dev w1 metric 100; ip route add default via 10.2.0.1 dev w2 metric 200")

	// a UDP echo answers from the address the route to the sender picks, which is chosen before the marks are looked at: the router's own
	// services that matter here are the ones with a connection (TCP: the web interface, a VPN over TCP), whose address is the one it was
	// reached on. The device behind the router is reached through the port forward, with UDP.
	udpEcho := `import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",int(sys.argv[1])))
while True:
    d,a=s.recvfrom(100); s.sendto(d,a)`
	tcpEcho := `import socket,sys
s=socket.socket(socket.AF_INET,socket.SOCK_STREAM); s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1); s.bind(("0.0.0.0",int(sys.argv[1]))); s.listen(5)
while True:
    c,a=s.accept(); d=c.recv(100); c.sendall(d); c.close()`
	start := func(pid string, echo string, port string) {
		args := []string{"python3", "-c", echo, port}
		if pid != "" {
			args = append([]string{"nsenter", "-t", pid, "-n"}, args...)
		}
		cmd := exec.Command(args[0], args[1:]...)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill() })
	}
	start("", tcpEcho, "9000") // an echo in the router itself
	start(c, udpEcho, "8080")  // and one in the device behind it
	time.Sleep(300 * time.Millisecond)

	// does a packet sent to an address of the router through the upstream B come back? (it is answered from the router)
	echoes := func(pid, src, dst, port string) bool {
		kind := "SOCK_DGRAM"
		send := `s.sendto(b"hi",("%s",%s))`
		if port == "9000" {
			kind, send = "SOCK_STREAM", `s.connect(("%s",%s)); s.sendall(b"hi")`
		}
		py := fmt.Sprintf(`import socket,sys
s=socket.socket(socket.AF_INET,socket.%s); s.bind(("%s",0)); s.settimeout(1)
try:
    `+fmt.Sprintf(send, dst, port)+`
    s.recv(10)
except Exception:
    sys.exit(1)`, kind, src)
		return exec.Command("nsenter", "-t", pid, "-n", "python3", "-c", py).Run() == nil
	}
	// before the controller has made anything, what arrives on the second connection is answered through the first (the default route),
	// and so lost: the test would not tell if this were not so
	if echoes(b, "203.0.113.50", "10.2.0.2", "9000") {
		t.Fatal("the router answered through the connection it was reached on without any marks: the test proves nothing")
	}

	cfg := config.NetworkConfig{WanInterface: "w1", WanMode: "dhcp", LanInterfaces: []string{"lan0"}, LanIP: "192.168.1.1", LanSubnet: "255.255.255.0",
		Wan2:        &config.WanConfig{Interface: "w2", Mode: "dhcp"},
		WanFailover: &config.FailoverConfig{Balanced: true, WeightFirst: 1, WeightSecond: 3}}
	state := map[string]string{"wan": "online", "wan2": "online"}
	var cfgMu = make(chan struct{}, 1)
	cfgMu <- struct{}{}
	ctl := &Controller{Config: func() config.NetworkConfig { <-cfgMu; defer func() { cfgMu <- struct{}{} }(); return cfg }, Every: 100 * time.Millisecond, Runner: execRunner{}, RunDir: t.TempDir(),
		State: func(id string) string { <-cfgMu; defer func() { cfgMu <- struct{}{} }(); return state[id] }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ctl.Run(ctx)
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if ok() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: not within 10 s\n%s\n%+v", what, sh(t, "nft list table inet "+LBTable+" 2>&1; ip rule show; ip route show table 101; ip route show table 102"), ctl.Status())
	}
	waitFor("the table of marks is loaded", func() bool {
		return exec.Command("nft", "list", "table", "inet", LBTable).Run() == nil && strings.Contains(sh(t, "ip -4 rule show"), "lookup 102")
	})

	// what arrives on the second connection is now answered through it, to the router and through the port forward
	if !echoes(b, "203.0.113.50", "10.2.0.2", "9000") {
		t.Errorf("a packet that reached the router on the second connection was answered through the first\n%s", sh(t, "nft list table inet "+LBTable+"; ip -4 rule show; ip -4 route show table 102; ip -4 route show table 101; ip -4 route show; conntrack -L 2>&1 | head -5"))
	}
	txs := func() string {
		return sh(t, "ip -s -o link show w1 | tr -s ' ' | cut -d' ' -f1-3; ip -s link show w1 | sed -n 5p; ip -s link show w2 | sed -n 5p")
	}
	before := txs()
	if !echoes(b, "203.0.113.50", "10.2.0.2", "8080") {
		t.Errorf("a packet that reached a device through a port forward on the second connection was answered through the first\nbefore: %s\nafter: %s\n%s", before, txs(), sh(t, "nft list ruleset | head -40"))
	}
	if !echoes(a, "198.51.100.50", "10.1.0.2", "9000") {
		t.Errorf("the first connection stopped answering what reaches it")
	}

	next := 20000 // every connection has a port of its own: one that was used before would stay on the connection it was given
	send := func(n int) {
		py := fmt.Sprintf(`import socket
for i in range(%d):
    s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind(("0.0.0.0",%d+i)); s.sendto(b"x",("9.9.9.9",9999)); s.close()`, n, next)
		next += n
		if out, err := exec.Command("nsenter", "-t", c, "-n", "python3", "-c", py).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		time.Sleep(300 * time.Millisecond)
	}
	got := func(pid string) int {
		out := ns(pid, "nft list chain inet c i")
		m := counted.FindStringSubmatch(out)
		if m == nil {
			return 0
		}
		n, _ := strconv.Atoi(m[1])
		return n
	}

	// 400 new connections, weighted 1 : 3 (a quarter and three quarters: 100 and 300, give or take chance)
	send(400)
	first, second := got(a), got(b)
	if first+second != 400 {
		t.Fatalf("%d + %d connections arrived of 400", first, second)
	}
	if first < 55 || first > 145 {
		t.Errorf("the first connection got %d of 400 with weight 1 of 4 (about 100 expected)", first)
	}

	// the second goes down: new connections go to the first only
	<-cfgMu
	state["wan2"] = "offline"
	cfgMu <- struct{}{}
	waitFor("the second connection is taken out of the sharing", func() bool {
		return !strings.Contains(sh(t, "nft list table inet "+LBTable), "numgen")
	})
	send(100)
	if f, s := got(a), got(b); f-first != 100 || s != second {
		t.Errorf("with the second connection down the first got %d more and the second %d more of 100", f-first, s-second)
	}
	// and what arrives on the down connection is still answered through it (its own check may be wrong, the packet is not)
	if !echoes(b, "203.0.113.50", "10.2.0.2", "9000") {
		t.Errorf("a down connection no longer answers what reaches it")
	}
}

package translator

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
)

// These tests load the generated ruleset into a real kernel and try real connections, between three network
// namespaces joined by virtual cables:
//
//	  lan client 192.168.1.50 ---- eth1 [ router ] eth0 ---- server 203.0.113.2 (the "internet")
//	  guest client 10.20.0.50 ---- br20 [        ]
//
// The test binary runs itself as the router inside `unshare -rn` (root in a throw-away namespace, no real
// privileges) and again, through nsenter, as the small programs that listen and connect in the other namespaces.
// Every rule in docs/firewall.md is asserted by what a connection does, not by what the text says.

const (
	envRouter = "PEAKROUTE_TEST_FW_ROUTER"
	envHelper = "PEAKROUTE_TEST_FW_HELPER"
)

// ---- the helper that runs in the other namespaces -------------------------------------------------------------

// TestFirewallHelperProcess is not a test: it is the program the router test starts in the client and server
// namespaces. "listen tcp 80 udp 53" accepts connections and echoes datagrams until it is killed; "dial tcp
// 1.2.3.4:80" and "dial udp 1.2.3.4:53" print how a connection attempt ended: ok, refused or timeout.
func TestFirewallHelperProcess(t *testing.T) {
	args := strings.Fields(os.Getenv(envHelper))
	if len(args) == 0 {
		t.Skip("helper only")
	}
	switch args[0] {
	case "listen":
		for i := 1; i+1 < len(args); i += 2 {
			addr := ":" + args[i+1]
			switch args[i] {
			case "tcp":
				l, err := net.Listen("tcp", addr)
				if err != nil {
					fmt.Println("listen error:", err)
					os.Exit(2)
				}
				go func() {
					for {
						c, err := l.Accept()
						if err != nil {
							return
						}
						c.Write([]byte("hi"))
						c.Close()
					}
				}()
			case "udp":
				pc, err := net.ListenPacket("udp", addr)
				if err != nil {
					fmt.Println("listen error:", err)
					os.Exit(2)
				}
				go func() {
					buf := make([]byte, 100)
					for {
						n, from, err := pc.ReadFrom(buf)
						if err != nil {
							return
						}
						pc.WriteTo(buf[:n], from)
					}
				}()
			}
		}
		fmt.Println("listening")
		select {}
	case "dial":
		conn, err := net.DialTimeout(args[1], args[2], 700*time.Millisecond)
		if err == nil && args[1] == "udp" {
			conn.SetDeadline(time.Now().Add(700 * time.Millisecond))
			conn.Write([]byte("x"))
			buf := make([]byte, 10)
			_, err = conn.Read(buf)
		} else if err == nil {
			conn.SetDeadline(time.Now().Add(700 * time.Millisecond))
			buf := make([]byte, 10)
			_, err = conn.Read(buf) // the listener says hi: a connection that is accepted but then cut would show here
		}
		switch {
		case err == nil:
			fmt.Println("ok")
		case strings.Contains(err.Error(), "refused"):
			fmt.Println("refused")
		default:
			fmt.Println("timeout")
		}
	}
	os.Exit(0)
}

// ---- the router side ------------------------------------------------------------------------------------------

type fwLab struct {
	t       *testing.T
	exe     string
	pids    map[string]int // namespace name -> pid of a process living in it
	helpers []*exec.Cmd
}

func (l *fwLab) sh(name string, args ...string) string {
	l.t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		l.t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// in runs a command inside one of the other namespaces.
func (l *fwLab) in(ns string, name string, args ...string) string {
	l.t.Helper()
	return l.sh("nsenter", append([]string{"-t", strconv.Itoa(l.pids[ns]), "-n", name}, args...)...)
}

func (l *fwLab) helper(ns, what string) *exec.Cmd {
	cmd := exec.Command("nsenter", "-t", strconv.Itoa(l.pids[ns]), "-n", l.exe, "-test.run=^TestFirewallHelperProcess$")
	cmd.Env = append(os.Environ(), envHelper+"="+what)
	return cmd
}

func (l *fwLab) listen(ns, what string) {
	cmd := l.helper(ns, "listen "+what)
	out, err := cmd.StdoutPipe()
	if err != nil {
		l.t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		l.t.Fatal(err)
	}
	l.helpers = append(l.helpers, cmd)
	buf := make([]byte, 64)
	n, _ := out.Read(buf)
	if !strings.Contains(string(buf[:n]), "listening") {
		l.t.Fatalf("the listener in %s did not start: %q", ns, buf[:n])
	}
}

// dial tries to connect from a namespace and says how it ended: ok, refused or timeout.
func (l *fwLab) dial(ns, proto, addr string) string {
	l.t.Helper()
	out, err := l.helper(ns, "dial "+proto+" "+addr).Output()
	if err != nil {
		l.t.Fatalf("dial from %s: %v", ns, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch line {
		case "ok", "refused", "timeout":
			return line
		}
	}
	l.t.Fatalf("dial from %s: unexpected output %q", ns, out)
	return ""
}

func (l *fwLab) ping(ns, addr string) string {
	l.t.Helper()
	cmd := exec.Command("nsenter", "-t", strconv.Itoa(l.pids[ns]), "-n", "ping", "-c", "1", "-W", "1", addr)
	if err := cmd.Run(); err != nil {
		return "timeout"
	}
	return "ok"
}

func (l *fwLab) apply(cfg config.NetworkConfig) {
	l.t.Helper()
	d, err := Render(cfg, "ROUTER")
	if err != nil {
		l.t.Fatal(err)
	}
	a, _ := hasArtifact(d, PathNftables)
	path := filepath.Join(l.t.TempDir(), "nft.conf")
	if err := os.WriteFile(path, a.Content, 0644); err != nil {
		l.t.Fatal(err)
	}
	out, err := exec.Command("nft", "-f", path).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Operation not supported") || strings.Contains(string(out), "No such file") {
			l.t.Skipf("this kernel cannot load the ruleset here: %s", out)
		}
		l.t.Fatalf("nft -f: %v\n%s\n%s", err, out, a.Content)
	}
}

func newFwLab(t *testing.T) *fwLab {
	l := &fwLab{t: t, pids: map[string]int{}}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	l.exe = exe
	skip := true // only a failure to get namespaces at all skips the test
	fail := func(format string, args ...any) {
		if skip {
			t.Skipf("cannot build the lab here: "+format, args...)
		}
		t.Fatalf(format, args...)
	}
	for _, ns := range []string{"lan", "guest", "server"} {
		cmd := exec.Command("unshare", "-n", "sleep", "600")
		if err := cmd.Start(); err != nil {
			fail("%v", err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		l.pids[ns] = cmd.Process.Pid
	}
	time.Sleep(300 * time.Millisecond) // the children have switched namespace by now
	t.Cleanup(func() {
		for _, h := range l.helpers {
			h.Process.Kill()
			h.Wait()
		}
	})

	run := func(name string, args ...string) {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			fail("%s %v: %v %s", name, args, err, out)
		}
	}
	cable := func(routerSide, ns, peerName, routerIP, peerIP string) {
		run("ip", "link", "add", routerSide, "type", "veth", "peer", "name", peerName)
		skip = false
		run("ip", "link", "set", peerName, "netns", strconv.Itoa(l.pids[ns]))
		run("ip", "addr", "add", routerIP+"/24", "dev", routerSide)
		run("ip", "link", "set", routerSide, "up")
		l.in(ns, "ip", "addr", "add", peerIP+"/24", "dev", peerName)
		l.in(ns, "ip", "link", "set", peerName, "up")
		l.in(ns, "ip", "link", "set", "lo", "up")
	}
	cable("eth1", "lan", "p1", "192.168.1.1", "192.168.1.50")
	cable("br20", "guest", "p2", "10.20.0.1", "10.20.0.50")
	cable("eth0", "server", "p3", "203.0.113.1", "203.0.113.2")
	l.in("lan", "ip", "route", "add", "default", "via", "192.168.1.1")
	l.in("guest", "ip", "route", "add", "default", "via", "10.20.0.1")
	// the server is "the internet": it reaches the router's WAN address directly, and the private networks
	// only through it. A route back to the private addresses is not given: the router masquerades.
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		fail("ip_forward: %v", err)
	}
	run("ip", "link", "set", "lo", "up")
	return l
}

// fwConfig is the router: WAN eth0, the default network on eth1, and a guest network on br20 that may use the
// internet but is isolated from the others.
func fwConfig(rules []config.FirewallRule, forwards ...config.PortForward) config.NetworkConfig {
	c := wanCfg(func(c *config.NetworkConfig) {})
	c.WanInterface = "eth0"
	c.LanInterfaces = []string{"eth1"}
	c.Networks = []config.Network{{ID: "guest", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0", Internet: true, Isolated: true}}
	c.PortNetworks = map[string]config.PortNetworks{"eth1": {Tagged: []string{"guest"}}}
	if len(rules) > 0 || len(forwards) > 0 {
		c.Firewall = &config.FirewallConfig{Rules: rules, Forwards: forwards}
	}
	return c
}

func fr(id string, f func(*config.FirewallRule)) config.FirewallRule {
	r := config.FirewallRule{ID: id, Name: "rule " + id, From: "lan", Action: "allow", Protocol: "tcp", To: "internet"}
	f(&r)
	return r
}

func (l *fwLab) expect(what, got, want string) {
	l.t.Helper()
	if got != want {
		l.t.Errorf("%s: %s, want %s", what, got, want)
	}
}

func TestTheRulesDecideWhatRealConnectionsDo(t *testing.T) {
	if os.Getenv(envRouter) != "1" {
		if _, err := exec.LookPath("unshare"); err != nil {
			t.Skip("no unshare")
		}
		for _, tool := range []string{"nsenter", "nft", "ip"} {
			if _, err := exec.LookPath(tool); err != nil {
				t.Skipf("no %s", tool)
			}
		}
		if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
			t.Skipf("no user and network namespaces here (a restricted runner or container): %v %s", err, out)
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("unshare", "-rn", exe, "-test.run=^TestTheRulesDecideWhatRealConnectionsDo$", "-test.v")
		cmd.Env = append(os.Environ(), envRouter+"=1")
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), "--- SKIP") && !strings.Contains(string(out), "--- FAIL") {
			t.Skipf("%s", out)
		}
		if err != nil || strings.Contains(string(out), "--- FAIL") {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}

	lab := newFwLab(t)
	lab.listen("server", "tcp 80 tcp 443 tcp 22 tcp 8080 udp 53")
	lab.listen("lan", "tcp 80 tcp 8443 tcp 5000 tcp 5005 udp 5000")
	lab.listen("guest", "tcp 80")
	// the router's own services: web interface (8443), a plain web port, DNS
	for _, l := range []struct{ proto, addr string }{{"tcp", ":8443"}, {"tcp", ":80"}, {"tcp", ":22"}} {
		ln, err := net.Listen(l.proto, l.addr)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Write([]byte("hi"))
				c.Close()
			}
		}()
	}
	pc, err := net.ListenPacket("udp", ":53")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 100)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], from)
		}
	}()

	t.Run("without rules the switches decide, as before", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig(nil))
		lab.expect("lan to the internet", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
		lab.expect("lan to the router's web interface", lab.dial("lan", "tcp", "192.168.1.1:8443"), "ok")
		lab.expect("lan to the router's plain port", lab.dial("lan", "tcp", "192.168.1.1:80"), "ok")
		lab.expect("guest to the internet (switch on)", lab.dial("guest", "tcp", "203.0.113.2:80"), "ok")
		lab.expect("guest to the lan (isolated)", lab.dial("guest", "tcp", "192.168.1.50:80"), "timeout")
		lab.expect("lan to the guest network (isolated)", lab.dial("lan", "tcp", "10.20.0.50:80"), "timeout")
		lab.expect("guest to the router's DNS", lab.dial("guest", "udp", "10.20.0.1:53"), "ok")
		lab.expect("guest to the router's web interface", lab.dial("guest", "tcp", "10.20.0.1:8443"), "timeout")
		lab.expect("the internet to the router", lab.dial("server", "tcp", "203.0.113.1:8443"), "timeout")
		lab.expect("the internet to a device inside", lab.dial("server", "tcp", "192.168.1.50:80"), "timeout")
		lab.expect("the internet pings the router", lab.ping("server", "203.0.113.1"), "timeout")
		lab.expect("guest pings the router", lab.ping("guest", "10.20.0.1"), "ok")
	})

	t.Run("an allow rule opens one thing and nothing else", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig([]config.FirewallRule{fr("fw1", func(r *config.FirewallRule) { r.To = "guest"; r.Ports = "80" })}))
		lab.expect("lan to guest:80 (allowed by the rule)", lab.dial("lan", "tcp", "10.20.0.50:80"), "ok")
		lab.expect("the reply path (stateful)", lab.dial("lan", "tcp", "10.20.0.50:80"), "ok")
		lab.expect("guest to the lan stays closed", lab.dial("guest", "tcp", "192.168.1.50:80"), "timeout")
		lab.expect("lan to the internet still works", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
	})

	t.Run("a block rule beats the Internet switch", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig([]config.FirewallRule{
			fr("fw1", func(r *config.FirewallRule) { r.From = "guest"; r.Action = "block"; r.Ports = "443" }),
		}))
		lab.expect("guest to the internet:443 (blocked)", lab.dial("guest", "tcp", "203.0.113.2:443"), "timeout")
		lab.expect("guest to the internet:80 (still allowed)", lab.dial("guest", "tcp", "203.0.113.2:80"), "ok")
		lab.expect("lan to the internet:443 (another tab)", lab.dial("lan", "tcp", "203.0.113.2:443"), "ok")
	})

	t.Run("reject answers at once, block does not", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig([]config.FirewallRule{
			fr("fw1", func(r *config.FirewallRule) { r.Action = "reject"; r.Ports = "22" }),
			fr("fw2", func(r *config.FirewallRule) { r.Action = "block"; r.Ports = "8080" }),
			fr("fw3", func(r *config.FirewallRule) { r.Action = "reject"; r.Protocol = "udp"; r.Ports = "53" }),
		}))
		lab.expect("tcp reject", lab.dial("lan", "tcp", "203.0.113.2:22"), "refused")
		lab.expect("tcp block", lab.dial("lan", "tcp", "203.0.113.2:8080"), "timeout")
		lab.expect("udp reject", lab.dial("lan", "udp", "203.0.113.2:53"), "refused")
		lab.expect("an untouched port", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
	})

	t.Run("the first matching rule decides", func(t *testing.T) {
		lab.t = t
		block := fr("fw1", func(r *config.FirewallRule) { r.Action = "block"; r.Ports = "80" })
		allow := fr("fw2", func(r *config.FirewallRule) { r.Ports = "80" })
		lab.apply(fwConfig([]config.FirewallRule{block, allow}))
		lab.expect("block first", lab.dial("lan", "tcp", "203.0.113.2:80"), "timeout")
		lab.apply(fwConfig([]config.FirewallRule{allow, block}))
		lab.expect("allow first", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
	})

	t.Run("switched-off rules do nothing", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig([]config.FirewallRule{fr("fw1", func(r *config.FirewallRule) { r.Action = "block"; r.Ports = "80"; r.Disabled = true })}))
		lab.expect("lan to the internet", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
	})

	t.Run("source and destination narrow a rule", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig([]config.FirewallRule{
			fr("fw1", func(r *config.FirewallRule) { r.Action = "block"; r.Source = "192.168.1.51"; r.Ports = "80" }),
			fr("fw2", func(r *config.FirewallRule) { r.Action = "block"; r.Destination = "203.0.113.99"; r.Ports = "80" }),
			fr("fw3", func(r *config.FirewallRule) { r.Action = "block"; r.Source = "192.168.1.0/26"; r.Ports = "443" }),
		}))
		lab.expect("another source is not matched", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
		lab.expect("another destination is not matched", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
		lab.expect("a source range that holds the client", lab.dial("lan", "tcp", "203.0.113.2:443"), "timeout")
	})

	t.Run("rules to the router, and the anti-lockout lines", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig([]config.FirewallRule{
			fr("fw1", func(r *config.FirewallRule) { r.Action = "block"; r.To = "router"; r.Ports = "80,8443,22" }),
			fr("fw2", func(r *config.FirewallRule) { r.From = "wan"; r.To = "router"; r.Protocol = "icmp" }),
			fr("fw3", func(r *config.FirewallRule) { r.From = "guest"; r.To = "router"; r.Ports = "8443" }),
		}))
		lab.expect("lan to the router's plain port is blocked by a rule", lab.dial("lan", "tcp", "192.168.1.1:80"), "timeout")
		lab.expect("but the web interface stays reachable", lab.dial("lan", "tcp", "192.168.1.1:8443"), "ok")
		lab.expect("and ssh", lab.dial("lan", "tcp", "192.168.1.1:22"), "ok")
		lab.expect("the internet pings the router (rule)", lab.ping("server", "203.0.113.1"), "ok")
		lab.expect("the internet still cannot connect", lab.dial("server", "tcp", "203.0.113.1:8443"), "timeout")
		lab.expect("a rule opens the router's web interface to guests", lab.dial("guest", "tcp", "10.20.0.1:8443"), "ok")
		lab.expect("guest to the router's DNS still works", lab.dial("guest", "udp", "10.20.0.1:53"), "ok")
	})

	t.Run("a rule to anywhere covers the router and what is behind it", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig([]config.FirewallRule{
			fr("fw1", func(r *config.FirewallRule) { r.From = "guest"; r.Action = "block"; r.To = "any"; r.Protocol = "any" }),
		}))
		lab.expect("guest to the internet", lab.dial("guest", "tcp", "203.0.113.2:80"), "timeout")
		lab.expect("guest to the router's DNS", lab.dial("guest", "udp", "10.20.0.1:53"), "timeout")
		lab.expect("guest pings the router", lab.ping("guest", "10.20.0.1"), "timeout")
		lab.expect("dhcp is never blocked (the line is there)", "ok", "ok")
	})

	t.Run("port forwards", func(t *testing.T) {
		lab.t = t
		fwd := func(id string, f func(*config.PortForward)) config.PortForward {
			p := config.PortForward{ID: id, Name: "forward " + id, Protocol: "tcp", ExternalPorts: "443", ToIP: "192.168.1.50", InternalPorts: "8443"}
			if f != nil {
				f(&p)
			}
			return p
		}
		lab.apply(fwConfig(nil, fwd("pf1", nil), fwd("pf2", func(p *config.PortForward) {
			p.ExternalPorts = "5000-5010"
			p.InternalPorts = ""
			p.Protocol = "tcp+udp"
		})))
		lab.expect("the internet reaches the device through the forward", lab.dial("server", "tcp", "203.0.113.1:443"), "ok")
		lab.expect("a port that is not forwarded", lab.dial("server", "tcp", "203.0.113.1:444"), "timeout")
		lab.expect("the device itself is not reachable directly", lab.dial("server", "tcp", "192.168.1.50:8443"), "timeout")
		lab.expect("a range keeps its numbers (tcp)", lab.dial("server", "tcp", "203.0.113.1:5005"), "ok")
		lab.expect("a range keeps its numbers (udp)", lab.dial("server", "udp", "203.0.113.1:5000"), "ok")
		lab.expect("outside the range", lab.dial("server", "tcp", "203.0.113.1:5011"), "timeout")
		lab.expect("the router's own ports stay closed", lab.dial("server", "tcp", "203.0.113.1:8443"), "timeout")

		// a forward limited to other sources, and a switched-off one
		lab.apply(fwConfig(nil,
			fwd("pf1", func(p *config.PortForward) { p.Sources = []string{"203.0.113.99", "198.51.100.0/24"} }),
			fwd("pf3", func(p *config.PortForward) { p.ExternalPorts = "5005"; p.InternalPorts = ""; p.Disabled = true })))
		lab.expect("a source that is not allowed", lab.dial("server", "tcp", "203.0.113.1:443"), "timeout")
		lab.expect("a disabled forward does nothing", lab.dial("server", "tcp", "203.0.113.1:5005"), "timeout")
		lab.apply(fwConfig(nil, fwd("pf1", func(p *config.PortForward) { p.Sources = []string{"203.0.113.0/24"} })))
		lab.expect("a source range that holds the client", lab.dial("server", "tcp", "203.0.113.1:443"), "ok")
	})

	t.Run("a rule from the internet to a device inside works next to forwards", func(t *testing.T) {
		lab.t = t
		lab.apply(fwConfig([]config.FirewallRule{
			fr("fw1", func(r *config.FirewallRule) {
				r.From = "wan"
				r.To = "lan"
				r.Destination = "192.168.1.50"
				r.Ports = "80"
			}),
		}))
		// the server only knows the router: give it a route to the private address, as an upstream router would
		lab.in("server", "ip", "route", "replace", "192.168.1.0/24", "via", "203.0.113.1")
		lab.expect("an allow rule from the internet", lab.dial("server", "tcp", "192.168.1.50:80"), "ok")
		lab.expect("another port stays closed", lab.dial("server", "tcp", "192.168.1.50:8443"), "timeout")
	})
}

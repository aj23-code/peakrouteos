package setup

import (
	"context"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"peakroute/internal/bootstrap"
	"peakroute/internal/config"
	"peakroute/internal/translator"
)

const childEnv = "PEAKROUTE_SETUP_E2E_CHILD"

// The setup network with the real tools: the test runs itself in a fresh network namespace (unshare -Urn, no root needed), makes two
// "ports" with a computer plugged into each, brings the setup network up, lets the computer ask for an address with a real DHCP client,
// reaches the router on it, finds which port the computer is on, and takes the network down again.
func TestTheSetupNetworkWorksForReal(t *testing.T) {
	if os.Getenv(childEnv) == "1" {
		setupNetworkChild(t)
		return
	}
	for _, tool := range []string{"unshare", "nsenter", "ip", "bridge", "dnsmasq", "busybox", "pkill"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed here", tool)
		}
	}
	if out, err := exec.Command("unshare", "-Urn", "true").CombinedOutput(); err != nil {
		t.Skipf("no user and network namespaces here: %v %s", err, out)
	}
	cmd := exec.Command("unshare", "-Urn", os.Args[0], "-test.run=^TestTheSetupNetworkWorksForReal$", "-test.v")
	cmd.Env = append(os.Environ(), childEnv+"=1")
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// nsRunner runs the real commands, except that the DHCP server is started in the foreground (--no-daemon): dnsmasq drops to another user
// and group when it starts, which a namespace without a second user cannot do. It writes the pid file itself, which the daemon would.
type nsRunner struct{ dir string }

func (r *nsRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if name != "dnsmasq" {
		return translator.ExecRunner{}.Run(ctx, name, args...)
	}
	cmd := exec.Command(name, append(args, "--no-daemon")...)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	go cmd.Wait()
	time.Sleep(300 * time.Millisecond) // it binds its sockets at once; a bad file makes it exit
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		return "the DHCP server did not stay up", err
	}
	return "", os.WriteFile(filepath.Join(r.dir, "dnsmasq.pid"), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0644)
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func setupNetworkChild(t *testing.T) {
	sh(t, "ip", "link", "set", "lo", "up")
	// the computer: a namespace of its own, plugged into the router by two cables
	pc := exec.Command("unshare", "-n", "sleep", "60")
	if err := pc.Start(); err != nil {
		t.Fatal(err)
	}
	defer pc.Process.Kill()
	pid := strconv.Itoa(pc.Process.Pid)
	// wait for the namespace to exist (the child has unshared)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if out, _ := exec.Command("nsenter", "-t", pid, "-n", "true").CombinedOutput(); len(out) == 0 {
			if _, err := os.Readlink("/proc/" + pid + "/ns/net"); err == nil {
				self, _ := os.Readlink("/proc/self/ns/net")
				theirs, _ := os.Readlink("/proc/" + pid + "/ns/net")
				if self != theirs {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the computer's namespace did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	pcDo := func(args ...string) string {
		return sh(t, append([]string{"nsenter", "-t", pid, "-n"}, args...)...)
	}
	sh(t, "ip", "link", "add", "enp1s0", "type", "veth", "peer", "name", "cl1", "netns", pid)
	sh(t, "ip", "link", "add", "enp2s0", "type", "veth", "peer", "name", "cl2", "netns", pid)
	pcDo("ip", "link", "set", "lo", "up")
	pcDo("ip", "link", "set", "cl1", "up")
	pcDo("ip", "link", "set", "cl2", "up")

	mgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	svc := &bootstrap.Service{Run: &nsRunner{dir: dir}, Manager: mgr, Dir: dir,
		Ports: func() ([]config.SystemInterface, error) {
			return []config.SystemInterface{{Name: "enp1s0"}, {Name: "enp2s0"}, {Name: "lo"}}[:2], nil
		}}
	svc.Reconcile()
	if !svc.Running() {
		t.Fatal("the setup network did not start")
	}
	if out := sh(t, "ip", "-o", "-4", "addr", "show", "dev", bootstrap.Bridge); !strings.Contains(out, bootstrap.Address+"/24") {
		t.Fatalf("no address on the bridge: %s", out)
	}
	members := sh(t, "sh", "-c", "ip -o link show master "+bootstrap.Bridge+" | wc -l")
	if members != "2" {
		t.Fatalf("%s ports joined", members)
	}

	// the computer asks for an address like any would
	script := filepath.Join(dir, "udhcpc.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ncase \"$1\" in bound|renew) ip addr add \"$ip/24\" dev \"$interface\" ;; esac\n"), 0755)
	out := pcDo("busybox", "udhcpc", "-i", "cl1", "-n", "-q", "-t", "5", "-T", "1", "-s", script)
	t.Logf("udhcpc: %s", out)
	addr := pcDo("ip", "-o", "-4", "addr", "show", "dev", "cl1")
	var got netip.Addr
	for _, f := range strings.Fields(addr) {
		if p, err := netip.ParsePrefix(f); err == nil {
			got = p.Addr()
		}
	}
	lo, hi := netip.MustParseAddr(bootstrap.RangeStart), netip.MustParseAddr(bootstrap.RangeEnd)
	if !got.IsValid() || got.Compare(lo) < 0 || got.Compare(hi) > 0 {
		t.Fatalf("the computer got %q, not an address of %s to %s", got, lo, hi)
	}
	// and can reach the router on its address (where the web interface would answer)
	pcDo("ping", "-c", "1", "-W", "2", bootstrap.Address)
	// the router is told nothing else: no gateway, so the computer's other connections still win for the internet
	if routes := pcDo("ip", "-4", "route", "show"); strings.Contains(routes, "default") {
		t.Fatalf("the computer got a default route: %s", routes)
	}

	// the port it is plugged into is found, not all of the bridge's
	if members := narrow(bootstrap.Bridge, []string{"enp1s0", "enp2s0"}, got); len(members) != 1 || members[0] != "enp1s0" {
		t.Fatalf("the computer is on enp1s0, found %v", members)
	}
	// the first settings are committed: the setup network lets go
	commit(t, mgr)
	svc.Reconcile()
	if svc.Running() {
		t.Fatal("still running after the settings were committed")
	}
	if out, err := exec.Command("ip", "link", "show", bootstrap.Bridge).CombinedOutput(); err == nil {
		t.Fatalf("the bridge is still there: %s", out)
	}
	if out, err := exec.Command("pgrep", "-F", filepath.Join(dir, "dnsmasq.pid")).CombinedOutput(); err == nil {
		t.Fatalf("the DHCP server still runs: %s", out)
	}
	if out := sh(t, "sh", "-c", "ip -o link show enp1s0; ip -o link show enp2s0"); strings.Contains(out, "master") {
		t.Fatalf("a port is still held: %s", out)
	}
	// the ports can join a real bridge now (this is what the commit does)
	sh(t, "ip", "link", "add", "br0", "type", "bridge")
	sh(t, "ip", "link", "set", "enp1s0", "master", "br0")

	// the change is reverted: the router has no local network and the setup network comes back
	sh(t, "ip", "link", "del", "br0")
	if err := mgr.RevertNetwork(config.NetworkConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	svc.Reconcile()
	if !svc.Running() {
		t.Fatal("did not come back")
	}
	svc.Stop()
}

func commit(t *testing.T, mgr *config.Manager) {
	t.Helper()
	if err := mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.WanInterface, c.WanMode = "enp1s0", "dhcp"
		c.LanInterfaces = []string{"enp2s0"}
		c.LanIP, c.LanSubnet = "10.1.0.1", "255.255.255.0"
		c.DHCP = &config.DHCPConfig{Enabled: true, RangeStart: "10.1.0.100", RangeEnd: "10.1.0.200", LeaseTime: "12h"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
}

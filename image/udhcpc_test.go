package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The access point's DHCP client script runs here with ip and logger replaced by recorders.

type dhcpLab struct {
	t                       *testing.T
	dir, calls, run, resolv string
}

func newDHCPLab(t *testing.T) *dhcpLab {
	t.Helper()
	need(t, "sh", "sed")
	dir := t.TempDir()
	l := &dhcpLab{t: t, dir: dir, calls: filepath.Join(dir, "calls"), run: filepath.Join(dir, "run"), resolv: filepath.Join(dir, "resolv.conf")}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ip", "logger"} {
		script := "#!/bin/sh\necho \"" + name + " $*\" >> " + l.calls + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// event runs the script for one udhcpc event and returns the ip commands it ran.
func (l *dhcpLab) event(ev string, vars ...string) []string {
	return l.eventOf("udhcpc-script", ev, vars...)
}

func (l *dhcpLab) eventOf(script, ev string, vars ...string) []string {
	l.t.Helper()
	_ = os.Remove(l.calls)
	cmd := exec.Command("sh", "overlay/usr/lib/peakroute/"+script, ev)
	cmd.Env = append([]string{"PATH=" + filepath.Join(l.dir, "bin") + ":/usr/bin:/bin", "PEAKROUTE_RUN=" + l.run,
		"PEAKROUTE_RESOLV_CONF=" + l.resolv, "PEAKROUTE_RESOLVCONF=/nonexistent", "interface=br0"}, vars...)
	if out, err := cmd.CombinedOutput(); err != nil {
		l.t.Fatalf("%s: %v\n%s", ev, err, out)
	}
	b, _ := os.ReadFile(l.calls)
	var ips []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, "ip ") {
			ips = append(ips, line)
		}
	}
	return ips
}

func TestTheDHCPScriptLeavesTheRecoveryAddressAlone(t *testing.T) {
	l := newDHCPLab(t)
	got := l.event("deconfig")
	if strings.Join(got, "\n") != "ip link set br0 up" {
		t.Fatalf("deconfig with nothing leased must only bring the link up (no flush): %q", got)
	}
	got = l.event("bound", "ip=192.168.1.50", "mask=24", "router=192.168.1.1 192.168.1.254", "dns=192.168.1.1 1.1.1.1", "domain=lan")
	want := "ip -4 addr replace 192.168.1.50/24 dev br0\nip -4 route replace default via 192.168.1.1 dev br0"
	if strings.Join(got, "\n") != want {
		t.Fatalf("bound: %q", got)
	}
	if b, _ := os.ReadFile(l.resolv); string(b) != "search lan\nnameserver 192.168.1.1\nnameserver 1.1.1.1\n" {
		t.Fatalf("resolv.conf: %q", b)
	}
	// a new address: the old one goes, nothing else
	got = l.event("renew", "ip=192.168.1.60", "mask=24", "router=192.168.1.1")
	want = "ip -4 addr del 192.168.1.50/24 dev br0\nip -4 route del default via 192.168.1.1 dev br0\n" +
		"ip -4 addr replace 192.168.1.60/24 dev br0\nip -4 route replace default via 192.168.1.1 dev br0"
	if strings.Join(got, "\n") != want {
		t.Fatalf("renew with a new address: %q", got)
	}
	// the lease is lost: only what the script set is taken away
	got = l.event("deconfig")
	want = "ip link set br0 up\nip -4 addr del 192.168.1.60/24 dev br0\nip -4 route del default via 192.168.1.1 dev br0"
	if strings.Join(got, "\n") != want {
		t.Fatalf("deconfig after a lease: %q", got)
	}
	for _, c := range got {
		if strings.Contains(c, "flush") || strings.Contains(c, "192.168.77.1") {
			t.Fatalf("touched the recovery address: %q", c)
		}
	}
}

// The router's script for two internet connections (docs/wan-failover.md): each default route has its own metric, and a connection
// that is told so does not touch the resolver file.
func TestTheTwoConnectionScriptGivesEachDefaultRouteItsMetricAndMergesBothConnectionsResolvers(t *testing.T) {
	l := newDHCPLab(t)
	run := func(ev string, vars ...string) string {
		return strings.Join(l.eventOf("udhcpc-wan", ev, vars...), "\n")
	}
	got := run("bound", "interface=eth0", "IF_METRIC=100", "ip=203.0.113.10", "mask=24", "router=203.0.113.1", "dns=203.0.113.1", "domain=isp-a")
	want := "ip -4 addr replace 203.0.113.10/24 dev eth0\nip -4 route replace default via 203.0.113.1 dev eth0 metric 100"
	if got != want {
		t.Fatalf("first connection: %q", got)
	}
	if b, _ := os.ReadFile(l.resolv); string(b) != "domain isp-a\nnameserver 203.0.113.1\n" {
		t.Fatalf("resolv.conf: %q", b)
	}
	// the second connection: its own metric, and the resolver file stays as the first left it
	got = run("bound", "interface=eth1", "IF_METRIC=200", "IF_PEAKROUTE_RESOLV=no", "ip=198.51.100.7", "mask=24", "router=198.51.100.1", "dns=198.51.100.1")
	want = "ip -4 addr replace 198.51.100.7/24 dev eth1\nip -4 route replace default via 198.51.100.1 dev eth1 metric 200"
	if got != want {
		t.Fatalf("second connection: %q", got)
	}
	// the second connection's resolvers are added after the first's (the one that carries the traffic first), the domain stays the first's
	if b, _ := os.ReadFile(l.resolv); string(b) != "domain isp-a\nnameserver 203.0.113.1\nnameserver 198.51.100.1\n" {
		t.Fatalf("resolv.conf with both: %q", b)
	}
	// a renewal with nothing changed removes nothing; a new gateway or metric replaces the old route
	if got = run("renew", "interface=eth1", "IF_METRIC=200", "IF_PEAKROUTE_RESOLV=no", "ip=198.51.100.7", "mask=24", "router=198.51.100.1"); strings.Contains(got, "route del") {
		t.Fatalf("renewal: %q", got)
	}
	got = run("renew", "interface=eth1", "IF_METRIC=100", "IF_PEAKROUTE_RESOLV=no", "ip=198.51.100.7", "mask=24", "router=198.51.100.254")
	if !strings.Contains(got, "route del default via 198.51.100.1 dev eth1 metric 200") || !strings.Contains(got, "route replace default via 198.51.100.254 dev eth1 metric 100") {
		t.Fatalf("a new gateway and metric: %q", got)
	}
	// the lease is lost: its own route goes, and the address
	got = run("deconfig", "interface=eth1")
	if !strings.Contains(got, "route del default via 198.51.100.254 dev eth1 metric 100") || !strings.Contains(got, "addr flush dev eth1") {
		t.Fatalf("deconfig: %q", got)
	}
	// and what it told goes with it: the first connection's resolvers are what is left
	if b, _ := os.ReadFile(l.resolv); string(b) != "domain isp-a\nnameserver 203.0.113.1\n" {
		t.Fatalf("resolv.conf after the second connection's lease is gone: %q", b)
	}
	// without a metric (a stanza that has none) the route is as Debian's script makes it
	got = run("bound", "interface=eth2", "ip=192.0.2.5", "mask=24", "router=192.0.2.1")
	if !strings.HasSuffix(got, "route replace default via 192.0.2.1 dev eth2 metric 0") {
		t.Fatalf("no metric: %q", got)
	}
}

// With the real kernel: two internet connections, each default route set by the script with its metric. The preferred one carries
// the traffic, the other takes over when the first one's route goes, and comes back when it is set again.
func TestTwoDefaultRoutesInTheRealKernelPreferTheLowerMetricAndFailOverWhenItGoes(t *testing.T) {
	need(t, "unshare", "ip", "sh")
	dir := t.TempDir()
	script, err := filepath.Abs("overlay/usr/lib/peakroute/udhcpc-wan")
	if err != nil {
		t.Fatal(err)
	}
	sh := `
set -e
export PEAKROUTE_RUN=` + dir + `/run PEAKROUTE_RESOLV_CONF=` + dir + `/resolv.conf PEAKROUTE_RESOLVCONF=/nonexistent
ip link add wan1 type dummy; ip link add wan2 type dummy
ip link set wan1 up; ip link set wan2 up
dhcp() { # interface metric ip router
    env interface="$1" IF_METRIC="$2" ip="$3" mask=24 router="$4" dns="$4" sh ` + script + ` bound
}
dhcp wan1 100 203.0.113.10 203.0.113.1
dhcp wan2 200 198.51.100.7 198.51.100.1
echo "both: $(ip -4 route show default | tr '\n' ';')"
echo "via: $(ip -4 route get 1.1.1.1 | head -1)"
ip -4 route del default via 203.0.113.1 dev wan1 metric 100
echo "first gone: $(ip -4 route get 1.1.1.1 | head -1)"
dhcp wan1 100 203.0.113.10 203.0.113.1
echo "first back: $(ip -4 route get 1.1.1.1 | head -1)"
# a renewal that changes nothing keeps both routes
dhcp wan2 200 198.51.100.7 198.51.100.1
echo "after renewal: $(ip -4 route show default | wc -l)"
`
	out, err := exec.Command("unshare", "-rn", "sh", "-c", sh).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Cannot find device") {
			t.Skipf("cannot make interfaces here: %s", out)
		}
		t.Fatalf("%v\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"both: default via 203.0.113.1 dev wan1 metric 100 ;default via 198.51.100.1 dev wan2 metric 200 ;",
		"via: 1.1.1.1 via 203.0.113.1 dev wan1",
		"first gone: 1.1.1.1 via 198.51.100.1 dev wan2",
		"first back: 1.1.1.1 via 203.0.113.1 dev wan1",
		"after renewal: 2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

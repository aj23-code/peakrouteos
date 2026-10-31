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
	l.t.Helper()
	_ = os.Remove(l.calls)
	cmd := exec.Command("sh", "overlay/usr/lib/peakroute/udhcpc-script", ev)
	cmd.Env = append([]string{"PATH=" + filepath.Join(l.dir, "bin") + ":/usr/bin:/bin", "PEAKROUTE_RUN=" + l.run,
		"PEAKROUTE_RESOLV_CONF=" + l.resolv, "interface=br0"}, vars...)
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

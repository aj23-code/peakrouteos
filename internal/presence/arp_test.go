package presence

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestARPRequestFrame(t *testing.T) {
	mac := net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55}
	f := arpRequest(mac, netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("192.168.1.10"))
	want := "ffffffffffff" + "021122334455" + "0806" + "0001" + "0800" + "06" + "04" + "0001" +
		"021122334455" + "c0a80101" + "000000000000" + "c0a8010a"
	if got := hexOf(f); got != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(digits[c>>4])
		sb.WriteByte(digits[c&15])
	}
	return sb.String()
}

func reply(op byte) []byte {
	f := arpRequest(net.HardwareAddr{0xAA, 0xBB, 0xCC, 0, 0, 1}, netip.MustParseAddr("192.168.1.10"), netip.MustParseAddr("192.168.1.1"))
	f[21] = op
	return f
}

func TestParseARPReply(t *testing.T) {
	mac, ip, ok := parseARPReply(reply(2))
	if !ok || mac != "aa:bb:cc:00:00:01" || ip != netip.MustParseAddr("192.168.1.10") {
		t.Fatalf("%v %v %v", mac, ip, ok)
	}
	if _, _, ok := parseARPReply(reply(1)); ok {
		t.Fatal("a request is not an answer")
	}
	bad := func(i int, v byte) []byte { f := reply(2); f[i] = v; return f }
	for name, f := range map[string][]byte{
		"short":        reply(2)[:41],
		"not ARP":      bad(13, 0x00),
		"not ethernet": bad(15, 6),
		"not IPv4":     bad(17, 0x06),
		"hw length":    bad(18, 8),
		"proto length": bad(19, 16),
		"op high":      bad(20, 1),
	} {
		if _, _, ok := parseARPReply(f); ok {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestRoutePicksTheInterfaceOnTheDevicesSubnet(t *testing.T) {
	mac := func(b byte) net.HardwareAddr { return net.HardwareAddr{2, 0, 0, 0, 0, b} }
	pfx := netip.MustParsePrefix
	ifaces := []ifaceInfo{
		{Index: 1, MAC: mac(1), Up: true, Addrs: []netip.Prefix{pfx("127.0.0.1/8")}},
		{Index: 2, MAC: mac(2), Up: false, Addrs: []netip.Prefix{pfx("192.168.1.1/24")}}, // down
		{Index: 3, MAC: mac(3), Up: true, Addrs: []netip.Prefix{pfx("fe80::1/64"), pfx("192.168.1.1/24")}},
		{Index: 4, MAC: mac(4), Up: true, Addrs: []netip.Prefix{pfx("10.20.0.1/24")}},
		{Index: 5, Up: true, Addrs: []netip.Prefix{pfx("10.30.0.1/24")}}, // no hardware address
	}
	ifc, src, ok := route(ifaces, netip.MustParseAddr("192.168.1.10"))
	if !ok || ifc.Index != 3 || src != netip.MustParseAddr("192.168.1.1") {
		t.Fatalf("%+v %v %v", ifc, src, ok)
	}
	if ifc, _, ok := route(ifaces, netip.MustParseAddr("10.20.0.55")); !ok || ifc.Index != 4 {
		t.Fatalf("%+v", ifc)
	}
	for _, dst := range []string{"172.16.0.5", "10.30.0.5", "192.168.1.1"} { // not attached / no MAC / our own address
		if _, _, ok := route(ifaces, netip.MustParseAddr(dst)); ok {
			t.Errorf("%s routed", dst)
		}
	}
}

// The raw-socket probe is tried against real interfaces: the test runs itself inside a throw-away network
// namespace (unshare -rn needs no real privileges) with two ends of a virtual cable, one of which owns the
// address being looked for.
func TestARPProberOnARealInterface(t *testing.T) {
	if os.Getenv("PEAKROUTE_TEST_NETNS") == "1" {
		probeInsideNamespace(t)
		return
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skip("no unshare")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("no ip")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-rn", exe, "-test.run=^TestARPProberOnARealInterface$", "-test.v")
	cmd.Env = append(os.Environ(), "PEAKROUTE_TEST_NETNS=1")
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "SKIP-NS") {
		t.Skip("cannot create a network namespace here: " + string(out))
	}
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("%s", out)
	}
}

func probeInsideNamespace(t *testing.T) {
	skip := "SKIP-NS " // only a failure to get a namespace at all skips the test; later failures are real
	sh := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s%s %v: %v %s", skip, name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// the other end of the cable lives in a namespace of its own: in the same one the kernel would drop the
	// question as coming from one of its own addresses
	peer := exec.Command("unshare", "-n", "sleep", "30")
	if err := peer.Start(); err != nil {
		t.Fatalf("%s%v", skip, err)
	}
	defer peer.Process.Kill()
	time.Sleep(300 * time.Millisecond) // it has switched namespace by now
	pid := fmt.Sprint(peer.Process.Pid)
	inPeer := func(args ...string) string { return sh("nsenter", append([]string{"-t", pid, "-n"}, args...)...) }

	sh("ip", "link", "add", "v0", "type", "veth", "peer", "name", "v1")
	skip = ""
	sh("ip", "link", "set", "v1", "netns", pid)
	sh("ip", "addr", "add", "10.9.0.1/24", "dev", "v0")
	sh("ip", "link", "set", "v0", "up")
	inPeer("ip", "addr", "add", "10.9.0.2/24", "dev", "v1")
	inPeer("ip", "link", "set", "v1", "up")
	peerMAC := ""
	for i, f := range strings.Fields(inPeer("ip", "-o", "link", "show", "v1")) {
		if f == "link/ether" {
			peerMAC = strings.Fields(inPeer("ip", "-o", "link", "show", "v1"))[i+1]
		}
	}
	if peerMAC == "" {
		t.Fatal("no hardware address for the far end")
	}
	time.Sleep(300 * time.Millisecond) // the links come up

	p := ARPProber{Attempts: 3, Wait: 400 * time.Millisecond}
	here := Target{MAC: strings.ToLower(peerMAC), IP: netip.MustParseAddr("10.9.0.2")}
	nobody := Target{MAC: "02:00:00:00:99:99", IP: netip.MustParseAddr("10.9.0.77")}
	liar := Target{MAC: "02:00:00:00:88:88", IP: netip.MustParseAddr("10.9.0.2")} // right address, other device
	elsewhere := Target{MAC: "02:00:00:00:77:77", IP: netip.MustParseAddr("172.16.5.5")}

	got, err := p.Probe(context.Background(), []Target{here, nobody, liar, elsewhere})
	if err != nil {
		t.Fatal(err)
	}
	if !got[here.MAC] {
		t.Errorf("the device that is there did not answer: %v", got)
	}
	if got[nobody.MAC] || got[liar.MAC] || got[elsewhere.MAC] {
		t.Errorf("answers from devices that are not there: %v", got)
	}

	// cancelled: stops at once
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := (ARPProber{Attempts: 5, Wait: 2 * time.Second}).Probe(ctx, []Target{nobody}); err == nil || time.Since(start) > time.Second {
		t.Errorf("a cancelled probe must stop quickly with an error: %v after %v", err, time.Since(start))
	}
}

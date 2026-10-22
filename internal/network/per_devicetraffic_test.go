package network

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCaptureTargetsAreTheLanSideInterfacesOnly(t *testing.T) {
	devs := []string{"br0", "br20", "br0", "enp1s0", "lo", "wg0", "", "brsetup"}
	got := captureTargets(devs, []string{"enp1s0", "enp1s0", "ppp0"})
	want := []string{"br0", "br20", "brsetup"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets %v, want %v", got, want)
	}
}

func TestTheArpTableGivesClientsAndTheirInterfaces(t *testing.T) {
	arp := filepath.Join(t.TempDir(), "arp")
	os.WriteFile(arp, []byte(`IP address       HW type     Flags       HW address            Mask     Device
192.168.1.119    0x1         0x2         f8:94:C2:a4:13:a7     *        br0
192.168.1.120    0x1         0x0         00:00:00:00:00:00     *        br0
10.20.0.5        0x1         0x2         48:b0:2d:f1:fe:ca     *        br20
`), 0644)
	tr := &TrafficTracker{arpPath: arp, ipToMac: map[string]string{}}
	devs := tr.updateARPTable()
	if !reflect.DeepEqual(devs, []string{"br0", "br20"}) {
		t.Errorf("devices %v", devs)
	}
	if tr.ipToMac["192.168.1.119"] != "f8:94:c2:a4:13:a7" || len(tr.ipToMac) != 2 {
		t.Errorf("clients %v", tr.ipToMac)
	}
}

// The tracker on a real interface: a client at the other end of a virtual cable is found in the ARP table, a capture starts on the
// interface the table names, and what the client sends and receives is counted for its MAC address.
func TestTheTrackerCountsAClientsTrafficForReal(t *testing.T) {
	if os.Getenv("PEAKROUTE_TEST_NETNS") == "1" {
		trackInsideNamespace(t)
		return
	}
	for _, tool := range []string{"unshare", "nsenter", "ip", "ping"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
		t.Skipf("no user and network namespaces here (a restricted runner or container): %v %s", err, out)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("unshare", "-rn", exe, "-test.run=^TestTheTrackerCountsAClientsTrafficForReal$", "-test.v")
	cmd.Env = append(os.Environ(), "PEAKROUTE_TEST_NETNS=1")
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "SKIP-NS") {
		t.Skip("cannot create a network namespace here: " + string(out))
	}
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func trackInsideNamespace(t *testing.T) {
	skip := "SKIP-NS "
	sh := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s%s %v: %v %s", skip, name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	peer := exec.Command("unshare", "-n", "sleep", "30")
	if err := peer.Start(); err != nil {
		t.Fatalf("%s%v", skip, err)
	}
	defer peer.Process.Kill()
	time.Sleep(300 * time.Millisecond)
	pid := fmt.Sprint(peer.Process.Pid)
	inPeer := func(args ...string) string { return sh("nsenter", append([]string{"-t", pid, "-n"}, args...)...) }

	sh("ip", "link", "add", "v0", "type", "veth", "peer", "name", "v1")
	skip = ""
	sh("ip", "link", "set", "v1", "netns", pid)
	sh("ip", "addr", "add", "10.9.0.1/24", "dev", "v0")
	sh("ip", "link", "set", "v0", "up")
	inPeer("ip", "addr", "add", "10.9.0.2/24", "dev", "v1")
	inPeer("ip", "link", "set", "v1", "up")
	fields := strings.Fields(inPeer("ip", "-o", "link", "show", "v1"))
	peerMAC := ""
	for i, f := range fields {
		if f == "link/ether" {
			peerMAC = fields[i+1]
		}
	}
	time.Sleep(300 * time.Millisecond)
	sh("ping", "-c", "1", "-W", "2", "10.9.0.2") // the client is in the ARP table now

	tr := &TrafficTracker{macBytes: map[string]uint64{}, ipToMac: map[string]string{}, capture: map[string]bool{}, arpPath: "/proc/net/arp"}
	tr.refresh()
	time.Sleep(500 * time.Millisecond) // the capture is open
	sh("ping", "-c", "3", "-i", "0.2", "-W", "2", "10.9.0.2")
	time.Sleep(300 * time.Millisecond)

	// 3 requests and 3 replies of at least 98 bytes each
	if got := tr.GetBytesForMAC(strings.ToUpper(peerMAC)); got < 6*98 {
		t.Errorf("counted %d bytes for the client %s, want at least %d", got, peerMAC, 6*98)
	}
}

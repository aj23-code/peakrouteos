package translator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
	"peakroute/internal/network"
)

// The ruleset's counters and the daemon's reading of them, with a real kernel and real packets: a device on the default network opens
// connections to the "internet" and what it sent and received is found under its hardware address. (The three namespaces and the
// router are the lab of the firewall tests.)
func TestTheUsageCountersCountRealTraffic(t *testing.T) {
	if os.Getenv(envRouter) != "1" {
		for _, tool := range []string{"unshare", "nsenter", "nft", "ip"} {
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
		cmd := exec.Command("unshare", "-rn", exe, "-test.run=^TestTheUsageCountersCountRealTraffic$", "-test.v")
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
	lab.listen("server", "tcp 80")
	// the guest network may use the internet, except for port 80: what the rules drop must not be counted for the device
	block := fr("fw1", func(r *config.FirewallRule) { r.From = "guest"; r.Action = "block"; r.Ports = "80" })
	lab.apply(fwConfig([]config.FirewallRule{block}))

	macOf := func(ns, dev string) string {
		f := strings.Fields(lab.in(ns, "ip", "-o", "link", "show", dev))
		for i, w := range f {
			if w == "link/ether" && i+1 < len(f) {
				return strings.ToLower(f[i+1])
			}
		}
		t.Fatalf("no hardware address for %s in %s", dev, ns)
		return ""
	}
	lanMAC, guestMAC := macOf("lan", "p1"), macOf("guest", "p2")

	tr := network.NewUsageTracker(ExecRunner{}, "")
	tr.Poll(t.Context())
	if tr.GetBytesForMAC(lanMAC) != 0 {
		t.Fatal("bytes before anything was sent")
	}
	for i := 0; i < 3; i++ {
		lab.expect("the lan device reaches the internet", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
	}
	lab.expect("the guest device is refused", lab.dial("guest", "tcp", "203.0.113.2:80"), "timeout")

	tr.Poll(t.Context())
	lan := tr.GetBytesForMAC(lanMAC)
	// three connections: at least the three-way handshakes and the data, both ways, of 60 bytes and more a packet
	if lan < 3*2*60 {
		t.Errorf("the lan device is credited %d bytes, want at least %d", lan, 3*2*60)
	}
	if guest := tr.GetBytesForMAC(guestMAC); guest != 0 {
		t.Errorf("the guest device is credited %d bytes for traffic the firewall dropped", guest)
	}

	// a second look adds only what happened since
	before := tr.GetBytesForMAC(lanMAC)
	tr.Poll(t.Context())
	if got := tr.GetBytesForMAC(lanMAC); got != before {
		t.Errorf("looking again with nothing new changed the total: %d -> %d", before, got)
	}
	lab.expect("one more connection", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
	tr.Poll(t.Context())
	if got := tr.GetBytesForMAC(lanMAC); got <= before {
		t.Errorf("a new connection did not raise the total: %d -> %d", before, got)
	}

	// a firewall file from an older version has no counting table (and a commit that rewrites it only comes with the next change): the
	// tracker loads the table itself, and counting goes on
	if out, err := exec.Command("nft", "delete", "table", "inet", network.UsageTable).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	cfg := fwConfig([]config.FirewallRule{block})
	late := network.NewUsageTracker(ExecRunner{}, "")
	loaded := 0
	late.Ensure = func(ctx context.Context) error {
		loaded++
		f := filepath.Join(t.TempDir(), "usage.nft")
		if err := os.WriteFile(f, []byte(AccountingRuleset(cfg)), 0600); err != nil {
			return err
		}
		out, err := ExecRunner{}.Run(ctx, "nft", "-f", f)
		if err != nil {
			t.Logf("nft -f: %s", out)
		}
		return err
	}
	late.Poll(t.Context()) // the table is missing: it is loaded here
	lab.expect("a connection with the table loaded by the tracker", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
	late.Poll(t.Context())
	if loaded != 1 || late.GetBytesForMAC(lanMAC) < 2*60 {
		t.Errorf("table loaded %d times; the lan device is credited %d bytes after it", loaded, late.GetBytesForMAC(lanMAC))
	}
	// loading the ruleset again starts the counters from zero; the total keeps growing from where it was
	total := tr.GetBytesForMAC(lanMAC)
	lab.apply(fwConfig([]config.FirewallRule{block}))
	lab.expect("after a reload", lab.dial("lan", "tcp", "203.0.113.2:80"), "ok")
	tr.Poll(t.Context())
	if got := tr.GetBytesForMAC(lanMAC); got <= total {
		t.Errorf("after the ruleset was loaded again the total did not grow: %d -> %d", total, got)
	}
}

package failover

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheTableMarksWhatArrivesAndSharesNewConnectionsByWeight(t *testing.T) {
	wans := map[string]string{"wan": "eth0", "wan2": "eth2"}
	both := map[string]bool{"wan": true, "wan2": true}
	text := lbRuleset([]string{"br0", "br20"}, wans, both, map[string]int{"wan": 1, "wan2": 3}, true)
	for _, want := range []string{
		"add table inet peakroute_lb\ndelete table inet peakroute_lb\n",
		`iifname "eth0" ct mark and 0xff000000 == 0 ct mark set ct mark and 0x00ffffff or 0x01000000`,
		`iifname "eth2" ct mark and 0xff000000 == 0 ct mark set ct mark and 0x00ffffff or 0x02000000`,
		`iifname { "br0", "br20" } ct mark and 0xff000000 == 0 ct state new jump pick`,
		"numgen random mod 4 vmap { 0 : jump to_wan, 1-3 : jump to_wan2 }",
		"meta mark set ct mark and 0xff000000",
		"type route hook output",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	// a connection that is down is not given new connections: the other takes them all
	one := lbRuleset([]string{"br0"}, wans, map[string]bool{"wan": true, "wan2": false}, map[string]int{"wan": 1, "wan2": 3}, true)
	if !strings.Contains(one, "chain pick {\n        jump to_wan\n") || strings.Contains(one, "numgen") {
		t.Errorf("one connection up:\n%s", one)
	}
	// both down: nothing is shared (the traffic takes the main table), but what arrives is still answered the same way
	none := lbRuleset([]string{"br0"}, wans, map[string]bool{}, map[string]int{"wan": 1, "wan2": 1}, true)
	if strings.Contains(none, "jump pick") || !strings.Contains(none, `iifname "eth0" ct mark`) {
		t.Errorf("both down:\n%s", none)
	}
	// failover: no sharing at all
	fo := lbRuleset([]string{"br0"}, wans, both, map[string]int{"wan": 1, "wan2": 1}, false)
	if strings.Contains(fo, "pick") || strings.Contains(fo, "numgen") || !strings.Contains(fo, `iifname "eth2" ct mark`) || !strings.Contains(fo, "meta mark set ct mark") {
		t.Errorf("failover:\n%s", fo)
	}
	// equal weights
	if eq := lbRuleset([]string{"br0"}, wans, both, map[string]int{"wan": 1, "wan2": 1}, true); !strings.Contains(eq, "numgen random mod 2 vmap { 0 : jump to_wan, 1 : jump to_wan2 }") {
		t.Errorf("%s", eq)
	}
}

func TestTheRealNftablesAcceptsTheBalancingTable(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/nft"); err != nil {
		t.Skip("nft is not installed")
	}
	wans := map[string]string{"wan": "eth0", "wan2": "eth2"}
	for name, text := range map[string]string{
		"shared, uneven": lbRuleset([]string{"br0", "br20"}, wans, map[string]bool{"wan": true, "wan2": true}, map[string]int{"wan": 1, "wan2": 7}, true),
		"one connection": lbRuleset([]string{"br0"}, wans, map[string]bool{"wan": false, "wan2": true}, map[string]int{"wan": 1, "wan2": 1}, true),
		"none up":        lbRuleset([]string{"br0"}, wans, map[string]bool{}, map[string]int{"wan": 1, "wan2": 1}, true),
		"failover":       lbRuleset([]string{"br0"}, wans, map[string]bool{"wan": true, "wan2": true}, map[string]int{"wan": 1, "wan2": 1}, false),
		"a PPPoE device": lbRuleset([]string{"br0"}, map[string]string{"wan": "ppp0", "wan2": "ppp1"}, map[string]bool{"wan": true, "wan2": true}, map[string]int{"wan": 2, "wan2": 3}, true),
		"no network":     lbRuleset(nil, wans, map[string]bool{"wan": true, "wan2": true}, map[string]int{"wan": 1, "wan2": 1}, true),
		"a heavy weight": lbRuleset([]string{"br0"}, wans, map[string]bool{"wan": true, "wan2": true}, map[string]int{"wan": 100, "wan2": 99}, true),
	} {
		path := filepath.Join(t.TempDir(), "lb.nft")
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("unshare", "-rn", "nft", "-c", "-f", path).CombinedOutput()
		if err != nil {
			if strings.Contains(string(out), "Operation not permitted") && !strings.Contains(string(out), "Error:") {
				t.Skipf("nft cannot check rules here: %s", out)
			}
			t.Errorf("%s: %v\n%s\n%s", name, err, out, text)
		}
	}
}

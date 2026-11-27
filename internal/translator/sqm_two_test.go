package translator

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

// a router with a second internet connection, each line with its own speeds
func shapedTwo(edit func(*config.NetworkConfig, *config.SQMConfig)) config.NetworkConfig {
	cfg := twoWanCfg(nil)
	s := config.SQMConfig{Enabled: true, DownloadKbit: 900000, UploadKbit: 40000, Link: config.SQMDocsis, PerHost: true,
		Second: &config.SQMLine{DownloadKbit: 50000, UploadKbit: 10000, Link: config.SQMVDSL}}
	if edit != nil {
		edit(&cfg, &s)
	}
	cfg.SQM = &s
	return cfg
}

func TestEachConnectionIsShapedToItsOwnSpeedsAndKind(t *testing.T) {
	file := content(t, shapedTwo(nil), PathSQM)
	for _, want := range []string{
		"SQM_DEV=eth0\nSQM_DOWN_KBIT=855000\nSQM_UP_KBIT=38000\nSQM_LINK=docsis\nSQM_PERHOST=1\n",
		"SQM2_DEV=eth2\nSQM2_DOWN_KBIT=47500\nSQM2_UP_KBIT=9500\nSQM2_LINK=vdsl\nSQM2_PERHOST=1\n",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("missing %q in\n%s", want, file)
		}
	}
	// one connection with the first's file as it always was
	one := content(t, shaped(nil), PathSQM)
	if strings.Contains(one, "SQM2") || !strings.Contains(one, "SQM_DEV=eth0\n") {
		t.Errorf("a router with one connection:\n%s", one)
	}
	// the second's settings mean nothing without a second connection
	cfg := shapedTwo(nil)
	cfg.Wan2 = nil
	if f := content(t, cfg, PathSQM); strings.Contains(f, "SQM2") {
		t.Errorf("no second connection, yet:\n%s", f)
	}
}

func TestOnlyTheSecondConnectionCanBeShaped(t *testing.T) {
	cfg := shapedTwo(func(c *config.NetworkConfig, s *config.SQMConfig) { s.DownloadKbit, s.UploadKbit = 0, 0 })
	file := content(t, cfg, PathSQM)
	if strings.Contains(file, "SQM_DEV") || !strings.Contains(file, "SQM2_DEV=eth2\n") {
		t.Errorf("\n%s", file)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a router shaping only its second connection is refused: %v", err)
	}
	// nothing at all: refused, as with one connection
	cfg = shapedTwo(func(c *config.NetworkConfig, s *config.SQMConfig) {
		s.DownloadKbit, s.UploadKbit = 0, 0
		s.Second.DownloadKbit, s.Second.UploadKbit = 0, 0
	})
	if err := cfg.Validate(); err == nil {
		t.Error("shaping with no speed on either line was accepted")
	}
}

func TestEachConnectionDividesItsOwnLineBetweenTheNetworks(t *testing.T) {
	cfg := shapedTwo(func(c *config.NetworkConfig, s *config.SQMConfig) {
		c.Networks = []config.Network{guestNet()}
		c.Networks[0].QoSPriority = config.QoSLow
		c.LanInterfaces = []string{"eth1", "eth3"}
	})
	file := content(t, cfg, PathSQM)
	first, second, ok := strings.Cut(file, "SQM2_DEV")
	if !ok || !strings.Contains(first, "SQM_CLASSES=\"") || !strings.Contains(second, "SQM2_CLASSES=\"") {
		t.Fatalf("\n%s", file)
	}
	// the shares are of each line's speeds: the first line's classes add up to its upload, the second's to its own
	rates := func(text, key string) []string {
		i := strings.Index(text, key+"=\"")
		block := text[i+len(key)+2:]
		block = block[:strings.Index(block, "\"")]
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(block), "\n") {
			f := strings.Fields(l)
			out = append(out, f[3]) // the upload rate
		}
		return out
	}
	r1, r2 := rates(first, "SQM_CLASSES"), rates(second, "SQM2_CLASSES")
	if len(r1) != len(r2) || strings.Join(r1, ",") == strings.Join(r2, ",") {
		t.Errorf("the lines have the same shares: %v %v", r1, r2)
	}
	// the firewall marks the networks' connections once, for both lines
	if nft := content(t, cfg, PathNftables); strings.Count(nft, "table inet peakroute_qos") != 1 {
		t.Errorf("the marking table is not there exactly once:\n%s", nft)
	}
	// and when only the second line is shaped, the marks are still made
	only := cfg
	s := *only.SQM
	s.DownloadKbit, s.UploadKbit = 0, 0
	only.SQM = &s
	if nft := content(t, only, PathNftables); !strings.Contains(nft, "table inet peakroute_qos") {
		t.Errorf("no marks with only the second line shaped:\n%s", nft)
	}
}

func TestTheSecondLinesSettingsHaveTheirOwnValidationAndReview(t *testing.T) {
	for name, edit := range map[string]func(*config.SQMConfig){
		"sqm.second.download_kbit": func(s *config.SQMConfig) { s.Second.DownloadKbit = config.MaxSQMKbit + 1 },
		"sqm.second.upload_kbit":   func(s *config.SQMConfig) { s.Second.UploadKbit = -1 },
		"sqm.second.link":          func(s *config.SQMConfig) { s.Second.Link = "carrier-pigeon" },
	} {
		cfg := shapedTwo(func(c *config.NetworkConfig, s *config.SQMConfig) { edit(s) })
		found := false
		if ve, ok := cfg.Validate().(config.ValidationError); ok {
			for _, e := range ve {
				found = found || e.Field == name
			}
		}
		if !found {
			t.Errorf("%s accepted", name)
		}
	}
	a, b := shapedTwo(nil), shapedTwo(func(c *config.NetworkConfig, s *config.SQMConfig) { s.Second.DownloadKbit = 100000 })
	if !a.SQMSettings().Same(a.SQMSettings()) || a.SQMSettings().Same(b.SQMSettings()) {
		t.Error("Same does not compare the second line")
	}
	changes := config.DiffNetwork(a, b)
	if len(changes) != 1 || changes[0].Field != "sqm" || !strings.Contains(changes[0].New, "second connection: download 100 Mbit/s") {
		t.Errorf("%+v", changes)
	}
}

func TestTheHelperShapesTwoRealInterfacesEachToItsOwnSpeed(t *testing.T) {
	for _, tool := range []string{"tc", "ip", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
		t.Skipf("no user and network namespaces here: %v %s", err, out)
	}
	helper, _ := filepath.Abs("../../image/overlay/usr/lib/peakroute/sqm")
	dir := t.TempDir()
	conf := filepath.Join(dir, "conf")
	state, state2 := filepath.Join(dir, "run", "sqm.dev"), filepath.Join(dir, "run", "sqm2.dev")
	script := `
set -e
export SQM_CONF=` + conf + ` SQM_STATE=` + state + ` SQM_STATE2=` + state2 + `
ip link add wan0 type dummy; ip link set wan0 up; ip link add wan1 type dummy; ip link set wan1 up
printf 'SQM_DEV=wan0\nSQM_DOWN_KBIT=20000\nSQM_UP_KBIT=10000\nSQM_LINK=docsis\nSQM_PERHOST=1\nSQM2_DEV=wan1\nSQM2_DOWN_KBIT=8000\nSQM2_UP_KBIT=2000\nSQM2_LINK=adsl\nSQM2_PERHOST=0\n' > ` + conf + `
echo "== both"; sh ` + helper + ` reconcile
echo "-- wan0"; tc qdisc show dev wan0; tc qdisc show dev ifb-wan
echo "-- wan1"; tc qdisc show dev wan1; tc qdisc show dev ifb-wan2
echo "== the second comes up again"; tc qdisc del dev wan1 root; sh ` + helper + ` start-if wan1; tc qdisc show dev wan1 | grep -c cake
echo "== only the second"; printf 'SQM2_DEV=wan1\nSQM2_DOWN_KBIT=8000\nSQM2_UP_KBIT=2000\nSQM2_LINK=adsl\nSQM2_PERHOST=0\n' > ` + conf + `; sh ` + helper + ` reconcile
echo "-- wan0"; tc qdisc show dev wan0 | grep -c cake || true; ip link show ifb-wan 2>&1 | head -1
echo "-- wan1"; tc qdisc show dev wan1 | grep -c cake
echo "== only the first"; printf 'SQM_DEV=wan0\nSQM_DOWN_KBIT=20000\nSQM_UP_KBIT=10000\n' > ` + conf + `; sh ` + helper + ` reconcile
echo "-- wan0"; tc qdisc show dev wan0 | grep -c cake
echo "-- wan1"; tc qdisc show dev wan1 | grep -c cake || true; ip link show ifb-wan2 2>&1 | head -1
echo "== off"; rm ` + conf + `; sh ` + helper + ` reconcile
echo "-- all"; tc qdisc show dev wan0 | grep -c cake || true; tc qdisc show dev wan1 | grep -c cake || true; ip link show ifb-wan 2>&1 | head -1; ip link show ifb-wan2 2>&1 | head -1
`
	out, err := exec.Command("unshare", "-rn", "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	s := string(out)
	section := func(name string) string {
		i := strings.Index(s, "== "+name+"\n")
		if i < 0 {
			t.Fatalf("no section %q in\n%s", name, s)
		}
		rest := s[i+len(name)+4:]
		if j := strings.Index(rest, "\n== "); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	both := section("both")
	first, second, _ := strings.Cut(both, "-- wan1")
	for _, want := range []string{"bandwidth 10Mbit", "dual-srchost", "overhead 18", "bandwidth 20Mbit", "dual-dsthost"} {
		if !strings.Contains(first, want) {
			t.Errorf("the first line: missing %q in\n%s", want, first)
		}
	}
	for _, want := range []string{"bandwidth 2Mbit", "triple-isolate", "atm", "bandwidth 8Mbit"} {
		if !strings.Contains(second, want) {
			t.Errorf("the second line: missing %q in\n%s", want, second)
		}
	}
	if strings.Contains(second, "bandwidth 10Mbit") || strings.Contains(first, "bandwidth 2Mbit") {
		t.Errorf("a line got the other's speed:\n%s", both)
	}
	if got := strings.TrimSpace(section("the second comes up again")); got != "1" {
		t.Errorf("the second line was not shaped again when it came up: %q", got)
	}
	onlySecond := section("only the second")
	if !strings.Contains(onlySecond, "-- wan0\n0\n") || !strings.Contains(onlySecond, "does not exist") || !strings.Contains(onlySecond, "-- wan1\n1") {
		t.Errorf("only the second: %q", onlySecond)
	}
	onlyFirst := section("only the first")
	if !strings.Contains(onlyFirst, "-- wan0\n1") || !strings.Contains(onlyFirst, "-- wan1\n0") || !strings.Contains(onlyFirst, "does not exist") {
		t.Errorf("only the first: %q", onlyFirst)
	}
	off := section("off")
	if !strings.Contains(off, "-- all\n0\n0\n") || strings.Count(off, "does not exist") != 2 {
		t.Errorf("not everything was taken away: %q", off)
	}
}

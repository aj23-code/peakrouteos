package translator

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func shaped(set func(*config.SQMConfig)) config.NetworkConfig {
	cfg := routerCfg()
	s := config.SQMConfig{Enabled: true, DownloadKbit: 900000, UploadKbit: 40000, Link: config.SQMDocsis, PerHost: true}
	if set != nil {
		set(&s)
	}
	cfg.SQM = &s
	return cfg
}

func TestNothingIsShapedUntilItIsSwitchedOn(t *testing.T) {
	for _, cfg := range []config.NetworkConfig{routerCfg(), shaped(func(s *config.SQMConfig) { s.Enabled = false })} {
		a, ok := file(t, cfg, PathSQM)
		if !ok || !a.Remove || a.Service != SvcSQM {
			t.Errorf("%+v", a)
		}
	}
	// a nothing-to-shape configuration is not a shaping one
	if _, ok := renderSQM(shaped(func(s *config.SQMConfig) { s.DownloadKbit, s.UploadKbit = 0, 0 })); ok {
		t.Error("two zero speeds are shaped")
	}
}

func TestTheShapingFileNamesTheConnectionAndWhatToDoWithIt(t *testing.T) {
	got := content(t, shaped(nil), PathSQM)
	for _, want := range []string{"SQM_DEV=eth0\n", "SQM_DOWN_KBIT=900000\n", "SQM_UP_KBIT=40000\n", "SQM_LINK=docsis\n", "SQM_PERHOST=1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if got := content(t, shaped(func(s *config.SQMConfig) { s.PerHost = false }), PathSQM); !strings.Contains(got, "SQM_PERHOST=0\n") {
		t.Errorf("%s", got)
	}
	// a PPPoE connection is shaped on the session's interface
	cfg := shaped(nil)
	cfg.WanMode = config.WanPPPoE
	cfg.WanPPPoE = &config.PPPoEConfig{Username: "u", Password: "p"}
	if got := content(t, cfg, PathSQM); !strings.Contains(got, "SQM_DEV=ppp0\n") {
		t.Errorf("%s", got)
	}
	// with a provider VLAN: the VLAN interface
	cfg = shaped(nil)
	cfg.WanVlan = 835
	if got := content(t, cfg, PathSQM); !strings.Contains(got, "SQM_DEV=eth0.835\n") {
		t.Errorf("%s", got)
	}
}

func TestSwitchingShapingOnAndOffRunsOnlyTheShapingAction(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, routerCfg()))
	plan, _ := PlanChanges(root, mustRender(t, shaped(nil)))
	if got := actionSummary(plan); got != "apply sqm" {
		t.Fatalf("on: %q (networking, the firewall and DHCP must not be touched)", got)
	}
	install(t, string(root), mustRender(t, shaped(nil)))
	if plan, _ = PlanChanges(root, mustRender(t, shaped(func(s *config.SQMConfig) { s.UploadKbit = 30000 }))); actionSummary(plan) != "apply sqm" {
		t.Fatalf("a new speed: %q", actionSummary(plan))
	}
	plan, _ = PlanChanges(root, mustRender(t, routerCfg()))
	if got := actionSummary(plan); got != "apply sqm" || len(plan.Files) != 1 || plan.Files[0].Kind != Remove {
		t.Fatalf("off: %q %+v", got, plan.Files)
	}
	cmd := serviceCommands[SvcSQM].String()
	for _, want := range []string{PathSQMGo + " reconcile", "not in this system image"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in %s", want, cmd)
		}
	}
}

func TestShapingHasItsOwnValidation(t *testing.T) {
	bad := map[string]func(*config.SQMConfig){
		"no speed":     func(s *config.SQMConfig) { s.DownloadKbit, s.UploadKbit = 0, 0 },
		"negative":     func(s *config.SQMConfig) { s.DownloadKbit = -5 },
		"over 10 Gbit": func(s *config.SQMConfig) { s.UploadKbit = 10_000_001 },
		"unknown line": func(s *config.SQMConfig) { s.Link = "carrier-pigeon" },
	}
	for name, set := range bad {
		if err := shaped(set).Validate(); err == nil || !strings.Contains(err.Error(), "sqm.") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// only one direction is fine, and a disabled setting with no speeds is fine
	if err := shaped(func(s *config.SQMConfig) { s.UploadKbit = 0 }).Validate(); err != nil {
		t.Errorf("download only: %v", err)
	}
	if err := shaped(func(s *config.SQMConfig) { s.Enabled, s.DownloadKbit, s.UploadKbit = false, 0, 0 }).Validate(); err != nil {
		t.Errorf("off: %v", err)
	}
}

func TestADSLOverEthernetAdvisesPickingTheKindOfLine(t *testing.T) {
	cfg := shaped(func(s *config.SQMConfig) { s.Link = config.SQMEthernet })
	cfg.WanMode = config.WanPPPoE
	cfg.WanPPPoE = &config.PPPoEConfig{Username: "u", Password: "p"}
	var found bool
	for _, n := range mustRender(t, cfg).Notes {
		found = found || strings.Contains(n, "PPPoE")
	}
	if !found {
		t.Error("no note")
	}
}

// The helper against a real kernel (a throw-away network namespace): the cake queue goes on the connection, the incoming side goes through
// an ifb, and taking the file away takes it all away.
func TestTheHelperShapesAndUnshapesARealInterface(t *testing.T) {
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
	conf, state := filepath.Join(dir, "conf"), filepath.Join(dir, "run", "sqm.dev")
	script := `
set -e
export SQM_CONF=` + conf + ` SQM_STATE=` + state + `
ip link add wan0 type dummy; ip link set wan0 up
echo "== no file"; sh ` + helper + ` reconcile
printf 'SQM_DEV=wan0\nSQM_DOWN_KBIT=20000\nSQM_UP_KBIT=10000\nSQM_LINK=docsis\nSQM_PERHOST=1\n' > ` + conf + `
echo "== on"; sh ` + helper + ` reconcile; tc qdisc show dev wan0; tc qdisc show dev ifb-wan
echo "== again"; sh ` + helper + ` reconcile; tc qdisc show dev wan0 | grep -c cake
echo "== start-if another device"; ip link add other0 type dummy; ip link set other0 up; sh ` + helper + ` start-if other0; tc qdisc show dev other0 | grep -c cake || true
echo "== upload only"; printf 'SQM_DEV=wan0\nSQM_DOWN_KBIT=0\nSQM_UP_KBIT=5000\nSQM_LINK=ethernet\nSQM_PERHOST=0\n' > ` + conf + `; sh ` + helper + ` reconcile; tc qdisc show dev wan0; ip link show ifb-wan 2>&1 | head -1
echo "== a device that is not there"; printf 'SQM_DEV=ppp9\nSQM_DOWN_KBIT=1000\nSQM_UP_KBIT=1000\n' > ` + conf + `; sh ` + helper + ` start; echo "exit $?"
echo "== off"; rm ` + conf + `; printf 'SQM_DEV=wan0\nSQM_DOWN_KBIT=20000\nSQM_UP_KBIT=10000\n' > ` + conf + `; sh ` + helper + ` reconcile; rm ` + conf + `; sh ` + helper + ` reconcile
tc qdisc show dev wan0 | grep -c cake || true; ip link show ifb-wan 2>&1 | head -1
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
		if j := strings.Index(rest, "== "); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}
	on := section("on")
	for _, want := range []string{"cake", "bandwidth 10Mbit", "diffserv3", "dual-srchost", "nat", "overhead 18", "mpu 64", "bandwidth 20Mbit", "dual-dsthost", "wash", "ingress"} {
		if !strings.Contains(on, want) {
			t.Errorf("shaping on: missing %q in\n%s", want, on)
		}
	}
	if strings.TrimSpace(section("again")) != "1" {
		t.Errorf("a second reconcile stacked queues: %q", section("again"))
	}
	if strings.TrimSpace(section("start-if another device")) != "0" {
		t.Errorf("another device was shaped: %q", section("start-if another device"))
	}
	up := section("upload only")
	if !strings.Contains(up, "bandwidth 5Mbit") || !strings.Contains(up, "triple-isolate") || !strings.Contains(up, "overhead 38") || strings.Contains(up, "ifb-wan: <") {
		t.Errorf("upload only: %q", up)
	}
	if !strings.Contains(up, "does not exist") {
		t.Errorf("the ifb device stayed although nothing comes in shaped: %q", up)
	}
	if !strings.Contains(section("a device that is not there"), "exit 0") {
		t.Errorf("a device that is not there yet is an error: %q", section("a device that is not there"))
	}
	off := section("off")
	if !strings.HasPrefix(strings.TrimSpace(off), "0") || !strings.Contains(off, "does not exist") {
		t.Errorf("not everything was taken away: %q", off)
	}
}

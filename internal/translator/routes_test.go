package translator

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func cfgWithRoutes(routes ...config.Route) config.NetworkConfig {
	c := routerCfg()
	c.Routes = routes
	return c
}

var labRoute = config.Route{Name: "Lab", Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: 100}
var homeRoute = config.Route{Destination: "172.16.0.0/12", Gateway: "192.168.1.51"}

func TestRoutesScriptContent(t *testing.T) {
	d := mustRender(t, cfgWithRoutes(labRoute, homeRoute))
	a, ok := hasArtifact(d, PathRoutes)
	if !ok || a.Remove || a.Service != SvcRoutes {
		t.Fatalf("%+v", a)
	}
	if a.Mode != 0755 {
		t.Errorf("mode %o: ifupdown only runs executable scripts", a.Mode)
	}
	want := `#!/bin/sh
# Static routes managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.
# ifupdown runs it whenever an interface comes up; the commit step runs it with "strict".
PATH=/usr/sbin:/sbin:/usr/bin:/bin
rc=0
ip route flush proto 77 2>/dev/null
ip route replace 10.20.0.0/16 via 192.168.1.50 metric 100 proto 77 || rc=1
ip route replace 172.16.0.0/12 via 192.168.1.51 proto 77 || rc=1
[ "$1" = strict ] && exit "$rc"
exit 0
`
	if string(a.Content) != want {
		t.Errorf("script:\n%s\nwant:\n%s", a.Content, want)
	}
	if strings.Contains(string(a.Content), "Lab") {
		t.Error("a route's name must never be written to a system file")
	}
}

func TestOnlyEnabledRoutesAreRendered(t *testing.T) {
	off := labRoute
	off.Disabled = true
	d := mustRender(t, cfgWithRoutes(off, homeRoute))
	a, _ := hasArtifact(d, PathRoutes)
	if strings.Contains(string(a.Content), "10.20.0.0") || !strings.Contains(string(a.Content), "172.16.0.0/12") {
		t.Errorf("%s", a.Content)
	}
}

func TestNoEnabledRoutesRemovesTheScript(t *testing.T) {
	off := labRoute
	off.Disabled = true
	for name, cfg := range map[string]config.NetworkConfig{"none": routerCfg(), "all turned off": cfgWithRoutes(off)} {
		a, ok := hasArtifact(mustRender(t, cfg), PathRoutes)
		if !ok || !a.Remove || a.Service != SvcRoutes {
			t.Errorf("%s: %+v", name, a)
		}
	}
}

func TestRoutesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	orig := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = orig }()

	for i, r := range []config.Route{
		{Destination: "10.20.0.0/16\nip route flush all", Gateway: "192.168.1.50"},
		{Destination: "10.20.0.0/16; reboot", Gateway: "192.168.1.50"},
		{Destination: "10.20.0.0/16", Gateway: "192.168.1.50 dev eth9"},
		{Destination: "10.20.0.0/16", Gateway: "$(reboot)"},
		{Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: -5},
		{Destination: "10.20.0.0/16", Gateway: "192.168.1.50", Metric: 1 << 20},
		{Destination: "default", Gateway: "192.168.1.50"},
		{Destination: "", Gateway: ""},
	} {
		if d, err := Render(cfgWithRoutes(r), "ROUTER"); err == nil || len(d.Artifacts) != 0 {
			t.Errorf("case %d: unsafe route reached the script (err=%v)", i, err)
		}
	}
	// a turned-off route is never rendered, so it is not inspected either
	bad := config.Route{Destination: "junk", Gateway: "junk", Disabled: true}
	if _, err := Render(cfgWithRoutes(bad), "ROUTER"); err != nil {
		t.Errorf("a turned-off route was inspected: %v", err)
	}
}

func TestRoutesNeverTouchTheOtherFiles(t *testing.T) {
	plain := mustRender(t, routerCfg())
	with := mustRender(t, cfgWithRoutes(labRoute))
	for _, p := range []string{PathSysctl, PathInterfaces, PathNftables, PathDnsmasqDHCP} {
		a, _ := hasArtifact(plain, p)
		b, _ := hasArtifact(with, p)
		if string(a.Content) != string(b.Content) {
			t.Errorf("%s changed because of a route", p)
		}
	}
}

// ---- plan ------------------------------------------------------------------------------

func TestAddingARouteOnlyAppliesRoutes(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, routerCfg()))

	plan, err := PlanChanges(root, mustRender(t, cfgWithRoutes(labRoute)))
	if err != nil {
		t.Fatal(err)
	}
	if got := actionSummary(plan); got != "apply routes" {
		t.Fatalf("actions: %q (a route must not restart networking or the DHCP server)", got)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != PathRoutes || plan.Files[0].Kind != Create {
		t.Fatalf("%+v", plan.Files)
	}
}

func TestChangingAndRemovingRoutes(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, cfgWithRoutes(labRoute)))

	moved := labRoute
	moved.Gateway = "192.168.1.60"
	plan, _ := PlanChanges(root, mustRender(t, cfgWithRoutes(moved)))
	if len(plan.Files) != 1 || plan.Files[0].Kind != Update || actionSummary(plan) != "apply routes" {
		t.Fatalf("%+v %q", plan.Files, actionSummary(plan))
	}

	plan, _ = PlanChanges(root, mustRender(t, routerCfg())) // the last route goes away
	if len(plan.Files) != 1 || plan.Files[0].Kind != Remove || actionSummary(plan) != "apply routes" {
		t.Fatalf("%+v %q", plan.Files, actionSummary(plan))
	}

	// the same routes again: nothing to do
	plan, _ = PlanChanges(root, mustRender(t, cfgWithRoutes(labRoute)))
	if !plan.Empty() {
		t.Fatalf("%+v", plan)
	}
}

func TestAWizardBoxWithoutRoutesShowsNoRouteChange(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, routerCfg()))
	plan, _ := PlanChanges(root, mustRender(t, routerCfg()))
	if !plan.Empty() {
		t.Fatalf("a box with no routes should plan nothing: %+v", plan)
	}
}

func TestNameOnlyChangesCauseNoSystemChange(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, cfgWithRoutes(labRoute)))
	renamed := labRoute
	renamed.Name = "A better name"
	plan, _ := PlanChanges(root, mustRender(t, cfgWithRoutes(renamed)))
	if !plan.Empty() {
		t.Fatalf("renaming a route restarted something: %+v", plan)
	}
}

func TestRoutesComeAfterNetworkingWhenBothChange(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, routerCfg()))
	c := cfgWithRoutes(labRoute)
	c.LanIP = "192.168.1.2"
	c.DHCP.RangeStart = "192.168.1.100"
	plan, _ := PlanChanges(root, mustRender(t, c))
	got := actionSummary(plan)
	if !strings.Contains(got, "restart networking, apply routes") || strings.Index(got, "routes") > strings.Index(got, "nftables") && strings.Contains(got, "nftables") {
		t.Fatalf("%q", got)
	}
}

// ---- apply -----------------------------------------------------------------------------

func TestApplyingRoutesChecksThenRunsTheScriptStrictly(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	if res := ap.Apply(context.Background(), d, plan); !res.OK {
		t.Fatalf("%+v", res)
	}
	run.reset()

	d, plan = planFor(t, root, cfgWithRoutes(labRoute))
	res := ap.Apply(context.Background(), d, plan)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if !exists(root, PathRoutes) {
		t.Fatal("script not written")
	}
	log := strings.Join(run.log(), "\n")
	if !strings.Contains(log, "sh -n "+PathRoutes) || !strings.Contains(log, "sh "+PathRoutes+" strict") {
		t.Fatalf("commands:\n%s", log)
	}
	if strings.Contains(log, "restart networking") || strings.Contains(log, "restart dnsmasq") {
		t.Fatalf("a route change restarted a service:\n%s", log)
	}
	checkAt, runAt := strings.Index(log, "sh -n"), strings.Index(log, " strict")
	if checkAt > runAt {
		t.Fatal("the script must be checked before it is run")
	}
}

func TestARouteTheKernelRefusesFailsTheCommitAndRollsBack(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, cfgWithRoutes(labRoute))
	if res := ap.Apply(context.Background(), d, plan); !res.OK {
		t.Fatalf("%+v", res)
	}
	before := read(t, root, PathRoutes)
	run.reset()
	run.fail["sh -c if [ -f "+PathRoutes+" ]; then sh "+PathRoutes+" strict"] = "Error: Nexthop has invalid gateway."
	run.failOnce["sh -c if [ -f "+PathRoutes+" ]; then sh "+PathRoutes+" strict"] = true // the rollback run succeeds

	moved := labRoute
	moved.Gateway = "192.168.1.250"
	d, plan = planFor(t, root, cfgWithRoutes(moved))
	res := ap.Apply(context.Background(), d, plan)
	if res.OK || !res.RolledBack || !strings.Contains(res.Error, "Nexthop has invalid gateway") {
		t.Fatalf("%+v", res)
	}
	if got := read(t, root, PathRoutes); got != before {
		t.Fatalf("the previous script was not restored:\n%s", got)
	}
	runs := 0
	for _, l := range run.log() {
		if strings.Contains(l, " strict") {
			runs++
		}
	}
	if runs != 2 {
		t.Fatalf("the previous routes should be applied again after the failure (%d runs)", runs)
	}
}

func TestRemovingTheLastRouteFlushesOurRoutes(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, cfgWithRoutes(labRoute))
	ap.Apply(context.Background(), d, plan)
	run.reset()

	d, plan = planFor(t, root, routerCfg())
	res := ap.Apply(context.Background(), d, plan)
	if !res.OK || exists(root, PathRoutes) {
		t.Fatalf("%+v", res)
	}
	// with the script gone the check is skipped by its own test and the routes are flushed by tag
	last := run.log()
	if !strings.Contains(strings.Join(last, "\n"), "ip route flush proto "+RouteProto) {
		t.Fatalf("%v", last)
	}
}

func TestRestoreBringsTheOldRoutesBack(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, cfgWithRoutes(labRoute))
	ap.Apply(context.Background(), d, plan)

	d, plan = planFor(t, root, routerCfg()) // remove them...
	res := ap.Apply(context.Background(), d, plan)
	if !res.OK || res.Snapshot == nil {
		t.Fatalf("%+v", res)
	}
	run.reset()
	rest := ap.Restore(context.Background(), *res.Snapshot) // ...and revert
	if !rest.OK || !exists(root, PathRoutes) {
		t.Fatalf("%+v", rest)
	}
	if !strings.Contains(strings.Join(run.log(), "\n"), "sh "+PathRoutes+" strict") {
		t.Fatalf("the restored script was not run: %v", run.log())
	}
}

func TestTheRoutesCommandsNeverContainConfigurationValues(t *testing.T) {
	for _, c := range []command{preChecks[SvcRoutes], serviceCommands[SvcRoutes]} {
		line := c.String()
		for _, v := range []string{"10.20", "192.168", "Lab"} {
			if strings.Contains(line, v) {
				t.Errorf("%q contains configuration data", line)
			}
		}
	}
}

// ---- the real shell, against a temporary file --------------------------------------------

func runShell(t *testing.T, c command) error {
	t.Helper()
	return exec.Command(c.name, c.args...).Run()
}

func TestTheCheckCommandSkipsAMissingScriptAndCatchesABrokenOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peakroute-routes")
	check := routesCheckCommand(path)

	if err := runShell(t, check); err != nil {
		t.Fatalf("a missing script (the last route was removed) must pass the check: %v", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nif then\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := runShell(t, check); err == nil {
		t.Fatal("a script with a syntax error passed the check")
	}
	script, _ := renderRoutes([]config.Route{labRoute})
	os.WriteFile(path, []byte(script), 0755)
	if err := runShell(t, check); err != nil {
		t.Fatalf("the generated script failed its own check: %v", err)
	}
}

func TestTheApplyCommandRunsTheScriptStrictlyOrFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "peakroute-routes")
	apply := routesApplyCommand(path)

	// a script that fails only when told to be strict
	os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = strict ] && exit 7\nexit 0\n"), 0755)
	err := runShell(t, apply)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("the commit step must run the script with \"strict\": %v", err)
	}
}

func TestTheGeneratedScriptIsLenientForIfupAndStrictForTheCommit(t *testing.T) {
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("no ip command here")
	}
	script, _ := renderRoutes([]config.Route{{Destination: "10.20.0.0/16", Gateway: "192.0.2.77"}})
	path := filepath.Join(t.TempDir(), "peakroute-routes")
	os.WriteFile(path, []byte(script), 0755)

	// Without the right to change routes (or without that gateway being reachable) the route fails
	if err := exec.Command("sh", path).Run(); err != nil {
		t.Errorf("run by ifupdown the script must never fail, or it could hold an interface back: %v", err)
	}
	if err := exec.Command("sh", path, "strict").Run(); err == nil {
		t.Skip("this machine accepted the test route; nothing to compare")
	}
}

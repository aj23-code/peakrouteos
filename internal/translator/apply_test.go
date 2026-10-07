package translator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"peakroute/internal/config"
)

// fakeRunner records commands and fails the ones it is told to. A failing command fails every
// time unless failOnce is used.
type fakeRunner struct {
	mu       sync.Mutex
	commands []string
	fail     map[string]string // command prefix -> output to return with an error
	failOnce map[string]bool
	delay    time.Duration
	onRun    func(line string) // test hook, called while holding no lock
	running  int32
	maxRun   int32
}

func newRunner() *fakeRunner {
	return &fakeRunner{fail: map[string]string{}, failOnce: map[string]bool{}}
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	n := atomic.AddInt32(&f.running, 1)
	for {
		m := atomic.LoadInt32(&f.maxRun)
		if n <= m || atomic.CompareAndSwapInt32(&f.maxRun, m, n) {
			break
		}
	}
	defer atomic.AddInt32(&f.running, -1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}

	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if f.onRun != nil {
		f.onRun(line)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, line)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	for prefix, out := range f.fail {
		if strings.HasPrefix(line, prefix) {
			if f.failOnce[prefix] {
				delete(f.fail, prefix)
			}
			return out, errors.New("exit status 1")
		}
	}
	return "ok", nil
}

func (f *fakeRunner) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeRunner) reset() {
	f.mu.Lock()
	f.commands = nil
	f.mu.Unlock()
}

func newApplier(t *testing.T) (*Applier, RootFS, *fakeRunner) {
	t.Helper()
	root := RootFS(t.TempDir())
	r := newRunner()
	return &Applier{FS: root, Run: r}, root, r
}

func planFor(t *testing.T, fs FileReader, cfg config.NetworkConfig) (Desired, Plan) {
	t.Helper()
	d := mustRender(t, cfg)
	p, err := PlanChanges(fs, d)
	if err != nil {
		t.Fatal(err)
	}
	return d, p
}

func read(t *testing.T, root RootFS, path string) string {
	t.Helper()
	b, err := root.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func exists(root RootFS, path string) bool {
	_, err := root.ReadFile(path)
	return err == nil
}

func TestApplyOnAnEmptySystemWritesChecksThenRestartsInOrder(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())

	res := ap.Apply(context.Background(), d, plan)
	if !res.OK || res.Error != "" || res.RolledBack {
		t.Fatalf("apply failed: %+v", res)
	}

	// files are really there with the right content and modes
	for _, art := range d.Artifacts {
		if art.Remove {
			continue
		}
		if got := read(t, root, art.Path); got != string(art.Content) {
			t.Errorf("%s content differs from what was rendered", art.Path)
		}
		if mode, _ := root.FileMode(art.Path); mode != art.Mode {
			t.Errorf("%s mode = %o, want %o", art.Path, mode, art.Mode)
		}
	}

	want := []string{
		// every check happens before any service is touched
		"ifquery --list --all",
		"nft -c -f " + PathNftables,
		"dnsmasq --test",
		// then the services, in dependency order
		"sysctl -p " + PathSysctl,
		"systemctl restart networking",
		"systemctl reload nftables",
		"systemctl restart dnsmasq",
	}
	if got := run.log(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("commands =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	for _, s := range res.Steps {
		if !s.OK {
			t.Errorf("step %q failed: %+v", s.Name, s)
		}
	}
}

func TestApplyingAnEmptyPlanDoesNothing(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	if res := ap.Apply(context.Background(), d, plan); !res.OK {
		t.Fatal(res.Error)
	}
	run.reset()

	d, plan = planFor(t, root, routerCfg())
	res := ap.Apply(context.Background(), d, plan)
	if !res.OK || len(res.Steps) != 0 || len(run.log()) != 0 {
		t.Errorf("an up-to-date system must be left alone: %+v, commands %v", res, run.log())
	}
}

func TestOnlyTheAffectedServiceIsCheckedAndRestarted(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	run.reset()

	next := routerCfg()
	next.DHCP.LeaseTime = "1d"
	d, plan = planFor(t, root, next)
	res := ap.Apply(context.Background(), d, plan)
	if !res.OK {
		t.Fatal(res.Error)
	}

	if got := strings.Join(run.log(), "|"); got != "dnsmasq --test|systemctl restart dnsmasq" {
		t.Errorf("commands = %q, want only dnsmasq to be checked and restarted", got)
	}
	if !strings.Contains(read(t, root, PathDnsmasqDHCP), ",1d\n") {
		t.Error("the new lease time was not written")
	}
}

func TestACheckThatFailsStopsEverythingAndRestoresTheFiles(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	before := map[string]string{}
	for _, p := range []string{PathInterfaces, PathNftables, PathDnsmasqDHCP, PathSysctl} {
		before[p] = read(t, root, p)
	}
	run.reset()

	run.fail["nft -c"] = "/etc/nftables.conf:7:9-12: Error: syntax error, unexpected eof"
	next := routerCfg()
	next.WanInterface = "eth5" // changes interfaces AND the firewall
	d, plan = planFor(t, root, next)
	res := ap.Apply(context.Background(), d, plan)

	if res.OK || !res.RolledBack {
		t.Fatalf("expected a failed, rolled back apply: %+v", res)
	}
	if !strings.Contains(res.Error, "nftables configuration was rejected") || !strings.Contains(res.Error, "syntax error") {
		t.Errorf("error = %q, want the check's own message", res.Error)
	}
	for p, content := range before {
		if got := read(t, root, p); got != content {
			t.Errorf("%s was not restored after a failed check", p)
		}
	}

	// the failing nft check ran; no service was restarted for the new config. The only
	// systemctl calls are the rollback re-applying the OLD files.
	for _, c := range run.log() {
		if strings.HasPrefix(c, "systemctl") || strings.HasPrefix(c, "sysctl") {
			if len(res.Problems) != 0 {
				t.Errorf("unexpected rollback problems: %v", res.Problems)
			}
		}
	}
	if strings.Contains(strings.Join(run.log(), "|"), "dnsmasq --test") {
		t.Error("checks should stop at the first failure")
	}
}

func TestAServiceThatFailsToRestartIsRolledBackAndReapplied(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	original := read(t, root, PathDnsmasqDHCP)
	run.reset()

	// dnsmasq refuses to start on the new config, but is fine on the old one
	run.fail["systemctl restart dnsmasq"] = "Job for dnsmasq.service failed because the control process exited with error code."
	run.failOnce["systemctl restart dnsmasq"] = true
	next := routerCfg()
	next.DHCP.RangeEnd = "192.168.1.150"
	d, plan = planFor(t, root, next)
	res := ap.Apply(context.Background(), d, plan)

	if res.OK || !res.RolledBack {
		t.Fatalf("expected a rollback: %+v", res)
	}
	if len(res.Problems) != 0 {
		t.Errorf("the rollback should have succeeded: %v", res.Problems)
	}
	if got := read(t, root, PathDnsmasqDHCP); got != original {
		t.Errorf("the old dnsmasq config was not restored:\n%s", got)
	}
	// dnsmasq was restarted twice: once on the bad config, once after restoring the good one
	restarts := 0
	for _, c := range run.log() {
		if c == "systemctl restart dnsmasq" {
			restarts++
		}
	}
	if restarts != 2 {
		t.Errorf("dnsmasq restarted %d times, want 2 (failed, then restored)\n%v", restarts, run.log())
	}
}

func TestRollbackProblemsAreReportedNotHidden(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	run.reset()

	run.fail["systemctl restart dnsmasq"] = "dnsmasq: cannot bind to interface"
	next := routerCfg()
	next.DHCP.RangeEnd = "192.168.1.150"
	d, plan = planFor(t, root, next)
	res := ap.Apply(context.Background(), d, plan)

	if res.OK || !res.RolledBack || len(res.Problems) == 0 {
		t.Fatalf("a rollback that cannot restart the service must say so: %+v", res)
	}
	if !strings.Contains(res.Problems[0], "dnsmasq") {
		t.Errorf("problem = %q", res.Problems[0])
	}
}

func TestAnInterruptedWriteRestoresTheFilesAlreadyWritten(t *testing.T) {
	ap, root, _ := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	before := map[string]string{
		PathSysctl:     read(t, root, PathSysctl),
		PathInterfaces: read(t, root, PathInterfaces),
	}

	// the nftables file's directory becomes unwritable half way through the plan
	failing := &failingWrites{RootFS: root, failPath: PathNftables}
	ap.FS = failing

	next := routerCfg()
	next.WanInterface = "eth5"
	d, plan = planFor(t, root, next)
	res := ap.Apply(context.Background(), d, plan)

	if res.OK || !res.RolledBack {
		t.Fatalf("expected a rolled back failure: %+v", res)
	}
	for p, content := range before {
		if got := read(t, root, p); got != content {
			t.Errorf("%s (written before the failure) was not restored", p)
		}
	}
}

type failingWrites struct {
	RootFS
	failPath string
}

func (f *failingWrites) WriteFile(path string, data []byte, mode os.FileMode) error {
	if path == f.failPath {
		return errors.New("disk full")
	}
	return f.RootFS.WriteFile(path, data, mode)
}

func TestCreatedFilesAreRemovedAgainOnFailure(t *testing.T) {
	ap, root, run := newApplier(t)
	run.fail["dnsmasq --test"] = "dnsmasq: bad option"

	d, plan := planFor(t, root, routerCfg()) // everything is new
	res := ap.Apply(context.Background(), d, plan)

	if res.OK || !res.RolledBack {
		t.Fatalf("expected failure: %+v", res)
	}
	for _, p := range []string{PathSysctl, PathInterfaces, PathNftables, PathDnsmasqDHCP} {
		if exists(root, p) {
			t.Errorf("%s was created and must be removed again after the failure", p)
		}
	}
}

func TestRemovedFilesAreRestoredOnFailure(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	original := read(t, root, PathDnsmasqDHCP)
	run.reset()

	off := routerCfg()
	off.DHCP = &config.DHCPConfig{Enabled: false}
	run.fail["systemctl restart dnsmasq"] = "boom"
	run.failOnce["systemctl restart dnsmasq"] = true
	d, plan = planFor(t, root, off)
	res := ap.Apply(context.Background(), d, plan)

	if res.OK {
		t.Fatal("expected failure")
	}
	if got := read(t, root, PathDnsmasqDHCP); got != original {
		t.Error("the removed DHCP file was not restored")
	}
}

func TestApplyDisablingDhcpRemovesTheFileAndRestartsDnsmasq(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	run.reset()

	off := routerCfg()
	off.DHCP = &config.DHCPConfig{Enabled: false}
	d, plan = planFor(t, root, off)
	res := ap.Apply(context.Background(), d, plan)

	if !res.OK {
		t.Fatal(res.Error)
	}
	if exists(root, PathDnsmasqDHCP) {
		t.Error("the DHCP file should be gone")
	}
	if got := strings.Join(run.log(), "|"); got != "dnsmasq --test|systemctl restart dnsmasq" {
		t.Errorf("commands = %q", got)
	}
}

func TestACancelledContextAppliesNothing(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := ap.Apply(ctx, d, plan)

	if res.OK || exists(root, PathInterfaces) || len(run.log()) != 0 {
		t.Errorf("a cancelled apply must not touch anything: %+v %v", res, run.log())
	}
}

func TestAppliesNeverRunAtTheSameTime(t *testing.T) {
	ap, root, run := newApplier(t)
	run.delay = 5 * time.Millisecond

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := routerCfg()
			cfg.DHCP.LeaseTime = []string{"1h", "2h", "3h", "4h", "5h", "6h"}[i]
			d := mustRender(t, cfg)
			plan, err := PlanChanges(root, d)
			if err != nil {
				t.Error(err)
				return
			}
			ap.Apply(context.Background(), d, plan)
		}(i)
	}
	wg.Wait()

	if m := atomic.LoadInt32(&run.maxRun); m != 1 {
		t.Errorf("up to %d commands ran at once, want strictly one apply at a time", m)
	}
}

func TestEverythingRenderedIsAppliedIdempotently(t *testing.T) {
	ap, root, run := newApplier(t)
	cfgs := []config.NetworkConfig{routerCfg()}
	two := routerCfg()
	two.LanInterfaces = []string{"eth1", "eth2"}
	static := routerCfg()
	static.WanMode, static.WanIP, static.WanSubnet, static.WanGateway = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1"
	static.WanDNS = []string{"1.1.1.1"}
	cfgs = append(cfgs, two, static, routerCfg())

	for i, cfg := range cfgs {
		d, plan := planFor(t, root, cfg)
		if res := ap.Apply(context.Background(), d, plan); !res.OK {
			t.Fatalf("config %d: %+v", i, res)
		}
		run.reset()
		// after applying, the system matches: planning again must be empty
		_, again := planFor(t, root, cfg)
		if !again.Empty() {
			t.Errorf("config %d: still not in sync after applying: %+v", i, again)
		}
	}
}

// --- runners ---

func TestExecRunnerRunsRealCommandsWithAFixedEnvironment(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), "echo", "hello", "world")
	if err != nil || out != "hello world" {
		t.Errorf("echo = %q, %v", out, err)
	}

	out, err = ExecRunner{}.Run(context.Background(), "sh", "-c", "echo $PATH; echo $PEAKROUTE_LEAK")
	if err != nil || !strings.HasPrefix(out, "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
		t.Errorf("PATH = %q, %v", out, err)
	}
	t.Setenv("PEAKROUTE_LEAK", "secret")
	out, _ = ExecRunner{}.Run(context.Background(), "sh", "-c", "echo [$PEAKROUTE_LEAK]")
	if out != "[]" {
		t.Errorf("the daemon's environment leaked into the command: %q", out)
	}
}

func TestExecRunnerReportsFailuresWithTheirOutput(t *testing.T) {
	out, err := ExecRunner{}.Run(context.Background(), "sh", "-c", "echo broken config >&2; exit 3")
	if err == nil || out != "broken config" {
		t.Errorf("out=%q err=%v, want the command's message and an error", out, err)
	}

	if _, err := (ExecRunner{}).Run(context.Background(), "/no/such/binary"); err == nil {
		t.Error("a missing binary must be an error")
	}
}

func TestExecRunnerStopsCommandsThatRunTooLong(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ExecRunner{}.Run(ctx, "sleep", "30")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the command was not stopped")
	}
}

func TestExecRunnerCapsHugeOutput(t *testing.T) {
	out, _ := ExecRunner{}.Run(context.Background(), "sh", "-c", "head -c 100000 /dev/zero | tr '\\0' x")
	if len(out) > maxCommandOutput+64 || !strings.HasSuffix(out, "[output truncated]") {
		t.Errorf("output of %d bytes was not capped", len(out))
	}
}

// Dev mode must never run a real command, whatever it is asked to run
func TestLogRunnerNeverExecutesAnything(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	lr := &LogRunner{}

	out, err := lr.Run(context.Background(), "touch", marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("LogRunner actually ran the command")
	}
	if !strings.Contains(out, "dry run") {
		t.Errorf("output = %q", out)
	}
	lr.Run(context.Background(), "systemctl", "restart", "networking")
	if got := lr.Commands(); len(got) != 2 || got[1] != "systemctl restart networking" {
		t.Errorf("commands = %v", got)
	}
}

func TestRootFSWritesIntoItsRootAndCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	root := RootFS(dir)

	if err := root.WriteFile("/etc/deep/new/file.conf", []byte("x"), 0640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "etc/deep/new/file.conf"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("file = %v, %v", info, err)
	}

	if err := root.RemoveFile("/etc/deep/new/file.conf"); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveFile("/etc/deep/new/file.conf"); err != nil {
		t.Errorf("removing a missing file must not be an error: %v", err)
	}
	if exists(root, "/etc/deep/new/file.conf") {
		t.Error("file still exists")
	}
}

// If the apply is cancelled half way (the daemon is shutting down), the system must still be put
// back: the rollback may not reuse the cancelled context.
func TestRollbackStillRunsWhenTheApplyIsCancelledMidway(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	original := read(t, root, PathInterfaces)
	run.reset()

	ctx, cancel := context.WithCancel(context.Background())
	run.onRun = func(line string) {
		if line == "systemctl restart networking" && ctx.Err() == nil {
			cancel() // shutdown arrives while networking is restarting
		}
	}

	next := routerCfg()
	next.WanInterface = "eth5"
	d, plan = planFor(t, root, next)
	res := ap.Apply(ctx, d, plan)

	if res.OK || !res.RolledBack {
		t.Fatalf("expected a rolled back failure: %+v", res)
	}
	if got := read(t, root, PathInterfaces); got != original {
		t.Error("the interfaces file was not restored after the cancellation")
	}
	if len(res.Problems) != 0 {
		t.Errorf("the rollback commands must run on a fresh context, got problems: %v", res.Problems)
	}
	restarts := 0
	for _, c := range run.log() {
		if c == "systemctl restart networking" {
			restarts++
		}
	}
	if restarts != 2 {
		t.Errorf("networking restarted %d times, want 2 (the cancelled one and the rollback)", restarts)
	}
}

// --- snapshots and restore (the basis of revert) ---

func TestASuccessfulApplyReturnsASnapshotOfThePreviousState(t *testing.T) {
	ap, root, _ := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	if res := ap.Apply(context.Background(), d, plan); !res.OK || res.Snapshot == nil {
		t.Fatalf("first apply: %+v", res)
	}

	next := routerCfg()
	next.DHCP.LeaseTime = "1d"
	oldDHCP := read(t, root, PathDnsmasqDHCP)
	d, plan = planFor(t, root, next)
	res := ap.Apply(context.Background(), d, plan)
	if !res.OK || res.Snapshot == nil {
		t.Fatalf("apply: %+v", res)
	}

	if len(res.Snapshot.Files) != 1 || res.Snapshot.Files[0].Path != PathDnsmasqDHCP ||
		!res.Snapshot.Files[0].Existed || string(res.Snapshot.Files[0].Content) != oldDHCP {
		t.Errorf("snapshot files = %+v, want the previous dnsmasq file", res.Snapshot.Files)
	}
	if len(res.Snapshot.Actions) != 1 || res.Snapshot.Actions[0].Service != SvcDnsmasq {
		t.Errorf("snapshot actions = %+v", res.Snapshot.Actions)
	}
}

func TestAFailedApplyHasNoSnapshot(t *testing.T) {
	ap, root, run := newApplier(t)
	run.fail["dnsmasq --test"] = "bad"
	d, plan := planFor(t, root, routerCfg())
	if res := ap.Apply(context.Background(), d, plan); res.OK || res.Snapshot != nil {
		t.Errorf("a failed apply must not offer a snapshot: %+v", res)
	}
}

func TestRestoreReturnsTheSystemToTheSnapshot(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	original := map[string]string{PathInterfaces: read(t, root, PathInterfaces), PathNftables: read(t, root, PathNftables), PathDnsmasqDHCP: read(t, root, PathDnsmasqDHCP)}
	run.reset()

	next := routerCfg()
	next.WanInterface = "eth5"
	d, plan = planFor(t, root, next)
	applied := ap.Apply(context.Background(), d, plan)
	if !applied.OK {
		t.Fatal(applied.Error)
	}
	for p, content := range original {
		if read(t, root, p) == content && p != PathDnsmasqDHCP {
			t.Fatalf("%s was not changed by the apply", p)
		}
	}
	run.reset()

	res := ap.Restore(context.Background(), *applied.Snapshot)
	if !res.OK || len(res.Problems) != 0 {
		t.Fatalf("restore: %+v", res)
	}
	for p, content := range original {
		if got := read(t, root, p); got != content {
			t.Errorf("%s was not restored", p)
		}
	}
	// the services are re-run so they pick the old files up again
	if got := strings.Join(run.log(), "|"); got != "systemctl restart networking|systemctl reload nftables|systemctl restart dnsmasq" {
		t.Errorf("restore commands = %q", got)
	}
}

// A revert after a daemon restart works from a snapshot read back from disk
func TestASnapshotSurvivesBeingSavedAsJSON(t *testing.T) {
	ap, root, _ := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	before := read(t, root, PathInterfaces)

	next := routerCfg()
	next.WanInterface = "eth5"
	d, plan = planFor(t, root, next)
	applied := ap.Apply(context.Background(), d, plan)

	data, err := json.Marshal(applied.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Snapshot
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	if res := ap.Restore(context.Background(), loaded); !res.OK {
		t.Fatalf("restore from a reloaded snapshot: %+v", res)
	}
	if got := read(t, root, PathInterfaces); got != before {
		t.Error("the interfaces file was not restored from the reloaded snapshot")
	}
}

func TestRestoreRemovesFilesTheApplyCreatedAndRestoresModes(t *testing.T) {
	ap, root, _ := newApplier(t)
	d, plan := planFor(t, root, routerCfg()) // everything is new on an empty system
	applied := ap.Apply(context.Background(), d, plan)
	if !applied.OK {
		t.Fatal(applied.Error)
	}

	if res := ap.Restore(context.Background(), *applied.Snapshot); !res.OK {
		t.Fatalf("restore: %+v", res)
	}
	for _, p := range []string{PathSysctl, PathInterfaces, PathNftables, PathDnsmasqDHCP} {
		if exists(root, p) {
			t.Errorf("%s was created by the apply and must be gone after the restore", p)
		}
	}
}

func TestRestorePreservesTheOriginalFileMode(t *testing.T) {
	ap, root, _ := newApplier(t)
	if err := root.WriteFile(PathNftables, []byte("old rules\n"), 0750); err != nil {
		t.Fatal(err)
	}
	d, plan := planFor(t, root, routerCfg())
	applied := ap.Apply(context.Background(), d, plan)
	if !applied.OK {
		t.Fatal(applied.Error)
	}
	if mode, _ := root.FileMode(PathNftables); mode != 0755 {
		t.Fatalf("applied mode = %o", mode)
	}

	ap.Restore(context.Background(), *applied.Snapshot)
	if mode, _ := root.FileMode(PathNftables); mode != 0750 {
		t.Errorf("restored mode = %o, want the original 0750", mode)
	}
	if got := read(t, root, PathNftables); got != "old rules\n" {
		t.Errorf("restored content = %q", got)
	}
}

func TestRestoreReportsWhatItCouldNotUndo(t *testing.T) {
	ap, root, run := newApplier(t)
	d, plan := planFor(t, root, routerCfg())
	ap.Apply(context.Background(), d, plan)
	next := routerCfg()
	next.DHCP.LeaseTime = "1d"
	d, plan = planFor(t, root, next)
	applied := ap.Apply(context.Background(), d, plan)

	run.fail["systemctl restart dnsmasq"] = "dnsmasq: failed to start"
	res := ap.Restore(context.Background(), *applied.Snapshot)
	if res.OK || len(res.Problems) == 0 || res.Error == "" {
		t.Errorf("a restore that cannot restart the service must say so: %+v", res)
	}
}

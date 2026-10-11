package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/translator"
)

// --- fakes ---

type fakeRunner struct {
	mu       sync.Mutex
	commands []string
	fail     map[string]string
	failOnce map[string]bool
	gate     chan struct{} // if set, "systemctl restart networking" waits for it
	entered  chan struct{} // closed-over notifier when the gated command starts
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if f.gate != nil && line == "systemctl restart networking" {
		if f.entered != nil {
			select {
			case f.entered <- struct{}{}:
			default:
			}
		}
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, line)
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

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { was := !t.stopped; t.stopped = true; return was }

type fakeTimers struct {
	mu  sync.Mutex
	all []*fakeTimer
}

func (ft *fakeTimers) after(d time.Duration, f func()) Timer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := &fakeTimer{d: d, f: f}
	ft.all = append(ft.all, t)
	return t
}

func (ft *fakeTimers) last() *fakeTimer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if len(ft.all) == 0 {
		return nil
	}
	return ft.all[len(ft.all)-1]
}

func (ft *fakeTimers) count() int {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return len(ft.all)
}

// --- environment ---

type env struct {
	t      *testing.T
	svc    *Service
	mgr    *config.Manager
	root   translator.RootFS
	run    *fakeRunner
	dir    string
	clock  *fakeClock
	timers *fakeTimers
	mode   string
}

func baseCfg() config.NetworkConfig {
	return config.NetworkConfig{
		WanInterface:  "eth0",
		WanMode:       "dhcp",
		LanInterfaces: []string{"eth1"},
		LanIP:         "192.168.1.1",
		LanSubnet:     "255.255.255.0",
		DHCP:          &config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"},
	}
}

func lanMoved(c *config.NetworkConfig) {
	c.LanIP = "10.0.0.1"
	c.DHCP.RangeStart, c.DHCP.RangeEnd = "10.0.0.100", "10.0.0.200"
}

func leaseChanged(c *config.NetworkConfig) { c.DHCP.LeaseTime = "1d" }

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{
		t: t, dir: dir, root: translator.RootFS(t.TempDir()),
		run:    &fakeRunner{fail: map[string]string{}, failOnce: map[string]bool{}},
		clock:  &fakeClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)},
		timers: &fakeTimers{},
		mode:   "ROUTER",
	}
	e.start(dir)
	t.Cleanup(func() { e.svc.Close() })
	return e
}

// start builds a Manager and Service over the directory (also used to simulate a restart)
func (e *env) start(dir string) {
	e.t.Helper()
	mgr, err := config.NewManager(dir)
	if err != nil {
		e.t.Fatal(err)
	}
	e.mgr = mgr
	e.svc = New(Options{
		Manager: mgr, FS: e.root, Runner: e.run, StateDir: dir,
		ConfirmWindow: 60 * time.Second,
		Mode:          func() string { return e.mode },
		Now:           e.clock.Now,
		AfterFunc:     e.timers.after,
	})
}

func (e *env) restart() {
	e.t.Helper()
	e.svc.Close()
	e.timers = &fakeTimers{}
	e.start(e.dir)
}

// baseline makes cfg both the committed configuration and what is on the system
func (e *env) baseline(cfg config.NetworkConfig) {
	e.t.Helper()
	if err := e.mgr.StageNetwork(func(c *config.NetworkConfig) error { *c = cfg; return nil }); err != nil {
		e.t.Fatal(err)
	}
	if err := e.mgr.CommitNetwork(); err != nil {
		e.t.Fatal(err)
	}
	d, err := translator.Render(cfg, "ROUTER")
	if err != nil {
		e.t.Fatal(err)
	}
	plan, err := translator.PlanChanges(e.root, d)
	if err != nil {
		e.t.Fatal(err)
	}
	ap := &translator.Applier{FS: e.root, Run: e.run}
	if res := ap.Apply(context.Background(), d, plan); !res.OK {
		e.t.Fatalf("baseline apply failed: %+v", res)
	}
	e.run.reset()
}

func (e *env) stage(edit func(*config.NetworkConfig)) {
	e.t.Helper()
	if err := e.mgr.StageNetwork(func(c *config.NetworkConfig) error { edit(c); return nil }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) file(path string) string {
	e.t.Helper()
	b, err := e.root.ReadFile(path)
	if err != nil {
		e.t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func (e *env) fileExists(path string) bool {
	_, err := e.root.ReadFile(path)
	return err == nil
}

func (e *env) confirmFileExists() bool {
	_, err := os.Stat(filepath.Join(e.dir, "confirm.json"))
	return err == nil
}

func (e *env) commitAndWait() {
	e.t.Helper()
	if err := e.svc.Commit(); err != nil {
		e.t.Fatalf("Commit: %v", err)
	}
	e.svc.Wait()
}

func (e *env) fire() {
	e.t.Helper()
	tm := e.timers.last()
	if tm == nil {
		e.t.Fatal("no timer was armed")
	}
	tm.f()
	e.svc.Wait()
}

// --- review ---

func TestReviewShowsTheChangesThePlanAndWhetherConfirmationIsNeeded(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	e.stage(lanMoved)
	rev := e.svc.Review()
	if !rev.Valid || len(rev.Changes) == 0 || rev.Plan == nil {
		t.Fatalf("review = %+v", rev)
	}
	if !rev.NeedsConfirm || rev.ConfirmSeconds != 60 {
		t.Errorf("moving the LAN address must need confirmation: %+v", rev)
	}
	verbs := ""
	for _, a := range rev.Plan.Actions {
		verbs += a.Verb + " " + a.Service + ";"
	}
	if !strings.Contains(verbs, "restart networking") || !strings.Contains(verbs, "restart dnsmasq") {
		t.Errorf("plan actions = %s", verbs)
	}

	// a lease-time-only change cannot lock anyone out
	if err := e.mgr.DiscardNetwork(); err != nil {
		t.Fatal(err)
	}
	e.stage(leaseChanged)
	rev = e.svc.Review()
	if !rev.Valid || rev.NeedsConfirm {
		t.Errorf("a DHCP-only change must not need confirmation: %+v", rev)
	}
}

func TestReviewReportsInvalidSettingsPerFieldAndNoPlan(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(func(c *config.NetworkConfig) { c.LanIP = "192.168.1.0" }) // the network address

	rev := e.svc.Review()
	if rev.Valid || rev.Plan != nil {
		t.Fatalf("an invalid configuration must not produce a plan: %+v", rev)
	}
	found := false
	for _, fe := range rev.Errors {
		if fe.Field == "lan_ip" {
			found = true
		}
	}
	if !found {
		t.Errorf("errors = %+v, want one on lan_ip", rev.Errors)
	}
	if len(rev.Changes) == 0 {
		t.Error("the changes should still be listed")
	}
}

func TestReviewWithNothingStaged(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	rev := e.svc.Review()
	if !rev.Valid || len(rev.Changes) != 0 || rev.Plan != nil {
		t.Errorf("review = %+v", rev)
	}
}

func TestReviewExplainsAnUnsupportedMode(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(leaseChanged)
	e.mode = "AP"

	rev := e.svc.Review()
	if rev.Valid || !strings.Contains(rev.Problem, "not supported") {
		t.Errorf("review = %+v, want a problem explaining the mode is not supported", rev)
	}
}

func TestOnlyNetworkingAndFirewallChangesNeedConfirmation(t *testing.T) {
	plan := func(services ...string) translator.Plan {
		var p translator.Plan
		for _, s := range services {
			p.Actions = append(p.Actions, translator.Action{Service: s})
		}
		return p
	}
	for services, want := range map[string]bool{
		"dnsmasq":           false,
		"sysctl":            false,
		"sysctl,dnsmasq":    false,
		"networking":        true,
		"nftables":          true,
		"nftables,dnsmasq":  true,
		"sysctl,networking": true,
		"routes":            true, // a wrong route can cut off an admin who comes from another network
		"routes,dnsmasq":    true,
		"wifi":              true, // restarting the access points drops every Wi-Fi device, an admin's included
		"wifi,dnsmasq":      true,
		"":                  false,
	} {
		var list []string
		if services != "" {
			list = strings.Split(services, ",")
		}
		if got := needsConfirm(plan(list...)); got != want {
			t.Errorf("needsConfirm(%q) = %v, want %v", services, got, want)
		}
	}
}

// --- commit ---

func TestCommitRefusesWhenNothingIsStaged(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	if err := e.svc.Commit(); !errors.Is(err, ErrNothingToCommit) {
		t.Errorf("Commit = %v, want ErrNothingToCommit", err)
	}
}

func TestCommitRefusesInvalidSettingsAndChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	before := e.file(translator.PathInterfaces)
	e.stage(func(c *config.NetworkConfig) { c.LanIP = "not-an-ip" })

	err := e.svc.Commit()
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("Commit = %v, want a ValidationError", err)
	}
	e.svc.Wait()

	if got := e.mgr.NetworkConfig().LanIP; got != "192.168.1.1" {
		t.Errorf("the active config changed to %q", got)
	}
	if e.file(translator.PathInterfaces) != before || len(e.run.log()) != 0 {
		t.Errorf("an invalid commit touched the system: %v", e.run.log())
	}
	if e.svc.Status().State != StateIdle {
		t.Error("the service should still be idle")
	}
	if !e.svc.Status().HasChanges {
		t.Error("the staged edit should still be pending so it can be fixed")
	}
}

func TestADhcpOnlyChangeIsAppliedWithoutAConfirmationWindow(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	e.stage(leaseChanged)
	e.commitAndWait()

	st := e.svc.Status()
	if st.State != StateIdle || st.HasChanges || st.Confirm != nil {
		t.Fatalf("status = %+v", st)
	}
	if st.Last == nil || st.Last.Kind != "committed" {
		t.Errorf("last = %+v", st.Last)
	}
	if e.confirmFileExists() || e.timers.count() != 0 {
		t.Error("a DHCP-only change must not arm the safety net")
	}
	if !strings.Contains(e.file(translator.PathDnsmasqDHCP), ",1d\n") {
		t.Error("the new lease time is not on the system")
	}
	if got := strings.Join(e.run.log(), "|"); got != "dnsmasq --test|systemctl restart dnsmasq" {
		t.Errorf("commands = %q", got)
	}
	if got := e.mgr.NetworkConfig().DHCP.LeaseTime; got != "1d" {
		t.Errorf("network.json lease = %q", got)
	}
}

func TestACommitThatMatchesTheSystemAlreadyJustSaves(t *testing.T) {
	e := newEnv(t)
	cfg := baseCfg()
	cfg.DHCP = nil // JSON had no dhcp section: unmanaged, the system is untouched
	e.baseline(cfg)

	e.stage(func(c *config.NetworkConfig) {
		c.DHCP = &config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"}
	})
	// first adopt what the wizard would have written, so the system already matches the new JSON
	d, _ := translator.Render(e.stagedConfig(), "ROUTER")
	for _, a := range d.Artifacts {
		if a.Path == translator.PathDnsmasqDHCP && !a.Remove {
			if err := e.root.WriteFile(a.Path, a.Content, a.Mode); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := e.svc.Commit(); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if len(e.run.log()) != 0 {
		t.Errorf("nothing needed doing, but ran %v", e.run.log())
	}
	if last := e.svc.Status().Last; last == nil || !strings.Contains(last.Message, "nothing was restarted") {
		t.Errorf("last = %+v", last)
	}
	if e.mgr.NetworkConfig().DHCP == nil {
		t.Error("the configuration should still have been saved")
	}
}

func (e *env) stagedConfig() config.NetworkConfig {
	c, _ := e.mgr.StagedNetworkConfig()
	return c
}

// --- the safety net ---

func TestALanChangeWaitsForConfirmationAndArmsTheSafetyNet(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	e.stage(lanMoved)
	e.commitAndWait()

	st := e.svc.Status()
	if st.State != StateAwaitingConfirm || st.Confirm == nil || st.Confirm.SecondsLeft != 60 {
		t.Fatalf("status = %+v", st)
	}
	if !e.confirmFileExists() {
		t.Error("the pending confirm must be saved to disk so a restart still reverts")
	}
	tm := e.timers.last()
	if tm == nil || tm.d != 60*time.Second || tm.stopped {
		t.Errorf("timer = %+v, want a live 60s countdown", tm)
	}
	if !strings.Contains(e.file(translator.PathInterfaces), "address 10.0.0.1") {
		t.Error("the change should be on the system while it waits for confirmation")
	}

	if info, err := os.Stat(filepath.Join(e.dir, "confirm.json")); err != nil {
		t.Errorf("confirm.json: %v", err)
	} else if info.Mode().Perm() != 0600 {
		t.Errorf("confirm.json mode = %o, want 0600", info.Mode().Perm())
	}

	// the countdown the UI shows follows the clock
	e.clock.advance(20 * time.Second)
	if got := e.svc.Status().Confirm.SecondsLeft; got != 40 {
		t.Errorf("seconds left = %d, want 40", got)
	}
}

func TestConfirmingKeepsTheChange(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)
	e.commitAndWait()

	if err := e.svc.Confirm(); err != nil {
		t.Fatal(err)
	}
	st := e.svc.Status()
	if st.State != StateIdle || st.Confirm != nil || st.Last == nil || st.Last.Kind != "confirmed" {
		t.Errorf("status = %+v", st)
	}
	if e.confirmFileExists() {
		t.Error("the pending record must be removed once confirmed")
	}
	if !e.timers.last().stopped {
		t.Error("the countdown must be stopped")
	}
	if !strings.Contains(e.file(translator.PathInterfaces), "address 10.0.0.1") || e.mgr.NetworkConfig().LanIP != "10.0.0.1" {
		t.Error("a confirmed change must stay")
	}
	if st.HasChanges {
		t.Error("nothing should be pending after a confirmed commit")
	}
}

func TestAnUnconfirmedChangeIsRevertedWhenTheDeadlineArrives(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	oldIfaces, oldDHCP, oldNft := e.file(translator.PathInterfaces), e.file(translator.PathDnsmasqDHCP), e.file(translator.PathNftables)
	e.stage(lanMoved)
	e.commitAndWait()
	e.run.reset()

	e.fire() // 60 seconds pass without a confirmation

	st := e.svc.Status()
	if st.State != StateIdle || st.Confirm != nil {
		t.Fatalf("status = %+v", st)
	}
	if st.Last == nil || st.Last.Kind != "reverted" || !strings.Contains(st.Last.Message, "not confirmed in time") {
		t.Errorf("last = %+v", st.Last)
	}
	if e.file(translator.PathInterfaces) != oldIfaces || e.file(translator.PathDnsmasqDHCP) != oldDHCP || e.file(translator.PathNftables) != oldNft {
		t.Error("the previous system files were not restored")
	}
	if got := e.mgr.NetworkConfig().LanIP; got != "192.168.1.1" {
		t.Errorf("network.json LAN IP = %q, want the previous 192.168.1.1", got)
	}
	if e.confirmFileExists() {
		t.Error("the pending record must be cleared after the revert")
	}
	// the services were brought back to the old configuration
	if got := strings.Join(e.run.log(), "|"); !strings.Contains(got, "systemctl restart networking") || !strings.Contains(got, "systemctl restart dnsmasq") {
		t.Errorf("revert commands = %q", got)
	}
	// and the rejected edit is back in the pending changes, not lost
	if !st.HasChanges {
		t.Error("the unconfirmed edit should be pending again so it can be corrected")
	}
}

func TestRevertNowWorksTheSameAsTheDeadline(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	oldIfaces := e.file(translator.PathInterfaces)
	e.stage(lanMoved)
	e.commitAndWait()

	if err := e.svc.Revert(); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()

	st := e.svc.Status()
	if st.State != StateIdle || st.Last == nil || st.Last.Kind != "reverted" || !strings.Contains(st.Last.Message, "at your request") {
		t.Errorf("status = %+v", st)
	}
	if e.file(translator.PathInterfaces) != oldIfaces {
		t.Error("files were not restored")
	}
	if !e.timers.last().stopped || e.confirmFileExists() {
		t.Error("the countdown and the record should be gone")
	}
}

// A timer left over from a window that was already confirmed must never revert anything
func TestAStaleTimerAfterConfirmationDoesNothing(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)
	e.commitAndWait()
	stale := e.timers.last()
	if err := e.svc.Confirm(); err != nil {
		t.Fatal(err)
	}

	stale.f() // the old timer fires anyway (it had already been scheduled)
	e.svc.Wait()

	if !strings.Contains(e.file(translator.PathInterfaces), "address 10.0.0.1") || e.mgr.NetworkConfig().LanIP != "10.0.0.1" {
		t.Error("a stale timer reverted a confirmed change")
	}
	if st := e.svc.Status(); st.State != StateIdle || st.Last.Kind != "confirmed" {
		t.Errorf("status = %+v", st)
	}
}

func TestAStaleTimerFromAnEarlierWindowCannotRevertALaterOne(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	e.stage(lanMoved)
	e.commitAndWait()
	first := e.timers.last()
	if err := e.svc.Confirm(); err != nil {
		t.Fatal(err)
	}

	e.stage(func(c *config.NetworkConfig) { c.WanInterface = "eth5" })
	e.commitAndWait()
	if e.svc.Status().State != StateAwaitingConfirm {
		t.Fatal("expected a second confirm window")
	}

	first.f() // the first window's timer fires during the second window
	e.svc.Wait()
	if e.svc.Status().State != StateAwaitingConfirm {
		t.Error("an earlier window's timer ended the later window")
	}
	if e.mgr.NetworkConfig().WanInterface != "eth5" {
		t.Error("the later change was reverted by a stale timer")
	}
}

func TestConfirmAndRevertNeedSomethingWaiting(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	if err := e.svc.Confirm(); !errors.Is(err, ErrNothingToConfirm) {
		t.Errorf("Confirm = %v", err)
	}
	if err := e.svc.Revert(); !errors.Is(err, ErrNothingToConfirm) {
		t.Errorf("Revert = %v", err)
	}

	e.stage(lanMoved)
	e.commitAndWait()
	if err := e.svc.Confirm(); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Confirm(); !errors.Is(err, ErrNothingToConfirm) {
		t.Errorf("a second Confirm = %v, want ErrNothingToConfirm", err)
	}
}

// --- failures ---

func TestAFailedApplyRestoresEverythingAndKeepsTheEditPending(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	oldIfaces, oldDHCP := e.file(translator.PathInterfaces), e.file(translator.PathDnsmasqDHCP)

	e.run.fail["dnsmasq --test"] = "dnsmasq: bad dhcp-range at line 2"
	e.stage(lanMoved)
	e.commitAndWait()

	st := e.svc.Status()
	if st.State != StateIdle || st.Last == nil || st.Last.Kind != "failed" {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(st.Last.Message, "bad dhcp-range") || !strings.Contains(st.Last.Message, "previous configuration was restored") {
		t.Errorf("message = %q", st.Last.Message)
	}
	if e.file(translator.PathInterfaces) != oldIfaces || e.file(translator.PathDnsmasqDHCP) != oldDHCP {
		t.Error("the system files were not restored")
	}
	if got := e.mgr.NetworkConfig().LanIP; got != "192.168.1.1" {
		t.Errorf("network.json LAN IP = %q, want the previous one", got)
	}
	if !st.HasChanges {
		t.Error("the user's edit should be pending again so they can fix it")
	}
	if e.confirmFileExists() || e.timers.count() != 0 {
		t.Error("a failed apply must not arm the safety net")
	}
}

func TestRestoreProblemsAfterAFailedApplyAreShown(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	e.run.fail["systemctl restart dnsmasq"] = "dnsmasq: failed to start"
	e.stage(lanMoved)
	e.commitAndWait()

	last := e.svc.Status().Last
	if last == nil || last.Kind != "failed" || len(last.Problems) == 0 || !strings.Contains(last.Message, "had problems") {
		t.Errorf("last = %+v", last)
	}
}

func TestIfTheSafetyNetCannotBeArmedTheChangeIsUndone(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	oldIfaces := e.file(translator.PathInterfaces)

	// confirm.json cannot be written: a directory is in the way
	if err := os.Mkdir(filepath.Join(e.dir, "confirm.json"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "confirm.json", "x"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	e.stage(lanMoved)
	e.commitAndWait()

	st := e.svc.Status()
	if st.State != StateIdle || st.Last == nil || st.Last.Kind != "failed" || !strings.Contains(st.Last.Message, "safety net could not be armed") {
		t.Fatalf("status = %+v", st)
	}
	if e.file(translator.PathInterfaces) != oldIfaces {
		t.Error("an unprotected change must be undone")
	}
	if e.mgr.NetworkConfig().LanIP != "192.168.1.1" || !st.HasChanges {
		t.Error("the config should be back to the previous one with the edit pending")
	}
}

func TestConfirmRefusesIfItCannotRecordIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)
	e.commitAndWait()

	// the record cannot be removed: a restart would revert a change the user accepted
	if err := os.Chmod(e.dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(e.dir, 0700)

	if err := e.svc.Confirm(); err == nil {
		t.Fatal("Confirm must fail when the confirmation cannot be recorded")
	}
	if e.svc.Status().State != StateAwaitingConfirm || e.timers.last().stopped {
		t.Error("the countdown must keep running after a failed confirm")
	}
}

// --- busy ---

func TestNoSecondChangeWhileOneIsBeingApplied(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.run.gate = make(chan struct{})
	e.run.entered = make(chan struct{}, 1)

	e.stage(lanMoved)
	if err := e.svc.Commit(); err != nil {
		t.Fatal(err)
	}
	<-e.run.entered // the apply is now inside "restart networking"

	if st := e.svc.Status(); st.State != StateApplying {
		t.Errorf("state = %s, want applying", st.State)
	}
	e.stage(func(c *config.NetworkConfig) { c.WanInterface = "eth5" })
	if err := e.svc.Commit(); !errors.Is(err, ErrBusy) {
		t.Errorf("a second Commit = %v, want ErrBusy", err)
	}
	if err := e.svc.Discard(); !errors.Is(err, ErrBusy) {
		t.Errorf("Discard while applying = %v, want ErrBusy", err)
	}

	close(e.run.gate)
	e.svc.Wait()
	if e.svc.Status().State != StateAwaitingConfirm {
		t.Errorf("state = %s", e.svc.Status().State)
	}
}

func TestNoCommitWhileAChangeAwaitsConfirmation(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)
	e.commitAndWait()

	e.stage(func(c *config.NetworkConfig) { c.WanInterface = "eth5" })
	if err := e.svc.Commit(); !errors.Is(err, ErrBusy) {
		t.Errorf("Commit during a confirm window = %v, want ErrBusy", err)
	}
}

func TestConcurrentCommitsStartExactlyOneApply(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(leaseChanged)

	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- e.svc.Commit() }()
	}
	wg.Wait()
	close(results)

	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrBusy), errors.Is(err, ErrNothingToCommit):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	e.svc.Wait()
	if ok != 1 {
		t.Errorf("%d commits succeeded, want exactly 1", ok)
	}
	restarts := 0
	for _, c := range e.run.log() {
		if c == "systemctl restart dnsmasq" {
			restarts++
		}
	}
	if restarts != 1 {
		t.Errorf("dnsmasq restarted %d times, want 1", restarts)
	}
}

func TestDiscardClearsTheStagedChanges(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)
	if !e.svc.Status().HasChanges {
		t.Fatal("expected pending changes")
	}
	if err := e.svc.Discard(); err != nil {
		t.Fatal(err)
	}
	if st := e.svc.Status(); st.HasChanges || st.ChangeCount != 0 {
		t.Errorf("status = %+v", st)
	}
}

// The banner follows real changes, not the existence of a stage file
func TestTheBannerFollowsRealChangesNotAFile(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())

	if e.svc.Status().HasChanges {
		t.Error("no banner expected on a clean system")
	}
	e.stage(lanMoved)
	if st := e.svc.Status(); !st.HasChanges || st.ChangeCount == 0 {
		t.Errorf("status = %+v, want the banner on", st)
	}
	e.stage(func(c *config.NetworkConfig) { *c = baseCfg() }) // put everything back
	if st := e.svc.Status(); st.HasChanges || st.ChangeCount != 0 {
		t.Errorf("status = %+v, want the banner off once the edit is reverted", st)
	}
}

// --- restart survival ---

func TestAnOpenConfirmWindowSurvivesARestartAndStillReverts(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	oldIfaces := e.file(translator.PathInterfaces)
	e.stage(lanMoved)
	e.commitAndWait()
	e.clock.advance(25 * time.Second)

	e.restart() // power cut or crash during the confirm window
	if err := e.svc.Resume(); err != nil {
		t.Fatal(err)
	}

	st := e.svc.Status()
	if st.State != StateAwaitingConfirm || st.Confirm == nil || st.Confirm.SecondsLeft != 35 {
		t.Fatalf("after the restart: %+v, want the countdown to continue with 35s left", st)
	}
	tm := e.timers.last()
	if tm == nil || tm.d != 35*time.Second {
		t.Fatalf("timer = %+v, want one for the remaining 35s", tm)
	}

	e.fire()
	if e.file(translator.PathInterfaces) != oldIfaces || e.mgr.NetworkConfig().LanIP != "192.168.1.1" {
		t.Error("the revert after a restart did not restore the previous state")
	}
	if e.confirmFileExists() {
		t.Error("the record should be gone")
	}
	if !e.svc.Status().HasChanges {
		t.Error("the edit should be pending again")
	}
}

func TestAConfirmWindowThatExpiredWhileTheDaemonWasDownRevertsAtOnce(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	oldIfaces := e.file(translator.PathInterfaces)
	e.stage(lanMoved)
	e.commitAndWait()

	e.clock.advance(10 * time.Minute) // the box was off for a long time
	e.restart()
	if err := e.svc.Resume(); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()

	st := e.svc.Status()
	if st.State != StateIdle || st.Last == nil || st.Last.Kind != "reverted" || !strings.Contains(st.Last.Message, "before the daemon restarted") {
		t.Fatalf("status = %+v", st)
	}
	if e.file(translator.PathInterfaces) != oldIfaces || e.mgr.NetworkConfig().LanIP != "192.168.1.1" {
		t.Error("the expired change was not reverted")
	}
	if e.timers.count() != 0 {
		t.Error("no countdown should be armed for an expired window")
	}
}

func TestConfirmingAfterARestartKeepsTheChange(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)
	e.commitAndWait()

	e.restart()
	if err := e.svc.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Confirm(); err != nil {
		t.Fatal(err)
	}
	if e.confirmFileExists() || e.mgr.NetworkConfig().LanIP != "10.0.0.1" {
		t.Error("confirming after a restart should keep the change and clear the record")
	}
}

func TestResumeWithNothingPendingDoesNothing(t *testing.T) {
	e := newEnv(t)
	if err := e.svc.Resume(); err != nil {
		t.Fatal(err)
	}
	if st := e.svc.Status(); st.State != StateIdle || e.timers.count() != 0 {
		t.Errorf("status = %+v", st)
	}
}

func TestAnUnreadableConfirmRecordIsDiscardedWithAnError(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.dir, "confirm.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Resume(); err == nil {
		t.Error("a corrupt record should be reported")
	}
	if e.confirmFileExists() {
		t.Error("a corrupt record must be removed so it does not block every start")
	}
	if e.svc.Status().State != StateIdle {
		t.Error("the service should be idle")
	}
}

func TestCloseKeepsTheConfirmRecordForTheNextStart(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.stage(lanMoved)
	e.commitAndWait()

	e.svc.Close()
	if !e.confirmFileExists() {
		t.Error("shutting down must leave the pending confirm on disk so the next start reverts it")
	}
	if !e.timers.last().stopped {
		t.Error("the timer should be stopped on close")
	}
}

// --- a box with nothing applied yet ---

func TestTheVeryFirstCommitCanBeRevertedFromTheSnapshot(t *testing.T) {
	e := newEnv(t) // nothing configured, nothing on the system

	e.stage(func(c *config.NetworkConfig) { *c = baseCfg() })
	e.commitAndWait()
	if e.svc.Status().State != StateAwaitingConfirm {
		t.Fatalf("status = %+v", e.svc.Status())
	}
	if !e.fileExists(translator.PathInterfaces) {
		t.Fatal("the first configuration should have been written")
	}

	e.fire() // not confirmed

	for _, p := range []string{translator.PathInterfaces, translator.PathNftables, translator.PathDnsmasqDHCP, translator.PathSysctl} {
		if e.fileExists(p) {
			t.Errorf("%s should be gone again: the previous state was an empty system", p)
		}
	}
	if got := e.mgr.NetworkConfig(); got.WanInterface != "" {
		t.Errorf("the active config should be empty again, got %+v", got)
	}
	if !e.svc.Status().HasChanges {
		t.Error("the whole first configuration should be pending again")
	}
}

package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSaveAndLoadRoundTripWithExpectedPermissions(t *testing.T) {
	dir := t.TempDir()

	authPath := filepath.Join(dir, "auth.json")
	netPath := filepath.Join(dir, "network.json")
	sysPath := filepath.Join(dir, "sys.json")

	auth := &AuthConfig{Users: []User{{Username: "admin", PasswordHash: "hash", Role: "admin"}}}
	if err := SaveAuthConfig(authPath, auth); err != nil {
		t.Fatal(err)
	}
	net := &NetworkConfig{WanInterface: "eth0", LanInterfaces: []string{"eth1", "eth2"}, LanIP: "192.168.1.1"}
	if err := SaveNetworkConfig(netPath, net); err != nil {
		t.Fatal(err)
	}
	sys := &SysConfig{Hostname: "peakroute", OperationMode: "ROUTER", Configured: true}
	if err := SaveSysConfig(sysPath, sys); err != nil {
		t.Fatal(err)
	}

	gotAuth, err := LoadAuthConfig(authPath)
	if err != nil || len(gotAuth.Users) != 1 || gotAuth.Users[0].Username != "admin" {
		t.Errorf("auth round trip = %+v, %v", gotAuth, err)
	}
	gotNet, err := LoadNetworkConfig(netPath)
	if err != nil || gotNet.WanInterface != "eth0" || len(gotNet.LanInterfaces) != 2 {
		t.Errorf("network round trip = %+v, %v", gotNet, err)
	}
	gotSys, err := LoadSysConfig(sysPath)
	if err != nil || gotSys.Hostname != "peakroute" || !gotSys.Configured {
		t.Errorf("sys round trip = %+v, %v", gotSys, err)
	}

	// The password file and the network file (which can hold Wi-Fi passphrases) must not be readable by other users
	for path, want := range map[string]os.FileMode{authPath: 0600, netPath: 0600, sysPath: 0644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", filepath.Base(path), got, want)
		}
	}

	if names := listDir(t, dir); len(names) != 3 {
		t.Errorf("expected exactly the 3 config files, found %v", names)
	}
}

// Saves can come from the web UI and the daemon at the same time; the file must always be a
// complete, valid document written by exactly one of them.
func TestConcurrentSavesNeverCorruptTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sys.json")

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := &SysConfig{Hostname: fmt.Sprintf("host-%02d", i), OperationMode: "ROUTER"}
			if err := SaveSysConfig(path, cfg); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent save failed: %v", err)
	}

	got, err := LoadSysConfig(path)
	if err != nil {
		t.Fatalf("file is not valid JSON after concurrent saves: %v", err)
	}
	if len(got.Hostname) != len("host-00") {
		t.Errorf("hostname = %q, want one writer's complete value", got.Hostname)
	}
	if names := listDir(t, dir); len(names) != 1 {
		t.Errorf("temp files left behind: %v", names)
	}
}

func TestStagedNetworkChangesSurviveCommit(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.StageNetwork(func(cfg *NetworkConfig) error {
		cfg.WanInterface = "eth9"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "network.json.staged")); err != nil {
		t.Errorf("staged file missing: %v", err)
	}

	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}

	live, err := LoadNetworkConfig(filepath.Join(dir, "network.json"))
	if err != nil || live.WanInterface != "eth9" {
		t.Errorf("committed config = %+v, %v", live, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "network.json.staged")); !os.IsNotExist(err) {
		t.Error("staged file should be gone after commit")
	}
	for _, name := range listDir(t, dir) {
		if filepath.Ext(name) == ".tmp" {
			t.Errorf("temp file left behind: %s", name)
		}
	}
}

func TestSessionTimeout(t *testing.T) {
	cases := []struct {
		name    string
		minutes int
		want    time.Duration
	}{
		{"unset uses the default", 0, 15 * time.Minute},
		{"negative uses the default", -5, 15 * time.Minute},
		{"configured value is used", 45, 45 * time.Minute},
		{"minimum is allowed", 1, time.Minute},
		{"maximum is allowed", 24 * 60, 24 * time.Hour},
		{"huge values are capped at 24h", 100000, 24 * time.Hour},
	}
	for _, c := range cases {
		mgr, err := NewManager(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		mgr.auth.SessionTimeoutMinutes = c.minutes
		if got := mgr.SessionTimeout(); got != c.want {
			t.Errorf("%s: SessionTimeout() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSessionTimeoutIsPickedUpFromAuthJSON(t *testing.T) {
	dir := t.TempDir()
	if err := SaveAuthConfig(filepath.Join(dir, "auth.json"), &AuthConfig{SessionTimeoutMinutes: 120}); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.SessionTimeout(); got != 2*time.Hour {
		t.Errorf("SessionTimeout() = %v, want 2h from auth.json", got)
	}
}

// --- Manager: single source of truth ---

func newTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	mgr, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	return mgr, dir
}

func stageLans(t *testing.T, mgr *Manager, lans ...string) {
	t.Helper()
	if err := mgr.StageNetwork(func(cfg *NetworkConfig) error {
		cfg.LanInterfaces = lans
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// readOnlyDir makes saves fail (skipped when running as root, which ignores permissions)
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
}

func TestSetupAdminCreatesAccountAndConfiguredFlag(t *testing.T) {
	mgr, dir := newTestManager(t)

	if mgr.IsConfigured() {
		t.Fatal("a fresh manager must not be configured")
	}
	if err := mgr.SetupAdmin(User{Username: "admin", PasswordHash: "h1", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	if !mgr.IsConfigured() {
		t.Error("IsConfigured() = false after setup")
	}
	if users := mgr.Users(); len(users) != 1 || users[0].Username != "admin" {
		t.Errorf("Users() = %+v", users)
	}
	if _, err := os.Stat(filepath.Join(dir, "configured")); err != nil {
		t.Errorf("configured flag not written: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "auth.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Errorf("auth.json = %v, %v; want mode 0600", info, err)
	}

	// A second setup must be refused and change nothing
	err = mgr.SetupAdmin(User{Username: "intruder", PasswordHash: "h2", Role: "admin"})
	if !errors.Is(err, ErrAlreadyConfigured) {
		t.Errorf("second SetupAdmin error = %v, want ErrAlreadyConfigured", err)
	}
	if users := mgr.Users(); len(users) != 1 || users[0].Username != "admin" {
		t.Errorf("a refused setup changed the users: %+v", users)
	}
}

// Two first-run requests used to be able to pass the "not configured" check together, so the
// second admin overwrote the first. Exactly one may win.
func TestConcurrentSetupAdminHasExactlyOneWinner(t *testing.T) {
	mgr, dir := newTestManager(t)

	const attempts = 20
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- mgr.SetupAdmin(User{Username: fmt.Sprintf("user%02d", i), PasswordHash: "h", Role: "admin"})
		}(i)
	}
	wg.Wait()
	close(results)

	wins := 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrAlreadyConfigured):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d setups succeeded, want exactly 1", wins)
	}

	users := mgr.Users()
	onDisk, err := LoadAuthConfig(filepath.Join(dir, "auth.json"))
	if err != nil || len(users) != 1 || len(onDisk.Users) != 1 || onDisk.Users[0].Username != users[0].Username {
		t.Errorf("memory has %+v but disk has %+v (%v)", users, onDisk, err)
	}
}

func TestConfiguredStateSurvivesARestart(t *testing.T) {
	mgr, dir := newTestManager(t)
	if err := mgr.SetupAdmin(User{Username: "admin", PasswordHash: "h", Role: "admin"}); err != nil {
		t.Fatal(err)
	}

	again, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !again.IsConfigured() || len(again.Users()) != 1 {
		t.Errorf("after restart: configured=%v users=%+v", again.IsConfigured(), again.Users())
	}
}

func TestCorruptAuthFileIsTreatedAsEmptyNotACrash(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}

	mgr, err := NewManager(dir)
	if err != nil {
		t.Fatalf("NewManager failed on a corrupt auth.json: %v", err)
	}
	if len(mgr.Users()) != 0 {
		t.Errorf("Users() = %+v, want none", mgr.Users())
	}
}

func TestAccessorsReturnCopiesNotLiveState(t *testing.T) {
	mgr, _ := newTestManager(t)
	if err := mgr.SetupAdmin(User{Username: "admin", PasswordHash: "h", Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	stageLans(t, mgr, "eth1", "eth2")
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}

	mgr.Users()[0].Username = "hacked"
	mgr.NetworkConfig().LanInterfaces[0] = "hacked"
	staged, _ := mgr.StagedNetworkConfig()
	staged.LanInterfaces[1] = "hacked"

	if got := mgr.Users()[0].Username; got != "admin" {
		t.Errorf("mutating Users() changed the manager: %q", got)
	}
	if got := mgr.NetworkConfig().LanInterfaces; got[0] != "eth1" || got[1] != "eth2" {
		t.Errorf("mutating returned network config changed the manager: %v", got)
	}
}

// Staged and live configs used to share slice memory, so editing an element of the candidate
// silently edited the running configuration before any commit.
func TestStagedChangesDoNotLeakIntoLiveConfigBeforeCommit(t *testing.T) {
	mgr, _ := newTestManager(t)
	stageLans(t, mgr, "eth1", "eth2")
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}

	if err := mgr.StageNetwork(func(cfg *NetworkConfig) error {
		cfg.LanInterfaces[0] = "changed" // edits an element in place
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got := mgr.NetworkConfig().LanInterfaces[0]; got != "eth1" {
		t.Errorf("live config changed to %q before commit", got)
	}
	if staged, has := mgr.StagedNetworkConfig(); !has || staged.LanInterfaces[0] != "changed" {
		t.Errorf("staged = %+v, has=%v; want the edit", staged, has)
	}

	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
	if got := mgr.NetworkConfig().LanInterfaces[0]; got != "changed" {
		t.Errorf("live config after commit = %q, want changed", got)
	}
}

func TestDiscardRestoresTheActiveConfig(t *testing.T) {
	mgr, dir := newTestManager(t)
	stageLans(t, mgr, "eth1")
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
	stageLans(t, mgr, "eth9")

	if err := mgr.DiscardNetwork(); err != nil {
		t.Fatal(err)
	}
	if staged, has := mgr.StagedNetworkConfig(); has || staged.LanInterfaces[0] != "eth1" {
		t.Errorf("after discard staged = %+v, has=%v", staged, has)
	}
	if _, err := os.Stat(filepath.Join(dir, "network.json.staged")); !os.IsNotExist(err) {
		t.Error("staged file should be removed by discard")
	}
}

func TestFailedEditsLeaveStateUntouched(t *testing.T) {
	mgr, _ := newTestManager(t)
	stageLans(t, mgr, "eth1")
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("rejected")

	// fn mutates then fails: nothing may stick
	if err := mgr.StageNetwork(func(cfg *NetworkConfig) error {
		cfg.LanInterfaces[0] = "half-applied"
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("StageNetwork error = %v, want %v", err, boom)
	}
	if _, has := mgr.StagedNetworkConfig(); has {
		t.Error("a failed StageNetwork created a candidate")
	}
	if got := mgr.NetworkConfig().LanInterfaces[0]; got != "eth1" {
		t.Errorf("live config = %q after a failed stage", got)
	}

	if err := mgr.UpdateAuth(func(cfg *AuthConfig) error {
		cfg.SessionTimeoutMinutes = 999
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if got := mgr.SessionTimeout(); got != 15*time.Minute {
		t.Errorf("a failed UpdateAuth changed the timeout to %v", got)
	}

	if err := mgr.UpdateSys(func(cfg *SysConfig) error {
		cfg.Hostname = "half-applied"
		return boom
	}); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if got := mgr.SysConfig().Hostname; got == "half-applied" {
		t.Error("a failed UpdateSys changed the hostname")
	}
}

// If the save fails, memory must not move ahead of disk
func TestFailedSavesLeaveMemoryMatchingDisk(t *testing.T) {
	mgr, dir := newTestManager(t)
	stageLans(t, mgr, "eth1")
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
	stageLans(t, mgr, "eth2") // a pending candidate

	readOnlyDir(t, dir)

	if err := mgr.CommitNetwork(); err == nil {
		t.Fatal("expected CommitNetwork to fail in a read-only directory")
	}
	if got := mgr.NetworkConfig().LanInterfaces[0]; got != "eth1" {
		t.Errorf("live config = %q after a failed commit, want eth1", got)
	}
	if staged, has := mgr.StagedNetworkConfig(); !has || staged.LanInterfaces[0] != "eth2" {
		t.Errorf("the candidate was lost by a failed commit: %+v has=%v", staged, has)
	}

	if err := mgr.StageNetwork(func(cfg *NetworkConfig) error { cfg.LanInterfaces = []string{"eth3"}; return nil }); err == nil {
		t.Fatal("expected StageNetwork to fail in a read-only directory")
	}
	if staged, _ := mgr.StagedNetworkConfig(); staged.LanInterfaces[0] != "eth2" {
		t.Errorf("candidate = %v after a failed stage, want it unchanged", staged.LanInterfaces)
	}

	if err := mgr.UpdateAuth(func(cfg *AuthConfig) error { cfg.SessionTimeoutMinutes = 99; return nil }); err == nil {
		t.Fatal("expected UpdateAuth to fail in a read-only directory")
	}
	if got := mgr.SessionTimeout(); got != 15*time.Minute {
		t.Errorf("timeout = %v after a failed UpdateAuth", got)
	}
}

func TestChangeNotificationsCarryCopies(t *testing.T) {
	mgr, _ := newTestManager(t)
	stageLans(t, mgr, "eth1", "eth2")
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}

	select {
	case cfg := <-mgr.OnNetworkChange():
		cfg.LanInterfaces[0] = "mutated-by-listener"
	case <-time.After(time.Second):
		t.Fatal("no network change notification after commit")
	}

	if got := mgr.NetworkConfig().LanInterfaces[0]; got != "eth1" {
		t.Errorf("a listener mutated the live config: %q", got)
	}
}

func TestSysDefaultsAndBuildMetadataSync(t *testing.T) {
	mgr, dir := newTestManager(t)
	sysPath := filepath.Join(dir, "sys.json")

	if got := mgr.SysConfig(); got.Hostname != "peakroute-gateway" || got.OperationMode != "ROUTER" {
		t.Errorf("defaults = %+v", got)
	}
	if _, err := os.Stat(sysPath); !os.IsNotExist(err) {
		t.Fatal("sys.json should not exist before the first sync")
	}

	if err := mgr.SyncBuildMetadata(); err != nil {
		t.Fatal(err)
	}
	onDisk, err := LoadSysConfig(sysPath)
	if err != nil || onDisk.PeakRouteBuild == "" || onDisk.Arch == "" || onDisk.Kernel == "" {
		t.Fatalf("metadata not written: %+v, %v", onDisk, err)
	}

	// Unchanged metadata must not rewrite the file (flash wear, and pointless churn)
	before, _ := os.Stat(sysPath)
	time.Sleep(25 * time.Millisecond)
	if err := mgr.SyncBuildMetadata(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(sysPath)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("SyncBuildMetadata rewrote sys.json although nothing changed")
	}

	// Stale metadata is refreshed
	if err := mgr.UpdateSys(func(cfg *SysConfig) error { cfg.Arch = "stale-arch"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SyncBuildMetadata(); err != nil {
		t.Fatal(err)
	}
	if got := mgr.SysConfig().Arch; got == "stale-arch" {
		t.Error("SyncBuildMetadata did not refresh a stale value")
	}
}

// Readers on request goroutines and writers must not race (run with -race)
func TestConcurrentReadsAndWritesDoNotRace(t *testing.T) {
	mgr, _ := newTestManager(t)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = mgr.NetworkConfig()
					_ = mgr.Users()
					_ = mgr.SysConfig()
					_, _ = mgr.StagedNetworkConfig()
					_ = mgr.IsConfigured()
					_ = mgr.SessionTimeout()
				}
			}
		}()
	}

	for i := 0; i < 30; i++ {
		stageLans(t, mgr, fmt.Sprintf("eth%d", i))
		if i%3 == 0 {
			if err := mgr.CommitNetwork(); err != nil {
				t.Fatal(err)
			}
		}
		_ = mgr.UpdateAuth(func(cfg *AuthConfig) error { cfg.SessionTimeoutMinutes = 10 + i; return nil })
	}
	close(stop)
	wg.Wait()
}

// The version shown everywhere is the one the build was named with; only an unnamed build adds the commit it came from.
func TestTheVersionShownIsTheOneTheBuildWasNamedWith(t *testing.T) {
	old := BuildVersion
	defer func() { BuildVersion = old }()
	BuildVersion = "v9.8.7-beta"
	if got := GetBinaryVersion(); got != "v9.8.7-beta" {
		t.Fatalf("a named build shows %q", got)
	}
	BuildVersion = defaultBuildVersion
	if got := GetBinaryVersion(); !strings.HasPrefix(got, defaultBuildVersion) {
		t.Fatalf("an unnamed build shows %q", got)
	}
}

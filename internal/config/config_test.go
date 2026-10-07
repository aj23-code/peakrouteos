package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
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

	// The password file must not be readable by other users
	for path, want := range map[string]os.FileMode{authPath: 0600, netPath: 0644, sysPath: 0644} {
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

package update

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A lab is a device in a directory: a kernel command line, a release file, the two slots as files, and a program runner that plays
// grub-editenv, mount, e2fsck, resize2fs and tune2fs and writes down what it was asked.

type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

type lab struct {
	t        *testing.T
	dir      string
	s        *Service
	mu       sync.Mutex
	grub     map[string]string
	calls    []string
	failing  map[string]error // program name -> what it returns
	reboots  chan struct{}
	backups  int
	editable bool
}

func newLab(t *testing.T, running Slot, trial bool, kept Slot) *lab {
	t.Helper()
	dir := t.TempDir()
	l := &lab{t: t, dir: dir, grub: map[string]string{"slot": string(kept), "trial": ""}, failing: map[string]error{}, reboots: make(chan struct{}, 4), editable: true}
	cmd := "BOOT_IMAGE=/vmlinuz root=LABEL=peakroute-root-" + string(running) + " ro quiet peakroute.slot=" + string(running)
	if trial {
		cmd += " peakroute.trial=1"
	} else {
		cmd += " peakroute.trial="
	}
	write := func(name, data string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cmdline := write("cmdline", cmd+"\n")
	release := write("release", "PEAKROUTE_VERSION=v0.9.5-alpha\nPEAKROUTE_BUILT=2026-10-01T00:00:00Z\nPEAKROUTE_LAYOUT=1\nPEAKROUTE_ARCH=amd64\nDEBIAN_VERSION=13\n")
	write("state", "persistent\n")
	for _, n := range []string{"slot-a", "slot-b"} {
		if err := os.WriteFile(filepath.Join(dir, n), bytes.Repeat([]byte{0xAA}, 8<<20), 0644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(dir, "config")
	os.MkdirAll(cfg, 0700)
	l.s = &Service{started: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Env: Env{
		ConfigDir: cfg, ReleasePath: release, CmdlinePath: cmdline, GrubEnv: filepath.Join(dir, "grubenv"), ESPMount: "/boot/efi",
		StatePath:  filepath.Join(dir, "state"),
		SlotDevice: func(sl Slot) string { return filepath.Join(dir, "slot-"+string(sl)) },
		Mounted:    func() []string { return []string{"/dev/vda3", "tmpfs"} },
		Run:        l.run,
		Reboot:     func() { l.reboots <- struct{}{} },
		Editable:   func() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.editable },
		Backup:     func() (string, error) { l.mu.Lock(); l.backups++; l.mu.Unlock(); return "123", nil },
		Now:        func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
		Version:    "v0.9.5-alpha", Arch: "amd64", Kernel: func() string { return "6.12.0" }, DefaultRepo: "owner/repo",
		RebootDelay: time.Millisecond, AllowHTTP: true,
	}}
	return l
}

func (l *lab) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, name+" "+strings.Join(args, " "))
	if err := l.failing[name]; err != nil {
		return []byte("it failed"), err
	}
	if name == "grub-editenv" && len(args) >= 2 {
		switch args[1] {
		case "list":
			var b strings.Builder
			for k, v := range l.grub {
				fmt.Fprintf(&b, "%s=%s\n", k, v)
			}
			return []byte(b.String()), nil
		case "set":
			for _, kv := range args[2:] {
				k, v, _ := strings.Cut(kv, "=")
				l.grub[k] = v
			}
		}
	}
	return nil, nil
}

func (l *lab) called(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (l *lab) env(k string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.grub[k]
}

func (l *lab) upload(version string, rootfs []byte, mod func(*Manifest)) error {
	_, err := l.s.Upload(context.Background(), bytes.NewReader(bundleOf(l.t, version, rootfs, mod)), 0)
	return err
}

func (l *lab) slot(n Slot) []byte {
	b, _ := os.ReadFile(filepath.Join(l.dir, "slot-"+string(n)))
	return b
}

func (l *lab) waitReboot() {
	l.t.Helper()
	select {
	case <-l.reboots:
	case <-time.After(3 * time.Second):
		l.t.Fatal("the device did not restart")
	}
}

func (l *lab) noReboot() {
	l.t.Helper()
	select {
	case <-l.reboots:
		l.t.Fatal("the device restarted")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestAnUploadedBundleIsWrittenCheckedLabelledAndStaged(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	rootfs := fakeRootfs(3<<20 + 77)
	if err := l.upload("v0.10.0", rootfs, nil); err != nil {
		t.Fatal(err)
	}
	if got := l.slot(SlotB); !bytes.Equal(got[:len(rootfs)], rootfs) {
		t.Fatal("slot B does not hold the system")
	}
	if got := l.slot(SlotA); got[0] != 0xAA || got[len(got)-1] != 0xAA {
		t.Fatal("the running slot was touched")
	}
	// check, grow, then name; in that order
	var order []string
	for _, c := range l.calls {
		for _, p := range []string{"e2fsck", "resize2fs", "tune2fs"} {
			if strings.HasPrefix(c, p) {
				order = append(order, c)
			}
		}
	}
	dev := filepath.Join(l.dir, "slot-b")
	want := []string{"e2fsck -f -p " + dev, "resize2fs " + dev, "tune2fs -L peakroute-root-b " + dev}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("the slot was finished with %q, want %q", order, want)
	}
	st := l.s.State(context.Background())
	if st.Staged == nil || st.Staged.Slot != SlotB || st.Staged.Version != "v0.10.0" || st.Staged.From != "v0.9.5-alpha" {
		t.Fatalf("staged: %+v", st.Staged)
	}
	if st.OtherVer != "v0.10.0" || st.Job == nil || st.Job.Phase != "done" {
		t.Fatalf("state: other %q, job %+v", st.OtherVer, st.Job)
	}
	if l.env("trial") != "" || l.env("slot") != "a" {
		t.Fatal("the boot settings changed before the restart was asked for")
	}
	l.noReboot()
}

func TestRestartSetsATrialMakesACopyAndRestarts(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	if err := l.s.Restart(context.Background()); err == nil {
		t.Fatal("restarted with nothing to start")
	}
	if err := l.upload("v0.10.0", fakeRootfs(1<<20), nil); err != nil {
		t.Fatal(err)
	}
	if err := l.s.Restart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.env("trial") != "b" || l.env("slot") != "a" {
		t.Fatalf("grub environment: slot=%q trial=%q", l.env("slot"), l.env("trial"))
	}
	if l.backups != 1 {
		t.Errorf("%d copies of the settings", l.backups)
	}
	l.waitReboot()
	// the boot partition is writable only for the change
	l.mu.Lock()
	calls := strings.Join(l.calls, "\n")
	l.mu.Unlock()
	rw, ro := strings.Index(calls, "mount -o remount,rw /boot/efi"), strings.Index(calls, "mount -o remount,ro /boot/efi")
	set := strings.Index(calls, "grub-editenv "+l.s.GrubEnv+" set trial=b")
	if rw < 0 || set < rw || ro < set {
		t.Fatalf("the boot partition was not opened for the change and closed again:\n%s", calls)
	}
}

func TestRestartIsRefusedWhileAConfigurationChangeWaits(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	if err := l.upload("v0.10.0", fakeRootfs(1<<20), nil); err != nil {
		t.Fatal(err)
	}
	l.editable = false
	if err := l.s.Restart(context.Background()); err == nil {
		t.Fatal("restarted while a change waits to be confirmed")
	}
	if l.env("trial") != "" {
		t.Fatal("a trial was set")
	}
	l.noReboot()
}

func TestKeepingTheTrialMakesItTheStartingSlot(t *testing.T) {
	l := newLab(t, SlotB, true, SlotA) // the trial has started: running b, kept a
	l.s.st = stored{Settings: Settings{AutoCheck: true}, Versions: map[Slot]string{SlotA: "v0.9.5-alpha", SlotB: "v0.10.0"}, Staged: &Staged{Slot: SlotB, Version: "v0.10.0", From: "v0.9.5-alpha"}}
	l.s.loaded = true
	st := l.s.State(context.Background())
	if !st.OnTrial || st.TrialLeft <= 0 || st.TrialLeft > TrialSeconds {
		t.Fatalf("on trial %v, %d s left", st.OnTrial, st.TrialLeft)
	}
	if st.Staged != nil {
		t.Error("the version on trial is still shown as waiting to be started")
	}
	if st.OtherSlot != SlotA || st.OtherVer != "v0.9.5-alpha" {
		t.Errorf("during the trial the other slot should hold the version before: %q %q", st.OtherSlot, st.OtherVer)
	}
	if err := l.s.Keep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.env("slot") != "b" || l.env("trial") != "" {
		t.Fatalf("slot=%q trial=%q", l.env("slot"), l.env("trial"))
	}
	st = l.s.State(context.Background())
	if st.OnTrial || st.OtherVer != "v0.9.5-alpha" || st.Staged != nil {
		t.Fatalf("after keeping: %+v", st)
	}
	// and there is nothing more to keep
	if err := l.s.Keep(context.Background()); err == nil {
		t.Fatal("kept twice")
	}
}

func TestGoingBackFromATrialJustRestarts(t *testing.T) {
	l := newLab(t, SlotB, true, SlotA)
	if err := l.s.GoBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.waitReboot()
	if l.env("slot") != "a" || l.env("trial") != "" {
		t.Fatalf("the boot settings were changed: slot=%q trial=%q", l.env("slot"), l.env("trial"))
	}
}

func TestGoingBackFromAKeptVersionTriesTheOtherSlot(t *testing.T) {
	l := newLab(t, SlotB, false, SlotB)
	if err := l.s.GoBack(context.Background()); err == nil {
		t.Fatal("went back to an unknown version")
	}
	l.s.st = stored{Versions: map[Slot]string{SlotA: "v0.9.5-alpha", SlotB: "v0.9.5-alpha"}}
	l.s.loaded = true
	if err := l.s.GoBack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if l.env("trial") != "a" || l.env("slot") != "b" {
		t.Fatalf("slot=%q trial=%q", l.env("slot"), l.env("trial"))
	}
	l.waitReboot()
	if st := l.s.st.Staged; st == nil || st.Version != "v0.9.5-alpha" || st.From != "v0.9.5-alpha" {
		// (the lab's release file says it is running v0.9.5-alpha)
		t.Fatalf("staged: %+v", st)
	}
}

func TestNothingIsInstalledWhileAVersionIsOnTrial(t *testing.T) {
	l := newLab(t, SlotB, true, SlotA)
	if err := l.upload("v0.11.0", fakeRootfs(1<<20), nil); err == nil || !strings.Contains(err.Error(), "on trial") {
		t.Fatalf("got %v", err)
	}
	if got := l.slot(SlotA); got[0] != 0xAA {
		t.Fatal("slot A, the one to go back to, was written")
	}
}

func TestADeviceWithoutSlotsCannotBeUpdated(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	os.WriteFile(l.s.CmdlinePath, []byte("root=/dev/sda1 ro\n"), 0644)
	st := l.s.State(context.Background())
	if st.Supported || st.Problem == "" {
		t.Fatalf("supported: %+v", st)
	}
	if err := l.upload("v0.10.0", fakeRootfs(1<<20), nil); err == nil {
		t.Fatal("installed on a device without slots")
	}
	if got := l.slot(SlotB); got[0] != 0xAA {
		t.Fatal("a slot was written")
	}
}

func TestABundleThatDoesNotFitIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	for name, mod := range map[string]func(*Manifest){
		"another processor": func(m *Manifest) { m.Arch = "arm64" },
		"another layout":    func(m *Manifest) { m.Layout = 2 },
	} {
		if err := l.upload("v0.10.0", fakeRootfs(1<<20), mod); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// 8 MiB slot, 9 MiB system
	if err := l.upload("v0.10.0", fakeRootfs(9<<20), nil); err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("a system bigger than the slot: %v", err)
	}
	if got := l.slot(SlotB); !bytes.Equal(got, bytes.Repeat([]byte{0xAA}, 8<<20)) {
		t.Fatal("the slot was touched")
	}
	if l.called("tune2fs") != 0 {
		t.Fatal("a slot was named")
	}
}

func TestADamagedBundleLeavesTheOtherSlotUnnamedAndForgotten(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	if err := l.upload("v0.9.9", fakeRootfs(1<<20), nil); err != nil {
		t.Fatal(err)
	}
	if l.s.State(context.Background()).OtherVer != "v0.9.9" {
		t.Fatal("setup")
	}
	bad := bundleOf(t, "v0.10.0", fakeRootfs(2<<20), nil)
	bad[len(bad)/2] ^= 0xFF
	before := l.called("tune2fs")
	if _, err := l.s.Upload(context.Background(), bytes.NewReader(bad), 0); err == nil {
		t.Fatal("a damaged bundle was accepted")
	}
	if l.called("tune2fs") != before {
		t.Error("the slot was named although it was not written correctly")
	}
	st := l.s.State(context.Background())
	if st.OtherVer != "" || st.Staged != nil {
		t.Errorf("the other slot is still offered: %q %+v", st.OtherVer, st.Staged)
	}
	if st.Job == nil || st.Job.Phase != "failed" || st.Job.Error == "" {
		t.Errorf("job: %+v", st.Job)
	}
	if len(st.History) == 0 || st.History[0].Action != "failed" {
		t.Errorf("history: %+v", st.History)
	}
	if err := l.s.Restart(context.Background()); err == nil {
		t.Fatal("restarted into a slot that was not written")
	}
}

func TestACheckThatFindsErrorsMattersButCorrectedErrorsDoNot(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	l.failing["e2fsck"] = exitStatus(1) // errors corrected
	if err := l.upload("v0.10.0", fakeRootfs(1<<20), nil); err != nil {
		t.Fatalf("corrected errors failed the update: %v", err)
	}
	l.failing["e2fsck"] = exitStatus(4) // errors left
	before := l.called("tune2fs")
	if err := l.upload("v0.10.1", fakeRootfs(1<<20), nil); err == nil {
		t.Fatal("a file system with errors was accepted")
	}
	if l.called("tune2fs") != before {
		t.Error("it was named")
	}
}

func TestOnlyOneUpdateRunsAtATime(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	l.s.load()
	l.s.job = &Job{Phase: "writing"}
	if err := l.upload("v0.10.0", fakeRootfs(1<<20), nil); err != ErrBusy {
		t.Fatalf("got %v", err)
	}
	l.s.job = nil
	l.editable = false
	if err := l.upload("v0.10.0", fakeRootfs(1<<20), nil); err != ErrNotEditable {
		t.Fatalf("got %v", err)
	}
}

func TestTheRunningSlotIsNeverTheOneWrittenTo(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	l.s.Mounted = func() []string { return []string{filepath.Join(l.dir, "slot-b")} }
	if err := l.upload("v0.10.0", fakeRootfs(1<<20), nil); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("got %v", err)
	}
}

func TestSettingsAreKeptAndTheSourceIsNotOneOfThem(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	if err := l.s.SetSettings(Settings{Prereleases: true, AutoCheck: false}); err != nil {
		t.Fatal(err)
	}
	// a new service reads them back from disk
	s2 := &Service{Env: l.s.Env}
	if got := s2.Settings(); !got.Prereleases || got.AutoCheck {
		t.Fatalf("settings after a restart: %+v", got)
	}
	// a repository left in an older update.json is ignored: the source is the one built in
	os.WriteFile(filepath.Join(l.s.ConfigDir, storeFile), []byte(`{"repo":"someone/else","prereleases":false,"auto_check":true}`), 0600)
	s3 := &Service{Env: l.s.Env}
	if s3.Settings().Prereleases != false || s3.repo() != "owner/repo" {
		t.Fatalf("source %q", s3.repo())
	}
	// nothing set: the daily check is on, full releases only
	l2 := newLab(t, SlotA, false, SlotA)
	if got := l2.s.Settings(); got.Prereleases || !got.AutoCheck {
		t.Fatalf("defaults: %+v", got)
	}
}

func TestTheHistoryIsKeptShort(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	l.s.load()
	for i := 0; i < keepEvents+10; i++ {
		l.s.note("failed", fmt.Sprintf("v0.0.%d", i), "x")
	}
	if len(l.s.st.History) != keepEvents || l.s.st.History[len(l.s.st.History)-1].Version != fmt.Sprintf("v0.0.%d", keepEvents+9) {
		t.Fatalf("%d entries, last %q", len(l.s.st.History), l.s.st.History[len(l.s.st.History)-1].Version)
	}
	st := l.s.State(context.Background())
	if st.History[0].Version != fmt.Sprintf("v0.0.%d", keepEvents+9) {
		t.Errorf("newest first: %q", st.History[0].Version)
	}
}

// ---- GitHub ----------------------------------------------------------------------------------------------------------------

type fakeGitHub struct {
	srv      *httptest.Server
	releases string
	files    map[string][]byte // path -> body
	hits     map[string]int
	mu       sync.Mutex
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{files: map[string][]byte{}, hits: map[string]int{}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.hits[r.URL.Path]++
		g.mu.Unlock()
		if r.URL.Path == "/repos/owner/repo/releases" {
			if g.releases == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(strings.ReplaceAll(g.releases, "@@", g.srv.URL)))
			return
		}
		if b, ok := g.files[r.URL.Path]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

const releasesJSON = `[
 {"tag_name":"v0.9.0","draft":false,"prerelease":false,"body":"old","assets":[{"name":"peakroute-os-v0.9.0.update","size":10,"browser_download_url":"@@/dl/v0.9.0.update"},{"name":"peakroute-os-v0.9.0.update.sha256","size":90,"browser_download_url":"@@/dl/v0.9.0.update.sha256"}]},
 {"tag_name":"v0.11.0","draft":true,"prerelease":false,"assets":[{"name":"peakroute-os-v0.11.0.update","browser_download_url":"@@/dl/v0.11.0.update"},{"name":"peakroute-os-v0.11.0.update.sha256","browser_download_url":"@@/dl/v0.11.0.update.sha256"}]},
 {"tag_name":"v0.10.0","name":"Ten","draft":false,"prerelease":false,"body":"Updates are here.","published_at":"2026-10-19T10:00:00Z","assets":[{"name":"peakroute-os-v0.10.0.update","size":1234,"browser_download_url":"@@/dl/v0.10.0.update"},{"name":"peakroute-os-v0.10.0.update.sha256","size":90,"browser_download_url":"@@/dl/v0.10.0.update.sha256"},{"name":"peakroute-os.img.gz","browser_download_url":"@@/dl/other"}]},
 {"tag_name":"v0.10.1-beta","draft":false,"prerelease":true,"assets":[{"name":"peakroute-os-v0.10.1-beta.update","browser_download_url":"@@/dl/v0.10.1-beta.update","digest":"sha256:` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `"}]},
 {"tag_name":"v0.12.0","draft":false,"prerelease":false,"assets":[{"name":"peakroute-os-v0.12.0.update","browser_download_url":"@@/dl/v0.12.0.update"}]},
 {"tag_name":"nightly","draft":false,"prerelease":false,"assets":[]}
]`

func (g *fakeGitHub) point(l *lab) {
	l.s.APIBase = g.srv.URL
	l.s.HTTP = g.srv.Client()
}

func TestTheNewestReleaseWithAnUpdateFileAndAChecksumIsFound(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = releasesJSON
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)

	rel, err := l.s.FindRelease(context.Background(), "owner/repo", false, "v0.9.5-alpha")
	if err != nil {
		t.Fatal(err)
	}
	// not the draft v0.11.0, not the pre-release, not v0.12.0 (no checksum), not the tag with no update file
	if rel.Version != "v0.10.0" || !rel.Newer || rel.Notes != "Updates are here." || rel.AssetName != "peakroute-os-v0.10.0.update" ||
		rel.ChecksumURL == "" || rel.Size != 1234 {
		t.Fatalf("release: %+v", rel)
	}
	rel, err = l.s.FindRelease(context.Background(), "owner/repo", true, "v0.9.5-alpha")
	if err != nil || rel.Version != "v0.10.1-beta" || !rel.Prerelease || rel.Checksum == "" {
		t.Fatalf("with pre-releases: %+v %v", rel, err)
	}
	rel, err = l.s.FindRelease(context.Background(), "owner/repo", false, "v0.10.0")
	if err != nil || rel.Newer {
		t.Fatalf("already running it: %+v %v", rel, err)
	}
}

func TestGitHubTroubleIsExplained(t *testing.T) {
	g := newFakeGitHub(t)
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	if _, err := l.s.FindRelease(context.Background(), "owner/repo", false, "v1"); err == nil || !strings.Contains(err.Error(), "no repository") {
		t.Errorf("not found: %v", err)
	}
	g.releases = `[{"tag_name":"v1.0.0","assets":[]}]`
	if _, err := l.s.FindRelease(context.Background(), "owner/repo", false, "v0.1.0"); err == nil || !strings.Contains(err.Error(), "no release") {
		t.Errorf("no update file: %v", err)
	}
	g.releases = `not json`
	if _, err := l.s.FindRelease(context.Background(), "owner/repo", false, "v0.1.0"); err == nil {
		t.Error("garbage was accepted")
	}
	if _, err := l.s.FindRelease(context.Background(), "../etc", false, "v0.1.0"); err == nil {
		t.Error("a bad repository name was accepted")
	}
	// a link that is not https is not followed (outside the tests)
	l.s.AllowHTTP = false
	g.releases = releasesJSON
	if _, err := l.s.FindRelease(context.Background(), "owner/repo", false, "v0.1.0"); err == nil {
		t.Error("http addresses were used")
	}
}

func TestCheckNowRemembersTheAnswerAndTheSettingsDecideTheSource(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = releasesJSON
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	c := l.s.CheckNow(context.Background())
	if c.Error != "" || c.Release == nil || c.Release.Version != "v0.10.0" {
		t.Fatalf("check: %+v", c)
	}
	st := l.s.State(context.Background())
	if !st.Available || st.Check == nil || st.Check.Release.Version != "v0.10.0" {
		t.Fatalf("state: %+v", st.Check)
	}
	// the other channel: what was found belongs to the first
	if err := l.s.SetSettings(Settings{Prereleases: true, AutoCheck: true}); err != nil {
		t.Fatal(err)
	}
	if st := l.s.State(context.Background()); st.Check != nil || st.Available {
		t.Fatalf("the old answer is still shown: %+v", st.Check)
	}
	if c := l.s.CheckNow(context.Background()); c.Error != "" || c.Release == nil || c.Release.Version != "v0.10.1-beta" {
		t.Fatalf("check on the pre-release channel: %+v", c)
	}
	// a build that does not know where its updates are
	l.s.DefaultRepo = ""
	if st := l.s.State(context.Background()); st.HasSource {
		t.Fatal("a source without a repository")
	}
	if c := l.s.CheckNow(context.Background()); !strings.Contains(c.Error, "does not say") {
		t.Fatalf("check: %+v", c)
	}
}

func publish(g *fakeGitHub, version string, bundle []byte) {
	g.files["/dl/"+version+".update"] = bundle
	g.files["/dl/"+version+".update.sha256"] = []byte(sum(bundle) + "  peakroute-os-" + version + ".update\n")
}

func waitJob(t *testing.T, l *lab) Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if j := l.s.State(context.Background()).Job; j != nil && (j.Phase == "done" || j.Phase == "failed") {
			return *j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the update did not finish")
	return Job{}
}

func TestAReleaseIsDownloadedVerifiedAndWritten(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = releasesJSON
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	rootfs := fakeRootfs(2<<20 + 5)
	publish(g, "v0.10.0", bundleOf(t, "v0.10.0", rootfs, nil))

	if err := l.s.Install("v0.10.0"); err == nil {
		t.Fatal("installed a version nobody had looked for")
	}
	l.s.CheckNow(context.Background())
	if err := l.s.Install("v0.10.0"); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, l); j.Phase != "done" || j.Source != "github" || j.Version != "v0.10.0" {
		t.Fatalf("job: %+v", j)
	}
	if got := l.slot(SlotB); !bytes.Equal(got[:len(rootfs)], rootfs) {
		t.Fatal("slot B does not hold the download")
	}
	if l.called("tune2fs -L peakroute-root-b") != 1 {
		t.Fatal("not named")
	}
	if st := l.s.State(context.Background()); st.Staged == nil || st.Staged.Version != "v0.10.0" {
		t.Fatalf("staged: %+v", st.Staged)
	}
}

func TestADownloadThatDoesNotMatchTheReleaseChecksumIsNotNamed(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = releasesJSON
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	good := bundleOf(t, "v0.10.0", fakeRootfs(1<<20), nil)
	publish(g, "v0.10.0", good)
	// the checksum published is of another file (a corrupt or substituted download)
	g.files["/dl/v0.10.0.update.sha256"] = []byte(sum([]byte("other")) + "  x\n")
	l.s.CheckNow(context.Background())
	if err := l.s.Install("v0.10.0"); err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, l)
	if j.Phase != "failed" || !strings.Contains(j.Error, "checksum") {
		t.Fatalf("job: %+v", j)
	}
	if l.called("tune2fs") != 0 {
		t.Fatal("the slot was named although the download was wrong")
	}
}

func TestAMissingDownloadFails(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = releasesJSON
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	g.files["/dl/v0.10.0.update.sha256"] = []byte(sum([]byte("x")) + "\n")
	l.s.CheckNow(context.Background())
	if err := l.s.Install("v0.10.0"); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, l); j.Phase != "failed" {
		t.Fatalf("job: %+v", j)
	}
}

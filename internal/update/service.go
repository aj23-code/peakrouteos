package update

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"peakroute/internal/fsutil"
)

// The update service (docs/updates.md): which version and slot is running, finding and receiving a new version, writing it into the
// other slot, and the trial that follows.

// Slot is one of the two root file systems of the disk.
type Slot string

const (
	SlotA Slot = "a"
	SlotB Slot = "b"
)

func (s Slot) Valid() bool { return s == SlotA || s == SlotB }

// Other is the slot that is not s.
func (s Slot) Other() Slot {
	if s == SlotA {
		return SlotB
	}
	return SlotA
}

// Label is the label of the slot's file system, which the boot loader and the kernel find it by.
func (s Slot) Label() string { return "peakroute-root-" + string(s) }

// TrialSeconds is how long a version on trial has to be kept; the trial watch (image/overlay/usr/lib/peakroute/trialwatch) uses the
// same number.
const TrialSeconds = 600

// RunFunc runs a program and returns what it printed.
type RunFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRun is the RunFunc of the real machine.
func ExecRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}
	return cmd.CombinedOutput()
}

// Env is what the service needs from the machine; every place and program can be replaced, which is how it is tested.
type Env struct {
	ConfigDir   string // update.json is kept here
	ReleasePath string // /usr/lib/peakroute/release
	CmdlinePath string // /proc/cmdline
	GrubEnv     string // /boot/efi/grub/grubenv
	ESPMount    string // /boot/efi: remounted writable for a moment to change the grub environment; empty if it is not a mount of its own
	StatePath   string // /run/peakroute/state: "persistent" or "volatile", written by the initramfs
	// SlotDevice is the partition of a slot.
	SlotDevice func(Slot) string
	// Mounted lists the sources of what is mounted (the device a slot is written to must not be among them).
	Mounted func() []string
	Run     RunFunc
	// Reboot restarts the machine (called a moment after the answer has been sent).
	Reboot func()
	// Editable says whether changes may be made now (not while a configuration change waits for its confirmation).
	Editable func() bool
	// Backup keeps a copy of the router's state before it restarts into a new version; it returns the copy's id.
	Backup func() (string, error)
	Now    func() time.Time
	// Version is the version of the running daemon, used when the release file says nothing (a build run by hand).
	Version     string
	Arch        string
	Kernel      func() string
	DefaultRepo string
	// RebootDelay is how long after the answer the machine restarts (2 s when zero)
	RebootDelay time.Duration
	APIBase     string // GitHub's API (APIBase when empty)
	AllowHTTP   bool   // tests only
	HTTP        *http.Client
}

// Service holds the state of the updates: the settings, the last check, what was written and the job that runs.
type Service struct {
	Env

	mu      sync.Mutex
	st      stored
	loaded  bool
	job     *Job
	started time.Time
	cancel  context.CancelFunc
}

// Entry is a line of the history.
type Entry struct {
	Time    time.Time `json:"time"`
	Action  string    `json:"action"` // installed, trial, kept, reverted, failed
	Version string    `json:"version,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// Staged is a version that has been written into the other slot and waits to be started.
type Staged struct {
	Slot    Slot      `json:"slot"`
	Version string    `json:"version"`
	From    string    `json:"from"` // the version that was running when it was written (it is what "keep" leaves in the other slot)
	Written time.Time `json:"written"`
	Notes   string    `json:"notes,omitempty"`
}

// Check is the result of the last look at GitHub.
type Check struct {
	Time    time.Time `json:"time"`
	Error   string    `json:"error,omitempty"`
	Release *Release  `json:"release,omitempty"`
}

// Settings are the administrator's choices. Where updates come from is not one of them: the repository is fixed when the software is
// built (config.DefaultUpdateRepo), so a router only ever follows its maker's releases.
type Settings struct {
	Prereleases bool `json:"prereleases"`
	AutoCheck   bool `json:"auto_check"`
}

type stored struct {
	Settings
	LastCheck *Check          `json:"last_check,omitempty"`
	Versions  map[Slot]string `json:"versions,omitempty"` // what each slot holds (a slot that was being written, or never was, has none)
	Staged    *Staged         `json:"staged,omitempty"`
	History   []Entry         `json:"history,omitempty"`
}

const (
	storeFile  = "update.json"
	keepEvents = 30
)

// New makes the service of the real machine.
func New(configDir, version, defaultRepo string, editable func() bool, backup func() (string, error)) *Service {
	s := &Service{started: bootTime(), Env: Env{
		ConfigDir:   configDir,
		ReleasePath: "/usr/lib/peakroute/release",
		CmdlinePath: "/proc/cmdline",
		GrubEnv:     "/boot/efi/grub/grubenv",
		ESPMount:    "/boot/efi",
		StatePath:   "/run/peakroute/state",
		SlotDevice:  func(sl Slot) string { return "/dev/disk/by-partlabel/peakroute-root-" + string(sl) },
		Mounted:     procMounts,
		Run:         ExecRun,
		Reboot: func() {
			if out, err := ExecRun(context.Background(), "systemctl", "reboot"); err != nil {
				log.Printf("[update] restart failed: %v: %s", err, out)
			}
		},
		Editable:    editable,
		Backup:      backup,
		Now:         time.Now,
		Version:     version,
		Arch:        runtime.GOARCH,
		Kernel:      kernelRelease,
		DefaultRepo: defaultRepo,
	}}
	return s
}

// bootTime is when the machine started (a trial's countdown runs from then, not from when the daemon last started).
func bootTime() time.Time {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return time.Now()
	}
	var up float64
	if _, err := fmt.Sscanf(string(b), "%f", &up); err != nil || up < 0 {
		return time.Now()
	}
	return time.Now().Add(-time.Duration(up * float64(time.Second)))
}

func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func procMounts() []string {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) > 0 {
			out = append(out, fields[0])
		}
	}
	return out
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) editable() bool { return s.Editable == nil || s.Editable() }

func (s *Service) version() string {
	if v := s.Release()["PEAKROUTE_VERSION"]; v != "" {
		return v
	}
	if s.Version != "" {
		return s.Version
	}
	return "unknown"
}

// Release reads the file the image build wrote (the version, when it was built, the layout and the architecture).
func (s *Service) Release() map[string]string {
	out := map[string]string{}
	if s.ReleasePath == "" {
		return out
	}
	f, err := os.Open(s.ReleasePath)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && k != "" && !strings.HasPrefix(k, "#") {
			out[k] = v
		}
	}
	return out
}

// ---- persisted state -------------------------------------------------------------------------------------------------------

func (s *Service) path() string { return filepath.Join(s.ConfigDir, storeFile) }

// load reads update.json once; callers hold s.mu.
func (s *Service) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	s.st = stored{Settings: Settings{AutoCheck: true}}
	if s.ConfigDir == "" {
		return
	}
	raw, err := os.ReadFile(s.path())
	if err != nil {
		return
	}
	var st stored
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Printf("[update] %s cannot be read, starting empty: %v", s.path(), err)
		return
	}
	s.st = st
}

// save writes update.json; callers hold s.mu.
func (s *Service) save() {
	if s.ConfigDir == "" {
		return
	}
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err == nil {
		err = fsutil.WriteFileAtomic(s.path(), raw, 0600)
	}
	if err != nil {
		log.Printf("[update] saving %s: %v", s.path(), err)
	}
}

// setVersion records what a slot holds; callers hold s.mu.
func (s *Service) setVersion(sl Slot, version string) {
	if s.st.Versions == nil {
		s.st.Versions = map[Slot]string{}
	}
	s.st.Versions[sl] = version
}

func (s *Service) note(action, version, detail string) {
	s.st.History = append(s.st.History, Entry{Time: s.now(), Action: action, Version: version, Detail: detail})
	if n := len(s.st.History); n > keepEvents {
		s.st.History = append([]Entry(nil), s.st.History[n-keepEvents:]...)
	}
}

// Settings returns the update source and habits, with the default source filled in when none was chosen.
func (s *Service) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return s.st.Settings
}

func (s *Service) repo() string { return s.DefaultRepo }

// SetSettings changes the channel and the habits.
func (s *Service) SetSettings(in Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if in.Prereleases != s.st.Prereleases {
		s.st.LastCheck = nil // what was found belongs to the other channel
	}
	s.st.Settings = in
	s.save()
	return nil
}

// ---- the machine: slot, kept slot, trial -----------------------------------------------------------------------------------

// Boot is what the boot loader and the kernel say about this start.
type Boot struct {
	Running Slot // the slot this system is running from ("" when the device does not have the A/B layout)
	Kept    Slot // the slot that starts when no trial is set
	Trial   bool // this start was a trial (the kernel was told peakroute.trial=1)
	// OnTrial: a version that has not been kept is running.
	OnTrial bool
	Problem string // why the A/B scheme cannot be used on this device, if it cannot
}

func (s *Service) cmdlineOption(name string) (string, bool) {
	raw, err := os.ReadFile(s.CmdlinePath)
	if err != nil {
		return "", false
	}
	for _, f := range strings.Fields(string(raw)) {
		if v, ok := strings.CutPrefix(f, name+"="); ok {
			return v, true
		}
	}
	return "", false
}

// run runs a program (the real one unless the environment says otherwise).
func (s *Service) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if s.Env.Run != nil {
		return s.Env.Run(ctx, name, args...)
	}
	return ExecRun(ctx, name, args...)
}

// readGrubEnv returns the variables of the boot loader's environment block.
func (s *Service) readGrubEnv(ctx context.Context) (map[string]string, error) {
	out, err := s.run(ctx, "grub-editenv", s.GrubEnv, "list")
	if err != nil {
		return nil, fmt.Errorf("the boot loader's settings cannot be read: %w", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			env[k] = v
		}
	}
	return env, nil
}

// setGrubEnv changes variables of the boot loader's environment. The partition is read-only the rest of the time.
func (s *Service) setGrubEnv(ctx context.Context, kv ...string) (err error) {
	if s.ESPMount != "" {
		if out, rerr := s.run(ctx, "mount", "-o", "remount,rw", s.ESPMount); rerr != nil {
			return fmt.Errorf("the boot partition could not be made writable: %v: %s", rerr, strings.TrimSpace(string(out)))
		}
		defer func() {
			s.run(ctx, "sync")
			if out, rerr := s.run(ctx, "mount", "-o", "remount,ro", s.ESPMount); rerr != nil && err == nil {
				err = fmt.Errorf("the boot partition could not be made read-only again: %v: %s", rerr, strings.TrimSpace(string(out)))
			}
		}()
	}
	args := append([]string{s.GrubEnv, "set"}, kv...)
	if out, rerr := s.run(ctx, "grub-editenv", args...); rerr != nil {
		return fmt.Errorf("the boot loader's settings could not be changed: %v: %s", rerr, strings.TrimSpace(string(out)))
	}
	return nil
}

// BootState works out which slot runs and whether a version is on trial.
func (s *Service) BootState(ctx context.Context) Boot {
	var b Boot
	v, ok := s.cmdlineOption("peakroute.slot")
	if !ok || !Slot(v).Valid() {
		b.Problem = "This device was not started from one of the two system slots (it is a development run, or it was written from an older image). Updates need a device written from the current image."
		return b
	}
	b.Running = Slot(v)
	t, _ := s.cmdlineOption("peakroute.trial")
	b.Trial = t == "1"
	env, err := s.readGrubEnv(ctx)
	if err != nil {
		b.Problem = err.Error()
		return b
	}
	b.Kept = Slot(env["slot"])
	if !b.Kept.Valid() {
		b.Kept = SlotA // grub.cfg's own default
	}
	b.OnTrial = b.Trial && b.Kept != b.Running
	return b
}

func (s *Service) persistent() bool {
	b, err := os.ReadFile(s.StatePath)
	if err != nil {
		return true // not written: not a device that lays the state over the system, nothing to warn about
	}
	return strings.TrimSpace(string(b)) == "persistent"
}

// target is what a bundle has to fit on this device.
func (s *Service) target(running Slot) Target {
	t := Target{Arch: s.Arch, Layout: LayoutVersion, Running: s.version()}
	if l := s.Release()["PEAKROUTE_LAYOUT"]; l != "" {
		fmt.Sscanf(l, "%d", &t.Layout)
	}
	if running.Valid() {
		if f, err := os.Open(s.SlotDevice(running.Other())); err == nil {
			t.SlotBytes, _ = f.Seek(0, io.SeekEnd)
			f.Close()
		}
	}
	return t
}

// ---- the job ---------------------------------------------------------------------------------------------------------------

// Job is the update being received and written.
type Job struct {
	Version string    `json:"version"`
	Source  string    `json:"source"`          // "github" or "upload"
	Phase   string    `json:"phase"`           // starting, writing, checking, done, failed
	Done    int64     `json:"done"`            // bytes of the update file received
	Total   int64     `json:"total"`           // and how many there are
	Error   string    `json:"error,omitempty"` // set when the phase is failed
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitempty"`
}

// The reasons an update cannot be started now.
var (
	ErrBusy        = errors.New("an update is already being received")
	ErrNotEditable = errors.New("a configuration change is waiting to be confirmed: finish that first")
)

func (s *Service) busy() bool {
	return s.job != nil && s.job.Phase != "done" && s.job.Phase != "failed"
}

func (s *Service) progress(done, total int64) {
	s.mu.Lock()
	if s.job != nil {
		s.job.Done, s.job.Total = done, total
	}
	s.mu.Unlock()
}

// begin takes the one turn there is; the returned func ends the job with the error, if any, that the caller then has.
func (s *Service) begin(source, version string, total int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	if s.busy() {
		return ErrBusy
	}
	if !s.editable() {
		return ErrNotEditable
	}
	s.job = &Job{Version: version, Source: source, Phase: "starting", Total: total, Started: s.now()}
	return nil
}

func (s *Service) phase(p string) {
	s.mu.Lock()
	if s.job != nil {
		s.job.Phase = p
	}
	s.mu.Unlock()
}

func (s *Service) end(version string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return
	}
	s.job.Ended = s.now()
	if err != nil {
		s.job.Phase = "failed"
		s.job.Error = capital(err.Error())
		s.note("failed", version, s.job.Error)
		s.save()
		return
	}
	s.job.Phase = "done"
}

func capital(m string) string {
	if m == "" {
		return m
	}
	return strings.ToUpper(m[:1]) + m[1:]
}

// ---- receiving and writing -------------------------------------------------------------------------------------------------

// receive reads a bundle from r and writes it into the other slot. wantSum, when not empty, is the checksum the whole file must have.
// The caller has taken the turn (begin) and ends it.
func (s *Service) receive(ctx context.Context, r io.Reader, wantSum string) (m Manifest, err error) {
	boot := s.BootState(ctx)
	if boot.Problem != "" {
		return m, errors.New(boot.Problem)
	}
	if boot.OnTrial {
		return m, errors.New("a new version is on trial: keep it or go back to the old one before installing another")
	}
	slot := boot.Running.Other()
	dev := s.SlotDevice(slot)
	if s.deviceInUse(dev) {
		return m, errors.New("the space for the new system is in use")
	}

	whole := sha256.New()
	b, err := OpenBundle(io.TeeReader(r, whole))
	if err != nil {
		return m, err
	}
	m = b.Manifest
	s.mu.Lock()
	if s.job != nil {
		s.job.Version = m.Version
	}
	s.mu.Unlock()
	if err := m.Compatible(s.target(boot.Running)); err != nil {
		return m, err
	}

	// from here on the other slot is no longer a system that can be started
	s.mu.Lock()
	delete(s.st.Versions, slot)
	s.st.Staged = nil
	s.save()
	s.mu.Unlock()
	s.phase("writing")
	if err := WriteSlot(ctx, b, dev, s.progress); err != nil {
		return m, err
	}
	if err := b.Drain(); err != nil {
		return m, ErrWrongData
	}
	if _, err := io.Copy(io.Discard, io.TeeReader(r, whole)); err != nil {
		return m, ErrWrongData
	}
	if wantSum != "" && !strings.EqualFold(hex.EncodeToString(whole.Sum(nil)), wantSum) {
		return m, errors.New("the update file does not match the checksum of the release: the download is damaged")
	}

	s.phase("checking")
	if err := s.finishSlot(ctx, slot, dev); err != nil {
		return m, err
	}

	s.mu.Lock()
	s.setVersion(slot, m.Version)
	s.st.Staged = &Staged{Slot: slot, Version: m.Version, From: s.version(), Written: s.now(), Notes: m.Notes}
	s.note("installed", m.Version, "written to slot "+string(slot))
	s.save()
	s.mu.Unlock()
	return m, nil
}

func (s *Service) deviceInUse(dev string) bool {
	if s.Mounted == nil {
		return false
	}
	real, err := filepath.EvalSymlinks(dev)
	if err != nil {
		real = dev
	}
	for _, m := range s.Mounted() {
		if m == dev || m == real {
			return true
		}
		if r, err := filepath.EvalSymlinks(m); err == nil && r == real {
			return true
		}
	}
	return false
}

// exitCode is the exit status of a program that ran and failed, or -1.
func exitCode(err error) int {
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// finishSlot checks the file system that was written, grows it to the partition and gives it its slot's label. The label comes last:
// until it is set the boot loader cannot find the slot.
func (s *Service) finishSlot(ctx context.Context, slot Slot, dev string) error {
	if out, err := s.run(ctx, "e2fsck", "-f", "-p", dev); err != nil && exitCode(err) != 1 { // 1: errors were corrected
		return fmt.Errorf("the new system failed its check: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := s.run(ctx, "resize2fs", dev); err != nil {
		return fmt.Errorf("the new system could not be grown to its space: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := s.run(ctx, "tune2fs", "-L", slot.Label(), dev); err != nil {
		return fmt.Errorf("the new system could not be named: %v: %s", err, strings.TrimSpace(string(out)))
	}
	s.run(ctx, "udevadm", "settle", "--timeout=5") // best effort: the label link is made by udev
	return nil
}

// Upload receives a bundle that the administrator sends (the whole request body). It returns when the bundle is written or has failed.
func (s *Service) Upload(ctx context.Context, body io.Reader, size int64) (Manifest, error) {
	if err := s.begin("upload", "", size); err != nil {
		return Manifest{}, err
	}
	m, err := s.receive(ctx, body, "")
	s.end(m.Version, err)
	return m, err
}

// Install downloads the release that the last check found and writes it into the other slot. It starts the work in the background
// and returns at once; State shows the progress.
func (s *Service) Install(version string) error {
	s.mu.Lock()
	s.load()
	var rel *Release
	if lc := s.st.LastCheck; lc != nil && lc.Release != nil && lc.Release.Version == version {
		c := *lc.Release
		rel = &c
	}
	s.mu.Unlock()
	if rel == nil {
		return errors.New("that version was not found by the last check: check for updates again")
	}
	if err := s.begin("github", rel.Version, rel.Size); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	go func() {
		defer cancel()
		_, err := s.download(ctx, rel)
		s.end(rel.Version, err)
		if err != nil {
			log.Printf("[update] installing %s failed: %v", rel.Version, err)
		}
	}()
	return nil
}

func (s *Service) download(ctx context.Context, rel *Release) (Manifest, error) {
	sum, err := s.fetchChecksum(ctx, rel)
	if err != nil {
		return Manifest{}, err
	}
	// the download may stall: give up when nothing has arrived for a minute
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := s.get(dctx, rel.AssetURL, "application/octet-stream")
	if err != nil {
		return Manifest{}, fmt.Errorf("the update could not be downloaded: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("the update could not be downloaded (%s)", resp.Status)
	}
	s.progress(0, resp.ContentLength)
	idle := time.AfterFunc(time.Minute, cancel)
	defer idle.Stop()
	body := readerFunc(func(p []byte) (int, error) {
		n, err := resp.Body.Read(p)
		if n > 0 {
			idle.Reset(time.Minute)
		}
		return n, err
	})
	m, err := s.receive(dctx, body, sum)
	if err != nil && dctx.Err() != nil && ctx.Err() == nil {
		err = errors.New("the download stopped: nothing arrived for a minute")
	}
	return m, err
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// Cancel stops the download that is running.
func (s *Service) Cancel() {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

// ---- looking for releases --------------------------------------------------------------------------------------------------

// CheckNow asks GitHub for the newest release and remembers the answer.
func (s *Service) CheckNow(ctx context.Context) Check {
	s.mu.Lock()
	s.load()
	repo, pre := s.repo(), s.st.Prereleases
	s.mu.Unlock()
	c := Check{Time: s.now()}
	if repo == "" {
		c.Error = "This build does not say where its updates are published."
	} else {
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		rel, err := s.FindRelease(cctx, repo, pre, s.version())
		if err != nil {
			c.Error = capital(err.Error())
		} else {
			c.Release = rel
		}
	}
	s.mu.Lock()
	s.st.LastCheck = &c
	s.save()
	s.mu.Unlock()
	return c
}

// Watch looks for releases once a day while the switch is on, until ctx ends. The first look is a few minutes after the start, when the
// internet connection has had time to come up.
func (s *Service) Watch(ctx context.Context) {
	wait := 5 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 24 * time.Hour
		s.mu.Lock()
		s.load()
		auto := s.st.AutoCheck && s.repo() != ""
		s.mu.Unlock()
		if auto {
			s.CheckNow(ctx)
		}
	}
}

// ---- restart, keep, go back ------------------------------------------------------------------------------------------------

// Restart starts the version that was written into the other slot, on trial.
func (s *Service) Restart(ctx context.Context) error {
	boot := s.BootState(ctx)
	if boot.Problem != "" {
		return errors.New(boot.Problem)
	}
	s.mu.Lock()
	s.load()
	st := s.st.Staged
	busy := s.busy()
	s.mu.Unlock()
	switch {
	case busy:
		return errors.New("an update is still being written")
	case boot.OnTrial:
		return errors.New("a new version is on trial already")
	case st == nil || st.Slot != boot.Running.Other():
		return errors.New("there is no new version waiting to be started")
	case !s.editable():
		return ErrNotEditable
	}
	detail := "started on trial from slot " + string(boot.Running)
	if s.Backup != nil {
		if id, err := s.Backup(); err != nil {
			log.Printf("[update] no copy of the settings before the restart: %v", err)
			detail += "; no copy of the settings could be made"
		} else {
			detail += "; a copy of the settings was kept (" + id + ")"
		}
	}
	if err := s.setGrubEnv(ctx, "trial="+string(st.Slot)); err != nil {
		return err
	}
	s.mu.Lock()
	s.note("trial", st.Version, detail)
	s.save()
	s.mu.Unlock()
	s.rebootSoon()
	return nil
}

func (s *Service) rebootSoon() {
	if s.Reboot == nil {
		return
	}
	go func() {
		d := s.RebootDelay
		if d == 0 {
			d = 2 * time.Second // the answer to the request goes out first
		}
		time.Sleep(d)
		s.Reboot()
	}()
}

// Keep makes the version on trial the one that starts from now on.
func (s *Service) Keep(ctx context.Context) error {
	boot := s.BootState(ctx)
	if boot.Problem != "" {
		return errors.New(boot.Problem)
	}
	if !boot.OnTrial {
		return errors.New("no new version is on trial")
	}
	if err := s.setGrubEnv(ctx, "slot="+string(boot.Running), "trial="); err != nil {
		return err
	}
	s.mu.Lock()
	s.load()
	s.st.Staged = nil
	s.note("kept", s.version(), "slot "+string(boot.Running)+" is the one that starts now")
	s.save()
	s.mu.Unlock()
	return nil
}

// GoBack leaves a version that is on trial (the next start is the kept version) or, when none is on trial, tries the version in the
// other slot (the one before the last update).
func (s *Service) GoBack(ctx context.Context) error {
	boot := s.BootState(ctx)
	if boot.Problem != "" {
		return errors.New(boot.Problem)
	}
	s.mu.Lock()
	s.load()
	other := s.st.Versions[boot.Running.Other()]
	busy := s.busy()
	s.mu.Unlock()
	if busy {
		return errors.New("an update is still being written")
	}
	if boot.OnTrial {
		s.mu.Lock()
		s.note("reverted", s.version(), "went back to slot "+string(boot.Kept))
		s.st.Staged = nil
		s.save()
		s.mu.Unlock()
		s.rebootSoon()
		return nil
	}
	if other == "" {
		return errors.New("the other slot holds no earlier version")
	}
	if !s.editable() {
		return ErrNotEditable
	}
	if err := s.setGrubEnv(ctx, "trial="+string(boot.Running.Other())); err != nil {
		return err
	}
	s.mu.Lock()
	s.st.Staged = &Staged{Slot: boot.Running.Other(), Version: other, From: s.version(), Written: s.now()}
	s.note("trial", other, "the earlier version was started on trial")
	s.save()
	s.mu.Unlock()
	s.rebootSoon()
	return nil
}

// ---- what the page shows ---------------------------------------------------------------------------------------------------

// Running describes the system that is running.
type Running struct {
	Version string `json:"version"`
	Built   string `json:"built,omitempty"`
	Layout  int    `json:"layout,omitempty"`
	Arch    string `json:"arch"`
	Debian  string `json:"debian,omitempty"`
	Kernel  string `json:"kernel,omitempty"`
	Slot    Slot   `json:"slot,omitempty"`
	Kept    Slot   `json:"kept,omitempty"`
}

// StateView is everything the Updates page shows.
type StateView struct {
	Supported  bool     `json:"supported"`
	Problem    string   `json:"problem,omitempty"`
	Running    Running  `json:"running"`
	Persistent bool     `json:"persistent"` // false: the router's state is in memory and is lost at a restart
	OnTrial    bool     `json:"on_trial"`
	TrialLeft  int      `json:"trial_seconds_left,omitempty"`
	OtherSlot  Slot     `json:"other_slot,omitempty"`
	OtherVer   string   `json:"other_version,omitempty"`
	Staged     *Staged  `json:"staged,omitempty"`
	Job        *Job     `json:"job,omitempty"`
	Settings   Settings `json:"settings"`
	HasSource  bool     `json:"has_source"` // this build knows where its updates are published
	Check      *Check   `json:"check,omitempty"`
	History    []Entry  `json:"history"`
	Editable   bool     `json:"editable"`
	Available  bool     `json:"available"` // the last check found a newer version
}

// State gathers what the Updates page shows.
func (s *Service) State(ctx context.Context) StateView {
	boot := s.BootState(ctx)
	rel := s.Release()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	v := StateView{
		Supported:  boot.Problem == "",
		Problem:    boot.Problem,
		Persistent: s.persistent(),
		Settings:   s.st.Settings,
		HasSource:  s.DefaultRepo != "",
		Editable:   s.editable(),
		History:    append([]Entry{}, s.st.History...),
		Running: Running{Version: s.version(), Built: rel["PEAKROUTE_BUILT"], Arch: s.Arch, Debian: rel["DEBIAN_VERSION"],
			Slot: boot.Running, Kept: boot.Kept},
	}
	if s.Kernel != nil {
		v.Running.Kernel = s.Kernel()
	}
	fmt.Sscanf(rel["PEAKROUTE_LAYOUT"], "%d", &v.Running.Layout)
	if a := rel["PEAKROUTE_ARCH"]; a != "" {
		v.Running.Arch = a
	}
	// newest first
	for i, j := 0, len(v.History)-1; i < j; i, j = i+1, j-1 {
		v.History[i], v.History[j] = v.History[j], v.History[i]
	}
	if boot.Running.Valid() {
		v.OtherSlot = boot.Running.Other()
		if s.st.Versions[boot.Running] != v.Running.Version { // the running slot is known to hold what is running
			s.setVersion(boot.Running, v.Running.Version)
			s.save()
		}
		v.OtherVer = s.st.Versions[boot.Running.Other()]
	}
	v.OnTrial = boot.OnTrial
	if boot.OnTrial {
		left := int(s.started.Add(TrialSeconds * time.Second).Sub(s.now()).Seconds())
		if left < 0 {
			left = 0
		}
		v.TrialLeft = left
	}
	if st := s.st.Staged; st != nil && boot.Running.Valid() && st.Slot == boot.Running.Other() && !boot.OnTrial {
		c := *st
		v.Staged = &c
	}
	if s.job != nil {
		j := *s.job
		v.Job = &j
	}
	if s.st.LastCheck != nil {
		c := *s.st.LastCheck
		v.Check = &c
		v.Available = c.Release != nil && c.Release.Newer
	}
	return v
}

// Started records when the daemon began, which is when a trial's countdown began (the trial watch starts with the system).
func (s *Service) Started(t time.Time) {
	s.mu.Lock()
	s.started = t
	s.mu.Unlock()
}

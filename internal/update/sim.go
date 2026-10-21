package update

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A simulated device for the development run (peakroute -dev): two slots as files, a boot loader that is a text file, and a restart that
// is only the daemon pretending. It lets the whole Updates page be tried without building an image: upload the sample update file the
// simulation makes, restart, keep it, go back. Nothing in it touches the machine the daemon runs on.

const simSlotBytes = 64 << 20

type sim struct {
	dir string
	mu  sync.Mutex
	s   *Service
}

// NewSimulated makes the update service of a development run. The files are in dir. The sample update files it makes are listed by
// Samples.
func NewSimulated(dir, version string, editable func() bool, backup func() (string, error)) (*Service, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	sm := &sim{dir: dir}
	for _, sl := range []Slot{SlotA, SlotB} {
		p := filepath.Join(dir, "slot-"+string(sl))
		if _, err := os.Stat(p); err != nil {
			f, err := os.Create(p)
			if err != nil {
				return nil, err
			}
			if err := f.Truncate(simSlotBytes); err != nil {
				f.Close()
				return nil, err
			}
			f.Close()
		}
	}
	// the running slot holds the version this daemon is
	if _, err := os.Stat(filepath.Join(dir, "cmdline")); err != nil {
		if err := sm.writeVersion(SlotA, version); err != nil {
			return nil, err
		}
		if err := sm.writeEnv(map[string]string{"slot": "a", "trial": ""}); err != nil {
			return nil, err
		}
		if err := sm.boot(); err != nil {
			return nil, err
		}
	}
	s := New(dir, version, "", editable, backup)
	s.ConfigDir = dir
	s.ReleasePath = filepath.Join(dir, "release")
	s.CmdlinePath = filepath.Join(dir, "cmdline")
	s.GrubEnv = filepath.Join(dir, "grubenv")
	s.ESPMount = ""
	s.StatePath = filepath.Join(dir, "state")
	s.SlotDevice = func(sl Slot) string { return filepath.Join(dir, "slot-"+string(sl)) }
	s.Mounted = func() []string { return nil }
	s.Run = sm.run
	s.Reboot = func() { sm.restart() }
	s.Kernel = func() string { return "simulated" }
	sm.s = s
	s.Started(time.Now()) // the simulated machine has just started
	if err := sm.samples(version); err != nil {
		return nil, err
	}
	return s, nil
}

func (sm *sim) path(n string) string { return filepath.Join(sm.dir, n) }

func (sm *sim) readEnv() map[string]string {
	env := map[string]string{}
	raw, err := os.ReadFile(sm.path("grubenv"))
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

func (sm *sim) writeEnv(env map[string]string) error {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return os.WriteFile(sm.path("grubenv"), []byte(b.String()), 0600)
}

// run plays the programs the service calls.
func (sm *sim) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if name == "grub-editenv" && len(args) >= 2 {
		env := sm.readEnv()
		switch args[1] {
		case "list":
			var b strings.Builder
			for k, v := range env {
				fmt.Fprintf(&b, "%s=%s\n", k, v)
			}
			return []byte(b.String()), nil
		case "set":
			for _, kv := range args[2:] {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			return nil, sm.writeEnv(env)
		}
	}
	return nil, nil // mount, sync, e2fsck, resize2fs, tune2fs, udevadm: nothing to do to a file
}

const versionKey = "PEAKROUTE_VERSION="

// writeVersion puts a version in the head of a slot, which is all the simulated "system" is.
func (sm *sim) writeVersion(sl Slot, version string) error {
	f, err := os.OpenFile(sm.path("slot-"+string(sl)), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteAt([]byte(versionKey+version+"\n"), 0)
	return err
}

func (sm *sim) readVersion(sl Slot) string {
	f, err := os.Open(sm.path("slot-" + string(sl)))
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 128)
	n, _ := f.Read(head)
	line, _, _ := strings.Cut(string(head[:n]), "\n")
	if v, ok := strings.CutPrefix(line, versionKey); ok {
		return v
	}
	return ""
}

// boot is what the boot loader and the kernel do at a start: pick the slot, clear the trial, and tell the system what it is.
func (sm *sim) boot() error {
	env := sm.readEnv()
	slot, trial := Slot(env["slot"]), "peakroute.trial="
	if t := Slot(env["trial"]); t.Valid() && sm.readVersion(t) != "" {
		slot, trial = t, "peakroute.trial=1"
		env["trial"] = ""
		if err := sm.writeEnv(env); err != nil {
			return err
		}
	}
	if !slot.Valid() {
		slot = SlotA
	}
	v := sm.readVersion(slot)
	if v == "" {
		slot = SlotA
		v = sm.readVersion(slot)
	}
	rel := fmt.Sprintf("PEAKROUTE_VERSION=%s\nPEAKROUTE_BUILT=%s\nPEAKROUTE_LAYOUT=%d\nPEAKROUTE_ARCH=%s\nDEBIAN_VERSION=13 (simulated)\n",
		v, time.Now().UTC().Format(time.RFC3339), LayoutVersion, "amd64")
	if err := os.WriteFile(sm.path("release"), []byte(rel), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(sm.path("state"), []byte("persistent\n"), 0644); err != nil {
		return err
	}
	return os.WriteFile(sm.path("cmdline"), []byte("simulated peakroute.slot="+string(slot)+" "+trial+"\n"), 0644)
}

// restart is the simulated restart: the next start, in the same process.
func (sm *sim) restart() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if err := sm.boot(); err != nil {
		return
	}
	if sm.s != nil {
		sm.s.Started(time.Now())
	}
}

// samples writes update files to try with: a newer version, and a damaged one.
func (sm *sim) samples(running string) error {
	rootfs := func(version string) []byte {
		b := bytes.Repeat([]byte("PeakRoute simulated system.\n"), 40000)
		return append([]byte(versionKey+version+"\n"), b...)
	}
	newer := "v99.0.0-sim"
	data, err := MakeBundle(newer, "A simulated update: it only changes the version number the page shows.", rootfs(newer))
	if err != nil {
		return err
	}
	if err := os.WriteFile(sm.path("sample-"+newer+".update"), data, 0644); err != nil {
		return err
	}
	bad := append([]byte(nil), data...)
	bad[len(bad)/2] ^= 0xFF
	return os.WriteFile(sm.path("sample-damaged.update"), bad, 0644)
}

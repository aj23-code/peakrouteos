package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The script that goes back from a version on trial that is not kept runs here with a grub-editenv that reads a text file and a
// "reboot" that writes a file.

type trialLab struct {
	dir string
	env []string
}

func newTrialLab(t *testing.T, cmdline, kept string) *trialLab {
	t.Helper()
	need(t, "sh", "tr", "sed", "head")
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0755)
	fake := `#!/bin/sh
# grub-editenv FILE list
cat "$1.txt"
`
	os.WriteFile(filepath.Join(bin, "grub-editenv"), []byte(fake), 0755)
	os.WriteFile(filepath.Join(dir, "grubenv.txt"), []byte("slot="+kept+"\ntrial=\n"), 0644)
	os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline+"\n"), 0644)
	return &trialLab{dir: dir, env: []string{
		"PATH=" + bin + ":/usr/bin:/bin",
		"PEAKROUTE_GRUBENV=" + filepath.Join(dir, "grubenv"),
		"PEAKROUTE_CMDLINE=" + filepath.Join(dir, "cmdline"),
		"PEAKROUTE_TRIAL_SECONDS=1",
		"PEAKROUTE_TRIAL_STEP=1",
		"PEAKROUTE_REBOOT=touch " + filepath.Join(dir, "rebooted"),
	}}
}

func (l *trialLab) run(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("sh", "overlay/usr/lib/peakroute/trialwatch")
	cmd.Env = l.env
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the script did not end")
	}
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

func (l *trialLab) rebooted() bool {
	_, err := os.Stat(filepath.Join(l.dir, "rebooted"))
	return err == nil
}

func TestAVersionThatIsNotKeptIsGoneBackFrom(t *testing.T) {
	l := newTrialLab(t, "ro root=LABEL=peakroute-root-b peakroute.slot=b peakroute.trial=1", "a")
	out := l.run(t)
	if !l.rebooted() || !strings.Contains(out, "going back") {
		t.Fatalf("no restart: %q", out)
	}
}

func TestAVersionThatIsKeptIsLeftAlone(t *testing.T) {
	l := newTrialLab(t, "ro peakroute.slot=b peakroute.trial=1", "b")
	l.run(t)
	if l.rebooted() {
		t.Fatal("restarted although the version was kept")
	}
}

func TestAVersionKeptWhileWaitingIsNotRestarted(t *testing.T) {
	l := newTrialLab(t, "ro peakroute.slot=b peakroute.trial=1", "a")
	go func() {
		time.Sleep(300 * time.Millisecond)
		os.WriteFile(filepath.Join(l.dir, "grubenv.txt"), []byte("slot=b\ntrial=\n"), 0644)
	}()
	l.env = append(l.env, "PEAKROUTE_TRIAL_SECONDS=3")
	l.run(t)
	if l.rebooted() {
		t.Fatal("restarted although the version was kept in time")
	}
}

func TestNoSlotOnTheCommandLineMeansNothingToWatch(t *testing.T) {
	l := newTrialLab(t, "ro root=/dev/sda1", "a")
	l.run(t)
	if l.rebooted() {
		t.Fatal("restarted a device that has no slots")
	}
}

func TestTheStateScriptsArePlainShell(t *testing.T) {
	need(t, "sh")
	for _, f := range []string{"overlay/usr/lib/peakroute/trialwatch", "overlay/usr/lib/peakroute/growdata",
		"overlay/etc/initramfs-tools/scripts/local-bottom/peakroute-state", "overlay/etc/initramfs-tools/hooks/peakroute-state"} {
		if out, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", f, err, out)
		}
	}
}

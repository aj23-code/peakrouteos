package translator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The check before a WireGuard commit must fail when wg-quick cannot read or understand a tunnel file. It once did not: in
// "wg-quick strip f | wg setconf ..." only the last command's result counts, and wg setconf is happy with empty input. Here wg-quick, wg
// and ip are stand-ins, so the rule is tested whatever the machine has installed or allows.
func TestTheWireGuardCheckFailsWhenWgQuickDoes(t *testing.T) {
	bin := t.TempDir()
	stub := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	stub("wg-quick", `grep -q BAD "$2" && { echo "wg-quick: cannot read $2" >&2; exit 1; }; cat "$2"`)
	stub("wg", `cat >/dev/null; exit 0`) // takes anything, empty input included, like the real one
	stub("ip", `exit 0`)

	dir := t.TempDir()
	script := strings.ReplaceAll(preChecks[SvcWireGuard].args[1], PathWireGuardDir, dir)
	run := func() (string, error) {
		c := exec.Command("sh", "-c", script)
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := c.CombinedOutput()
		return string(out), err
	}

	if out, err := run(); err != nil {
		t.Fatalf("no files: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "wgs1.conf"), []byte("[Interface]\nPrivateKey = fine\n"), 0600)
	if out, err := run(); err != nil {
		t.Fatalf("a good file was refused: %v %s", err, out)
	}
	os.WriteFile(filepath.Join(dir, "wgra.conf"), []byte("BAD"), 0600)
	out, err := run()
	if err == nil {
		t.Fatalf("a file wg-quick fails on was accepted: %s", out)
	}
	if !strings.Contains(out, "cannot read") {
		t.Errorf("wg-quick's own message is lost: %q", out)
	}
}

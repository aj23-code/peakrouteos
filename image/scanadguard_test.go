package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The scan of the AdGuard Home program (.github/scripts/scan-adguard.sh) reports and never fails: govulncheck is replaced by a program
// that answers as it does (0: nothing, 3: vulnerabilities found, anything else: it could not run).
func TestTheAdGuardScanReportsWhatItFindsAndNeverFailsTheBuild(t *testing.T) {
	need(t, "sh", "sed", "grep")
	dir := t.TempDir()
	bin := filepath.Join(dir, "AdGuardHome")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'AdGuard Home, version v9.9.9'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	fake := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	script, _ := filepath.Abs("../.github/scripts/scan-adguard.sh")
	run := func(gv string) (string, int) {
		cmd := exec.Command("sh", script, bin)
		cmd.Env = append(os.Environ(), "GOVULNCHECK="+gv)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}

	clean := fake("clean", "echo 'No vulnerabilities found.'\nexit 0\n")
	if out, code := run(clean); code != 0 || !strings.Contains(out, "AdGuard Home v9.9.9") || !strings.Contains(out, "no known vulnerabilities") {
		t.Errorf("clean: %d %q", code, out)
	}
	found := fake("found", `cat <<'EOT'
Vulnerability #1: GO-2026-0001
    Something in a package
  More info: https://pkg.go.dev/vuln/GO-2026-0001
  Module: example.com/x
    Found in: example.com/x@v1.0.0
    Fixed in: example.com/x@v1.0.1
EOT
exit 3
`)
	out, code := run(found)
	if code != 0 {
		t.Errorf("a vulnerability failed the build (exit %d)", code)
	}
	for _, want := range []string{"KNOWN VULNERABILITIES", "Vulnerability #1: GO-2026-0001", "Fixed in: example.com/x@v1.0.1", "More info:"} {
		if !strings.Contains(out, want) {
			t.Errorf("found: missing %q in %q", want, out)
		}
	}
	// the scanner could not run: said so, with its own status, and still not a failure of anything that matters (the caller ignores it)
	broken := fake("broken", "echo 'no network' >&2\nexit 1\n")
	if out, code := run(broken); code != 2 || !strings.Contains(out, "could not run") {
		t.Errorf("broken: %d %q", code, out)
	}
	// no program to scan
	cmd := exec.Command("sh", script, filepath.Join(dir, "missing"))
	cmd.Env = append(os.Environ(), "GOVULNCHECK="+clean)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "is not there") {
		t.Errorf("%v %q", err, out)
	}
}

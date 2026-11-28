package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The weekly check of a release's packages against Debian (.github/scripts/debian-updates.sh) runs here with apt-cache replaced by a
// table of what the archives hold; dpkg's own version comparison is the real one.
func TestTheWeeklyCheckFindsWhatDebianHasPublishedSince(t *testing.T) {
	need(t, "sh", "dpkg", "sed", "awk", "grep")
	dir := t.TempDir()
	// name | candidate | the archive it is in
	table := `openssl|3.5.1-1+deb13u2|debian-security trixie-security/main
libc6|2.41-12+deb13u1|debian trixie-updates/main
curl|8.14.1-2|debian trixie/main
tzdata|2025b-5+deb13u1|debian trixie-updates/main
kmod|34.2-2|debian trixie/main
ghostpkg|(none)|
epoch|1:2.0-1|debian trixie/main
tilde|1.0~rc2-1|debian trixie/main
`
	if err := os.WriteFile(filepath.Join(dir, "table"), []byte(table), 0644); err != nil {
		t.Fatal(err)
	}
	fake := `#!/bin/sh
# apt-cache policy NAME | apt-cache madison NAME
row=$(grep "^$2|" ` + filepath.Join(dir, "table") + ` | head -1)
[ -n "$row" ] || exit 0
cand=$(echo "$row" | cut -d'|' -f2); origin=$(echo "$row" | cut -d'|' -f3)
case "$1" in
    policy) printf '%s:\n  Installed: x\n  Candidate: %s\n' "$2" "$cand" ;;
    madison) printf '%s | %s | http://deb.debian.org/%s amd64 Packages\n' "$2" "$cand" "$origin" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "apt-cache"), []byte(fake), 0755); err != nil {
		t.Fatal(err)
	}
	list := `openssl 3.5.1-1+deb13u1
libc6 2.41-12
curl 8.14.1-2
tzdata 2025b-5
kmod 34.3-1
ghostpkg 1.0
notindebian 1.0
epoch 1:1.0-1
tilde 1.0-1

`
	if err := os.WriteFile(filepath.Join(dir, "packages"), []byte(list), 0644); err != nil {
		t.Fatal(err)
	}
	script, _ := filepath.Abs("../.github/scripts/debian-updates.sh")
	cmd := exec.Command("sh", script, filepath.Join(dir, "packages"))
	cmd.Env = append(os.Environ(), "APT_CACHE="+filepath.Join(dir, "apt-cache"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := []string{
		"openssl 3.5.1-1+deb13u1 -> 3.5.1-1+deb13u2 (security)", // security first
		"libc6 2.41-12 -> 2.41-12+deb13u1 (update)",
		"tzdata 2025b-5 -> 2025b-5+deb13u1 (update)",
		"epoch 1:1.0-1 -> 1:2.0-1 (update)",
	}
	// (tilde: 1.0~rc2-1 is older than 1.0-1, so it is not an update; curl is the same; kmod in the release is newer than Debian's)
	if len(lines) != len(want) {
		t.Fatalf("got\n%s\nwant\n%s", out, strings.Join(want, "\n"))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d: %q, want %q", i, lines[i], want[i])
		}
	}
	// nothing to report: nothing printed
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte("curl 8.14.1-2\nkmod 34.2-2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("sh", script, filepath.Join(dir, "current"))
	cmd.Env = append(os.Environ(), "APT_CACHE="+filepath.Join(dir, "apt-cache"))
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("%v %q", err, out)
	}
}

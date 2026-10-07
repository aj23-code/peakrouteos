package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func leftovers(t *testing.T, dir string, want ...string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, w := range want {
		keep[w] = true
	}
	var extra []string
	for _, e := range entries {
		if !keep[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

func TestWriteFileAtomicCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if err := WriteFileAtomic(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, path); got != "first" {
		t.Errorf("content = %q, want %q", got, "first")
	}

	if err := WriteFileAtomic(path, []byte("second, longer"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, path); got != "second, longer" {
		t.Errorf("content after replace = %q", got)
	}

	if extra := leftovers(t, dir, "config.json"); len(extra) != 0 {
		t.Errorf("temp files left behind: %v", extra)
	}
}

func TestWriteFileAtomicAppliesPermissions(t *testing.T) {
	dir := t.TempDir()

	for _, perm := range []os.FileMode{0600, 0644} {
		path := filepath.Join(dir, fmt.Sprintf("f-%o", perm))
		if err := WriteFileAtomic(path, []byte("x"), perm); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != perm {
			t.Errorf("mode = %o, want %o", got, perm)
		}
	}
}

// Previously every writer shared "<file>.tmp", so concurrent saves clobbered each other.
func TestWriteFileAtomicConcurrentWritersNeverMixOrFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	const writers = 20
	contents := make([]string, writers)
	for i := range contents {
		contents[i] = strings.Repeat(fmt.Sprintf("writer-%02d;", i), 500)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := WriteFileAtomic(path, []byte(contents[i]), 0600); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	got := readString(t, path)
	found := false
	for _, c := range contents {
		if got == c {
			found = true
		}
	}
	if !found {
		t.Errorf("final file (%d bytes) is not exactly one writer's content", len(got))
	}
	if extra := leftovers(t, dir, "config.json"); len(extra) != 0 {
		t.Errorf("temp files left behind: %v", extra)
	}
}

func TestFailedWriteLeavesOriginalUntouched(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := WriteFileAtomic(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0500); err != nil { // read-only: no new files can be created
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)

	if err := WriteFileAtomic(path, []byte("replacement"), 0600); err == nil {
		t.Fatal("expected an error writing into a read-only directory")
	}

	os.Chmod(dir, 0700)
	if got := readString(t, path); got != "original" {
		t.Errorf("original was modified: %q", got)
	}
	if extra := leftovers(t, dir, "config.json"); len(extra) != 0 {
		t.Errorf("temp files left behind: %v", extra)
	}
}

func TestStageFileDoesNotTouchTargetUntilCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tls.crt")
	if err := os.WriteFile(path, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}

	staged, err := StageFile(path, []byte("new"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	if got := readString(t, path); got != "old" {
		t.Errorf("target changed at stage time: %q", got)
	}
	if got := readString(t, staged); got != "new" {
		t.Errorf("staged content = %q, want %q", got, "new")
	}

	if err := CommitFile(staged, path); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, path); got != "new" {
		t.Errorf("target after commit = %q, want %q", got, "new")
	}
	if extra := leftovers(t, dir, "tls.crt"); len(extra) != 0 {
		t.Errorf("temp files left behind: %v", extra)
	}
}

func TestWriteFileAtomicIntoMissingDirectoryFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "config.json")
	if err := WriteFileAtomic(path, []byte("x"), 0600); err == nil {
		t.Error("expected an error when the directory does not exist")
	}
}

func TestFailedRenameCleansUpTheTempFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "config.json")

	// A non-empty directory in the way makes the final rename fail after the temp file was written
	if err := os.MkdirAll(filepath.Join(target, "child"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(target, []byte("x"), 0600); err == nil {
		t.Fatal("expected the rename over a directory to fail")
	}
	if extra := leftovers(t, dir, "config.json"); len(extra) != 0 {
		t.Errorf("temp files left behind after a failed rename: %v", extra)
	}
}

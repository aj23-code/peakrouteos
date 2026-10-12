// Package fsutil holds small filesystem helpers shared across the daemon.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// WriteFileAtomic replaces path with data so that a reader (or a power cut) sees either the
// complete old file or the complete new one, never a partial write.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	staged, err := StageFile(path, data, perm)
	if err != nil {
		return err
	}
	if err := CommitFile(staged, path); err != nil {
		os.Remove(staged)
		return err
	}
	return nil
}

// StageFile writes data to a uniquely named temp file next to path, flushes it to disk and
// returns its name. Nothing at path changes until CommitFile. Use the two steps directly when
// several files must be staged first and swapped in together; on error the caller should
// os.Remove any staged files it no longer needs.
func StageFile(path string, data []byte, perm os.FileMode) (string, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}

	// A unique name per call, so concurrent writers never share (or clobber) a temp file
	f, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file for %s: %w", path, err)
	}
	staged := f.Name()

	fail := func(err error) (string, error) {
		f.Close()
		os.Remove(staged)
		return "", err
	}

	if _, err := f.Write(data); err != nil {
		return fail(fmt.Errorf("write %s: %w", staged, err))
	}
	if err := f.Chmod(perm); err != nil {
		return fail(fmt.Errorf("chmod %s: %w", staged, err))
	}
	// Data must reach the disk before the rename does, or a crash can leave an empty file
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("sync %s: %w", staged, err))
	}
	if err := f.Close(); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("close %s: %w", staged, err)
	}

	return staged, nil
}

// CommitFile renames a staged file over path, then flushes the directory so the rename itself
// survives a power cut.
func CommitFile(staged, path string) error {
	if err := os.Rename(staged, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", staged, path, err)
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s: %w", dir, err)
	}
	defer d.Close()

	// Some filesystems cannot fsync a directory (EINVAL); there is nothing more to do there
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("sync directory %s: %w", dir, err)
	}
	return nil
}

// SweepStale removes the temporary files that StageFile leaves behind when the process dies between writing one and renaming it
// (a name like ".network.json.123456.tmp") from dir, but only those older than minAge, so that a write in progress is never touched.
// It looks in dir only, not below, and returns how many it removed.
func SweepStale(dir string, minAge time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".tmp") || strings.Count(name, ".") < 3 {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < minAge {
			continue
		}
		if os.Remove(filepath.Join(dir, name)) == nil {
			n++
		}
	}
	return n
}

package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// The update bundle (docs/updates.md, section 5): a tar archive of a manifest and the compressed root file system.

const (
	// FormatVersion is the version of the bundle's own layout (the manifest and its entries).
	FormatVersion = 1
	// LayoutVersion is the version of the disk layout a bundle fits: the partitions, the boot loader and how a slot is chosen. A bundle
	// of another layout needs a fresh image, not an update.
	LayoutVersion = 1

	manifestName = "manifest.json"
	payloadName  = "rootfs.img.gz"
	maxManifest  = 64 << 10
	maxRootfs    = int64(64) << 30 // no root file system is bigger than this; a manifest that says so is wrong
)

// Manifest says what a bundle holds.
type Manifest struct {
	Format        int      `json:"format"`
	Version       string   `json:"version"`
	Built         string   `json:"built"`
	Arch          string   `json:"arch"`
	Layout        int      `json:"layout"`
	RootfsSize    int64    `json:"rootfs_size"`    // bytes of the file system once decompressed
	RootfsSHA256  string   `json:"rootfs_sha256"`  // of the decompressed file system
	PayloadSHA256 string   `json:"payload_sha256"` // of the compressed entry
	Notes         string   `json:"notes,omitempty"`
	Reset         []string `json:"reset,omitempty"` // paths to drop from the router's own layer after the update (none are used yet)
}

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate says whether the manifest is complete and makes sense by itself (whether it fits a device is Compatible).
func (m Manifest) Validate() error {
	if m.Format != FormatVersion {
		return fmt.Errorf("the update file is of a format (%d) this version does not know", m.Format)
	}
	if _, ok := ParseVersion(m.Version); !ok {
		return fmt.Errorf("the update file names a version (%q) that is not a version number", m.Version)
	}
	if m.Arch == "" {
		return errors.New("the update file does not say which processors it is for")
	}
	if m.RootfsSize <= 0 || m.RootfsSize > maxRootfs {
		return fmt.Errorf("the update file says its system is %d bytes", m.RootfsSize)
	}
	if !hex64.MatchString(m.RootfsSHA256) || !hex64.MatchString(m.PayloadSHA256) {
		return errors.New("the update file has no checksums")
	}
	return nil
}

// Target is what a bundle has to fit.
type Target struct {
	Arch      string
	Layout    int
	SlotBytes int64  // the size of the partition that would be written; 0 when not known yet
	Running   string // the version that is running
}

// Compatible says whether the bundle can be written to the target; Same reports that it is the version already running.
func (m Manifest) Compatible(t Target) error {
	if m.Arch != t.Arch {
		return fmt.Errorf("this update is for %s processors and this router has %s", m.Arch, t.Arch)
	}
	if m.Layout != t.Layout {
		return fmt.Errorf("this update needs the disk laid out differently (layout %d, this router has %d): write the new image to the disk instead", m.Layout, t.Layout)
	}
	if t.SlotBytes > 0 && m.RootfsSize > t.SlotBytes {
		return fmt.Errorf("the system in this update (%d MB) does not fit the space for it on this disk (%d MB): write the new image to the disk instead", m.RootfsSize>>20, t.SlotBytes>>20)
	}
	return nil
}

// Same reports whether the bundle is the version that is running.
func (m Manifest) Same(running string) bool {
	a, ok1 := ParseVersion(m.Version)
	b, ok2 := ParseVersion(running)
	return ok1 && ok2 && a.Compare(b) == 0
}

// Bundle is a bundle being read: its manifest is known, the payload is still to come.
type Bundle struct {
	Manifest    Manifest
	Payload     io.Reader // the compressed file system
	PayloadSize int64
	tr          *tar.Reader
}

// OpenBundle reads the manifest at the start of a bundle and leaves the payload to be read. Nothing but the first two entries is read.
func OpenBundle(r io.Reader) (*Bundle, error) {
	tr := tar.NewReader(r)
	h, err := tr.Next()
	if err != nil {
		return nil, errors.New("this is not an update file")
	}
	if h.Name != manifestName || h.Typeflag != tar.TypeReg {
		return nil, errors.New("this is not an update file (it does not start with a manifest)")
	}
	if h.Size > maxManifest {
		return nil, errors.New("the manifest in the update file is too big")
	}
	raw, err := io.ReadAll(io.LimitReader(tr, maxManifest+1))
	if err != nil {
		return nil, errors.New("the update file is damaged")
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("the manifest in the update file cannot be read")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	h, err = tr.Next()
	if err != nil || h.Name != payloadName || h.Typeflag != tar.TypeReg {
		return nil, errors.New("the update file holds no system")
	}
	return &Bundle{Manifest: m, Payload: tr, PayloadSize: h.Size, tr: tr}, nil
}

// Drain reads what is left of the archive (so that a checksum of the whole file covers all of it). The caller reads whatever follows
// the archive's end marker (padding) itself.
func (b *Bundle) Drain() error {
	for {
		if _, err := io.Copy(io.Discard, b.tr); err != nil {
			return err
		}
		if _, err := b.tr.Next(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// Progress is told how much of the payload has been read, and how much there is.
type Progress func(done, total int64)

type countingReader struct {
	r    io.Reader
	n    int64
	tell func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if n > 0 && c.tell != nil {
		c.tell(c.n)
	}
	return n, err
}

// ErrWrongData is returned when what was read does not match the manifest's checksums or sizes.
var ErrWrongData = errors.New("the update file is damaged: its contents do not match its checksums")

const (
	wipeBytes = 2 << 20  // the head of the slot that is wiped first, so that a half-written one has no file system
	syncEvery = 32 << 20 // dirty data is pushed to the disk this often, which keeps a small machine's memory free
	chunk     = 1 << 20
)

// WriteSlot decompresses the bundle's payload onto the device (the partition of the slot that is not running), checking it against the
// manifest as it goes. The head of the partition is wiped first and the label is the last thing set (by the caller), so that a slot that
// was not written to the end can never be mistaken for a system.
//
// dev is a path; in tests it is a plain file.
func WriteSlot(ctx context.Context, b *Bundle, dev string, progress Progress) error {
	m := b.Manifest
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("the space for the new system could not be opened: %w", err)
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("the size of the space for the new system is not known: %w", err)
	}
	if m.RootfsSize > size {
		return fmt.Errorf("the system in the update (%d MB) does not fit the space for it (%d MB)", m.RootfsSize>>20, size>>20)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	wipe := make([]byte, chunk)
	for off := int64(0); off < wipeBytes && off < size; off += chunk {
		if _, err := f.Write(wipe); err != nil {
			return fmt.Errorf("writing to the space for the new system failed: %w", err)
		}
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("writing to the space for the new system failed: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	payloadSum := sha256.New()
	rawSum := sha256.New()
	counted := &countingReader{r: io.TeeReader(b.Payload, payloadSum)}
	if progress != nil {
		counted.tell = func(n int64) { progress(n, b.PayloadSize) }
	}
	zr, err := gzip.NewReader(counted)
	if err != nil {
		return ErrWrongData
	}
	buf := make([]byte, chunk)
	var written, synced int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := zr.Read(buf)
		if n > 0 {
			if written+int64(n) > m.RootfsSize {
				return ErrWrongData
			}
			rawSum.Write(buf[:n])
			if _, err := f.Write(buf[:n]); err != nil {
				return fmt.Errorf("writing to the space for the new system failed: %w", err)
			}
			written += int64(n)
			if written-synced >= syncEvery {
				if err := f.Sync(); err != nil {
					return fmt.Errorf("writing to the space for the new system failed: %w", err)
				}
				synced = written
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if errors.Is(rerr, context.Canceled) || errors.Is(rerr, context.DeadlineExceeded) {
				return rerr
			}
			return ErrWrongData
		}
	}
	// whatever follows the compressed data in the entry is still part of what the checksum covers
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return ErrWrongData
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("writing to the space for the new system failed: %w", err)
	}
	if written != m.RootfsSize || !strings.EqualFold(hex.EncodeToString(rawSum.Sum(nil)), m.RootfsSHA256) ||
		!strings.EqualFold(hex.EncodeToString(payloadSum.Sum(nil)), m.PayloadSHA256) {
		return ErrWrongData
	}
	return nil
}

// MakeBundle builds a bundle from a root file system image the way buildImage.sh does (used by the development run, which has no image
// build, and by tests).
func MakeBundle(version, notes string, rootfs []byte) ([]byte, error) {
	var gzb bytes.Buffer
	zw := gzip.NewWriter(&gzb)
	if _, err := zw.Write(rootfs); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	m := Manifest{Format: FormatVersion, Version: version, Built: time.Now().UTC().Format(time.RFC3339), Arch: runtime.GOARCH, Layout: LayoutVersion,
		RootfsSize: int64(len(rootfs)), RootfsSHA256: sum(rootfs), PayloadSHA256: sum(gzb.Bytes()), Notes: notes}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for _, e := range []struct {
		name string
		data []byte
	}{{manifestName, raw}, {payloadName, gzb.Bytes()}} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0644, Size: int64(len(e.data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(e.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

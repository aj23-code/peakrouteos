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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRootfs is a "file system" for tests: bytes with a recognisable pattern (not just zeros, so that a short write is seen).
func fakeRootfs(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func gz(t testing.TB, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// bundleOf makes a bundle the way buildImage.sh does; mod may spoil the manifest before it is written.
func bundleOf(t testing.TB, version string, rootfs []byte, mod func(*Manifest)) []byte {
	t.Helper()
	payload := gz(t, rootfs)
	m := Manifest{Format: FormatVersion, Version: version, Built: "2026-10-20T10:00:00Z", Arch: "amd64", Layout: LayoutVersion,
		RootfsSize: int64(len(rootfs)), RootfsSHA256: sum(rootfs), PayloadSHA256: sum(payload), Notes: "test"}
	if mod != nil {
		mod(&m)
	}
	raw, _ := json.Marshal(m)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range []struct {
		name string
		data []byte
	}{{manifestName, raw}, {payloadName, payload}} {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0644, Size: int64(len(e.data)), Typeflag: tar.TypeReg})
		tw.Write(e.data)
	}
	tw.Close()
	return buf.Bytes()
}

func slotFile(t testing.TB, size int, fill byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "slot")
	data := bytes.Repeat([]byte{fill}, size)
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestABundleIsWrittenIntoTheSlotExactly(t *testing.T) {
	rootfs := fakeRootfs(5<<20 + 123)
	b, err := OpenBundle(bytes.NewReader(bundleOf(t, "v0.10.0", rootfs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if b.Manifest.Version != "v0.10.0" || b.PayloadSize == 0 {
		t.Fatalf("manifest: %+v, payload %d", b.Manifest, b.PayloadSize)
	}
	dev := slotFile(t, 8<<20, 0xAA)
	var last int64
	if err := WriteSlot(context.Background(), b, dev, func(done, total int64) { last = done; _ = total }); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dev)
	if !bytes.Equal(got[:len(rootfs)], rootfs) {
		t.Fatal("the slot does not hold the file system")
	}
	if last != b.PayloadSize {
		t.Errorf("progress ended at %d of %d", last, b.PayloadSize)
	}
	// behind the file system the old contents are not touched (the file system is grown to the partition afterwards)
	if got[len(rootfs)+10] != 0xAA {
		t.Error("the slot was overwritten behind the file system")
	}
}

func TestTheHeadOfTheSlotIsWipedBeforeAnythingIsWritten(t *testing.T) {
	rootfs := fakeRootfs(3 << 20)
	// a bundle whose payload goes wrong halfway: the slot must not be left with the old file system's start, nor a good-looking new one
	data := bundleOf(t, "v0.10.0", rootfs, nil)
	data[len(data)/2+700] ^= 0xFF
	b, err := OpenBundle(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	dev := slotFile(t, 8<<20, 0xAA)
	if err := WriteSlot(context.Background(), b, dev, nil); err == nil {
		t.Fatal("a damaged payload was accepted")
	}
	got, _ := os.ReadFile(dev)
	if bytes.Contains(got[:wipeBytes], bytes.Repeat([]byte{0xAA}, 4096)) {
		t.Error("the old file system's start was left in place")
	}
}

func TestDamagedBundlesAreRefused(t *testing.T) {
	rootfs := fakeRootfs(2 << 20)
	good := bundleOf(t, "v0.10.0", rootfs, nil)
	cases := map[string]func() []byte{
		"a different file system than the manifest says": func() []byte {
			return bundleOf(t, "v0.10.0", rootfs, func(m *Manifest) { m.RootfsSHA256 = sum([]byte("other")) })
		},
		"a payload that is not the one named": func() []byte {
			return bundleOf(t, "v0.10.0", rootfs, func(m *Manifest) { m.PayloadSHA256 = sum([]byte("other")) })
		},
		"a size that is too small": func() []byte {
			return bundleOf(t, "v0.10.0", rootfs, func(m *Manifest) { m.RootfsSize = int64(len(rootfs)) - 1 })
		},
		"a size that is too big": func() []byte {
			return bundleOf(t, "v0.10.0", rootfs, func(m *Manifest) { m.RootfsSize = int64(len(rootfs)) + 1 })
		},
		"a cut-off file": func() []byte { return good[:len(good)-3000] },
		"a flipped byte": func() []byte {
			d := append([]byte(nil), good...)
			d[len(d)/2] ^= 1
			return d
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			b, err := OpenBundle(bytes.NewReader(mk()))
			if err != nil {
				return // refused at the manifest
			}
			dev := slotFile(t, 4<<20, 0)
			if err := WriteSlot(context.Background(), b, dev, nil); err == nil {
				t.Fatal("it was accepted")
			}
		})
	}
}

func TestManifestsThatMakeNoSenseAreRefused(t *testing.T) {
	rootfs := fakeRootfs(1 << 20)
	for name, mod := range map[string]func(*Manifest){
		"a format from the future":       func(m *Manifest) { m.Format = 2 },
		"no version":                     func(m *Manifest) { m.Version = "" },
		"a version that is not a number": func(m *Manifest) { m.Version = "latest" },
		"no architecture":                func(m *Manifest) { m.Arch = "" },
		"no size":                        func(m *Manifest) { m.RootfsSize = 0 },
		"an absurd size":                 func(m *Manifest) { m.RootfsSize = maxRootfs + 1 },
		"a checksum that is not one":     func(m *Manifest) { m.RootfsSHA256 = "abc" },
	} {
		if _, err := OpenBundle(bytes.NewReader(bundleOf(t, "v0.10.0", rootfs, mod))); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// not an update at all
	for name, data := range map[string][]byte{"text": []byte("hello"), "empty": nil, "gzip": gz(t, []byte("x"))} {
		if _, err := OpenBundle(bytes.NewReader(data)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestTheManifestMustComeFirst(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: payloadName, Mode: 0644, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("abc"))
	tw.Close()
	if _, err := OpenBundle(&buf); err == nil || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("a bundle with no manifest first: %v", err)
	}
}

func TestABundleThatDoesNotFitTheSlotIsRefusedBeforeWriting(t *testing.T) {
	rootfs := fakeRootfs(3 << 20)
	b, err := OpenBundle(bytes.NewReader(bundleOf(t, "v0.10.0", rootfs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	dev := slotFile(t, 2<<20, 0x55)
	if err := WriteSlot(context.Background(), b, dev, nil); err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Fatalf("got %v", err)
	}
	got, _ := os.ReadFile(dev)
	if !bytes.Equal(got, bytes.Repeat([]byte{0x55}, 2<<20)) {
		t.Error("the slot was touched although the system does not fit")
	}
}

func TestWritingCanBeCancelled(t *testing.T) {
	rootfs := fakeRootfs(6 << 20)
	b, err := OpenBundle(bytes.NewReader(bundleOf(t, "v0.10.0", rootfs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err = WriteSlot(ctx, b, slotFile(t, 8<<20, 0), func(done, total int64) {
		if calls++; calls == 3 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestCompatibility(t *testing.T) {
	m := Manifest{Arch: "amd64", Layout: LayoutVersion, RootfsSize: 100 << 20}
	ok := Target{Arch: "amd64", Layout: LayoutVersion, SlotBytes: 200 << 20}
	if err := m.Compatible(ok); err != nil {
		t.Fatal(err)
	}
	for name, tg := range map[string]Target{
		"another processor": {Arch: "arm64", Layout: LayoutVersion},
		"another layout":    {Arch: "amd64", Layout: 2},
		"a slot too small":  {Arch: "amd64", Layout: LayoutVersion, SlotBytes: 50 << 20},
	} {
		if err := m.Compatible(tg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// an unknown slot size is not a reason to refuse (the write checks it)
	if err := m.Compatible(Target{Arch: "amd64", Layout: LayoutVersion}); err != nil {
		t.Errorf("unknown slot size: %v", err)
	}
	m.Version = "v0.10.0"
	if !m.Same("v0.10.0") || m.Same("v0.10.1") || m.Same("garbage") {
		t.Error("Same")
	}
}

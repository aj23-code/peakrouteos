package update

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newUBootLab is a U6+ that runs PeakRoute AP from slot a (on trial or not), with U-Boot's environment as install.sh leaves it.
func newUBootLab(t *testing.T, running Slot, trial bool) *lab {
	t.Helper()
	l := newLab(t, running, trial, SlotA)
	cmd := "console=ttyS0,115200n8 root=LABEL=peakroute-root-" + string(running) + " ro rootwait panic=10 cma=16M peakroute.slot=" + string(running) + " peakroute.trial="
	if trial {
		cmd += "1"
	}
	cmd += " firmware_class.path=/run/peakroute/firmware\n"
	for name, data := range map[string]string{
		"cmdline": cmd,
		"release": "PEAKROUTE_VERSION=v0.5.1-alpha\nPEAKROUTE_LAYOUT=1\nPEAKROUTE_ARCH=arm64\nPEAKROUTE_PRODUCT=ap\nPEAKROUTE_BOARD=ubnt-u6plus\n",
	} {
		if err := os.WriteFile(filepath.Join(l.dir, name), []byte(data), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, sl := range []Slot{SlotA, SlotB} {
		if err := os.WriteFile(filepath.Join(l.dir, "boot-"+string(sl)), make([]byte, 1<<20), 0644); err != nil {
			t.Fatal(err)
		}
	}
	l.s.Arch = "arm64"
	l.s.Version = "v0.5.1-alpha"
	l.s.BootDevice = func(sl Slot) string { return filepath.Join(l.dir, "boot-"+string(sl)) }
	l.s.PartitionStart = func(name string) (int64, error) {
		switch name {
		case "peakroute-boot-a":
			return 1595392, nil
		case "peakroute-boot-b":
			return 1726464, nil
		}
		return 0, errors.New("no such partition")
	}
	// as install.sh writes it: slot a, its boot image, the steps (no pb_slot and no pb_count_a: the first installs did not say)
	for k, v := range map[string]string{
		"pb_start": "0x185800", "pb_count": "0x161de", "pb_args": strings.TrimSpace(strings.Replace(cmd, "peakroute.trial=1", "peakroute.trial=", 1)),
		"boot_peak": "run pl_sig; run pl_dev pl_read pl_kernel pl_boot", "bootcmd_real": "run boot_peak; run boot_openwrt",
	} {
		l.uboot[k] = v
	}
	return l
}

// apBundle is a U6+ bundle: the system and, unless noBoot, its boot image (a third entry), with the manifest saying so.
func apBundle(t *testing.T, version string, rootfs, boot []byte, mod func(*Manifest)) []byte {
	t.Helper()
	payload := gz(t, rootfs)
	m := Manifest{Format: FormatVersion, Version: version, Built: "2026-11-01T10:00:00Z", Arch: "arm64", Product: ProductAP, Board: "ubnt-u6plus",
		Layout: LayoutVersion, RootfsSize: int64(len(rootfs)), RootfsSHA256: sum(rootfs), PayloadSHA256: sum(payload)}
	entries := []struct {
		name string
		data []byte
	}{{manifestName, nil}, {payloadName, payload}}
	if boot != nil {
		m.Boot = &BootImage{Size: int64(len(boot)), SHA256: sum(boot)}
		entries = append(entries, struct {
			name string
			data []byte
		}{BootName, boot})
	}
	if mod != nil {
		mod(&m)
	}
	entries[0].data, _ = json.Marshal(m)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0644, Size: int64(len(e.data)), Typeflag: tar.TypeReg})
		tw.Write(e.data)
	}
	tw.Close()
	return buf.Bytes()
}

// An update on the U6+, the whole way: the system goes into the other slot and its boot image into that slot's boot partition; the
// restart sets U-Boot up to start it once, on trial (any start after it is the kept slot); keeping it makes it what U-Boot starts.
func TestAnUpdateOnTheU6PlusFromWritingToKeeping(t *testing.T) {
	l := newUBootLab(t, SlotA, false)
	ctx := context.Background()
	l.s.State(ctx) // records what slot a holds, as the page does
	if b := l.s.BootState(ctx); b.Problem != "" || b.Running != SlotA || b.Kept != SlotA || b.OnTrial {
		t.Fatalf("before: %+v", b)
	}
	rootfs := fakeRootfs(1 << 20)
	boot := bytes.Repeat([]byte("FIT!"), 3000) // 12000 bytes: 24 sectors (0x18), rounded up
	if _, err := l.s.Upload(ctx, bytes.NewReader(apBundle(t, "v0.6.0-alpha", rootfs, boot, nil)), 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(l.dir, "boot-b")); !bytes.HasPrefix(got, boot) {
		t.Fatal("the boot image is not in slot b's boot partition")
	}
	if got, _ := os.ReadFile(filepath.Join(l.dir, "boot-a")); !bytes.Equal(got, make([]byte, 1<<20)) {
		t.Fatal("the running slot's boot partition was written")
	}
	if l.uboot["pb_count_b"] != "0x18" {
		t.Fatalf("pb_count_b = %q", l.uboot["pb_count_b"])
	}
	if l.uboot["bootcmd_real"] != "run boot_peak; run boot_openwrt" || l.uboot["pb_start"] != "0x185800" {
		t.Fatal("writing changed what starts")
	}

	if err := l.s.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	<-l.reboots
	u := l.uboot
	if u["pt_start"] != "0x1a5800" || u["pt_count"] != "0x18" {
		t.Fatalf("trial boot image: %s %s", u["pt_start"], u["pt_count"])
	}
	for _, want := range []string{"root=LABEL=peakroute-root-b", "peakroute.slot=b", "peakroute.trial=1", "console=ttyS0,115200n8", "cma=16M", "firmware_class.path=/run/peakroute/firmware"} {
		if !strings.Contains(" "+u["pt_args"]+" ", " "+want+" ") {
			t.Errorf("pt_args lacks %s: %s", want, u["pt_args"])
		}
	}
	if u["bootcmd_real"] != "setenv bootcmd_real run boot_kept; saveenv; run boot_trial; run boot_kept" || u["boot_kept"] != "run boot_peak; run boot_openwrt" ||
		u["boot_trial"] != "run pl_sig; run pl_dev pt_read pl_kernel pt_boot" || !strings.Contains(u["pt_read"], "${pt_start} ${pt_count}") ||
		!strings.Contains(u["pt_boot"], "setenv bootargs ${pt_args}; booti ") {
		t.Fatalf("the trial start: %v", u)
	}
	if u["pb_start"] != "0x185800" || u["pb_args"] == u["pt_args"] {
		t.Fatal("the kept slot was changed by the trial")
	}

	// U-Boot starts slot b once and sets itself back first
	l.uboot["bootcmd_real"] = "run boot_kept"
	l.s.CmdlinePath = filepath.Join(l.dir, "cmdline-b")
	if err := os.WriteFile(l.s.CmdlinePath, []byte(u["pt_args"]+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if b := l.s.BootState(ctx); b.Running != SlotB || b.Kept != SlotA || !b.OnTrial {
		t.Fatalf("on trial: %+v", b)
	}
	if err := l.s.Keep(ctx); err != nil {
		t.Fatal(err)
	}
	if u["pb_slot"] != "b" || u["pb_start"] != "0x1a5800" || u["pb_count"] != "0x18" || u["bootcmd_real"] != "run boot_kept" {
		t.Fatalf("kept: %v", u)
	}
	if !strings.Contains(u["pb_args"], "peakroute.slot=b") || !strings.Contains(u["pb_args"], "root=LABEL=peakroute-root-b") ||
		!strings.Contains(u["pb_args"], "peakroute.trial= ") && !strings.HasSuffix(u["pb_args"], "peakroute.trial=") || strings.Contains(u["pb_args"], "peakroute.trial=1") {
		t.Fatalf("kept command line: %s", u["pb_args"])
	}
	if b := l.s.BootState(ctx); b.Kept != SlotB || b.OnTrial {
		t.Fatalf("after keeping: %+v", b)
	}

	// going back to the version before (slot a): a trial of slot a, whose boot image length keeping slot b saved
	if u["pb_count_a"] != "0x161de" {
		t.Fatalf("pb_count_a = %q", u["pb_count_a"])
	}
	if err := l.s.GoBack(ctx); err != nil {
		t.Fatal(err)
	}
	<-l.reboots
	if u["pt_start"] != "0x185800" || u["pt_count"] != "0x161de" || !strings.Contains(u["pt_args"], "peakroute.slot=a") ||
		!strings.Contains(u["pt_args"], "root=LABEL=peakroute-root-a") || !strings.Contains(u["pt_args"], "peakroute.trial=1") {
		t.Fatalf("trial of slot a: %s %s %s", u["pt_start"], u["pt_count"], u["pt_args"])
	}
}

// A bundle for the U6+ must bring its boot image: without one (or with one that does not match the manifest) nothing is staged.
func TestAU6PlusBundleWithoutItsBootImageIsRefused(t *testing.T) {
	ctx := context.Background()
	rootfs := fakeRootfs(1 << 20)
	boot := bytes.Repeat([]byte("FIT!"), 3000)
	for name, data := range map[string][]byte{
		"no boot image": apBundle(t, "v0.6.0-alpha", rootfs, nil, nil),
		"a damaged boot image": apBundle(t, "v0.6.0-alpha", rootfs, boot, func(m *Manifest) {
			m.Boot.SHA256 = strings.Repeat("0", 64)
		}),
	} {
		l := newUBootLab(t, SlotA, false)
		if _, err := l.s.Upload(ctx, bytes.NewReader(data), 0); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if l.s.st.Staged != nil || l.uboot["pb_count_b"] != "" {
			t.Errorf("%s: something was staged", name)
		}
		if err := l.s.Restart(ctx); err == nil {
			t.Errorf("%s: a restart into it was allowed", name)
		}
	}
}

// A router's bundle never has a boot image, and a router reads past one (GRUB starts the kernel inside the system): no U-Boot.
func TestARouterIgnoresABootImage(t *testing.T) {
	l := newLab(t, SlotA, false, SlotA)
	rootfs := fakeRootfs(1 << 20)
	data := apBundle(t, "v1.0.0", rootfs, []byte("not for a router"), func(m *Manifest) { m.Arch, m.Product, m.Board = "amd64", ProductRouter, BoardGeneric })
	if _, err := l.s.Upload(context.Background(), bytes.NewReader(data), 0); err != nil {
		t.Fatal(err)
	}
	if l.called("fw_") != 0 {
		t.Fatal("a router used U-Boot's tools")
	}
}

func TestBootArgsForASlot(t *testing.T) {
	base := "console=ttyS0,115200n8 root=LABEL=peakroute-root-a ro rootwait panic=10 cma=16M peakroute.slot=a peakroute.trial= firmware_class.path=/x"
	if got := bootArgsFor(base, SlotB, true); got != "console=ttyS0,115200n8 root=LABEL=peakroute-root-b ro rootwait panic=10 cma=16M peakroute.slot=b peakroute.trial=1 firmware_class.path=/x" {
		t.Fatal(got)
	}
	if got := bootArgsFor("console=ttyS0", SlotA, false); got != "console=ttyS0 root=LABEL=peakroute-root-a peakroute.slot=a peakroute.trial=" {
		t.Fatal(got)
	}
}

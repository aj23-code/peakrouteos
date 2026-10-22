package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The script that grows the data partition runs here against a disk image in a file, with the commands that need a real block
// device (findmnt, lsblk, partx, resize2fs) replaced by recorders. sfdisk and sgdisk are the real ones.

type growLab struct {
	t     *testing.T
	dir   string
	disk  string
	calls string
	mark  string
	env   []string
}

func need(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed here", tool)
		}
	}
}

func newGrowLab(t *testing.T, imageMiB, diskMiB int) *growLab {
	t.Helper()
	need(t, "sfdisk", "sgdisk", "awk", "sh")
	dir := t.TempDir()
	l := &growLab{t: t, dir: dir, disk: filepath.Join(dir, "disk.img"), calls: filepath.Join(dir, "calls"), mark: filepath.Join(dir, "state", "datagrown")}
	if err := os.Truncate(l.disk, 0); err != nil {
		f, err := os.Create(l.disk)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if err := os.Truncate(l.disk, int64(imageMiB)<<20); err != nil {
		t.Fatal(err)
	}
	layout := "label: gpt\nunit: sectors\nfirst-lba: 2048\n" +
		"start=2048, size=2048, type=21686148-6449-6E6F-744E-656564454649, name=\"BIOS boot\"\n" +
		"size=16384, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, name=\"EFI System\"\n" +
		"size=8192, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"peakroute-root-a\"\n" +
		"size=8192, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"peakroute-root-b\"\n" +
		"type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"peakroute-data\"\n"
	cmd := exec.Command("sfdisk", "--quiet", l.disk)
	cmd.Stdin = strings.NewReader(layout)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %v\n%s", err, out)
	}
	// the image is written to a bigger disk
	if err := os.Truncate(l.disk, int64(diskMiB)<<20); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0755)
	fake := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	fake("findmnt", "echo /dev/fakedata5\n")
	// the real readlink would follow the label path the mount table can show to the device; here the one link the tests know
	fake("readlink", `case "$2" in /dev/disk/by-label/peakroute-data) echo /dev/fakedata5 ;; *) echo "$2" ;; esac`+"\n")
	fake("lsblk", "echo fakedisk\n")
	fake("partx", `echo "partx $*" >> `+l.calls+"\n")
	fake("resize2fs", `echo "resize2fs $*" >> `+l.calls+"\n")

	sys := filepath.Join(dir, "sys")
	os.MkdirAll(filepath.Join(sys, "fakedata5"), 0755)
	os.WriteFile(filepath.Join(sys, "fakedata5", "partition"), []byte("5\n"), 0644)
	l.env = []string{"PATH=" + bin + ":/usr/sbin:/usr/bin:/sbin:/bin", "PEAKROUTE_DATA_DEV=/dev/fakedata5", "PEAKROUTE_SYS_BLOCK=" + sys,
		"PEAKROUTE_DISK=" + l.disk, "PEAKROUTE_GROW_MARK=" + l.mark}
	return l
}

func (l *growLab) run(extraEnv ...string) (string, error) {
	cmd := exec.Command("sh", "overlay/usr/lib/peakroute/growdata")
	cmd.Env = append(append([]string(nil), l.env...), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (l *growLab) recorded() string {
	b, _ := os.ReadFile(l.calls)
	return strings.TrimSpace(string(b))
}

// partition returns the start and size, in sectors, of a partition of the image.
func (l *growLab) partition(n int) (start, size int64) {
	l.t.Helper()
	out, err := exec.Command("sfdisk", "-d", l.disk).CombinedOutput()
	if err != nil {
		l.t.Fatalf("sfdisk -d: %v\n%s", err, out)
	}
	re := regexp.MustCompile(`disk\.img` + strconv.Itoa(n) + ` : start=\s*(\d+), size=\s*(\d+)`)
	m := re.FindStringSubmatch(string(out))
	if m == nil {
		l.t.Fatalf("no partition %d in:\n%s", n, out)
	}
	start, _ = strconv.ParseInt(m[1], 10, 64)
	size, _ = strconv.ParseInt(m[2], 10, 64)
	return
}

func (l *growLab) verify() string {
	out, _ := exec.Command("sgdisk", "-v", l.disk).CombinedOutput()
	return string(out)
}

func TestTheDataPartitionGrowsToFillALargerDisk(t *testing.T) {
	l := newGrowLab(t, 64, 512)
	out, err := l.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	start, size := l.partition(5)
	diskSectors := int64(512<<20) / 512
	// the last 33 sectors hold the second copy of the partition table; sfdisk keeps the end on a 1 MiB boundary
	if end := start + size; end > diskSectors-33 || end < diskSectors-33-2048 {
		t.Fatalf("the data partition ends at sector %d of %d (it starts at %d and is %d long): %s", end, diskSectors, start, size, out)
	}
	if v := l.verify(); !strings.Contains(v, "No problems found") {
		t.Fatalf("the partition table is not clean afterwards:\n%s", v)
	}
	// the other four partitions (the two slots among them) did not move
	for n, want := range map[int][2]int64{1: {2048, 2048}, 2: {4096, 16384}, 3: {20480, 8192}, 4: {28672, 8192}} {
		if s, z := l.partition(n); s != want[0] || z != want[1] {
			t.Fatalf("partition %d: %d %d, want %v", n, s, z, want)
		}
	}
	calls := l.recorded()
	if !strings.Contains(calls, "partx -u --nr 5 "+l.disk) || !strings.Contains(calls, "resize2fs /dev/fakedata5") {
		t.Fatalf("the kernel and the file system were not told:\n%s", calls)
	}
	if strings.Index(calls, "partx") > strings.Index(calls, "resize2fs") {
		t.Fatalf("the file system was grown before the kernel knew the partition was bigger:\n%s", calls)
	}
	if _, err := os.Stat(l.mark); err != nil {
		t.Fatalf("no mark: %v", err)
	}
	// and the next start does nothing
	os.Remove(l.calls)
	if out, err := l.run(); err != nil || l.recorded() != "" {
		t.Fatalf("it ran again: %v %s %q", err, out, l.recorded())
	}
}

func TestADiskWithNoRoomLeftOnlyChecksTheFileSystem(t *testing.T) {
	l := newGrowLab(t, 64, 64)
	_, sizeBefore := l.partition(5)
	out, err := l.run()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(l.recorded(), "partx") {
		t.Fatalf("the partition was changed with no room:\n%s", l.recorded())
	}
	if !strings.Contains(l.recorded(), "resize2fs") {
		t.Fatal("the file system was not checked")
	}
	if _, sizeAfter := l.partition(5); sizeAfter != sizeBefore {
		t.Fatalf("the partition changed from %d to %d sectors with no room", sizeBefore, sizeAfter)
	}
	if _, err := os.Stat(l.mark); err != nil {
		t.Fatal("no mark")
	}
}

func TestNothingIsDoneWhenTheDataIsNotOnAPartition(t *testing.T) {
	l := newGrowLab(t, 64, 128)
	before, _ := os.ReadFile(l.disk)
	// the data file system is an overlay or a network share, not a /dev name
	if out, err := l.run("PEAKROUTE_DATA_DEV=overlay"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// or a device with no partition number (a whole disk)
	os.Remove(l.mark)
	if out, err := l.run("PEAKROUTE_DATA_DEV=/dev/fakedisk"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if l.recorded() != "" {
		t.Fatalf("commands ran: %s", l.recorded())
	}
	after, _ := os.ReadFile(l.disk)
	if string(before) != string(after) {
		t.Fatal("the disk changed")
	}
	if _, err := os.Stat(l.mark); err != nil {
		t.Fatal("no mark: it would be tried again at every start")
	}
}

func TestAFailureIsReportedAndTriedAgain(t *testing.T) {
	l := newGrowLab(t, 64, 128)
	os.WriteFile(filepath.Join(l.dir, "bin", "resize2fs"), []byte("#!/bin/sh\necho broken >&2\nexit 1\n"), 0755)
	out, err := l.run()
	if err == nil {
		t.Fatalf("a failing resize2fs was ignored:\n%s", out)
	}
	if _, err := os.Stat(l.mark); err == nil {
		t.Fatal("marked as done although it failed")
	}
	// a disk that cannot be found
	if out, err := l.run("PEAKROUTE_DISK=" + filepath.Join(l.dir, "missing")); err == nil {
		t.Fatalf("a missing disk was ignored:\n%s", out)
	}
}

func TestTheScriptIsPlainShell(t *testing.T) {
	need(t, "sh")
	for _, f := range []string{"overlay/usr/lib/peakroute/growdata", "../internal/scripts/hostapd-run.sh"} {
		if out, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", f, err, out)
		}
	}
}

// The initramfs mounts the data partition by its label, so the mount table can name it /dev/disk/by-label/peakroute-data. The script
// once took that for "not a partition", wrote its mark and grew nothing, so a router flashed to a big disk kept a data partition of
// about 0.2 GB. It follows the link to the device first now.
func TestAPartitionNamedByItsLabelIsStillGrown(t *testing.T) {
	l := newGrowLab(t, 64, 512)
	out, err := l.run("PEAKROUTE_DATA_DEV=/dev/disk/by-label/peakroute-data")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	start, size := l.partition(5)
	diskSectors := int64(512<<20) / 512
	if end := start + size; end > diskSectors-33 || end < diskSectors-33-2048 {
		t.Fatalf("the data partition was not grown (ends at sector %d of %d): %s", end, diskSectors, out)
	}
	if !strings.Contains(l.recorded(), "resize2fs /dev/fakedata5") {
		t.Fatalf("the file system was not grown:\n%s", l.recorded())
	}
}

func TestTheMarkOfTheFirstVersionDoesNotStopTheNewOne(t *testing.T) {
	data, err := os.ReadFile("overlay/etc/systemd/system/peakroute-growdata.service")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("overlay/usr/lib/peakroute/growdata")
	if err != nil {
		t.Fatal(err)
	}
	// a router that the first version marked done (having done nothing) has /var/lib/peakroute/datagrown: neither the service nor the script
	// may look at that file
	if strings.Contains(string(data), "datagrown\n") || strings.Contains(string(script), "datagrown}") || !strings.Contains(string(data), "datagrown2") || !strings.Contains(string(script), "datagrown2") {
		t.Fatal("the marker is not the new one in both the service and the script")
	}
}

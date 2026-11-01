package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The A/B scheme on a board whose boot loader is U-Boot (the UniFi 6 Plus, docs/u6plus.md), beside GRUB's on a PC. What runs is the
// same on both: the kernel is told peakroute.slot (and peakroute.trial=1 on a trial). What differs is where the kept slot and the trial
// are said, and that U-Boot starts a boot image of its own (kernel, device tree, initramfs) from a partition per slot
// (peakroute-boot-a/-b), which an update writes too (the bundle's boot.fit).
//
// U-Boot's environment (fw_printenv / fw_setenv; the U6+'s is in SPI flash), as image/boards/ubnt-u6plus/install.sh lays it out and
// this file keeps it:
//
//	pb_slot                     the kept slot (a when it is not set: the installer installs slot a)
//	pb_start pb_count pb_args   the kept slot's boot image (first sector, length in sectors, both hexadecimal) and kernel command line
//	pb_count_a pb_count_b       each slot's boot image length, as it was written
//	boot_peak                   starts the kept slot (install.sh: pl_sig, pl_dev, pl_read, pl_kernel, pl_boot)
//	boot_kept                   run boot_peak; run boot_openwrt (OpenWrt when PeakRoute's boot image cannot be read)
//	pt_start pt_count pt_args   the slot on trial; pt_read and pt_boot read and start it; boot_trial runs them
//	bootcmd_real                what Ubiquiti's bootubnt runs: run boot_kept, or for one start "set myself back to run boot_kept,
//	                            save, start the trial": any start after it (a crash, the watchdog, a power cut) is the kept slot.
//
// U-Boot here has the simple command parser: no && or ||, and "run a b c" stops at the first variable whose commands failed.

// Bootloaders: where the kept slot and a trial are said.
const (
	LoaderGRUB  = "grub"
	LoaderUBoot = "uboot"
)

// Where the U6+'s U-Boot puts what it reads (the same as install.sh's LOADADDR and KERNEL_ADDR; buildImageU6Plus.sh says why).
const (
	ubootLoadAddr   = "0x44000000"
	ubootKernelAddr = "0x40200000"
)

// loader is the boot loader of this system: what its release file says (PEAKROUTE_BOOTLOADER), else U-Boot on the U6+ (whose first
// images did not say), else GRUB.
func (s *Service) loader() string {
	rel := s.Release()
	switch rel["PEAKROUTE_BOOTLOADER"] {
	case LoaderUBoot:
		return LoaderUBoot
	case LoaderGRUB:
		return LoaderGRUB
	}
	if s.Identity().Board == "ubnt-u6plus" {
		return LoaderUBoot
	}
	return LoaderGRUB
}

// readUBootEnv returns U-Boot's environment.
func (s *Service) readUBootEnv(ctx context.Context) (map[string]string, error) {
	out, err := s.run(ctx, "fw_printenv")
	if err != nil {
		return nil, fmt.Errorf("the boot loader's settings cannot be read: %w", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimRight(line, "\r"), "="); ok && k != "" {
			env[k] = v
		}
	}
	return env, nil
}

// setUBootEnv sets variables of U-Boot's environment, all at once (fw_setenv -s, Debian's libubootenv: name=value lines; an empty value
// removes the variable).
func (s *Service) setUBootEnv(ctx context.Context, kv [][2]string) error {
	f, err := os.CreateTemp(s.ConfigDir, ".uboot-env-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	for _, p := range kv {
		if strings.ContainsAny(p[0], "=\n") || strings.Contains(p[1], "\n") {
			f.Close()
			return fmt.Errorf("not a boot loader setting: %q", p[0])
		}
		fmt.Fprintf(f, "%s=%s\n", p[0], p[1])
	}
	if err := f.Close(); err != nil {
		return err
	}
	if out, err := s.run(ctx, "fw_setenv", "-s", f.Name()); err != nil {
		return fmt.Errorf("the boot loader's settings could not be changed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ubootKept is the kept slot U-Boot's environment says.
func ubootKept(env map[string]string) Slot {
	if sl := Slot(env["pb_slot"]); sl.Valid() {
		return sl
	}
	return SlotA
}

// bootArgsFor is the kernel command line for a slot, from the one in use: the root file system, peakroute.slot and peakroute.trial
// set for it, everything else (console, cma, firmware path...) as it is.
func bootArgsFor(base string, sl Slot, trial bool) string {
	set := map[string]string{"root": "LABEL=" + sl.Label(), "peakroute.slot": string(sl), "peakroute.trial": ""}
	if trial {
		set["peakroute.trial"] = "1"
	}
	order := []string{"root", "peakroute.slot", "peakroute.trial"}
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(base) {
		k, _, _ := strings.Cut(f, "=")
		if v, ok := set[k]; ok {
			out = append(out, k+"="+v)
			seen[k] = true
			continue
		}
		out = append(out, f)
	}
	for _, k := range order {
		if !seen[k] {
			out = append(out, k+"="+set[k])
		}
	}
	return strings.Join(out, " ")
}

// ubootTrial sets U-Boot up to start a slot once, on trial (and the kept slot at every start after it).
func (s *Service) ubootTrial(ctx context.Context, sl Slot) error {
	env, err := s.readUBootEnv(ctx)
	if err != nil {
		return err
	}
	count := env["pb_count_"+string(sl)]
	if count == "" && sl == ubootKept(env) {
		count = env["pb_count"]
	}
	if count == "" {
		return fmt.Errorf("slot %s has no boot image written by an update", sl)
	}
	start, err := s.partitionStart("peakroute-boot-" + string(sl))
	if err != nil {
		return err
	}
	base := env["pb_args"]
	if base == "" {
		return errors.New("the boot loader's settings have no kernel command line (pb_args): install with install.sh first")
	}
	return s.setUBootEnv(ctx, [][2]string{
		{"pt_start", fmt.Sprintf("0x%x", start)},
		{"pt_count", count},
		{"pt_args", bootArgsFor(base, sl, true)},
		{"pt_read", "mmc read " + ubootLoadAddr + " ${pt_start} ${pt_count}"},
		{"pt_boot", "setenv bootargs ${pt_args}; booti " + ubootKernelAddr + " " + ubootLoadAddr + ":ramdisk-1 " + ubootLoadAddr + ":fdt-1"},
		{"boot_trial", "run pl_sig; run pl_dev pt_read pl_kernel pt_boot"},
		{"boot_kept", "run boot_peak; run boot_openwrt"},
		{"bootcmd_real", "setenv bootcmd_real run boot_kept; saveenv; run boot_trial; run boot_kept"},
	})
}

// ubootKeep makes the slot that runs on trial the kept one: its boot image and command line become what boot_peak starts.
func (s *Service) ubootKeep(ctx context.Context, sl Slot) error {
	env, err := s.readUBootEnv(ctx)
	if err != nil {
		return err
	}
	if env["pt_start"] == "" || env["pt_count"] == "" || env["pt_args"] == "" {
		return errors.New("the boot loader's settings say nothing of a trial")
	}
	kv := [][2]string{
		{"pb_start", env["pt_start"]},
		{"pb_count", env["pt_count"]},
		{"pb_args", bootArgsFor(env["pt_args"], sl, false)},
		{"pb_slot", string(sl)},
		{"bootcmd_real", "run boot_kept"},
	}
	// the slot kept until now stays one to go back to: its boot image length is pb_count's (the first installs did not save pb_count_a)
	if old := ubootKept(env); old != sl && env["pb_count_"+string(old)] == "" && env["pb_count"] != "" {
		kv = append(kv, [2]string{"pb_count_" + string(old), env["pb_count"]})
	}
	return s.setUBootEnv(ctx, kv)
}

// partitionStart is the first sector of the partition of that name (GPT name), from the kernel's view of the disks.
func (s *Service) partitionStart(name string) (int64, error) {
	if s.PartitionStart != nil {
		return s.PartitionStart(name)
	}
	uevents, _ := filepath.Glob("/sys/class/block/*/uevent")
	for _, ue := range uevents {
		raw, err := os.ReadFile(ue)
		if err != nil || !strings.Contains(string(raw), "\nPARTNAME="+name+"\n") && !strings.HasSuffix(string(raw), "\nPARTNAME="+name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(filepath.Dir(ue), "start"))
		if err != nil {
			return 0, err
		}
		var start int64
		if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &start); err != nil {
			return 0, err
		}
		return start, nil
	}
	return 0, fmt.Errorf("there is no partition %s", name)
}

// bootDevice is the boot image partition of a slot.
func (s *Service) bootDevice(sl Slot) string {
	if s.BootDevice != nil {
		return s.BootDevice(sl)
	}
	return "/dev/disk/by-partlabel/peakroute-boot-" + string(sl)
}

// writeBootImage reads the bundle's boot image (the entry after the system) and writes it to the slot's boot partition, checked against
// the manifest; then it says its length to U-Boot (pb_count_<slot>), for the trial.
func (s *Service) writeBootImage(ctx context.Context, b *Bundle, sl Slot) error {
	m := b.Manifest
	if m.Boot == nil {
		return errors.New("this update file has no boot image, which this device starts from: it is not for this device")
	}
	h, err := b.tr.Next()
	if err != nil || h.Name != BootName || h.Size != m.Boot.Size {
		return errors.New("the update file holds no boot image where its manifest says it does")
	}
	dev, err := os.OpenFile(s.bootDevice(sl), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("the boot partition of slot %s cannot be written: %w", sl, err)
	}
	sum := sha256.New()
	n, err := io.Copy(dev, io.TeeReader(io.LimitReader(b.tr, m.Boot.Size), sum))
	if err == nil {
		err = dev.Sync()
	}
	if cerr := dev.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("the boot image could not be written: %w", err)
	}
	if n != m.Boot.Size || hex.EncodeToString(sum.Sum(nil)) != strings.ToLower(m.Boot.SHA256) {
		return ErrWrongData
	}
	return s.setUBootEnv(ctx, [][2]string{{"pb_count_" + string(sl), fmt.Sprintf("0x%x", (m.Boot.Size+511)/512)}})
}

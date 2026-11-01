# PeakRoute AP on the UniFi 6 Plus (U6+)

PeakRoute AP (the access point build, `docs/access-point.md`) runs on the Ubiquiti UniFi 6 Plus: an MT7981A (two Cortex-A53, arm64),
256 MB of RAM, a 4 GB eMMC, one gigabit port with PoE in, and an MT7976C radio (2.4 and 5 GHz at once, two Wi-Fi adapters to Linux).
Its board name is `ubnt-u6plus` (docs/updates.md, "Product, board and processors").

**Status (2026-10-30): PeakRoute AP starts on a U6+ (on trial), the wizard sets it up as an access point and both radios serve
Wi-Fi; see NOTES.md, "PeakRoute AP on the U6+", for what was found on the way.**

## What is on the U6+

Ubiquiti's boot loader (BL2 and U-Boot 2022.04) is kept: it is signed, and the U6+ has no other way to start. The eMMC keeps Ubiquiti's
partitions as they are (`bl2`, `u-boot-env`, `factory` with the Wi-Fi calibration, `u-boot`, `EEPROM`, `kernel0` and `kernel1` with
OpenWrt, `bs`, `cfg`, `log`), and PeakRoute's go into the free space after them:

| Partition | Size | What |
|---|---|---|
| `peakroute-boot-a`, `-b` | 64 MiB | the boot image of slot A and B: a FIT image (kernel, device tree, initramfs) that U-Boot reads raw |
| `peakroute-root-a`, `-b` | 1088 MiB | the system of slot A and B (ext4, read-only, label `peakroute-root-a`/`-b`) |
| `peakroute-data` | the rest (about 645 MiB) | the device's own state, laid over the system (docs/updates.md, section 2) |

Ubiquiti's partition table has room for 10 entries, all used, says the disk ends at 3 GiB and has no backup copy. `install.sh partition`
rewrites it with every entry as it was (place, size, type, id, name), room for 128, the real end of the eMMC and a backup copy, and
writes only sectors 1 to 33 and the backup (sector 0 stays Ubiquiti's). It is made and checked with sfdisk on a copy first.

## How it starts

U-Boot runs `bootcmd=bootubnt`, which runs `bootcmd_real` when it is set (OpenWrt's installation sets it to `run boot_openwrt`).
PeakRoute adds its own variables to U-Boot's settings (`install.sh` writes them with `fw_setenv -s`). Three things about this U-Boot
decide how (all found on the device, 2026-10-30):

- **It has the simple command parser**: no `&&`, `||` or `if`. A list of commands runs on after one fails, but `run a b c` stops at
  the first variable whose commands failed, so every step the next one depends on is a variable of its own (`pl_*`).
- **`bootm` cannot start Debian's kernel**: it refuses to unpack more than its `CONFIG_SYS_BOOTM_LEN` (Debian's Image is 37 MB) and
  resets the board. So the kernel is stored unpacked in the boot image, copied to its place with `imxtract` and started with
  `booti`.
- **`booti` takes no initramfs as "address:size"** (no raw initrd support): it takes the initramfs and the device tree from the boot
  image by name (`0x44000000:ramdisk-1`, `0x44000000:fdt-1`).

So `boot_peak` is `run pl_sig; run pl_dev pl_read pl_kernel pl_boot`: take away the signature U-Boot would demand of Ubiquiti's own
images (as OpenWrt does), `mmc dev 0`, `mmc read` the boot image (`pb_start`, `pb_count`: its first sector and length) to
0x44000000, `imxtract` the kernel to 0x40200000, set `bootargs` (`pb_args`) and `booti`. Memory: the kernel at 0x40200000 up to its
image_size (below the ramoops area at 0x42ff0000), the boot image at 0x44000000 (at most 64 MB), U-Boot itself near the top.

- A **trial start**: `bootcmd_real` first sets itself to `boot_fallback` and saves that, then starts PeakRoute. Any later start (a
  crash, the watchdog, a power cut) is the fallback: U-Boot copies the kernel's RAM log (ramoops, 64 KiB at 0x42ff0000) to
  `peakroute-boot-b`, then starts OpenWrt. OpenWrt's kernel reads that RAM with another layout and clears it, hence the copy;
  `install.sh HOST ramoops` reads and decodes it (`ramoops.py`). PeakRoute's device tree writes the log at every restart, not only
  at a crash (`max-reason = <4>`, 32 KiB records). `install.sh keep` makes PeakRoute the one that starts; `install.sh openwrt` goes
  back.
- The **probes**, before the first trial: `probe-env` shows that U-Boot runs PeakRoute's commands and saves its settings, `probe-load`
  that it reads the boot image, finds its checksums right (`iminfo`) and copies its parts out (`pr_load` says how far it got).
  `trial` leaves `pr_trial`: `started`, or `bootm-returned` when U-Boot could not start the kernel. No serial console is needed.

The device tree (`image/boards/ubnt-u6plus/ubnt-u6plus.dts`) is the one OpenWrt's kernel ran with, decompiled; Debian's 6.12 kernel has
every driver it names, some as modules the initramfs must carry: the eMMC (`mtk-sd`) needs its 3.3 V supply (`regulator-fixed`,
module `fixed`), or it waits for it forever and the root file system never appears. The Wi-Fi calibration is the device's own, in
the first 4 KiB of `factory`: the initramfs copies it to `/run/peakroute/firmware`, where the kernel looks first (`firmware_class.path`).

## The system

Debian 13 for arm64 with Debian's kernel, only what an access point uses (no DHCP or DNS server, VPN, PPPoE, boot loader or other
firmware), and for 256 MB of RAM: the journal in memory (8 MB), zram swap (half the RAM, zstd), `GOMEMLIMIT=48MiB` for the daemon, the
hardware watchdog (30 s), and `cma=16M` (Debian's kernel sets 64 MB aside for contiguous memory otherwise). Measured after the first
start: 212 MB usable, 90 MB available; the daemon 20 MB; the kernel's own unreclaimable memory 46 MB, mostly the Wi-Fi driver's. The first start asks the network's DHCP server for an address and serves none (docs/setup.md); the name it
sends is `peakroute`, and `192.168.77.1` answers beside it.

## Building and installing

    sudo apt install debootstrap qemu-user-static binfmt-support u-boot-tools device-tree-compiler xz-utils
    ./buildImageU6Plus.sh                                  # out/ubnt-u6plus/peakroute-ap-ubnt-u6plus-<version>.fit and .rootfs
    DEV_SSH_KEY=$PWD/out/dev-ssh.pub ./buildImageU6Plus.sh # a development build: root signs in over SSH with that key (the
                                                           # access point's firewall lets it in), for bring-up only

On a U6+ running OpenWrt (24.10 was used), with an SSH key OpenWrt accepts:

    image/boards/ubnt-u6plus/install.sh 192.168.1.116 backup      # into out/ubnt-u6plus/backup-<date>
    image/boards/ubnt-u6plus/install.sh 192.168.1.116 partition
    image/boards/ubnt-u6plus/install.sh 192.168.1.116 write
    image/boards/ubnt-u6plus/install.sh 192.168.1.116 probe-env   # then: status (pr_env=saved)
    image/boards/ubnt-u6plus/install.sh 192.168.1.116 probe-load  # then: status (pr_load=ok)
    image/boards/ubnt-u6plus/install.sh 192.168.1.116 trial

While the U6+ runs PeakRoute its OpenWrt Wi-Fi is gone: keep the computer on another access point or a cable.

## Not done

- Updates: the Updates page says they are not supported here (they switch slots with GRUB). The slots and boot partitions are there
  for it: the U6+ way is to write the other slot and its boot image and point `pb_start` and `pb_args` at them, on trial.
- A release build and workflow (`ap-v*` tags); the image's version comes from them (`ap-v0.1.0` is v0.1.0).
- The LEDs and the reset button.
- One hostapd serves both radios and stops when one of them fails (band steering needs one process): a radio that cannot start takes
  the other down. The 5 GHz automatic channel did that until it got its 40 MHz line (NOTES.md).
- Committing the access point changes its address (a new bridge asks for a new lease): the page that has to confirm the change may
  have to be opened at the new address, which the router's client list shows.

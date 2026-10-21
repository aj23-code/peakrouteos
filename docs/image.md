# The disk image

PeakRoute OS is a **disk image**, written to the router's disk like OpenWrt's. There is no installer, no live ISO and no setup on the
console: a person writes the image, starts the machine, plugs a computer into any port and sees the welcome page
(`docs/setup.md`).

## Building it

On an Ubuntu or Debian machine with `debootstrap`, `fdisk`, `dosfstools`, `e2fsprogs`, `wget` and `util-linux`:

    ./full_build.sh

That builds the daemon (`peakroute`) and then runs `buildImage.sh`, which asks for `sudo` once (debootstrap, chroot and loop devices need
root) and makes **`peakroute-os.img`** (the disk image) and **`peakroute-os-<version>.update`** (the bundle that updates a running device,
`docs/updates.md`), each with a `.sha256`. `VERSION=v0.10.1 ./full_build.sh` names the build; `UPDATE_REPO=owner/name` sets the
default place a device looks for updates. The image is two root slots (the system plus half again, at least 3 GiB each) and a data
partition of 256 MiB that grows to the disk at the first start, so about 7 GB of mostly empty file; `IMAGE_SIZE=16G` makes it an exact size
(the data partition takes the rest). Debian is unpacked in `build/` in the project folder, or in `/var/tmp/peakroute-build` when the project
folder is on a drive mounted `nodev` or `noexec` (external and data drives often are: debootstrap cannot work there); `BUILD_DIR=/some/folder`
chooses another, which needs about 9 GB free. The script stops at the first error and checks that Debian really installed before it goes on. After a first build,
`REUSE_ROOTFS=1 ./buildImage.sh` skips the download of Debian and builds the image again in about a minute (to try a change to the
overlay, the scripts or the binary).

## Writing it

    sudo dd if=peakroute-os.img of=/dev/sdX bs=4M conv=fsync status=progress

(`sdX` is the whole disk, not a partition; this erases it.) On the first start the root partition grows to fill the disk.

## Trying it in a virtual machine

    image/run-qemu.sh                 # UEFI; https://localhost:9443 reaches the welcome page
    BOOT=bios image/run-qemu.sh       # legacy BIOS
    DISK=16G NICS=3 image/run-qemu.sh # a bigger disk, three network ports

The machine works on a throw-away layer on top of the image, on a disk bigger than the image (which is what a real disk looks like), with
the screen on this terminal's serial console (leave with Ctrl-A then X).

## What is in it

- **Layout (GPT, layout version 1):** a 1 MiB BIOS boot partition (legacy machines), a 128 MiB EFI system partition (FAT32, label
  `PRESP`: the boot loader, `grub.cfg` and `grubenv`, which says which slot starts), **two root slots** A and B (ext4, labels
  `peakroute-root-a` and `-b`; B is empty until the first update) and the **data partition** (ext4, `peakroute-data`). Both boot methods
  are installed (`grub-install` for `x86_64-efi --removable` and for `i386-pc`, with the boot directory on the ESP), so the image starts on
  UEFI and on legacy BIOS machines (such as a PC Engines board). Everything is found by label: the image can be written to any disk. The
  system is read-only; `/etc`, `/var`, `/home` and `/usr/local` are layers on the data partition (`docs/updates.md`, section 2).
- **Packages:** Debian 13 (Trixie) minimal plus everything the daemon uses, so that the dependencies are covered: the kernel and
  `initramfs-tools`, `udev`, `kmod`, `grub-efi-amd64`, `grub-pc-bin`, `sudo`, `procps`, `gdisk`; `ifupdown`, `bridge-utils`, `vlan`,
  `iproute2`, `udhcpc`, `ppp`; `nftables`, `conntrack`, `dnsmasq`, `dnsmasq-utils`; `hostapd`, `wpasupplicant`, `iw`, `wireless-regdb`,
  `rfkill` and the Wi-Fi firmware (`firmware-iwlwifi`, `-atheros`, `-mediatek`, `-realtek`, from `non-free-firmware`); `wireguard-tools`,
  `openvpn`; `systemd-timesyncd`, `tzdata`, `ca-certificates`, `iputils-ping`, `iputils-tracepath`, `libnss-myhostname` and a few small
  tools. Not in it any more: `live-boot` and `live-config` (only the live ISO needed them). The list and the reason for each are in
  `buildImage.sh`. The firmware adds about 270 MB: remove the ones for hardware you will never use to make the image smaller.
- **The overlay** (`image/overlay`): the files of the image as they are on the disk: the services (`peakroute`, `peakroute-wifi`,
  `peakroute-growdata`, `peakroute-trialwatch`), the kernel settings (`ip_forward` and a few safe defaults), the journal limits, the fstab, the boot loader's
  settings (`rootwait`, and the screen and the serial port both as consoles), the initramfs settings (`MODULES=most`: the drivers of
  most storage hardware, not only the build machine's), an empty `interfaces` (the daemon writes it), the host name `peakroute` and the
  console banner that says where the setup page is.
- **No passwords, no autologin.** Root is locked. The console shows a login prompt that nobody can pass until the welcome page has
  made the administrator, who is then the console login too (with `sudo`). `systemd-firstboot` is masked (it would ask on the console for
  a language and a password), and `machine-id` is empty so that every device makes its own identity at its first start.
- **No SSH.** It is not in the image.

## The first start

1. The boot loader starts the kernel with `rootwait` and both consoles.
2. The initramfs mounts the data partition and lays the layers over the read-only system (`peakroute-state`).
3. `peakroute-growdata` (once): moves the backup partition table to the end of the disk, stretches the data partition into the free
   space behind it, tells the kernel and grows the file system while it is mounted (`/usr/lib/peakroute/growdata`). It tries again at
   the next start if anything fails, and does nothing on a disk with no room.
4. `peakroute` starts and, because no local network is committed, builds the setup network (`192.168.77.1`, `docs/setup.md`).
5. The person sets up the device on the welcome page.

## Not done

- The first build and boot have to be run (they need root): until then the image is untested, as are the grow step and the setup network
  on a real disk. `image/run-qemu.sh` is for exactly that.
- Updates are built (`docs/updates.md`) but their image side has not been run for real yet: the new build, the boot from the ESP and
  the read-only root with layers have to be tried in QEMU and on hardware.
- Secure Boot (GRUB is not signed here).
- A compressed image for download (`zstd`) and a build in a container for CI.

#!/usr/bin/env bash
set -euo pipefail

# ==========================================
# PeakRoute OS: build a disk image (docs/image.md)
#
# Debian 13 (Trixie), native debootstrap on an Ubuntu/Debian host (no container needed).
# Output: peakroute-os.img, a raw disk image that is written to the router's disk like OpenWrt's:
#
#   sudo dd if=peakroute-os.img of=/dev/sdX bs=4M conv=fsync status=progress
#
# There is no installer and no console setup: the first thing a person sees is the welcome page of the web interface. It also
# boots in a virtual machine for trying (see docs/image.md).
#
# Layout (GPT): a 1 MiB BIOS boot partition (for machines that start in legacy mode), a 128 MiB EFI system partition (FAT32, label
# PRESP) and the root file system (ext4, label peakroute-root), which grows to fill the disk at the first start.
# ==========================================

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Override with e.g. BUILD_DIR=/tmp/peakroute-build if the project drive is mounted noexec/nodev
BUILD_DIR="${BUILD_DIR:-${PROJECT_DIR}/build}"
ROOTFS_DIR="${BUILD_DIR}/rootfs"
MOUNT_DIR="${BUILD_DIR}/mnt"
OUTPUT_IMG="${OUTPUT_IMG:-${PROJECT_DIR}/peakroute-os.img}"
GO_BINARY_PATH="${PROJECT_DIR}/peakroute"
OVERLAY_DIR="${PROJECT_DIR}/image/overlay"
# The size of the image file (sparse). The root partition fills it, and grows again to fill a bigger disk at the first start.
IMAGE_SIZE="${IMAGE_SIZE:-4G}"
# REUSE_ROOTFS=1 skips the download and install of Debian when a root file system from an earlier run is there (to change the
# overlay, the scripts or the binary and build the image again in about a minute).
REUSE_ROOTFS="${REUSE_ROOTFS:-0}"

# Everything below needs root (debootstrap, chroot, loop devices): ask for it once.
if [ "$(id -u)" -ne 0 ]; then
    exec sudo -E BUILD_DIR="${BUILD_DIR}" OUTPUT_IMG="${OUTPUT_IMG}" IMAGE_SIZE="${IMAGE_SIZE}" REUSE_ROOTFS="${REUSE_ROOTFS}" "$0" "$@"
fi

MISSING_TOOLS=()
for cmd in debootstrap sfdisk losetup mkfs.ext4 mkfs.vfat e2fsck wget dpkg-deb truncate sha256sum; do
    command -v "$cmd" >/dev/null 2>&1 || MISSING_TOOLS+=("$cmd")
done
if [ ${#MISSING_TOOLS[@]} -gt 0 ]; then
    echo "Error: Missing host build tools: ${MISSING_TOOLS[*]}"
    echo "Install on Ubuntu/Debian with:"
    echo "  sudo apt install debootstrap fdisk dosfstools e2fsprogs wget dpkg util-linux coreutils"
    exit 1
fi

if [ ! -f "${GO_BINARY_PATH}" ]; then
    echo "Error: PeakRoute Go binary not found at ${GO_BINARY_PATH}"
    echo "Compile it first: ./full_build.sh (or: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o peakroute)"
    exit 1
fi

LOOP_DEV=""
cleanup() {
    set +e
    for m in boot/efi sys/firmware/efi/efivars dev/pts dev proc sys ""; do
        mountpoint -q "${MOUNT_DIR}/${m}" 2>/dev/null && umount -l "${MOUNT_DIR}/${m}" 2>/dev/null
    done
    for m in dev/pts proc sys dev; do
        mountpoint -q "${ROOTFS_DIR}/${m}" 2>/dev/null && umount -l "${ROOTFS_DIR}/${m}" 2>/dev/null
    done
    [ -n "${LOOP_DEV}" ] && losetup -d "${LOOP_DEV}" 2>/dev/null
}
trap cleanup EXIT

echo "[1/5] Cleaning workspace..."
cleanup
if [ "${REUSE_ROOTFS}" = "1" ] && [ -f "${ROOTFS_DIR}/.peakroute-debootstrapped" ]; then
    echo "      reusing the root file system of the last run"
    rm -rf "${MOUNT_DIR}"
else
    rm -rf "${BUILD_DIR}"
    REUSE_ROOTFS=0
fi
mkdir -p "${ROOTFS_DIR}" "${MOUNT_DIR}"

if [ "${REUSE_ROOTFS}" != "1" ]; then
    echo "[2/5] Bootstrapping Debian 13 (Trixie) minbase with debootstrap..."

    # Ubuntu's packaged debian-archive-keyring predates Trixie and can't verify its Release files,
    # so fetch Debian's own keyring package (pinned by checksum) and hand it to debootstrap.
    KEYRING_DEB="debian-archive-keyring_2025.1_all.deb"
    KEYRING_SHA256="9ea7778e443144ca490668737a8ab22dd3e748bb99e805e22ec055abeb3c7fac"
    KEYRING_DIR="${BUILD_DIR}/keyring"
    mkdir -p "${KEYRING_DIR}"
    wget -q -O "${KEYRING_DIR}/${KEYRING_DEB}" \
        "https://deb.debian.org/debian/pool/main/d/debian-archive-keyring/${KEYRING_DEB}"
    echo "${KEYRING_SHA256}  ${KEYRING_DIR}/${KEYRING_DEB}" | sha256sum -c --quiet -
    dpkg-deb -x "${KEYRING_DIR}/${KEYRING_DEB}" "${KEYRING_DIR}/extracted"
    DEBIAN_KEYRING="${KEYRING_DIR}/extracted/usr/share/keyrings/debian-archive-keyring.gpg"

    # What is in it, and why:
    #
    # - linux-image-amd64, initramfs-tools, udev, kmod, systemd-sysv: the system itself (the kernel, the initramfs that finds the root
    #   file system, device names like enp1s0).
    # - grub-efi-amd64, grub-pc-bin, grub-common: the boot loader for both UEFI and legacy BIOS machines.
    # - sudo, procps, gdisk: the console administrator may use sudo; sysctl and pkill (the daemon uses both); sgdisk (the root partition
    #   is grown to the disk at the first start).
    # - ifupdown, bridge-utils, vlan, iproute2, udhcpc, ppp: the network the daemon writes (interfaces, bridges, VLANs, DHCP clients,
    #   PPPoE).
    # - nftables, conntrack, dnsmasq, dnsmasq-utils: the firewall and the DHCP and DNS server.
    # - hostapd, wpasupplicant, iw, wireless-regdb, rfkill and the firmware packages: Wi-Fi as an access point and as a client.
    #   The firmware lives in Debian's non-free-firmware section, hence the extra component: Intel (iwlwifi), Qualcomm Atheros
    #   (ath9k_htc, ath10k...), MediaTek (mt76) and Realtek. Together they add roughly 270 MB uninstalled; drop the ones for hardware
    #   you will never use to shrink the image.
    # - wireguard-tools, openvpn: the VPN. systemd-timesyncd, tzdata: the clock. ca-certificates: Dynamic DNS over https.
    # - iputils-ping, iputils-tracepath: the diagnostics. libnss-myhostname: the machine finds its own name.
    DEBIAN_FRONTEND=noninteractive debootstrap \
        --arch=amd64 \
        --variant=minbase \
        --components=main,non-free-firmware \
        --keyring="${DEBIAN_KEYRING}" \
        --include=linux-image-amd64,initramfs-tools,udev,kmod,systemd-sysv,grub-efi-amd64,grub-pc-bin,grub-common,sudo,procps,gdisk,dosfstools,e2fsprogs,rsync,ifupdown,bridge-utils,vlan,iproute2,iputils-ping,iputils-tracepath,nftables,dnsmasq,dnsmasq-utils,udhcpc,ppp,conntrack,hostapd,wpasupplicant,iw,wireless-regdb,rfkill,libnss-myhostname,systemd-timesyncd,tzdata,wireguard-tools,openvpn,firmware-iwlwifi,firmware-atheros,firmware-mediatek,firmware-realtek,ca-certificates,lsb-release,util-linux,fdisk,nano,passwd \
        trixie "${ROOTFS_DIR}" http://deb.debian.org/debian/
    touch "${ROOTFS_DIR}/.peakroute-debootstrapped"
else
    echo "[2/5] (skipped: reusing the root file system)"
fi

echo "[3/5] Configuring the root file system..."

# The overlay holds the files of the image as they are on the disk: the configuration, the services, the scripts
# (image/overlay), copied over what Debian made, owned by root.
cp -a --no-preserve=ownership "${OVERLAY_DIR}/." "${ROOTFS_DIR}/"
chown -R root:root "${ROOTFS_DIR}/etc" "${ROOTFS_DIR}/usr/lib/peakroute"
chmod 755 "${ROOTFS_DIR}/usr/lib/peakroute/growroot"

# the hostapd start script is shared with the tests
install -D -m 755 "${PROJECT_DIR}/internal/scripts/hostapd-run.sh" "${ROOTFS_DIR}/usr/lib/peakroute/hostapd-run"

# the daemon
install -D -m 755 "${GO_BINARY_PATH}" "${ROOTFS_DIR}/usr/local/bin/peakroute"

# Bind-mount pseudo-filesystems for the chroot
mount --bind /proc "${ROOTFS_DIR}/proc"
mount --bind /sys "${ROOTFS_DIR}/sys"
mount --bind /dev "${ROOTFS_DIR}/dev"
mount --bind /dev/pts "${ROOTFS_DIR}/dev/pts"

# No password for anyone: root is locked (there is no known password in the image) and the console is not signed in. The
# administrator made on the welcome page is the only way in, on the web and on the console.
chroot "${ROOTFS_DIR}" passwd -l root
# the host's name servers were only for the download
: > "${ROOTFS_DIR}/etc/resolv.conf"

# Services: the daemon, the Wi-Fi access points (it only runs while the daemon has written a configuration for it), growing the root
# partition (once), the firewall and the network. Nothing starts a setup on the console.
chroot "${ROOTFS_DIR}" systemctl enable peakroute.service peakroute-wifi.service peakroute-growroot.service \
    nftables.service networking.service systemd-timesyncd.service
# ifupdown starts wpa_supplicant for a Wi-Fi client itself (wpa-conf), so the service that serves other programs is not needed, and
# the first-boot questions of systemd would ask for a language and a password on the console
chroot "${ROOTFS_DIR}" systemctl disable wpa_supplicant.service 2>/dev/null || true
chroot "${ROOTFS_DIR}" systemctl mask systemd-firstboot.service

# Every device gets an identity of its own at its first start
: > "${ROOTFS_DIR}/etc/machine-id"
rm -f "${ROOTFS_DIR}/var/lib/dbus/machine-id"

# The initramfs again, with the configuration of the image (all storage drivers, no resume)
chroot "${ROOTFS_DIR}" update-initramfs -u -k all

# Small: no caches, no lists, no manual pages (the licences stay)
chroot "${ROOTFS_DIR}" apt-get clean || true
rm -rf "${ROOTFS_DIR}"/var/lib/apt/lists/* "${ROOTFS_DIR}"/var/cache/apt/* "${ROOTFS_DIR}"/tmp/* \
       "${ROOTFS_DIR}"/usr/share/man/* "${ROOTFS_DIR}"/usr/share/info/*

umount -l "${ROOTFS_DIR}/dev/pts" "${ROOTFS_DIR}/proc" "${ROOTFS_DIR}/sys" "${ROOTFS_DIR}/dev"

echo "[4/5] Making the disk image (${IMAGE_SIZE})..."

rm -f "${OUTPUT_IMG}"
truncate -s "${IMAGE_SIZE}" "${OUTPUT_IMG}"
sfdisk --quiet "${OUTPUT_IMG}" << 'EOF'
label: gpt
unit: sectors
first-lba: 2048
start=2048, size=2048, type=21686148-6449-6E6F-744E-656564454649, name="BIOS boot"
size=262144, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, name="EFI System"
type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name="PeakRoute root"
EOF

LOOP_DEV="$(losetup --find --show --partscan "${OUTPUT_IMG}")"
# the partitions' device nodes appear a moment later
for _ in $(seq 1 50); do
    [ -b "${LOOP_DEV}p3" ] && break
    partx -u "${LOOP_DEV}" 2>/dev/null || true
    sleep 0.2
done
[ -b "${LOOP_DEV}p3" ] || { echo "Error: the partitions of ${LOOP_DEV} did not appear"; exit 1; }

mkfs.vfat -F32 -n PRESP "${LOOP_DEV}p2" > /dev/null
# GRUB reads the root file system to find its configuration: leave out a feature of the newest e2fsprogs that older GRUBs do not know
mkfs.ext4 -q -F -L peakroute-root -O ^orphan_file "${LOOP_DEV}p3" 2>/dev/null \
    || mkfs.ext4 -q -F -L peakroute-root "${LOOP_DEV}p3"

mount "${LOOP_DEV}p3" "${MOUNT_DIR}"
echo "      copying the root file system..."
cp -a "${ROOTFS_DIR}/." "${MOUNT_DIR}/"
rm -f "${MOUNT_DIR}/.peakroute-debootstrapped"
mkdir -p "${MOUNT_DIR}/boot/efi" "${MOUNT_DIR}"/{proc,sys,dev,run,mnt,tmp,var/tmp}
chmod 1777 "${MOUNT_DIR}/tmp" "${MOUNT_DIR}/var/tmp"
mount "${LOOP_DEV}p2" "${MOUNT_DIR}/boot/efi"

echo "[5/5] Installing the boot loader (UEFI and legacy BIOS)..."
mount --bind /proc "${MOUNT_DIR}/proc"
mount --bind /sys "${MOUNT_DIR}/sys"
mount --bind /dev "${MOUNT_DIR}/dev"
mount --bind /dev/pts "${MOUNT_DIR}/dev/pts"

# UEFI: the fallback path (EFI/BOOT/BOOTX64.EFI), found by every firmware, with no entry in this machine's NVRAM
chroot "${MOUNT_DIR}" grub-install --target=x86_64-efi --efi-directory=/boot/efi --removable --no-nvram --recheck
# legacy BIOS: the boot loader goes in the BIOS boot partition of the disk
chroot "${MOUNT_DIR}" grub-install --target=i386-pc --recheck "${LOOP_DEV}"
chroot "${MOUNT_DIR}" update-grub

sync
umount "${MOUNT_DIR}/dev/pts" "${MOUNT_DIR}/dev" "${MOUNT_DIR}/proc" "${MOUNT_DIR}/sys"
umount "${MOUNT_DIR}/boot/efi"
umount "${MOUNT_DIR}"
e2fsck -fn "${LOOP_DEV}p3" > /dev/null || { echo "Error: the root file system is not clean"; exit 1; }
losetup -d "${LOOP_DEV}"
LOOP_DEV=""

# the person who ran sudo owns the image
if [ -n "${SUDO_UID:-}" ]; then
    chown "${SUDO_UID}:${SUDO_GID:-${SUDO_UID}}" "${OUTPUT_IMG}"
fi
( cd "$(dirname "${OUTPUT_IMG}")" && sha256sum "$(basename "${OUTPUT_IMG}")" > "$(basename "${OUTPUT_IMG}").sha256" )

echo "[+] Successfully built the image: ${OUTPUT_IMG}"
ls -lhs "${OUTPUT_IMG}"
echo
echo "Write it to a disk (this ERASES the disk):"
echo "  sudo dd if=${OUTPUT_IMG} of=/dev/sdX bs=4M conv=fsync status=progress"

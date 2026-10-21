#!/usr/bin/env bash
set -euo pipefail

# ==========================================
# PeakRoute OS: build a disk image and an update bundle (docs/image.md, docs/updates.md)
#
# Debian 13 (Trixie), native debootstrap on an Ubuntu/Debian host (no container needed). Two outputs from one root file system:
#
#   peakroute-os.img                  a raw disk image that is written to the router's disk like OpenWrt's:
#                                       sudo dd if=peakroute-os.img of=/dev/sdX bs=4M conv=fsync status=progress
#   peakroute-os-<version>.update     the bundle that updates a device that is running (the Updates page): the same root file system,
#                                       compressed, with a manifest
#
# There is no installer and no console setup: the first thing a person sees is the welcome page of the web interface. It also
# boots in a virtual machine for trying (see docs/image.md).
#
# Layout (GPT, layout version 1): a 1 MiB BIOS boot partition (for machines that start in legacy mode), a 128 MiB EFI system partition
# (FAT32, label PRESP: the boot loader, its configuration and which slot to start), two root slots A and B (ext4, labels
# peakroute-root-a and -b; the image has the system in A, B is empty until the first update) and the data partition (ext4, label
# peakroute-data: everything that belongs to the router), which grows to fill the disk at the first start.
# ==========================================

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Where Debian is unpacked and the image is put together. debootstrap needs device nodes and programs to work there, so it must not be
# on a drive mounted nodev or noexec (external and data drives often are): by default the project's own folder is used when it is fine,
# else /var/tmp/peakroute-build. Override with BUILD_DIR=/some/folder.
default_build_dir() {
    local opts
    opts="$(findmnt -n -o OPTIONS -T "${PROJECT_DIR}" 2>/dev/null || true)"
    case ",${opts}," in
        *,nodev,*|*,noexec,*) echo "/var/tmp/peakroute-build" ;;
        *) echo "${PROJECT_DIR}/build" ;;
    esac
}
BUILD_DIR="${BUILD_DIR:-$(default_build_dir)}"
ROOTFS_DIR="${BUILD_DIR}/rootfs"
MOUNT_DIR="${BUILD_DIR}/mnt"
OUTPUT_IMG="${OUTPUT_IMG:-${PROJECT_DIR}/peakroute-os.img}"
GO_BINARY_PATH="${PROJECT_DIR}/peakroute"
OVERLAY_DIR="${PROJECT_DIR}/image/overlay"
ROOTFS_IMG="${BUILD_DIR}/rootfs.img"
# The version this build is (shown on the Updates page, named in the bundle). full_build.sh passes it on.
VERSION="${PEAKROUTE_VERSION:-v0.0.0-dev}"
# What the bundle's manifest says changed (one line), and the version of the partition layout: a bundle only goes onto a device with the
# same layout (docs/updates.md, section 5)
UPDATE_NOTES="${UPDATE_NOTES:-}"
LAYOUT_VERSION=1
# The size of one root slot, in MiB. "auto" (the default) is the root file system plus half again, at least 3 GiB: a slot cannot be
# resized after the disk is written, so there is room for the system to grow over many updates.
SLOT_SIZE_MB="${SLOT_SIZE_MB:-auto}"
# The size of the data partition in the image (it grows to fill the disk at the first start), in MiB.
DATA_SIZE_MB="${DATA_SIZE_MB:-256}"
# The size of the image file. "auto" (the default) is exactly what the partitions need; a size such as 16G (an exact size for the file)
# makes the data partition take the rest.
IMAGE_SIZE="${IMAGE_SIZE:-auto}"
# REUSE_ROOTFS=1 skips the download and install of Debian when a root file system from an earlier run is there (to change the
# overlay, the scripts or the binary and build the image again in about a minute).
REUSE_ROOTFS="${REUSE_ROOTFS:-0}"

# Everything below needs root (debootstrap, chroot, loop devices): ask for it once.
if [ "$(id -u)" -ne 0 ]; then
    exec sudo -E BUILD_DIR="${BUILD_DIR}" OUTPUT_IMG="${OUTPUT_IMG}" IMAGE_SIZE="${IMAGE_SIZE}" REUSE_ROOTFS="${REUSE_ROOTFS}" \
        PEAKROUTE_VERSION="${VERSION}" UPDATE_NOTES="${UPDATE_NOTES}" SLOT_SIZE_MB="${SLOT_SIZE_MB}" DATA_SIZE_MB="${DATA_SIZE_MB}" "$0" "$@"
fi

MISSING_TOOLS=()
for cmd in debootstrap sfdisk losetup mkfs.ext4 mkfs.vfat e2fsck resize2fs tune2fs gzip tar wget dpkg-deb truncate sha256sum; do
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
    # (this must not leave "stop at the first error" switched off for the rest of the script)
    local was_e=0
    case $- in *e*) was_e=1 ;; esac
    set +e
    for m in boot/efi sys/firmware/efi/efivars dev/pts dev proc sys ""; do
        mountpoint -q "${MOUNT_DIR}/${m}" 2>/dev/null && umount -l "${MOUNT_DIR}/${m}" 2>/dev/null
    done
    for m in dev/pts proc sys dev; do
        mountpoint -q "${ROOTFS_DIR}/${m}" 2>/dev/null && umount -l "${ROOTFS_DIR}/${m}" 2>/dev/null
    done
    [ -n "${LOOP_DEV}" ] && losetup -d "${LOOP_DEV}" 2>/dev/null
    [ "${was_e}" = 1 ] && set -e
    return 0
}
trap cleanup EXIT

# the folder must allow device nodes and programs, and have room (Debian is about 2.5 GB unpacked)
mkdir -p "${BUILD_DIR}"
opts="$(findmnt -n -o OPTIONS -T "${BUILD_DIR}" 2>/dev/null || true)"
case ",${opts}," in
    *,nodev,*|*,noexec,*)
        echo "Error: ${BUILD_DIR} is on a drive mounted nodev or noexec, where debootstrap cannot work."
        echo "Use another folder: BUILD_DIR=/var/tmp/peakroute-build ./buildImage.sh"
        exit 1 ;;
esac
avail_kb="$(df --output=avail -k "${BUILD_DIR}" | tail -n 1 | tr -d ' ')"
if [ "${avail_kb:-0}" -lt 9000000 ]; then
    echo "Error: ${BUILD_DIR} has less than 9 GB free (Debian, the root file system image, the disk image and the bundle)."
    exit 1
fi
echo "Building in ${BUILD_DIR}"

echo "[1/5] Cleaning workspace..."
cleanup
# an earlier version built inside the project folder
[ "${BUILD_DIR}" != "${PROJECT_DIR}/build" ] && rm -rf "${PROJECT_DIR}/build"
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

# What the next steps need, checked now rather than found missing one command at a time
for f in usr/bin/passwd usr/bin/systemctl usr/sbin/update-initramfs usr/sbin/grub-install usr/sbin/update-grub usr/bin/apt-get; do
    [ -e "${ROOTFS_DIR}/${f}" ] || { echo "Error: the root file system has no /${f}: the Debian install did not finish"; exit 1; }
done
ls "${ROOTFS_DIR}"/boot/vmlinuz-* > /dev/null 2>&1 || { echo "Error: the root file system has no kernel"; exit 1; }

echo "[3/5] Configuring the root file system..."

# The overlay holds the files of the image as they are on the disk: the configuration, the services, the scripts
# (image/overlay), copied over what Debian made, owned by root.
cp -a --no-preserve=ownership "${OVERLAY_DIR}/." "${ROOTFS_DIR}/"
chown -R root:root "${ROOTFS_DIR}/etc" "${ROOTFS_DIR}/usr/lib/peakroute"
chmod 755 "${ROOTFS_DIR}/usr/lib/peakroute/growdata" "${ROOTFS_DIR}/usr/lib/peakroute/trialwatch" \
    "${ROOTFS_DIR}/etc/initramfs-tools/hooks/peakroute-state" "${ROOTFS_DIR}/etc/initramfs-tools/scripts/local-bottom/peakroute-state"
# what earlier builds put in a root file system that is being reused
rm -f "${ROOTFS_DIR}/usr/lib/peakroute/growroot" "${ROOTFS_DIR}/etc/systemd/system/peakroute-growroot.service" \
      "${ROOTFS_DIR}/etc/systemd/system/multi-user.target.wants/peakroute-growroot.service" "${ROOTFS_DIR}/etc/default/grub" \
      "${ROOTFS_DIR}/usr/local/bin/peakroute"
# the mount points of the data partition and of the boot loader's partition
mkdir -p "${ROOTFS_DIR}/data" "${ROOTFS_DIR}/boot/efi"
# what this system is (shown by the Updates page and checked against a bundle's manifest)
cat > "${ROOTFS_DIR}/usr/lib/peakroute/release" << RELEASE
PEAKROUTE_VERSION=${VERSION}
PEAKROUTE_BUILT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
PEAKROUTE_LAYOUT=${LAYOUT_VERSION}
PEAKROUTE_ARCH=amd64
DEBIAN_VERSION=13
RELEASE

# the hostapd start script is shared with the tests
install -D -m 755 "${PROJECT_DIR}/internal/scripts/hostapd-run.sh" "${ROOTFS_DIR}/usr/lib/peakroute/hostapd-run"

# the daemon (not under /usr/local: that is laid over from the data partition, where a copy could shadow the system's)
install -D -m 755 "${GO_BINARY_PATH}" "${ROOTFS_DIR}/usr/lib/peakroute/peakroute"

# Bind-mount pseudo-filesystems for the chroot
mount --bind /proc "${ROOTFS_DIR}/proc"
mount --bind /sys "${ROOTFS_DIR}/sys"
mount --bind /dev "${ROOTFS_DIR}/dev"
mount --bind /dev/pts "${ROOTFS_DIR}/dev/pts"

# No default password for anyone: root is locked (passwd -l: no password of any kind, not even an empty one, can log in as root) and the
# console is not signed in. The administrator made on the welcome page is the only way in, on the web and on the console.
chroot "${ROOTFS_DIR}" passwd -l root
# the host's name servers were only for the download
: > "${ROOTFS_DIR}/etc/resolv.conf"

# Services: the daemon, the Wi-Fi access points (it only runs while the daemon has written a configuration for it), growing the data
# partition (once), going back from a version on trial that is not kept, the firewall and the network. Nothing starts a setup on the
# console.
chroot "${ROOTFS_DIR}" systemctl enable peakroute.service peakroute-wifi.service peakroute-growdata.service peakroute-trialwatch.service \
    nftables.service networking.service systemd-timesyncd.service
# ifupdown starts wpa_supplicant for a Wi-Fi client itself (wpa-conf), so the service that serves other programs is not needed, and
# the first-boot questions of systemd would ask for a language and a password on the console
chroot "${ROOTFS_DIR}" systemctl disable wpa_supplicant.service 2>/dev/null || true
chroot "${ROOTFS_DIR}" systemctl mask systemd-firstboot.service

# Every device gets an identity of its own at its first start
: > "${ROOTFS_DIR}/etc/machine-id"
rm -f "${ROOTFS_DIR}/var/lib/dbus/machine-id"

# The initramfs again, with the configuration of the image (all storage drivers, no resume) and the script that lays the data
# partition's layers over the read-only system
chroot "${ROOTFS_DIR}" update-initramfs -u -k all
# the boot loader's configuration starts /vmlinuz and /initrd.img (links Debian makes at the top of the file system)
for f in vmlinuz initrd.img; do
    [ -L "${ROOTFS_DIR}/${f}" ] || { echo "Error: the root file system has no /${f} link"; exit 1; }
done
# the initramfs must hold the script that lays the router's state over the system, or no device would keep its settings
initrd_file="$(readlink -f "${ROOTFS_DIR}/initrd.img")"
chroot "${ROOTFS_DIR}" lsinitramfs "${initrd_file#${ROOTFS_DIR}}" 2>/dev/null | grep -q 'scripts/local-bottom/peakroute-state' \
    || { echo "Error: the initramfs does not hold the state script"; exit 1; }

# Small: no caches, no lists, no manual pages (the licences stay)
chroot "${ROOTFS_DIR}" apt-get clean || true
rm -rf "${ROOTFS_DIR}"/var/lib/apt/lists/* "${ROOTFS_DIR}"/var/cache/apt/* "${ROOTFS_DIR}"/tmp/* \
       "${ROOTFS_DIR}"/usr/share/man/* "${ROOTFS_DIR}"/usr/share/info/*

umount -l "${ROOTFS_DIR}/dev/pts" "${ROOTFS_DIR}/proc" "${ROOTFS_DIR}/sys" "${ROOTFS_DIR}/dev"

echo "[4/5] Making the root file system image..."

# One ext4 file system, made once, is both slot A of the disk image and the payload of the update bundle.
rm -f "${ROOTFS_IMG}"
used_mb="$(du -sxm "${ROOTFS_DIR}" | cut -f1)"
rootfs_mb="$(( used_mb * 11 / 10 + 64 ))"
echo "      the system is ${used_mb} MB: a file system of ${rootfs_mb} MB"
truncate -s "${rootfs_mb}M" "${ROOTFS_IMG}"
# GRUB reads the root file system: leave out a feature of the newest e2fsprogs that older GRUBs do not know. No reserved blocks: it is
# read-only.
mkfs.ext4 -q -F -m 0 -L peakroute-root -O ^orphan_file "${ROOTFS_IMG}" 2>/dev/null \
    || mkfs.ext4 -q -F -m 0 -L peakroute-root "${ROOTFS_IMG}"
mount -o loop "${ROOTFS_IMG}" "${MOUNT_DIR}"
echo "      copying the root file system..."
cp -a "${ROOTFS_DIR}/." "${MOUNT_DIR}/"
rm -f "${MOUNT_DIR}/.peakroute-debootstrapped"
mkdir -p "${MOUNT_DIR}"/{proc,sys,dev,run,mnt,tmp,var/tmp,data,boot/efi}
chmod 1777 "${MOUNT_DIR}/tmp" "${MOUNT_DIR}/var/tmp"
sync
umount "${MOUNT_DIR}"
e2fsck -fy "${ROOTFS_IMG}" > /dev/null || [ $? -le 1 ] || { echo "Error: the root file system image is not clean"; exit 1; }

echo "[5/5] Making the disk image and the update bundle..."

# the slot: the file system plus half again, at least 3 GiB
if [ "${SLOT_SIZE_MB}" = "auto" ]; then
    SLOT_SIZE_MB="$(( rootfs_mb * 3 / 2 ))"
    [ "${SLOT_SIZE_MB}" -lt 3072 ] && SLOT_SIZE_MB=3072
fi
[ "${SLOT_SIZE_MB}" -ge "${rootfs_mb}" ] || { echo "Error: a slot of ${SLOT_SIZE_MB} MB cannot hold the ${rootfs_mb} MB file system"; exit 1; }
SLOT_SECTORS="$(( SLOT_SIZE_MB * 2048 ))"
DATA_SECTORS="$(( DATA_SIZE_MB * 2048 ))"
# 1 MiB before the first partition, the BIOS boot partition (1 MiB), the EFI partition (128 MiB), the slots, the data partition and the
# second copy of the partition table (33 sectors, which sfdisk keeps on a 1 MiB boundary)
min_sectors="$(( 2048 + 2048 + 262144 + 2 * SLOT_SECTORS + DATA_SECTORS + 2048 ))"

rm -f "${OUTPUT_IMG}"
if [ "${IMAGE_SIZE}" = "auto" ]; then
    truncate -s "$(( min_sectors * 512 ))" "${OUTPUT_IMG}"
    echo "      two slots of ${SLOT_SIZE_MB} MB and a data partition of ${DATA_SIZE_MB} MB: an image of $(( min_sectors / 2048 )) MB"
else
    truncate -s "${IMAGE_SIZE}" "${OUTPUT_IMG}"
    [ "$(stat -c %s "${OUTPUT_IMG}")" -ge "$(( min_sectors * 512 ))" ] || { echo "Error: ${IMAGE_SIZE} is too small for the partitions (they need $(( min_sectors / 2048 )) MB)"; exit 1; }
    echo "      an image of ${IMAGE_SIZE}"
fi
sfdisk --quiet "${OUTPUT_IMG}" << PARTITIONS
label: gpt
unit: sectors
first-lba: 2048
start=2048, size=2048, type=21686148-6449-6E6F-744E-656564454649, name="bios-boot"
size=262144, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, name="peakroute-esp"
size=${SLOT_SECTORS}, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name="peakroute-root-a"
size=${SLOT_SECTORS}, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name="peakroute-root-b"
type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name="peakroute-data"
PARTITIONS

LOOP_DEV="$(losetup --find --show --partscan "${OUTPUT_IMG}")"
# the partitions' device nodes appear a moment later
for _ in $(seq 1 50); do
    [ -b "${LOOP_DEV}p5" ] && break
    partx -u "${LOOP_DEV}" 2>/dev/null || true
    sleep 0.2
done
[ -b "${LOOP_DEV}p5" ] || { echo "Error: the partitions of ${LOOP_DEV} did not appear"; exit 1; }

mkfs.vfat -F32 -n PRESP "${LOOP_DEV}p2" > /dev/null
mkfs.ext4 -q -F -m 0 -L peakroute-data -O ^orphan_file "${LOOP_DEV}p5" 2>/dev/null \
    || mkfs.ext4 -q -F -m 0 -L peakroute-data "${LOOP_DEV}p5"

echo "      writing the system into slot A..."
dd if="${ROOTFS_IMG}" of="${LOOP_DEV}p3" bs=4M conv=fsync status=none
e2fsck -fp "${LOOP_DEV}p3" > /dev/null || [ $? -le 1 ]
resize2fs "${LOOP_DEV}p3" > /dev/null 2>&1
# the label is what the boot loader and the kernel find the slot by; slot B gets its own when an update is written there
tune2fs -L peakroute-root-a "${LOOP_DEV}p3" > /dev/null

echo "      installing the boot loader (UEFI and legacy BIOS)..."
# The boot loader works in slot A's system (read-only: nothing in it changes) and puts everything on the EFI partition, which is also
# where its configuration and the choice of slot are, so both slots are started the same way and an update of a slot never touches it.
mount -o ro "${LOOP_DEV}p3" "${MOUNT_DIR}"
mount "${LOOP_DEV}p2" "${MOUNT_DIR}/boot/efi"
mount --bind /proc "${MOUNT_DIR}/proc"
mount --bind /sys "${MOUNT_DIR}/sys"
mount --bind /dev "${MOUNT_DIR}/dev"
mount --bind /dev/pts "${MOUNT_DIR}/dev/pts"

# UEFI: the fallback path (EFI/BOOT/BOOTX64.EFI), found by every firmware, with no entry in this machine's NVRAM
chroot "${MOUNT_DIR}" grub-install --target=x86_64-efi --efi-directory=/boot/efi --boot-directory=/boot/efi --removable --no-nvram --recheck
# legacy BIOS: the boot loader goes in the BIOS boot partition of the disk
chroot "${MOUNT_DIR}" grub-install --target=i386-pc --boot-directory=/boot/efi --recheck "${LOOP_DEV}"
install -m 644 "${PROJECT_DIR}/image/grub/grub.cfg" "${MOUNT_DIR}/boot/efi/grub/grub.cfg"
# which slot starts: A. (grubenv is made by the same program the daemon uses to change it)
rm -f "${MOUNT_DIR}/boot/efi/grub/grubenv"
chroot "${MOUNT_DIR}" grub-editenv /boot/efi/grub/grubenv create
chroot "${MOUNT_DIR}" grub-editenv /boot/efi/grub/grubenv set slot=a
for f in EFI/BOOT/BOOTX64.EFI grub/grub.cfg grub/grubenv grub/x86_64-efi/normal.mod grub/i386-pc/normal.mod; do
    [ -e "${MOUNT_DIR}/boot/efi/${f}" ] || { echo "Error: the boot loader did not install ${f}"; exit 1; }
done

sync
umount "${MOUNT_DIR}/dev/pts" "${MOUNT_DIR}/dev" "${MOUNT_DIR}/proc" "${MOUNT_DIR}/sys"
umount "${MOUNT_DIR}/boot/efi"
umount "${MOUNT_DIR}"
e2fsck -fn "${LOOP_DEV}p3" > /dev/null || { echo "Error: slot A is not clean"; exit 1; }
e2fsck -fn "${LOOP_DEV}p5" > /dev/null || { echo "Error: the data partition is not clean"; exit 1; }
losetup -d "${LOOP_DEV}"
LOOP_DEV=""

# The update bundle (docs/updates.md, section 5): a manifest and the compressed file system, in a tar archive
BUNDLE="$(dirname "${OUTPUT_IMG}")/peakroute-os-${VERSION}.update"
BUNDLE_DIR="${BUILD_DIR}/bundle"
rm -rf "${BUNDLE_DIR}" "${BUNDLE}"
mkdir -p "${BUNDLE_DIR}"
echo "      compressing the file system for the update bundle..."
if command -v pigz > /dev/null 2>&1; then pigz -n -6 -c "${ROOTFS_IMG}" > "${BUNDLE_DIR}/rootfs.img.gz"; else gzip -n -6 -c "${ROOTFS_IMG}" > "${BUNDLE_DIR}/rootfs.img.gz"; fi
json_text() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr '\n\t' '  '; }
cat > "${BUNDLE_DIR}/manifest.json" << MANIFEST
{
  "format": 1,
  "version": "$(json_text "${VERSION}")",
  "built": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "arch": "amd64",
  "layout": ${LAYOUT_VERSION},
  "rootfs_size": $(stat -c %s "${ROOTFS_IMG}"),
  "rootfs_sha256": "$(sha256sum "${ROOTFS_IMG}" | cut -d' ' -f1)",
  "payload_sha256": "$(sha256sum "${BUNDLE_DIR}/rootfs.img.gz" | cut -d' ' -f1)",
  "notes": "$(json_text "${UPDATE_NOTES}")"
}
MANIFEST
tar -C "${BUNDLE_DIR}" --format=ustar -cf "${BUNDLE}" manifest.json rootfs.img.gz
rm -rf "${BUNDLE_DIR}"

# the person who ran sudo owns the outputs
( cd "$(dirname "${OUTPUT_IMG}")" && sha256sum "$(basename "${OUTPUT_IMG}")" > "$(basename "${OUTPUT_IMG}").sha256" \
    && sha256sum "$(basename "${BUNDLE}")" > "$(basename "${BUNDLE}").sha256" )
if [ -n "${SUDO_UID:-}" ]; then
    chown "${SUDO_UID}:${SUDO_GID:-${SUDO_UID}}" "${OUTPUT_IMG}" "${OUTPUT_IMG}.sha256" "${BUNDLE}" "${BUNDLE}.sha256"
fi

echo "[+] Successfully built ${VERSION}:"
ls -lhs "${OUTPUT_IMG}" "${BUNDLE}"
echo
echo "Write the image to a disk (this ERASES the disk):"
echo "  sudo dd if=${OUTPUT_IMG} of=/dev/sdX bs=4M conv=fsync status=progress"
echo "Update a running device: the Updates page, Upload an update file: ${BUNDLE}"

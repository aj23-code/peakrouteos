#!/usr/bin/env bash
set -euo pipefail

# ==========================================
# PeakRoute AP for the Ubiquiti UniFi 6 Plus (U6+): the boot image and the root file system (docs/u6plus.md)
#
# The U6+ is an MT7981 (arm64) access point with 256 MB of RAM and a 4 GB eMMC whose layout is Ubiquiti's: its boot loader (U-Boot)
# is kept, and PeakRoute goes into new partitions in the free space after Ubiquiti's. Two outputs, in out/ubnt-u6plus/:
#
#   peakroute-ap-arm64-ubnt-u6plus-<version>.fit      what U-Boot starts: Debian's arm64 kernel, the U6+'s device tree and the initramfs,
#                                               in one FIT image (U-Boot's format), written to the partition peakroute-boot
#   peakroute-ap-arm64-ubnt-u6plus-<version>.update   the bundle that updates a U6+ that runs PeakRoute AP: the manifest, the root file
#                                                     system (compressed) and the boot image (docs/updates.md, docs/u6plus.md)
#   peakroute-ap-arm64-ubnt-u6plus-<version>.rootfs   the root file system (ext4, label peakroute-root-a), written to peakroute-root-a
#
# image/boards/ubnt-u6plus/install.sh puts them on a U6+ that runs OpenWrt, with a trial boot that falls back to OpenWrt.
#
#   ./buildImageU6Plus.sh                         build (asks for sudo once: debootstrap and chroot need root)
#   DEV_SSH_KEY=~/.ssh/id_ed25519.pub ./...       a development build: an SSH server, root signs in with that key only (bring-up)
#   REUSE_ROOTFS=1 ./buildImageU6Plus.sh          keep Debian from the last run (a change to the overlay or the daemon: minutes)
#
# Needs on the build machine (Ubuntu/Debian, any processor): debootstrap, qemu-user-static and binfmt-support (Debian's arm64 programs
# run under emulation in the chroot), u-boot-tools (mkimage), device-tree-compiler (dtc), xz-utils, e2fsprogs, Go.
# ==========================================

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BOARD=ubnt-u6plus
BOARD_DIR="${PROJECT_DIR}/image/boards/${BOARD}"
BUILD_DIR="${BUILD_DIR:-/var/tmp/peakroute-build-${BOARD}}"
ROOTFS_DIR="${BUILD_DIR}/rootfs"
OUT_DIR="${OUT_DIR:-${PROJECT_DIR}/out/${BOARD}}"
REUSE_ROOTFS="${REUSE_ROOTFS:-0}"
DEV_SSH_KEY="${DEV_SSH_KEY:-}"
# the build's version: given, or from the access point's tags (ap-v0.1.0 is version v0.1.0; docs/updates.md)
git_version() {
    local tag dirty=""
    tag="$(git -C "${PROJECT_DIR}" describe --tags --match 'ap-v[0-9]*' --abbrev=0 2>/dev/null || echo ap-v0.0.0)"
    [ -z "$(git -C "${PROJECT_DIR}" status --porcelain 2>/dev/null)" ] || dirty=".dirty"
    if [ "$(git -C "${PROJECT_DIR}" describe --tags --match 'ap-v[0-9]*' --exact-match 2>/dev/null)" = "${tag}" ] && [ -z "${dirty}" ]; then
        echo "${tag#ap-}"
    else
        echo "${tag#ap-}-dev.g$(git -C "${PROJECT_DIR}" rev-parse --short HEAD 2>/dev/null || echo unknown)${dirty}"
    fi
}
VERSION="${PEAKROUTE_VERSION:-$(git_version)}"
UPDATE_REPO="${UPDATE_REPO:-aj23-code/peakrouteos}"
# named for the product, its processors and its board, as every release file is (docs/updates.md)
NAME="peakroute-ap-arm64-${BOARD}-${VERSION}"
# Where U-Boot puts things (docs/u6plus.md, "How it starts"). U-Boot's bootm cannot start Debian's kernel: it refuses to unpack more than
# its CONFIG_SYS_BOOTM_LEN and resets the board. So the kernel is stored unpacked and started with booti, after imxtract has copied each
# part of the boot image to its place, all of it below 0x48000000 (U-Boot's own memory is near the top of the 256 MB):
#   0x40200000 the kernel (its image_size, up to the ramoops area at 0x42ff0000)   0x43100000 the device tree
#   0x43200000 the initramfs (up to 0x44000000)                                    0x44000000 the boot image as read (up to 0x48000000)
MAX_FIT_BYTES=$(( 0x48000000 - 0x44000000 ))
MAX_KERNEL_BYTES=$(( 0x42ff0000 - 0x40200000 ))
MAX_INITRD_BYTES=$(( 0x44000000 - 0x43200000 ))

# 1. As the person building: the daemon for arm64 and the device tree (Go and dtc need no root), then the rest as root
if [ "$(id -u)" -ne 0 ]; then
    for cmd in go dtc mkimage debootstrap xz; do
        command -v "$cmd" >/dev/null 2>&1 || { echo "Error: ${cmd} is missing. sudo apt install debootstrap qemu-user-static binfmt-support u-boot-tools device-tree-compiler xz-utils"; exit 1; }
    done
    mkdir -p "${OUT_DIR}"
    echo "Building PeakRoute AP ${VERSION} for the ${BOARD}"
    ( cd "${PROJECT_DIR}" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
        -ldflags="-X 'peakroute/internal/config.BuildVersion=${VERSION}' -X 'peakroute/internal/config.DefaultUpdateRepo=${UPDATE_REPO}'" \
        -o "${OUT_DIR}/peakroute-ap" ./cmd/peakroute-ap )
    dtc -q -I dts -O dtb -o "${OUT_DIR}/${BOARD}.dtb" "${BOARD_DIR}/${BOARD}.dts"
    exec sudo -E BUILD_DIR="${BUILD_DIR}" OUT_DIR="${OUT_DIR}" REUSE_ROOTFS="${REUSE_ROOTFS}" DEV_SSH_KEY="${DEV_SSH_KEY}" \
        PEAKROUTE_VERSION="${VERSION}" UPDATE_NOTES="${UPDATE_NOTES:-}" SUDO_OWNER="$(id -u):$(id -g)" "$0" "$@"
fi

# 2. As root
for cmd in debootstrap chroot mkfs.ext4 e2fsck mkimage xz wget dpkg-deb sha256sum truncate gzip tar; do
    command -v "$cmd" >/dev/null 2>&1 || { echo "Error: ${cmd} is missing"; exit 1; }
done
# Debian's arm64 programs must run here: qemu-user-static registers itself with the kernel (binfmt_misc)
if [ "$(uname -m)" != "aarch64" ] && ! grep -qs '^enabled' /proc/sys/fs/binfmt_misc/qemu-aarch64; then
    echo "Error: this machine cannot run arm64 programs. sudo apt install qemu-user-static binfmt-support"
    exit 1
fi
[ -f "${OUT_DIR}/peakroute-ap" ] && [ -f "${OUT_DIR}/${BOARD}.dtb" ] || { echo "Error: run this script as yourself, not with sudo"; exit 1; }

cleanup() {
    set +e
    for m in dev/pts proc sys dev; do
        mountpoint -q "${ROOTFS_DIR}/${m}" 2>/dev/null && umount -l "${ROOTFS_DIR}/${m}"
    done
    mountpoint -q "${BUILD_DIR}/mnt" 2>/dev/null && umount -l "${BUILD_DIR}/mnt"
    return 0
}
trap cleanup EXIT

mkdir -p "${BUILD_DIR}"
case ",$(findmnt -n -o OPTIONS -T "${BUILD_DIR}" 2>/dev/null)," in
    *,nodev,*|*,noexec,*) echo "Error: ${BUILD_DIR} is on a drive mounted nodev or noexec, where debootstrap cannot work"; exit 1 ;;
esac

echo "[1/5] Debian 13 (Trixie) for arm64..."
cleanup
if [ "${REUSE_ROOTFS}" = "1" ] && [ -f "${ROOTFS_DIR}/.peakroute-debootstrapped" ]; then
    echo "      reusing the root file system of the last run"
else
    rm -rf "${ROOTFS_DIR}"
    mkdir -p "${ROOTFS_DIR}"
    # Debian's own keyring, pinned by checksum (Ubuntu's predates Trixie), as buildImage.sh does
    KEYRING_DEB="debian-archive-keyring_2025.1_all.deb"
    KEYRING_SHA256="9ea7778e443144ca490668737a8ab22dd3e748bb99e805e22ec055abeb3c7fac"
    KEYRING_DIR="${BUILD_DIR}/keyring"
    mkdir -p "${KEYRING_DIR}"
    wget -q -O "${KEYRING_DIR}/${KEYRING_DEB}" "https://deb.debian.org/debian/pool/main/d/debian-archive-keyring/${KEYRING_DEB}"
    echo "${KEYRING_SHA256}  ${KEYRING_DIR}/${KEYRING_DEB}" | sha256sum -c --quiet -
    dpkg-deb -x "${KEYRING_DIR}/${KEYRING_DEB}" "${KEYRING_DIR}/extracted"

    # What an access point needs, and nothing of a router (no DHCP or DNS server, PPPoE, VPN, boot loader, other Wi-Fi firmware):
    # - linux-image-arm64 (it has every MT7981 driver: eMMC, Ethernet and its PHY, the Wi-Fi), initramfs-tools, udev, kmod, systemd
    # - ifupdown, bridge-utils, vlan, iproute2, udhcpc: the bridge the daemon writes and its address from the main router
    # - nftables: the access point's own input policy. hostapd, wpasupplicant, iw, wireless-regdb, rfkill, firmware-mediatek: Wi-Fi
    # - libubootenv-tool: fw_printenv and fw_setenv, to change what U-Boot starts. systemd-zram-generator: compressed swap in RAM.
    # - systemd-timesyncd, tzdata, ca-certificates, iputils-*, libnss-myhostname, sudo, procps: as on the router
    # (from the build folder: debootstrap leaves its download logs, wget-log, in the folder it runs in)
    cd "${BUILD_DIR}"
    DEBIAN_FRONTEND=noninteractive debootstrap \
        --arch=arm64 \
        --variant=minbase \
        --components=main,non-free-firmware \
        --keyring="${KEYRING_DIR}/extracted/usr/share/keyrings/debian-archive-keyring.gpg" \
        --include=linux-image-arm64,initramfs-tools,udev,kmod,systemd-sysv,sudo,procps,e2fsprogs,ifupdown,bridge-utils,vlan,iproute2,iputils-ping,iputils-tracepath,nftables,udhcpc,busybox,hostapd,wpasupplicant,iw,wireless-regdb,rfkill,firmware-mediatek,libubootenv-tool,systemd-zram-generator,xz-utils,libnss-myhostname,systemd-timesyncd,tzdata,ca-certificates,util-linux,nano,passwd \
        trixie "${ROOTFS_DIR}" http://deb.debian.org/debian/
    touch "${ROOTFS_DIR}/.peakroute-debootstrapped"
fi
for f in usr/bin/systemctl usr/sbin/update-initramfs usr/bin/fw_setenv usr/sbin/hostapd; do
    [ -e "${ROOTFS_DIR}/${f}" ] || { echo "Error: the root file system has no /${f}: the Debian install did not finish"; exit 1; }
done
ls "${ROOTFS_DIR}"/boot/vmlinuz-* >/dev/null 2>&1 || { echo "Error: the root file system has no kernel"; exit 1; }
for f in mt7981_wm.bin mt7981_wa.bin mt7981_rom_patch.bin; do
    [ -e "${ROOTFS_DIR}/usr/lib/firmware/mediatek/${f}" ] || { echo "Error: firmware-mediatek has no mediatek/${f}"; exit 1; }
done

echo "[2/5] Configuring it as PeakRoute AP..."
# the overlay of every image, then the U6+'s own files over it (fstab, initramfs, journal, watchdog, U-Boot's environment)
cp -a --no-preserve=ownership "${PROJECT_DIR}/image/overlay/." "${ROOTFS_DIR}/"
cp -a --no-preserve=ownership "${BOARD_DIR}/overlay/." "${ROOTFS_DIR}/"
chown -R root:root "${ROOTFS_DIR}/etc" "${ROOTFS_DIR}/usr/lib/peakroute"
chmod 755 "${ROOTFS_DIR}/usr/lib/peakroute/udhcpc-script" \
    "${ROOTFS_DIR}/etc/initramfs-tools/hooks/peakroute-state" "${ROOTFS_DIR}/etc/initramfs-tools/scripts/local-bottom/peakroute-state" \
    "${ROOTFS_DIR}/etc/initramfs-tools/scripts/local-bottom/peakroute-caldata"
# the x86 image's partition growing: the U6+'s partitions are made to size. The trial watch stays: it reads U-Boot's kept slot
rm -f "${ROOTFS_DIR}/etc/systemd/system/peakroute-growdata.service" "${ROOTFS_DIR}/usr/lib/peakroute/growdata"
chmod 755 "${ROOTFS_DIR}/usr/lib/peakroute/trialwatch"
mkdir -p "${ROOTFS_DIR}/data"
cat > "${ROOTFS_DIR}/usr/lib/peakroute/release" << RELEASE
PEAKROUTE_VERSION=${VERSION}
PEAKROUTE_BUILT=$(date -u +%Y-%m-%dT%H:%M:%SZ)
PEAKROUTE_LAYOUT=1
PEAKROUTE_ARCH=arm64
PEAKROUTE_PRODUCT=ap
PEAKROUTE_BOARD=${BOARD}
PEAKROUTE_BOOTLOADER=uboot
DEBIAN_VERSION=13
RELEASE
install -D -m 755 "${PROJECT_DIR}/internal/scripts/hostapd-run.sh" "${ROOTFS_DIR}/usr/lib/peakroute/hostapd-run"
install -D -m 755 "${OUT_DIR}/peakroute-ap" "${ROOTFS_DIR}/usr/lib/peakroute/peakroute"

mount --bind /proc "${ROOTFS_DIR}/proc"
mount --bind /sys "${ROOTFS_DIR}/sys"
mount --bind /dev "${ROOTFS_DIR}/dev"
mount --bind /dev/pts "${ROOTFS_DIR}/dev/pts"

# the chroot's programs speak plain C (the build machine's language is not installed in it)
export LC_ALL=C LANG=C
chroot "${ROOTFS_DIR}" passwd -l root
chroot "${ROOTFS_DIR}" systemctl enable peakroute.service peakroute-wifi.service peakroute-trialwatch.service nftables.service networking.service systemd-timesyncd.service
chroot "${ROOTFS_DIR}" systemctl disable wpa_supplicant.service 2>/dev/null || true
chroot "${ROOTFS_DIR}" systemctl mask systemd-firstboot.service
# what a root file system from an earlier version of this script lacks (xz: the initramfs is compressed with it)
missing=""
[ -x "${ROOTFS_DIR}/usr/bin/xz" ] || missing="${missing} xz-utils"
if [ -n "${DEV_SSH_KEY}" ] && [ ! -x "${ROOTFS_DIR}/usr/sbin/sshd" ]; then
    missing="${missing} openssh-server"
fi
if [ -n "${missing}" ]; then
    echo "      installing:${missing}"
    cp /etc/resolv.conf "${ROOTFS_DIR}/etc/resolv.conf"   # the download needs a name server (removed again below)
    chroot "${ROOTFS_DIR}" apt-get update -q
    # shellcheck disable=SC2086
    chroot "${ROOTFS_DIR}" env DEBIAN_FRONTEND=noninteractive apt-get install -y -q ${missing}
fi
if [ -n "${DEV_SSH_KEY}" ]; then
    # bring-up only: an SSH server where root signs in with one key (no password works: root's is locked)
    echo "      development build: SSH with the key ${DEV_SSH_KEY}"
    install -D -m 600 "${DEV_SSH_KEY}" "${ROOTFS_DIR}/root/.ssh/authorized_keys"
    chmod 700 "${ROOTFS_DIR}/root/.ssh"
    # each device makes its own host keys at its first start (Debian's ssh.service expects them made at install)
    rm -f "${ROOTFS_DIR}"/etc/ssh/ssh_host_*
    mkdir -p "${ROOTFS_DIR}/etc/systemd/system/ssh.service.d"
    printf '[Service]\nExecStartPre=\nExecStartPre=/usr/bin/ssh-keygen -A\nExecStartPre=/usr/sbin/sshd -t\n' \
        > "${ROOTFS_DIR}/etc/systemd/system/ssh.service.d/peakroute-keys.conf"
    chroot "${ROOTFS_DIR}" systemctl enable ssh.service
    # the access point's firewall lets in only its web interface (translator.renderNftablesAP): a development build opens SSH beside
    # it whenever the rules are loaded
    mkdir -p "${ROOTFS_DIR}/etc/systemd/system/nftables.service.d"
    printf '[Service]\nExecStartPost=-/usr/sbin/nft insert rule inet filter input tcp dport 22 accept\nExecReload=-/usr/sbin/nft insert rule inet filter input tcp dport 22 accept\n' \
        > "${ROOTFS_DIR}/etc/systemd/system/nftables.service.d/peakroute-dev-ssh.conf"
    echo "PEAKROUTE_DEV_SSH=1" >> "${ROOTFS_DIR}/usr/lib/peakroute/release"
fi
# the build machine's name servers were only for the download
: > "${ROOTFS_DIR}/etc/resolv.conf"
: > "${ROOTFS_DIR}/etc/machine-id"
rm -f "${ROOTFS_DIR}/var/lib/dbus/machine-id"

chroot "${ROOTFS_DIR}" update-initramfs -u -k all
# the one kernel Debian installed, and its initramfs
kernel="$(ls "${ROOTFS_DIR}"/boot/vmlinuz-* | sort -V | tail -n 1)"
initrd_file="${ROOTFS_DIR}/boot/initrd.img-${kernel##*/vmlinuz-}"
[ -f "${initrd_file}" ] || { echo "Error: there is no initramfs for ${kernel##*/}"; exit 1; }
chroot "${ROOTFS_DIR}" lsinitramfs "${initrd_file#${ROOTFS_DIR}}" > "${BUILD_DIR}/initrd.list"
for f in 'scripts/local-bottom/peakroute-state' 'scripts/local-bottom/peakroute-caldata' 'overlay.ko' 'mtk-sd.ko' 'regulator/fixed.ko' 'ramoops.ko' 'mtk_wdt.ko' 'e2fsck'; do
    grep -q "${f}" "${BUILD_DIR}/initrd.list" || { echo "Error: the initramfs does not hold ${f}"; exit 1; }
done

chroot "${ROOTFS_DIR}" apt-get clean || true
rm -rf "${ROOTFS_DIR}"/var/lib/apt/lists/* "${ROOTFS_DIR}"/var/cache/apt/* "${ROOTFS_DIR}"/tmp/* \
       "${ROOTFS_DIR}"/usr/share/man/* "${ROOTFS_DIR}"/usr/share/info/*
umount -l "${ROOTFS_DIR}/dev/pts" "${ROOTFS_DIR}/proc" "${ROOTFS_DIR}/sys" "${ROOTFS_DIR}/dev"

echo "[3/5] The boot image (FIT)..."
FIT_DIR="${BUILD_DIR}/fit"
rm -rf "${FIT_DIR}"
mkdir -p "${FIT_DIR}"
# Debian's arm64 vmlinuz is the plain Image, stored as it is (see MAX_FIT_BYTES). Its header says how much memory it takes running.
cp "${kernel}" "${FIT_DIR}/Image"
image_size="$(od -An -t u8 -j 16 -N 8 "${FIT_DIR}/Image" | tr -d ' ')"
[ "${image_size}" -le "${MAX_KERNEL_BYTES}" ] || { echo "Error: the kernel takes $(( image_size >> 20 )) MB running: more than fits below 0x42ff0000"; exit 1; }
cp "${initrd_file}" "${FIT_DIR}/initrd.img"
cp "${OUT_DIR}/${BOARD}.dtb" "${FIT_DIR}/board.dtb"
cat > "${FIT_DIR}/image.its" << ITS
/dts-v1/;
/ {
    description = "PeakRoute AP ${VERSION} for the UniFi U6+";
    #address-cells = <1>;
    images {
        kernel-1 {
            description = "Linux $(basename "${kernel}" | sed 's/^vmlinuz-//')";
            data = /incbin/("Image");
            type = "kernel";
            arch = "arm64";
            os = "linux";
            compression = "none";
            load = <0x40200000>;
            entry = <0x40200000>;
            hash-1 { algo = "sha1"; };
        };
        ramdisk-1 {
            description = "initramfs";
            data = /incbin/("initrd.img");
            type = "ramdisk";
            arch = "arm64";
            os = "linux";
            compression = "none";
            hash-1 { algo = "sha1"; };
        };
        fdt-1 {
            description = "UniFi 6 Plus";
            data = /incbin/("board.dtb");
            type = "flat_dt";
            arch = "arm64";
            compression = "none";
            hash-1 { algo = "sha1"; };
        };
    };
    configurations {
        default = "config-1";
        config-1 {
            description = "PeakRoute AP";
            kernel = "kernel-1";
            ramdisk = "ramdisk-1";
            fdt = "fdt-1";
        };
    };
};
ITS
FIT="${OUT_DIR}/${NAME}.fit"
mkimage -q -f "${FIT_DIR}/image.its" "${FIT}"
fit_bytes="$(stat -c %s "${FIT}")"
echo "      kernel $(( $(stat -c %s "${FIT_DIR}/Image") >> 20 )) MB, initramfs $(( $(stat -c %s "${FIT_DIR}/initrd.img") >> 20 )) MB: a boot image of $(( fit_bytes >> 20 )) MB"
[ "${fit_bytes}" -le "${MAX_FIT_BYTES}" ] || { echo "Error: the boot image is bigger than its place in memory ($(( MAX_FIT_BYTES >> 20 )) MB)"; exit 1; }
[ "$(stat -c %s "${FIT_DIR}/initrd.img")" -le "${MAX_INITRD_BYTES}" ] || { echo "Error: the initramfs is bigger than its place in memory ($(( MAX_INITRD_BYTES >> 20 )) MB)"; exit 1; }

echo "[4/5] The root file system..."
ROOTFS_IMG="${OUT_DIR}/${NAME}.rootfs"
rm -f "${ROOTFS_IMG}"
used_mb="$(du -sxm "${ROOTFS_DIR}" | cut -f1)"
rootfs_mb="$(( used_mb * 11 / 10 + 64 ))"
truncate -s "${rootfs_mb}M" "${ROOTFS_IMG}"
mkfs.ext4 -q -F -m 0 -L peakroute-root-a "${ROOTFS_IMG}"
mkdir -p "${BUILD_DIR}/mnt"
mount -o loop "${ROOTFS_IMG}" "${BUILD_DIR}/mnt"
cp -a "${ROOTFS_DIR}/." "${BUILD_DIR}/mnt/"
rm -f "${BUILD_DIR}/mnt/.peakroute-debootstrapped"
mkdir -p "${BUILD_DIR}/mnt"/{proc,sys,dev,run,mnt,tmp,var/tmp,data}
chmod 1777 "${BUILD_DIR}/mnt/tmp" "${BUILD_DIR}/mnt/var/tmp"
sync
umount "${BUILD_DIR}/mnt"
e2fsck -fy "${ROOTFS_IMG}" > /dev/null || [ $? -le 1 ] || { echo "Error: the root file system image is not clean"; exit 1; }
echo "      the system is ${used_mb} MB: a file system of ${rootfs_mb} MB"

echo "[5/5] The update bundle and the checksums..."
# The update bundle (docs/updates.md, section 5): the manifest, the compressed root file system and, for the U6+, the boot image that
# goes with it as a third entry (docs/u6plus.md, "Updates"), in a tar archive. The same root file system as the .rootfs.
BUNDLE="${OUT_DIR}/${NAME}.update"
BUNDLE_DIR="${BUILD_DIR}/bundle"
rm -rf "${BUNDLE_DIR}" "${BUNDLE}"
mkdir -p "${BUNDLE_DIR}"
if command -v pigz > /dev/null 2>&1; then pigz -n -6 -c "${ROOTFS_IMG}" > "${BUNDLE_DIR}/rootfs.img.gz"; else gzip -n -6 -c "${ROOTFS_IMG}" > "${BUNDLE_DIR}/rootfs.img.gz"; fi
cp "${FIT}" "${BUNDLE_DIR}/boot.fit"
json_text() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr '\n\t' '  '; }
cat > "${BUNDLE_DIR}/manifest.json" << MANIFEST
{
  "format": 1,
  "version": "$(json_text "${VERSION}")",
  "built": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "arch": "arm64",
  "product": "ap",
  "board": "${BOARD}",
  "layout": 1,
  "rootfs_size": $(stat -c %s "${ROOTFS_IMG}"),
  "rootfs_sha256": "$(sha256sum "${ROOTFS_IMG}" | cut -d' ' -f1)",
  "payload_sha256": "$(sha256sum "${BUNDLE_DIR}/rootfs.img.gz" | cut -d' ' -f1)",
  "boot": {"size": $(stat -c %s "${BUNDLE_DIR}/boot.fit"), "sha256": "$(sha256sum "${BUNDLE_DIR}/boot.fit" | cut -d' ' -f1)"},
  "notes": "$(json_text "${UPDATE_NOTES:-}")"
}
MANIFEST
tar -C "${BUNDLE_DIR}" --format=ustar -cf "${BUNDLE}" manifest.json rootfs.img.gz boot.fit
rm -rf "${BUNDLE_DIR}"
( cd "${OUT_DIR}" && for f in "${NAME}.fit" "${NAME}.rootfs" "${NAME}.update"; do sha256sum "${f}" > "${f}.sha256"; done )
[ -n "${SUDO_OWNER:-}" ] && chown -R "${SUDO_OWNER}" "${OUT_DIR}"
echo "Done: ${OUT_DIR}/${NAME}.fit, .rootfs and .update. Install with image/boards/${BOARD}/install.sh (docs/u6plus.md)."

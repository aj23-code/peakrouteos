#!/usr/bin/env bash
set -euo pipefail

# ==========================================
# PeakRoute OS - Debian 13 (Trixie) Build Pipeline
# Native debootstrap on an Ubuntu/Debian host (no container needed)
# Target: Edge Router Appliance (x86_64)
# ==========================================

PROJECT_DIR="$(pwd)"
# Override with e.g. BUILD_DIR=/tmp/peakroute-build if the project drive is mounted noexec/nodev
BUILD_DIR="${BUILD_DIR:-${PROJECT_DIR}/build}"
ROOTFS_DIR="${BUILD_DIR}/rootfs"
ISO_DIR="${BUILD_DIR}/iso"
OUTPUT_ISO="peakroute-os.iso"
GO_BINARY_PATH="${PROJECT_DIR}/peakroute"

# Check host build dependencies (mformat comes from mtools, needed by grub-mkrescue for EFI)
MISSING_TOOLS=()
for cmd in debootstrap xorriso mksquashfs grub-mkrescue mformat openssl wget dpkg-deb; do
    command -v "$cmd" >/dev/null 2>&1 || MISSING_TOOLS+=("$cmd")
done
if [ ${#MISSING_TOOLS[@]} -gt 0 ]; then
    echo "Error: Missing host build tools: ${MISSING_TOOLS[*]}"
    echo "Install on Ubuntu/Debian with:"
    echo "  sudo apt install debootstrap xorriso squashfs-tools grub-common grub-efi-amd64-bin grub-pc-bin mtools openssl wget"
    exit 1
fi

if [ ! -f "${GO_BINARY_PATH}" ]; then
    echo "Error: PeakRoute Go binary not found at ${GO_BINARY_PATH}"
    echo "Compile your binary: CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o peakroute"
    exit 1
fi

echo "[1/5] Cleaning workspace..."

# Unmount active chroot mounts if previous run failed midway
sudo umount -l "${ROOTFS_DIR}/proc" 2>/dev/null || true
sudo umount -l "${ROOTFS_DIR}/sys" 2>/dev/null || true
sudo umount -l "${ROOTFS_DIR}/dev" 2>/dev/null || true

sudo rm -rf "${BUILD_DIR}"
mkdir -p "${ROOTFS_DIR}" "${ISO_DIR}/boot/grub"

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

# bridge-utils lets ifupdown build bridges (bridge_ports ...) and vlan lets it build VLAN interfaces
# (eth1.20): the router's LAN bridge and its VLAN networks are written the standard Debian way.
#
# Wireless (the Wireless page, and PeakRoute running on an access point): hostapd runs the access
# point, iw reads what an adapter can do (bands, channels), wireless-regdb gives the regulatory rules
# per country and rfkill lifts a radio's software block. Wi-Fi adapters (and some wired NICs)
# need firmware, which Debian keeps in the non-free-firmware section, hence the extra component:
# Intel (iwlwifi), Qualcomm Atheros (ath9k_htc, ath10k...), MediaTek (mt76) and Realtek. Together they
# add roughly 270 MB uninstalled; drop the ones for hardware you will never use to shrink the image.
sudo debootstrap \
    --arch=amd64 \
    --variant=minbase \
    --components=main,non-free-firmware \
    --keyring="${DEBIAN_KEYRING}" \
    --include=linux-image-amd64,systemd-sysv,live-boot,live-config,dosfstools,e2fsprogs,rsync,ifupdown,bridge-utils,vlan,iproute2,iputils-ping,iputils-tracepath,nftables,dnsmasq,dnsmasq-utils,udhcpc,ppp,conntrack,hostapd,wpasupplicant,iw,wireless-regdb,rfkill,libnss-myhostname,systemd-timesyncd,tzdata,wireguard-tools,openvpn,firmware-iwlwifi,firmware-atheros,firmware-mediatek,firmware-realtek,ca-certificates,lsb-release,util-linux,fdisk,grub-efi-amd64,nano,passwd \
    trixie "${ROOTFS_DIR}" http://deb.debian.org/debian/

echo "[3/5] Configuring rootfs, security, and embedding PeakRoute binary..."



# Disable recommends/suggests to keep disk footprint ultra-minimal
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/etc/apt/apt.conf.d/99no-recommends" > /dev/null
APT::Install-Recommends "0";
APT::Install-Suggests "0";
EOF

# Ensure sysctl directory exists
sudo mkdir -p "${ROOTFS_DIR}/etc/sysctl.d"
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/etc/sysctl.d/99-peakroute.conf" > /dev/null
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
net.ipv4.conf.all.accept_source_route = 0
net.ipv4.conf.all.accept_redirects = 0
net.ipv4.conf.all.send_redirects = 0
net.ipv4.conf.all.rp_filter = 1
net.ipv4.tcp_syncookies = 1
EOF

# The system journal is kept on disk (so that a crash can be explained after the reboot) but cannot grow without limit:
# at most 100 MB in files of at most 10 MB, and never more than a month. The firewall log has its own database.
sudo mkdir -p "${ROOTFS_DIR}/etc/systemd/journald.conf.d"
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/etc/systemd/journald.conf.d/peakroute.conf" > /dev/null
[Journal]
Storage=persistent
SystemMaxUse=100M
SystemMaxFileSize=10M
MaxRetentionSec=1month
EOF

# Generate SHA-512 password hash for 'peakroute' on host
ROOT_HASH=$(openssl passwd -6 peakroute)

# Replace root password line directly in the rootfs shadow file
sudo sed -i "s|^root:[^:]*|root:${ROOT_HASH}|" "${ROOTFS_DIR}/etc/shadow"

# Copy PeakRoute Go Binary
sudo cp "${GO_BINARY_PATH}" "${ROOTFS_DIR}/usr/local/bin/peakroute"
sudo chmod +x "${ROOTFS_DIR}/usr/local/bin/peakroute"

# Copy First-Boot Script
sudo cp "${PROJECT_DIR}/internal/scripts/first_boot.sh" "${ROOTFS_DIR}/usr/local/bin/first_boot.sh"
sudo chmod +x "${ROOTFS_DIR}/usr/local/bin/first_boot.sh"

# Create Systemd Service for PeakRoute
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/etc/systemd/system/peakroute.service" > /dev/null
[Unit]
Description=PeakRoute OS Edge Routing Daemon
ConditionPathExists=!/run/live
After=network-pre.target
Before=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/peakroute
Restart=always
RestartSec=3
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

# Wi-Fi: one hostapd for every configured adapter. It only starts while the daemon has written a
# configuration for it (the condition), and after networking, because the access points join the bridges.
sudo mkdir -p "${ROOTFS_DIR}/usr/lib/peakroute"
sudo cp "${PROJECT_DIR}/internal/scripts/hostapd-run.sh" "${ROOTFS_DIR}/usr/lib/peakroute/hostapd-run"
sudo chmod 755 "${ROOTFS_DIR}/usr/lib/peakroute/hostapd-run"
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/etc/systemd/system/peakroute-wifi.service" > /dev/null
[Unit]
Description=PeakRoute OS Wi-Fi access points
ConditionPathExists=!/run/live
ConditionPathExistsGlob=/etc/peakroute/hostapd/*.conf
After=network.target networking.service

[Service]
Type=forking
PIDFile=/run/peakroute-hostapd.pid
ExecStart=/usr/lib/peakroute/hostapd-run
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

# Create First-Boot Systemd Service
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/etc/systemd/system/peakroute-firstboot.service" > /dev/null
[Unit]
Description=PeakRoute OS First-Time Setup Wizard
ConditionPathExists=!/etc/peakroute/.firstboot_done
ConditionPathExists=!/run/live
After=local-fs.target network-pre.target
Before=getty@tty1.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/first_boot.sh
StandardInput=tty
StandardOutput=tty
StandardError=tty
TTYPath=/dev/tty1
TTYReset=yes
TTYVHangup=yes
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF

# Create Live Installer Systemd Service (live session only; launches the installer on tty1)
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/etc/systemd/system/peakroute-installer.service" > /dev/null
[Unit]
Description=PeakRoute OS Live Installer
ConditionPathExists=/run/live
After=local-fs.target sysinit.target
Before=getty@tty1.service getty.target

[Service]
Type=oneshot
Environment=TERM=linux
ExecStart=/usr/local/bin/peakroute-install
StandardInput=tty
StandardOutput=tty
StandardError=tty
TTYPath=/dev/tty1
TTYReset=yes
TTYVHangup=yes
TimeoutStartSec=infinity

[Install]
WantedBy=multi-user.target
EOF

# Create getty@tty1 autologin override for Live ISO environment
sudo mkdir -p "${ROOTFS_DIR}/etc/systemd/system/getty@tty1.service.d"
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/etc/systemd/system/getty@tty1.service.d/override.conf" > /dev/null
[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin root --noclear %I $TERM
Type=idle
EOF

# Bind-mount pseudo-filesystems for clean chroot operations
sudo mount --bind /proc "${ROOTFS_DIR}/proc"
sudo mount --bind /sys "${ROOTFS_DIR}/sys"
sudo mount --bind /dev "${ROOTFS_DIR}/dev"

# ----------------------------------------------------
# Add PeakRoute Disk Installer Script
# ----------------------------------------------------
cat << 'EOF' | sudo tee "${ROOTFS_DIR}/usr/local/bin/peakroute-install" > /dev/null
#!/usr/bin/env bash
set -euo pipefail

clear
echo "======================================================="
echo "        PEAKROUTE OS - Install                         "
echo "======================================================="
echo ""
echo "Press Ctrl+C at any prompt to cancel and drop to a shell."
echo "(Run 'peakroute-install' again to restart.)"
echo ""

# Hostname Prompt
DEFAULT_HOSTNAME="peakroute"

echo ""
echo "--- System Hostname Configuration ---"
read -rp "Enter system hostname [$DEFAULT_HOSTNAME]: " USER_HOSTNAME

# Fall back to default if input is empty
NEW_HOSTNAME="${USER_HOSTNAME:-$DEFAULT_HOSTNAME}"

# Sanitize input: enforce lowercase, replace invalid characters with hyphens
NEW_HOSTNAME=$(echo "$NEW_HOSTNAME" | tr '[:upper:]' '[:lower:]' | sed 's/[^a-z0-9-]/ /g' | tr -s ' ' '-' | sed 's/^-//;s/-$//')

# List available block devices (excluding loop and ram)
echo ""
echo "Available storage targets:"
lsblk -d -n -o NAME,SIZE,MODEL,TYPE | grep -v "loop" | grep -v "rom"
echo ""

read -rp "Enter target disk device (e.g., sda, nvme0n1, vda): " TARGET_DEV
TARGET_DEV="/dev/${TARGET_DEV#/dev/}"

if [ ! -b "$TARGET_DEV" ]; then
    echo "Error: Device $TARGET_DEV does not exist."
    exit 1
fi

read -rp "WARNING: This will ERASE ALL DATA on $TARGET_DEV. Continue? (y/N): " CONFIRM
if [[ ! "$CONFIRM" =~ ^[Yy]$ ]]; then
    echo "Aborted."
    exit 1
fi

echo "[1/4] Partitioning drive $TARGET_DEV..."
dd if=/dev/zero of="$TARGET_DEV" bs=1M count=10 status=none
wipefs -a "$TARGET_DEV"

cat << SFDISK_CONF | sfdisk --label gpt "$TARGET_DEV"
size=512M, type=C12A7328-F81F-11D2-BA4B-00A0C93EC93B, name="EFI System Partition"
type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name="PeakRoute Root"
SFDISK_CONF

sync
sleep 2

if [[ "$TARGET_DEV" =~ [0-9]$ ]]; then
    BOOT_PART="${TARGET_DEV}p1"
    ROOT_PART="${TARGET_DEV}p2"
else
    BOOT_PART="${TARGET_DEV}1"
    ROOT_PART="${TARGET_DEV}2"
fi

echo "[2/4] Formatting filesystems..."
mkfs.vfat -F32 "$BOOT_PART"
mkfs.ext4 -F "$ROOT_PART"

echo "[3/4] Copying Live OS to disk..."
INSTALL_MOUNT="/mnt/target"
mkdir -p "$INSTALL_MOUNT"
mount "$ROOT_PART" "$INSTALL_MOUNT"

# Sync Live OS to target partition (preserve top-level dir paths)
rsync -aAX --exclude=/proc/* --exclude=/sys/* --exclude=/dev/* --exclude=/run/* --exclude=/mnt/* --exclude=/tmp/* / "$INSTALL_MOUNT/"

# The auto-start unit is for the live session only; drop it from the installed system
rm -f "$INSTALL_MOUNT/etc/systemd/system/peakroute-installer.service" \
      "$INSTALL_MOUNT/etc/systemd/system/multi-user.target.wants/peakroute-installer.service"

# --- Apply Hostname & Sys Config to Installed Rootfs ---
echo "[+] Setting installed target hostname to: $NEW_HOSTNAME"
echo "$NEW_HOSTNAME" > "$INSTALL_MOUNT/etc/hostname"

# Update loopback alias in installed /etc/hosts
if [ -f "$INSTALL_MOUNT/etc/hosts" ]; then
    if grep -q "127.0.1.1" "$INSTALL_MOUNT/etc/hosts"; then
        sed -i "s/127.0.1.1.*/127.0.1.1\t$NEW_HOSTNAME/" "$INSTALL_MOUNT/etc/hosts"
    else
        echo -e "127.0.1.1\t$NEW_HOSTNAME" >> "$INSTALL_MOUNT/etc/hosts"
    fi
fi

# Write initial sys.json into installed rootfs
mkdir -p "$INSTALL_MOUNT/etc/peakroute"
cat << SYSJSON > "$INSTALL_MOUNT/etc/peakroute/sys.json"
{
  "hostname": "$NEW_HOSTNAME",
  "operation_mode": "ROUTER",
  "configured": false
}
SYSJSON

# Guarantee essential mount points exist with proper permissions
mkdir -p "$INSTALL_MOUNT"/{proc,sys,dev,run,mnt,tmp,var/tmp,boot/efi}
chmod 1777 "$INSTALL_MOUNT/tmp" "$INSTALL_MOUNT/var/tmp"
chmod 755 "$INSTALL_MOUNT/run"

# Copy kernel and initramfs from live media
KVER="$(uname -r)"
echo "Copying kernel and initramfs from live media..."
cp -L /run/live/medium/boot/vmlinuz "$INSTALL_MOUNT/boot/vmlinuz-$KVER" 2>/dev/null || cp -L /vmlinuz "$INSTALL_MOUNT/boot/vmlinuz-$KVER"
cp -L /run/live/medium/boot/initrd.img "$INSTALL_MOUNT/boot/initrd.img-$KVER" 2>/dev/null || cp -L /initrd.img "$INSTALL_MOUNT/boot/initrd.img-$KVER"

# Generate fstab entries
ROOT_UUID=$(blkid -s UUID -o value "$ROOT_PART")
BOOT_UUID=$(blkid -s UUID -o value "$BOOT_PART")

cat << FSTAB > "$INSTALL_MOUNT/etc/fstab"
UUID=$ROOT_UUID / ext4 defaults,noatime 0 1
UUID=$BOOT_UUID /boot/efi vfat defaults 0 2
FSTAB

echo "[4/4] Installing GRUB Bootloader..."
mount --bind /proc "$INSTALL_MOUNT/proc"
mount --bind /sys "$INSTALL_MOUNT/sys"
mount --bind /dev "$INSTALL_MOUNT/dev"
mount --bind /sys/firmware/efi/efivars "$INSTALL_MOUNT/sys/firmware/efi/efivars" 2>/dev/null || true
mount "$BOOT_PART" "$INSTALL_MOUNT/boot/efi"

# Configure default GRUB parameters
mkdir -p "$INSTALL_MOUNT/etc/default"
cat <<'EOFA' > "$INSTALL_MOUNT/etc/default/grub"
GRUB_DEFAULT=0
GRUB_TIMEOUT=3
GRUB_DISTRIBUTOR="PeakRoute"
GRUB_CMDLINE_LINUX_DEFAULT="quiet console=tty1"
GRUB_CMDLINE_LINUX=""
GRUB_DISABLE_OS_PROBER=true
EOFA

# Install GRUB for NVRAM and fallback EFI removable path
chroot "$INSTALL_MOUNT" grub-install --target=x86_64-efi --efi-directory=/boot/efi --bootloader-id=PeakRouteOS --recheck 2>/dev/null || true
chroot "$INSTALL_MOUNT" grub-install --target=x86_64-efi --efi-directory=/boot/efi --removable --recheck
chroot "$INSTALL_MOUNT" update-grub

# Copy generated grub.cfg across EFI boot locations
mkdir -p "$INSTALL_MOUNT/boot/efi/EFI/PeakRouteOS" "$INSTALL_MOUNT/boot/efi/EFI/BOOT"
cp "$INSTALL_MOUNT/boot/grub/grub.cfg" "$INSTALL_MOUNT/boot/efi/EFI/PeakRouteOS/grub.cfg" 2>/dev/null || true
cp "$INSTALL_MOUNT/boot/grub/grub.cfg" "$INSTALL_MOUNT/boot/efi/EFI/BOOT/grub.cfg" 2>/dev/null || true

# Unmount target partitions cleanly
umount "$INSTALL_MOUNT/boot/efi"
umount "$INSTALL_MOUNT/sys/firmware/efi/efivars" 2>/dev/null || true
umount "$INSTALL_MOUNT/proc"
umount "$INSTALL_MOUNT/sys"
umount "$INSTALL_MOUNT/dev"
umount "$INSTALL_MOUNT"

echo "Installation complete! Detach live media and reboot."
read -rp "Press Enter to reboot..."
reboot
EOF
sudo chmod +x "${ROOTFS_DIR}/usr/local/bin/peakroute-install"

# Enable PeakRoute service and clean APT inside chroot
sudo chroot "${ROOTFS_DIR}" systemctl enable peakroute.service
sudo chroot "${ROOTFS_DIR}" systemctl enable peakroute-wifi.service
sudo chroot "${ROOTFS_DIR}" systemctl enable peakroute-firstboot.service
sudo chroot "${ROOTFS_DIR}" systemctl enable peakroute-installer.service
sudo chroot "${ROOTFS_DIR}" apt-get clean || true

# Generate SHA-512 password hash
ROOT_HASH=$(openssl passwd -6 "peakroute")

# Inject directly into target rootfs shadow file
sudo sed -i "s|^root:[^:]*:|root:${ROOT_HASH}:|" "${ROOTFS_DIR}/etc/shadow"


sudo rm -rf "${ROOTFS_DIR}/var/lib/apt/lists/*" "${ROOTFS_DIR}/tmp/*"

# Unmount pseudo-filesystems cleanly
sudo umount -l "${ROOTFS_DIR}/proc"
sudo umount -l "${ROOTFS_DIR}/sys"
sudo umount -l "${ROOTFS_DIR}/dev"

echo "[4/5] Building Live ISO image files..."

KERNEL_IMG=$(ls "${ROOTFS_DIR}"/boot/vmlinuz-* | head -n 1)
INITRD_IMG=$(ls "${ROOTFS_DIR}"/boot/initrd.img-* | head -n 1)

cp "${KERNEL_IMG}" "${ISO_DIR}/boot/vmlinuz"
cp "${INITRD_IMG}" "${ISO_DIR}/boot/initrd.img"

mkdir -p "${ISO_DIR}/live"
sudo mksquashfs "${ROOTFS_DIR}" "${ISO_DIR}/live/filesystem.squashfs" -e boot

cat << 'EOF' > "${ISO_DIR}/boot/grub/grub.cfg"
set default=0
set timeout=3

menuentry "PeakRoute OS (Live Install)" {
    linux /boot/vmlinuz boot=live quiet
    initrd /boot/initrd.img
}
EOF

echo "[5/5] Repacking into bootable hybrid ISO..."
grub-mkrescue -o "${PROJECT_DIR}/${OUTPUT_ISO}" "${ISO_DIR}"

echo "[+] Successfully built ISO: ${OUTPUT_ISO}"
ls -lh "${PROJECT_DIR}/${OUTPUT_ISO}"


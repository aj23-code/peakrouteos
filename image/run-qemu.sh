#!/usr/bin/env bash
# Starts the disk image in a virtual machine, to try it without a spare machine (docs/image.md).
#
#   image/run-qemu.sh                 UEFI boot, the web interface forwarded to https://localhost:9443
#   BOOT=bios image/run-qemu.sh       legacy BIOS boot
#   DISK=8G image/run-qemu.sh         a bigger disk than the image (the root partition grows to fill it at the first start)
#   NICS=3 image/run-qemu.sh          more network ports (the second and later ones are joined by a virtual cable to nothing)
#
# The image itself is never written: the machine works on a copy-on-write layer (build/qemu.qcow2) that is thrown away at the start.
# The machine's screen is this terminal (serial console); leave with Ctrl-A then X.

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMG="${IMG:-${HERE}/peakroute-os.img}"
BOOT="${BOOT:-uefi}"
DISK="${DISK:-8G}"
NICS="${NICS:-2}"
WEBPORT="${WEBPORT:-9443}"
LAYER="${HERE}/build/qemu.qcow2"
OVMF="${OVMF:-/usr/share/OVMF/OVMF_CODE_4M.fd}"

[ -f "${IMG}" ] || { echo "No image at ${IMG}: build it first (./full_build.sh)"; exit 1; }
mkdir -p "${HERE}/build"
rm -f "${LAYER}"
qemu-img create -q -f qcow2 -F raw -b "${IMG}" "${LAYER}" "${DISK}"

args=(-m 1024 -smp 2 -nographic -drive "file=${LAYER},if=virtio,format=qcow2")
[ -w /dev/kvm ] && args+=(-enable-kvm -cpu host)
if [ "${BOOT}" = "uefi" ]; then
    [ -f "${OVMF}" ] || { echo "No UEFI firmware at ${OVMF} (apt install ovmf), or BOOT=bios"; exit 1; }
    VARS="${HERE}/build/OVMF_VARS.fd"
    cp -f /usr/share/OVMF/OVMF_VARS_4M.fd "${VARS}"
    args+=(-drive "if=pflash,format=raw,readonly=on,file=${OVMF}" -drive "if=pflash,format=raw,file=${VARS}")
fi
# the first port is on the host's network with the web interface forwarded (to the setup address, which is what a new device answers
# on); the others have no cable
args+=(-netdev "user,id=n0,hostfwd=tcp::${WEBPORT}-192.168.77.1:443" -device "virtio-net-pci,netdev=n0,mac=52:54:00:12:34:00")
for i in $(seq 2 "${NICS}"); do
    args+=(-netdev "socket,id=n${i},listen=127.0.0.1:$((19000 + i))" -device "virtio-net-pci,netdev=n${i},mac=52:54:00:12:34:0${i}")
done
echo "Starting (${BOOT}); the web interface will be at https://localhost:${WEBPORT} once the device is up"
exec qemu-system-x86_64 "${args[@]}"

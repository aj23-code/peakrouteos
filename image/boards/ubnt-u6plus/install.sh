#!/usr/bin/env bash
# Puts PeakRoute AP on a UniFi U6+ that runs OpenWrt, over SSH, keeping OpenWrt to fall back to (docs/u6plus.md). Run it on the
# computer that built the image, one step at a time:
#
#   install.sh HOST backup       copies what is changed later (the partition table, OpenWrt's kernel and root, U-Boot's settings)
#   install.sh HOST partition    adds PeakRoute's partitions in the free space after Ubiquiti's (nothing of theirs is moved or changed)
#   install.sh HOST write        writes the boot image and the root file system built by buildImageU6Plus.sh (and makes the data
#                                partition the first time)
#   install.sh HOST probe-env    one start that only shows that U-Boot can save its settings, then OpenWrt again
#   install.sh HOST probe-load   one start that only reads PeakRoute's boot image and checks it, then OpenWrt again
#   install.sh HOST trial        the next start is PeakRoute, once: any start after it (a power cut, a crash) is OpenWrt again
#   install.sh HOST keep         PeakRoute starts every time (run against PeakRoute itself, a development build with SSH)
#   install.sh HOST update       from PeakRoute running slot a (a development build with SSH): writes the newest build to slot b and
#                                its boot partition and sets U-Boot up to start it once, on trial, as an update does (for a build
#                                that cannot install updates yet); keep it on its Updates page, or it goes back
#   install.sh HOST openwrt      OpenWrt starts every time again (from OpenWrt, or from a development build of PeakRoute)
#   install.sh HOST env          U-Boot's settings for PeakRoute again (after a change to this script)
#   install.sh HOST ramoops      the kernel's log of the last PeakRoute start that ended in OpenWrt (read from OpenWrt)
#   install.sh HOST status       what U-Boot will start, and what the probes found
#
# The files are the newest peakroute-ap-arm64-ubnt-u6plus-*.fit in OUT_DIR (out/ubnt-u6plus) and its .rootfs, or the .rootfs.gz of a
# release: OUT_DIR=~/Downloads install.sh HOST write.
#
# HOST is the U6+'s address (OpenWrt's, or PeakRoute's for keep). SSH_OPTS adds options to ssh (a key: SSH_OPTS="-i key"). ASSUME_YES=1
# answers the partition step's question.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${HERE}/../../.." && pwd)"
OUT_DIR="${OUT_DIR:-${PROJECT_DIR}/out/ubnt-u6plus}"
HOST="${1:?usage: install.sh HOST backup|partition|write|probe-env|probe-load|trial|keep|openwrt|status}"
STEP="${2:?usage: install.sh HOST backup|partition|write|probe-env|probe-load|trial|keep|openwrt|status}"
# shellcheck disable=SC2086
remote() { ssh ${SSH_OPTS:-} -o ConnectTimeout=10 "root@${HOST}" "$@"; }

DISK=/dev/mmcblk0
SECTORS=7634944 # the U6+'s eMMC (4FTE4R, 3.64 GiB); checked before the partition table is touched
# Where U-Boot reads the boot image to, and where imxtract puts its parts (buildImageU6Plus.sh says why: bootm cannot start this
# kernel, booti can)
LOADADDR=0x44000000
KERNEL_ADDR=0x40200000
FDT_ADDR=0x43100000
# the kernel's command line for slot A (the root file system by label, as on x86; the Wi-Fi calibration copied to /run by the initramfs)
# (cma=16M: Debian's kernel sets 64 MB aside for contiguous memory, a quarter of the U6+'s RAM; an access point needs little)
BOOTARGS="console=ttyS0,115200n8 root=LABEL=peakroute-root-a ro rootwait panic=10 cma=16M peakroute.slot=a peakroute.trial= firmware_class.path=/run/peakroute/firmware"

# partition_dev NAME, partition_start NAME: the device and the first sector of the partition of that name (nothing if there is none)
partition_dev() { remote "for p in /sys/block/mmcblk0/mmcblk0p*; do grep -qx 'PARTNAME=$1' \$p/uevent && echo /dev/\${p##*/}; done; true"; }
partition_start() { remote "for p in /sys/block/mmcblk0/mmcblk0p*; do grep -qx 'PARTNAME=$1' \$p/uevent && cat \$p/start; done; true"; }

check_u6plus() {
    local model sectors
    model="$(remote cat /proc/device-tree/model 2>/dev/null | tr -d '\0')"
    sectors="$(remote cat /sys/block/mmcblk0/size)"
    [ "${model}" = "Ubiquiti UniFi 6 Plus" ] || { echo "Error: ${HOST} is not a UniFi 6 Plus (it says: ${model})"; exit 1; }
    [ "${sectors}" = "${SECTORS}" ] || { echo "Error: this U6+'s eMMC has ${sectors} sectors, not ${SECTORS}: the layout here is not known"; exit 1; }
}

# set_env FILE: U-Boot's settings from a file of "name value" lines (fw_setenv -s: no shell quoting of U-Boot's own ${...}). OpenWrt's
# fw_setenv (u-boot's envtools) reads "name value"; Debian's (libubootenv, on PeakRoute) reads "name=value" and ignores a line without
# "=" (so "keep" run on PeakRoute changed nothing until 2026-10-31): the lines are turned into that form where the tool wants it.
set_env() {
    remote "cat > /tmp/peakroute.env && if fw_setenv --help 2>&1 | grep -q 'key=value'; then sed -i -e 's/^\([^ =]*\) /\1=/' -e 's/^\([^ =]*\)\$/\1=/' /tmp/peakroute.env; fi && fw_setenv -s /tmp/peakroute.env && rm /tmp/peakroute.env" < "$1"
}

# write_env FIT: U-Boot's settings for PeakRoute's boot image. U-Boot here has the simple command parser (no &&, || or if): a list
# of commands runs on after one fails, but "run a b c" stops at the first variable whose commands failed, so every step that the next
# depends on is a variable of its own (pl_*), and pr_* only mark how far a probe got.
write_env() {
    local start count env
    # where the boot image is: its first sector and its length in sectors (hexadecimal, as U-Boot's mmc read takes them)
    start="$(partition_start peakroute-boot-a)"
    count=$(( ( $(stat -c %s "$1") + 511 ) / 512 ))
    env="$(mktemp)"
    {
        printf 'pb_start 0x%x\n' "${start}"
        printf 'pb_count 0x%x\n' "${count}"
        printf 'pb_args %s\n' "${BOOTARGS}"
        # the install is slot a, kept; updates write slot b and set U-Boot up for a trial of it (internal/update/uboot.go). A slot b
        # from before is not one to go back to: its boot image length is taken away (a name alone removes the variable)
        echo 'pb_slot a'
        printf 'pb_count_a 0x%x\n' "${count}"
        echo 'pb_count_b'
        echo 'pl_dev mmc dev 0'
        printf 'pl_read mmc read %s ${pb_start} ${pb_count}\n' "${LOADADDR}"
        # the signature U-Boot would demand of Ubiquiti's own images is taken away, as for OpenWrt (it may be gone already: not a step)
        echo 'pl_sig fdt addr ${fdtcontroladdr}; fdt rm /signature'
        printf 'pl_check iminfo %s\n' "${LOADADDR}"
        # the kernel is copied out of the boot image to its place; booti takes the initramfs and the device tree from the boot image
        # by name (this U-Boot takes no initramfs that is not in an image of its own: "address:size" fails)
        printf 'pl_kernel imxtract %s kernel-1 %s\n' "${LOADADDR}" "${KERNEL_ADDR}"
        printf 'pl_fdt imxtract %s fdt-1 %s\n' "${LOADADDR}" "${FDT_ADDR}"
        printf 'pl_boot setenv bootargs ${pb_args}; booti %s %s:ramdisk-1 %s:fdt-1\n' "${KERNEL_ADDR}" "${LOADADDR}" "${LOADADDR}"
        # the kernel's RAM log (ramoops, 64 KiB at 0x42ff0000) is copied to peakroute-boot-b before OpenWrt starts after a trial:
        # OpenWrt's kernel reads that RAM with a layout of its own and clears what it does not understand (install.sh HOST ramoops)
        printf 'pb_bstart 0x%x\n' "$(partition_start peakroute-boot-b)"
        echo 'pl_save_ramoops mmc dev 0; mmc write 0x42ff0000 ${pb_bstart} 0x80'
        echo 'boot_fallback run pl_save_ramoops; run boot_openwrt'
        echo 'pr_dev setenv pr_load dev'
        echo 'pr_read setenv pr_load read'
        echo 'pr_check setenv pr_load checked'
        echo 'pr_kernel setenv pr_load kernel'
        echo 'pr_fdt setenv pr_load fdt'
        echo 'pr_ok setenv pr_load ok'
        # start PeakRoute: read the boot image, copy its parts out and start the kernel; if a step fails the caller goes on (to OpenWrt)
        echo 'boot_peak run pl_sig; run pl_dev pl_read pl_kernel pl_boot'
    } > "${env}"
    set_env "${env}"
    rm -f "${env}"
}

# find_build: fit, rootfs and unpack for the newest build in OUT_DIR, checked against their checksums
find_build() {
    fit="$(ls -t "${OUT_DIR}"/peakroute-ap-arm64-ubnt-u6plus-*.fit | head -n 1)"
    # the root file system as built (.rootfs), or as a release has it (.rootfs.gz, written as it is unpacked)
    rootfs="${fit%.fit}.rootfs"
    unpack=cat
    if [ ! -f "${rootfs}" ] && [ -f "${rootfs}.gz" ]; then
        rootfs="${rootfs}.gz"
        unpack="gzip -dc"
    fi
    [ -f "${rootfs}" ] || { echo "Error: no ${fit%.fit}.rootfs or .rootfs.gz beside ${fit}"; exit 1; }
    ( cd "$(dirname "${fit}")" && sha256sum -c --quiet "$(basename "${fit}").sha256" "$(basename "${rootfs}").sha256" )
}

case "${STEP}" in
backup)
    check_u6plus
    dir="${OUT_DIR}/backup-$(date +%Y%m%d-%H%M%S)"
    mkdir -p "${dir}"
    echo "Saving to ${dir}"
    remote "dd if=${DISK} bs=512 count=34 2>/dev/null" > "${dir}/gpt-primary.bin"
    # the backup copy of the table is where the primary's header says; Ubiquiti's has none (the header says sector 0), and the
    # sectors at the end of the eMMC, where the new table puts its backup, are saved as they are
    alt="$(od -An -t u8 -j $(( 512 + 32 )) -N 8 "${dir}/gpt-primary.bin" | tr -d ' ')"
    echo "${alt}" > "${dir}/gpt-backup.lba"
    remote "dd if=${DISK} bs=512 skip=$(( SECTORS - 33 )) count=33 2>/dev/null" > "${dir}/disk-end.bin"
    remote "fw_printenv" > "${dir}/uboot-env.txt"
    for name in kernel0 kernel1 bs; do
        dev="$(partition_dev "${name}")"
        echo "  ${name} (${dev})"
        remote "dd if=${dev} bs=1M 2>/dev/null" > "${dir}/${name}.bin"
    done
    ( cd "${dir}" && sha256sum ./*.bin > SHA256SUMS )
    echo "Done. To put OpenWrt back by hand: write kernel0.bin and kernel1.bin to their partitions and run: install.sh HOST openwrt"
    ;;

partition)
    check_u6plus
    if [ -n "$(partition_dev peakroute-data)" ]; then
        echo "PeakRoute's partitions are there already:"
        remote "grep -H PARTNAME /sys/block/mmcblk0/mmcblk0p1[1-5]/uevent"
        exit 0
    fi
    [ -n "$(partition_dev log)" ] || { echo "Error: the U6+'s partition 'log' is not there: the layout is not the one this script knows"; exit 1; }
    work="$(mktemp -d)"
    trap 'rm -rf "${work}"' EXIT
    # The partition table is rewritten on a copy of the disk's size here (sfdisk), and only its sectors are written back. Ubiquiti's
    # table has room for 10 entries, all used, and says the disk ends at 3 GiB (where its backup copy is): the new one keeps every entry
    # as it is (place, size, type, id, name), has room for 128, reaches the real end of the eMMC and adds PeakRoute's after "log".
    truncate -s $(( SECTORS * 512 )) "${work}/disk"
    remote "dd if=${DISK} bs=512 count=34 2>/dev/null" | dd of="${work}/disk" conv=notrunc status=none
    sfdisk --dump "${work}/disk" 2>/dev/null > "${work}/before"
    grep -q 'name="log"' "${work}/before" || { echo "Error: the copied partition table has no partition 'log'"; cat "${work}/before"; exit 1; }
    log_end=$(( $(remote cat /sys/block/mmcblk0/mmcblk0p10/start) + $(remote cat /sys/block/mmcblk0/mmcblk0p10/size) ))
    first=$(( (log_end + 2047) / 2048 * 2048 ))
    {
        grep -E '^(label|label-id|unit|first-lba|sector-size):' "${work}/before"
        echo "table-length: 128"
        echo
        grep '^/' "${work}/before" | sed 's#^/[^ ]* : ##'
        echo "start=${first}, size=64MiB, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"peakroute-boot-a\""
        echo "size=64MiB, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"peakroute-boot-b\""
        echo "size=1088MiB, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"peakroute-root-a\""
        echo "size=1088MiB, type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"peakroute-root-b\""
        echo "type=0FC63DAF-8483-4772-8E79-3D69D8477DE4, name=\"peakroute-data\""
    } > "${work}/table"
    sfdisk --quiet "${work}/disk" < "${work}/table" 2>/dev/null
    sfdisk --dump "${work}/disk" > "${work}/after"
    # nothing that was there may have changed
    grep '^/' "${work}/before" | sed 's#^/[^ ]* : ##' | while read -r line; do
        grep -qF "${line}" "${work}/after" || { echo "Error: the new table changes an existing partition: ${line}"; exit 1; }
    done
    [ "$(grep -c 'name="peakroute-' "${work}/after")" = 5 ] || { echo "Error: the new partitions were not made"; cat "${work}/after"; exit 1; }
    echo "The new partitions:"
    grep 'peakroute' "${work}/after" | sed 's#^/[^ ]* : ##'
    # asked on the terminal (ssh reads standard input); ASSUME_YES=1 for a run that was agreed beforehand
    if [ "${ASSUME_YES:-}" != "1" ]; then
        read -r -p "Write this partition table to ${HOST}? [y/N] " ok < /dev/tty
        [ "${ok}" = "y" ] || exit 1
    fi
    # sectors 1 to 33 (the header and the entries; sector 0, the protective MBR, stays Ubiquiti's) and the backup at the end
    dd if="${work}/disk" bs=512 skip=1 count=33 status=none | remote "dd of=${DISK} bs=512 seek=1 count=33 conv=fsync 2>/dev/null"
    dd if="${work}/disk" bs=512 skip=$(( SECTORS - 33 )) count=33 status=none | remote "dd of=${DISK} bs=512 seek=$(( SECTORS - 33 )) count=33 conv=fsync 2>/dev/null"
    # check what is on the disk now against the table made here
    remote "dd if=${DISK} bs=512 skip=1 count=33 2>/dev/null" | cmp -s - <(dd if="${work}/disk" bs=512 skip=1 count=33 status=none) \
        || { echo "Error: the partition table read back from ${HOST} is not the one written: restore it (install.sh backup saved it)"; exit 1; }
    remote "partx -a ${DISK} 2>/dev/null; sleep 1; grep -H PARTNAME /sys/block/mmcblk0/mmcblk0p1[1-5]/uevent"
    ;;

write)
    check_u6plus
    find_build
    boot="$(partition_dev peakroute-boot-a)"; root="$(partition_dev peakroute-root-a)"; data="$(partition_dev peakroute-data)"
    [ -n "${boot}" ] && [ -n "${root}" ] && [ -n "${data}" ] || { echo "Error: run the partition step first"; exit 1; }
    echo "Writing $(basename "${fit}") to ${boot}..."
    remote "dd of=${boot} bs=1M conv=fsync 2>/dev/null" < "${fit}"
    echo "Writing $(basename "${rootfs}") to ${root} ($(( $(stat -c %s "${rootfs}") >> 20 )) MB)..."
    ${unpack} "${rootfs}" | remote "dd of=${root} bs=1M conv=fsync 2>/dev/null"
    remote "e2fsck -fn ${root} >/dev/null" || { echo "Error: the written root file system does not check clean"; exit 1; }
    # (OpenWrt has no blkid: the ext4 label is read from the superblock, 16 bytes at 1144)
    if [ "$(remote "dd if=${data} bs=1 skip=1144 count=16 2>/dev/null" | tr -d '\0')" != "peakroute-data" ]; then
        echo "Making the data partition..."
        remote "mkfs.ext4 -q -F -m 0 -L peakroute-data ${data}"
    fi
    write_env "${fit}"
    echo "Written. U-Boot still starts OpenWrt: next, probe-env, probe-load, then trial."
    ;;

probe-env)
    env="$(mktemp)"
    cat > "${env}" << 'ENV'
pr_env
bootcmd_real setenv bootcmd_real run boot_openwrt; setenv pr_env saved; saveenv; run boot_openwrt
ENV
    set_env "${env}"; rm -f "${env}"
    echo "Restarting ${HOST}: it comes back as OpenWrt in a minute or two. Then: install.sh ${HOST} status"
    remote reboot
    ;;

probe-load)
    env="$(mktemp)"
    cat > "${env}" << ENV
pr_load
bootcmd_real setenv bootcmd_real run boot_openwrt; setenv pr_load none; run pl_sig; run pl_dev pr_dev pl_read pr_read pl_check pr_check pl_kernel pr_kernel pl_fdt pr_ok; saveenv; run boot_openwrt
ENV
    set_env "${env}"; rm -f "${env}"
    echo "Restarting ${HOST}: it comes back as OpenWrt in a minute or two. Then: install.sh ${HOST} status"
    remote reboot
    ;;

trial)
    [ "$(remote fw_printenv -n pr_load 2>/dev/null)" = "ok" ] || { echo "Error: run probe-load first (U-Boot must have read the boot image)"; exit 1; }
    env="$(mktemp)"
    cat > "${env}" << 'ENV'
bootcmd_real setenv bootcmd_real run boot_fallback; setenv pr_trial started; saveenv; run boot_peak; setenv pr_trial bootm-returned; saveenv; run boot_fallback
ENV
    set_env "${env}"; rm -f "${env}"
    echo "Restarting ${HOST} into PeakRoute, once. If it does not come up, a power cycle brings OpenWrt back."
    echo "PeakRoute asks your router for an address (look for the name 'peakroute') and keeps 192.168.77.1 beside it."
    remote reboot
    ;;

keep)
    env="$(mktemp)"
    echo 'bootcmd_real run boot_peak; run boot_openwrt' > "${env}"
    set_env "${env}"; rm -f "${env}"
    echo "PeakRoute starts every time now (OpenWrt only if PeakRoute's boot image cannot be read)."
    ;;

openwrt)
    env="$(mktemp)"
    echo 'bootcmd_real run boot_openwrt' > "${env}"
    set_env "${env}"; rm -f "${env}"
    echo "OpenWrt starts every time again (restart ${HOST} for it to)."
    ;;

update)
    # what internal/update/uboot.go does, from outside: slot b and its boot partition, then a one-time start of it
    check_u6plus
    remote "grep -qw peakroute.slot=a /proc/cmdline" || { echo "Error: run it against PeakRoute running from slot a"; exit 1; }
    find_build
    boot="$(partition_dev peakroute-boot-b)"; root="$(partition_dev peakroute-root-b)"
    [ -n "${boot}" ] && [ -n "${root}" ] || { echo "Error: ${HOST} has no slot b partitions"; exit 1; }
    remote "! grep -q '^${root} ' /proc/mounts" || { echo "Error: ${root} is in use"; exit 1; }
    count=$(( ( $(stat -c %s "${fit}") + 511 ) / 512 ))
    # slot b is not one to start until it is all written
    env="$(mktemp)"
    echo 'pb_count_b' > "${env}"
    set_env "${env}"
    echo "Writing $(basename "${fit}") to ${boot}..."
    remote "dd of=${boot} bs=1M conv=fsync 2>/dev/null" < "${fit}"
    echo "Writing $(basename "${rootfs}") to ${root} ($(( $(stat -c %s "${rootfs}") >> 20 )) MB)..."
    ${unpack} "${rootfs}" | remote "dd of=${root} bs=1M conv=fsync 2>/dev/null"
    # checked, grown to the partition and named for slot b (root=LABEL=peakroute-root-b), as the daemon does
    remote "e2fsck -fp ${root} >/dev/null; [ \$? -le 1 ] && resize2fs ${root} >/dev/null 2>&1 && tune2fs -L peakroute-root-b ${root} >/dev/null" \
        || { echo "Error: the written root file system does not check clean"; exit 1; }
    pb_count="$(remote "fw_printenv -n pb_count")"
    {
        # slot a stays the kept one, to go back to
        echo 'pb_slot a'
        printf 'pb_count_a %s\n' "${pb_count}"
        printf 'pb_count_b 0x%x\n' "${count}"
        printf 'pt_start 0x%x\n' "$(partition_start peakroute-boot-b)"
        printf 'pt_count 0x%x\n' "${count}"
        args="${BOOTARGS/root=LABEL=peakroute-root-a/root=LABEL=peakroute-root-b}"
        args="${args/peakroute.slot=a/peakroute.slot=b}"
        printf 'pt_args %s\n' "${args/peakroute.trial= /peakroute.trial=1 }"
        printf 'pt_read mmc read %s ${pt_start} ${pt_count}\n' "${LOADADDR}"
        printf 'pt_boot setenv bootargs ${pt_args}; booti %s %s:ramdisk-1 %s:fdt-1\n' "${KERNEL_ADDR}" "${LOADADDR}" "${LOADADDR}"
        echo 'boot_trial run pl_sig; run pl_dev pt_read pl_kernel pt_boot'
        echo 'boot_kept run boot_peak; run boot_openwrt'
        echo 'bootcmd_real setenv bootcmd_real run boot_kept; saveenv; run boot_trial; run boot_kept'
    } > "${env}"
    set_env "${env}"; rm -f "${env}"
    echo "Written. Restart ${HOST} (ssh root@${HOST} reboot) to start slot b once, then keep it on its Updates page within 10 minutes."
    ;;

env)
    # U-Boot's settings again, for the boot image that was written last (after a change to this script)
    check_u6plus
    write_env "$(ls -t "${OUT_DIR}"/peakroute-ap-arm64-ubnt-u6plus-*.fit | head -n 1)"
    echo "Done."
    ;;

ramoops)
    # the kernel's RAM log of the last PeakRoute start, as U-Boot copied it to peakroute-boot-b (see write_env)
    dev="$(partition_dev peakroute-boot-b)"
    remote "dd if=${dev} bs=512 count=128 2>/dev/null" > "${OUT_DIR}/ramoops.bin"
    python3 -I "${HERE}/ramoops.py" "${OUT_DIR}/ramoops.bin"
    ;;

status)
    remote "fw_printenv bootcmd_real pr_env pr_load pr_trial pb_start pb_count 2>&1; echo; uname -a; grep -H PARTNAME /sys/block/mmcblk0/mmcblk0p1[1-5]/uevent 2>/dev/null"
    ;;

*)
    echo "Unknown step ${STEP}"; exit 1 ;;
esac

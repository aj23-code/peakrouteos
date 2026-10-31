"""Prints the kernel logs in a raw copy of the U6+'s ramoops area (install.sh HOST ramoops; docs/u6plus.md).

The area is 64 KiB of RAM that survives a restart. The kernel divides it into records (persistent_ram_zone: "DBGC", start, size, then
the data as a ring), each one log written at a restart or a crash ("====<seconds>-<C|D>" and the text, deflated when C). The layout
depends on the kernel that wrote it (OpenWrt's: 4 KiB records, PeakRoute's: 32 KiB), so every 4 KiB boundary is looked at.
"""
import struct
import sys
import zlib

SIG = 0x43474244  # "DBGC"


def records(raw):
    for off in range(0, len(raw) - 12, 0x1000):
        sig, start, size = struct.unpack_from("<III", raw, off)
        if sig != SIG:
            continue
        # the zone ends where the next one starts (or the area ends)
        end = len(raw)
        for nxt in range(off + 0x1000, len(raw) - 12, 0x1000):
            if struct.unpack_from("<I", raw, nxt)[0] == SIG:
                end = nxt
                break
        data = raw[off + 12:end]
        if size > len(data) or start > len(data):
            yield off, None, "a record whose sizes do not fit its zone (written with another layout)"
            continue
        yield off, data[start:size] + data[:start] if start < size else data[:size], None


def text(rec):
    head, _, body = rec.partition(b"\n")
    if head.startswith(b"====") and head.endswith(b"-C"):
        for wbits in (-15, 15, 31):
            try:
                return head.decode(errors="replace") + " (deflated)\n" + zlib.decompressobj(wbits).decompress(body).decode(errors="replace")
            except zlib.error:
                continue
        return head.decode(errors="replace") + " (deflated, but it does not inflate)\n"
    return rec.decode(errors="replace")


def main():
    raw = open(sys.argv[1], "rb").read()
    found = False
    for off, rec, problem in records(raw):
        found = True
        print(f"---- record at 0x{off:x}")
        print(problem if problem else text(rec))
    if not found:
        nonzero = sum(1 for b in raw if b not in (0, 0xFF))
        print(f"no ramoops record in the copy ({nonzero} bytes are neither 0 nor 0xff): the kernel did not get as far as loading ramoops")


main()

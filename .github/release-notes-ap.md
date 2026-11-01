## PeakRoute AP

The access point build of PeakRoute (`docs/access-point.md`): no router in it, the same web interface and API for what an access point
does. Each file is named for the processors and the board it is for: `peakroute-ap-<arch>-<board>-<version>`.

## Which file do I need?

For the **Ubiquiti UniFi 6 Plus (U6+)** (`arm64`, board `ubnt-u6plus`; `docs/u6plus.md`):

| File | What it is | Use it to |
|---|---|---|
| `peakroute-ap-arm64-ubnt-u6plus-<version>.update` | the system and its boot image, with a manifest | **update a U6+ that runs PeakRoute AP**: System → Updates (a device with updates on finds it by itself, or upload the file) |
| `peakroute-ap-arm64-ubnt-u6plus-<version>.fit` and `.rootfs.gz` | the boot image and the root file system | **install** PeakRoute AP on a U6+ that runs OpenWrt: `image/boards/ubnt-u6plus/install.sh` (with `OUT_DIR` set to the folder they are in) |
| `*.sha256` | checksums | check a download: `sha256sum -c <file>.sha256` |

Installing keeps Ubiquiti's boot loader and OpenWrt (as the fallback), and starts PeakRoute AP once on trial before it is kept: read
`docs/u6plus.md` first.

Releases with `-alpha` or `-beta` in the version are pre-releases. Devices only offer them when *Include pre-releases* is on.

## Which file do I need?

| File | What it is | Use it to |
|---|---|---|
| `peakroute-os-<version>.img.gz` | the whole disk image, compressed | write a **new** device, or write one again: `gunzip -k` it, then `dd` it to the disk (at least 8 GB) |
| `peakroute-os-<version>.update` | the system only, with a manifest | **update a running device**: System → Updates (a device with updates on finds it by itself, or upload the file) |
| `*.sha256` | checksums | check a download: `sha256sum -c <file>.sha256` |

A device written from an image older than `v0.9.0-alpha` has a different disk layout and cannot update to this one: write the image once
(carry the settings over with Backup and restore). From `v0.9.0-alpha` on, updates do the work.

## Why the disk image is so big

The image unpacks to about **6.6 GB for about 1.1 GB of system**. That is deliberate: updates are A/B (the new version is written next to the one
that runs and tried once before it is kept), so the disk holds **two root slots of 3 GiB each**, plus the boot partitions and a data partition
for settings and history. The slots cannot be resized after the disk is written, so they have room to grow over many updates. Most of the
file is empty space, which compresses to almost nothing: the `.img.gz` you download is about as big as the system itself. A 128 GB drive is
used fully (the data partition takes the rest). A smaller layout is planned once this one has been booted for real; the reasoning is in
`docs/image.md`.

Releases with `-alpha` or `-beta` in the version are pre-releases. Devices only offer them when *Include pre-releases* is on.

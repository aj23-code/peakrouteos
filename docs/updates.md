# Updates

How an installed PeakRoute OS device is brought to a new version without being written again. Decided with the user on 2026-10-20:
**A/B root partitions** (the device is a live router, so an update must be safe to undo), releases **fetched from GitHub or uploaded as a
file**, and **checksum only** for trust at first (a signature can be added to the bundle later without changing the layout).

## 1. The idea

The disk has two copies of the system, **slot A** and **slot B**. One is running. An update writes the new system into the *other* one
while the router keeps routing, then restarts once into it **on trial**. If the new version comes up and the administrator keeps it, it
becomes the running slot; if not (it does not boot, the web interface does not answer, nobody confirms), the next start is the old slot
again. The old version is never touched until the new one is kept, so there is always a way back.

Everything that is the *router's* (settings, administrators, certificates, history, the machine's identity) is on a third partition,
**data**, shared by both slots, so it is the same before and after an update and after a rollback.

## 2. Disk layout (GPT, layout version 1)

| # | Name (partition label) | Size | Holds |
|---|---|---|---|
| 1 | `bios-boot` | 1 MiB | GRUB for legacy BIOS machines |
| 2 | `peakroute-esp` | 128 MiB | FAT32 `PRESP`: the UEFI loader, **GRUB's files, `grub.cfg` and `grubenv`** (which slot to start) |
| 3 | `peakroute-root-a` | slot size | ext4, label `peakroute-root-a`: the system (kernel, initramfs, Debian, the daemon) |
| 4 | `peakroute-root-b` | slot size | the same for the other version; empty (no file system) until the first update |
| 5 | `peakroute-data` | the rest of the disk | ext4, label `peakroute-data`: everything that is the router's |

A slot is the root file system plus room to grow (the image build uses the file system's size plus half again, at least 3 GiB). A slot
cannot be resized later without moving the data partition, so a release must fit the slot the device was written with; the bundle says
how big its file system is and the device refuses one that does not fit. The data partition is last and grows to the disk at the first
start (`peakroute-growroot`, `/usr/lib/peakroute/growroot`, which now grows `/data`).

The root file system of a slot is mounted **read-only**. What must be written lives on the data partition and is laid over the system
by the initramfs (`/etc/initramfs-tools/scripts/local-bottom/peakroute-state`) before systemd starts:

| Path | Layer | Why |
|---|---|---|
| `/etc` | overlay: the slot's `/etc` below, `/data/overlay/etc` above | the daemon writes configuration, certificates, accounts, the host name here |
| `/var` | overlay | journal, DHCP leases, the databases of history, state of services |
| `/home` | overlay | console administrators' homes |
| `/usr/local` | overlay | the daemon writes small helper scripts here (`peakroute-wg-sync`, `peakroute-ovpn-sync`) |
| `/data` | the data partition itself | the overlays' upper layers and work directories |

A **factory reset** removes `/data/overlay` (all four layers at once) at the next start; the daemon asks for it with the file `/data/factory-reset` and the initramfs does it before laying the layers (docs/settings.md, section 4).

Files the router has *changed* are in the upper layer and win over the slot's own copy; files it never touched come from the slot. An
update therefore brings new defaults for everything the router has not changed. A file in `/etc` that the router *has* changed keeps its
router version after an update: the daemon writes nearly all of its configuration, so this is what we want. (When an image ever needs to
force a changed `/etc` file onto existing devices, the bundle carries a list of paths to drop from the upper layer; the manifest has a
place for it, `reset`, unused so far.) **The daemon's own binary must not be in `/usr/local`**: it is `/usr/lib/peakroute/peakroute`,
so that a copy left in the upper layer can never shadow the one in the slot.

If the data partition cannot be mounted the initramfs lays tmpfs layers instead, so the device still starts (with the setup network and no
saved state), and the daemon shows a warning on the Updates and Overview pages.

## 3. Choosing the slot at the boot

GRUB's files are on the ESP, not in a slot, so both slots are equal and GRUB never has to be updated by an update of a slot (a new
GRUB would be a separate, rare operation). `grub.cfg` is written by the image build and reads `grubenv` on the same partition:

- `slot`: the slot that is **kept** (`a` or `b`). It is what starts, unless a trial is set.
- `trial`: a slot to try **once**. At the start GRUB reads it, **clears it**, and starts that slot. So a trial that does not get confirmed
  is a single start: the next start (a crash, a power cut, the trial watchdog) is the kept slot again.

GRUB finds the slot by the file system label (`search --label peakroute-root-<slot>`) and passes `root=LABEL=peakroute-root-<slot>`,
`ro`, `peakroute.slot=<slot>` and, when it is a trial, `peakroute.trial=1` to the kernel. It also adds `panic=10` so a kernel panic
restarts instead of hanging. A second menu entry starts the *other* slot by hand (for the console, 2 s menu).

The daemon changes `grubenv` with `grub-editenv` (the ESP is mounted read-only and remounted writable for the moment of the change, then
`sync`ed: a FAT partition that is written to all the time is a risk on a router that is switched off at the wall).

## 4. The life of an update

1. **Find or receive a bundle** (§5): *Check for updates* asks GitHub, or the administrator uploads a file.
2. **Look at it.** The bundle's manifest says the version, when it was built, its layout version and architecture, the size of its file
   system and its checksums. The page shows it next to the running version. Refused: another product or board (a router's bundle on an
   access point, one access point model's bundle on another: see "Product, board and processors" below), wrong architecture, other layout version (needs a
   fresh image), a file system bigger than the slot, the same version already running (unless the administrator says to install it
   again), and anything while a configuration change is waiting to be confirmed.
3. **Copy.** The daemon streams the bundle into the *other* slot's partition: download (or upload) → checksum → decompress → checksum →
   write → `sync`. Nothing is kept in memory or in a temporary file. Then it checks the written file system (`e2fsck -f -p`), grows it to
   the partition (`resize2fs`) and gives it the slot's label (`tune2fs -L peakroute-root-<slot>`: that is what GRUB and the kernel find
   it by, and why a half-written slot can never be started: until the last step it has no label). The router keeps routing the whole
   time. The page shows progress; only one update runs at a time.
4. **A copy of the settings.** The daemon makes the same copy of itself that a restore does (`docs/backup.md`), so a newer version that
   changed a file format can be undone.
5. **Restart on trial.** `trial=<slot>` is set and the device restarts (the administrator is asked first; the page says what will
   happen: a minute or two without internet).
6. **On trial.** The new version starts. The Updates page (and a banner) says *“Version X is on trial”* and counts down 10 minutes. A
   service started only on trial (`peakroute-trialwatch`) restarts the device into the kept slot when the countdown ends without
   confirmation, even if the daemon is dead. The administrator, who got to the page through the new version, presses **Keep this
   version** (`slot=<this>`) or **Go back to the old version** (restart, `slot` is unchanged).
7. **Kept.** The other slot now holds the previous version, ready for a rollback (*Go back*, which is the same as a trial of it).

Reverting a kept version is just another trial of the other slot: an update with the previous version already written.

What is not covered: a bundle never changes GRUB, the partition table or the ESP. That is what the *layout version* is for: a release that
needs one is announced as a full image (a fresh write), and the Updates page says so rather than trying.

## 5. The bundle

One file, `peakroute-os-amd64-generic-<version>.update` for the router (see "Release files are named for what they are" below), a tar
archive:

| Entry | Content |
|---|---|
| `manifest.json` (first) | `format` (1), `version` (`v0.10.0`), `built` (RFC 3339), `arch` (`amd64`), `product` (`router` or `ap`), `board` (`pc` or an access point model such as `ubnt-u6plus`), `layout` (1), `rootfs_size` (bytes of the file system), `rootfs_sha256` (of the decompressed file system), `payload_sha256` (of the next entry), `notes` (what changed, a few lines), `reset` (paths, see §2; none yet) |
| `rootfs.img.gz` | the root file system image (an ext4 file system, built by `buildImage.sh` and gzip-compressed) |

A GitHub release has the file and its `.update.sha256` (the checksum of the whole file, `sha256sum` format). The device
checks the file it downloaded against it, and the payload and the file system against the manifest, so a truncated or corrupt download
never gets as far as a slot. (A signature would be a third entry, `manifest.sig`, checked against a public key in the image: planned,
not built.) The first image carries the same file system as slot A.

**Product, board and processors (2026-10-30).** There are two builds of the software (the router and PeakRoute AP,
`docs/access-point.md`), and an access point image is made for one model, whose kernel, device tree and disk are its own. So a bundle
names three things and goes only onto a system that is the same in all three: its **product** (`router`, `ap`), its **board** (`generic` for
any machine of its processors, such as any x86-64 PC, or the model; `pc`, its name in the builds of 2026-10-30 and 31, is read as
`generic`) and its **processors** (`arch`). The system says what it is in `/usr/lib/peakroute/release`
(`PEAKROUTE_PRODUCT`, `PEAKROUTE_BOARD`, `PEAKROUTE_ARCH`), written by the image build; the Updates page shows the product and board
("Built for"). A bundle or a system from before these fields existed says neither and is the router, generic (all of them were, on
x86-64); a system whose release file does not say the product (a development run) is what the daemon was built as. The check is
made for every bundle, downloaded or uploaded, before anything is written (`Manifest.Compatible`).

**One repository, a release per product.** The router's tags are `v1.2.3`, PeakRoute AP's `ap-v1.2.3`.

**Release files are named for what they are** (2026-10-31): `peakroute-<os|ap>-<arch>-<board>-<version>.<ext>`, where `os` is the
router (PeakRoute OS), `arch` the processors as the manifest names them (`amd64`, `arm64`) and `board` as above. The router today:
`peakroute-os-amd64-generic-v0.10.0.img.gz` and `.update`; PeakRoute AP on the U6+: `peakroute-ap-arm64-ubnt-u6plus-<version>.fit`,
`.rootfs` (and `.update` when it has one). Each has its `.sha256`. A device looks only at its own product's tags and only at the file
for its own processors and board in them (`Identity.assetRank`), so a router never offers an access point's release, an arm64 router
never an amd64 one, and a U6+ never another model's (or a file put in the wrong release by mistake). Releases from before the names
said the processors and board (`peakroute-os-v0.9.8-alpha.update`) are still found by the router on amd64 (only), a file named in full
being preferred where a release has both. Devices from before this look for `peakroute-os-*.update` under tags that are version numbers:
the router's new names start that way, so they keep finding the router's releases, and skip the access point's. `full_build.sh` names a
router build from `v*` tags only. Building and publishing the access point's releases is not done yet (it comes with its image).

**Where releases come from.** One GitHub repository, `owner/name`, **fixed when the software is built** (`UPDATE_REPO`, linker flag
`config.DefaultUpdateRepo`). The administrator cannot point a router at another one, so a router only follows its maker's releases (an
uploaded file is the way to install anything else); a build without it cannot look for updates. The daemon
asks `https://api.github.com/repos/<owner>/<name>/releases`, takes the newest release that is not a draft (and not a pre-release unless
the page says *include pre-releases*) with a `.update` asset and its `.sha256`, and compares its tag with the running version. Nothing is
sent but the request itself. Optionally once a day (a switch), the result only shows as a mark on the Updates entry of the menu and the
Overview; **nothing installs by itself**.

Version order is semantic versioning with the project's tags (`v0.9.5-alpha` < `v0.10.0-alpha` < `v0.10.0`).

## 6. Pages and API

The **Updates** page (System menu): the running version and slot, the other slot's version, whether a trial is open (with Keep / Go
back), *Check for updates* and what was found, *Install* for it, *Upload an update file*, the update settings (include
pre-releases, daily look), progress while one runs, and a short history. The version information (PeakRoute OS, build date, Debian, kernel,
slot) is the first card of that page.

| Method and path | What |
|---|---|
| `GET /api/v1/updates` | the state: versions, slots, trial and its deadline, running job, settings, last check, history |
| `POST /api/v1/updates/check` | ask GitHub now |
| `POST /api/v1/updates/install` | `{"version": "..."}`: download that release into the other slot |
| `POST /api/v1/updates/upload` | the bundle as the request body (streamed); installs into the other slot |
| `POST /api/v1/updates/restart` | restart on trial into the slot that was written |
| `POST /api/v1/updates/keep` | confirm the version on trial |
| `POST /api/v1/updates/revert` | go back: restart into the kept slot (and, on a kept version, try the other slot) |
| `PUT /api/v1/updates/settings` | pre-releases, daily check |

All need an administrator who may write (a read-only account only reads the state). The upload and the download are long, so the
handlers lift the 10-second limits of the server for themselves.

## 7. Building releases

`buildImage.sh` makes the root file system image once and uses it for both outputs: slot A of `peakroute-os.img` and the bundle
`peakroute-os-amd64-generic-<version>.update` (+ `.sha256`). GitHub has no update mechanism of its own: a release is files attached to a tag, and the
device does the rest (§5). `.github/workflows/release.yml` builds both when a tag `v*` is pushed and attaches the bundle, the compressed
disk image (`peakroute-os-amd64-generic-<tag>.img.gz`, for a fresh write; a release file may be at most 2 GiB, which is why it is compressed) and their
checksums. A tag with a hyphen (`v0.11.0-beta`) is a pre-release. To release: bump nothing by hand, just `git tag v0.11.0 && git push
origin v0.11.0`. `UPDATE_REPO=owner/name ./full_build.sh` sets the source compiled into the daemon (the workflow passes the
repository it runs in).

## 7a. Trying it without hardware

`peakroute -dev` simulates the two slots in `dev-data/update-sim` (files, a text `grubenv`, a restart that only re-reads it) and writes a
sample update file and a damaged one there, so the whole page can be tried with `go run . -dev`. For the real thing: build two versions
(`VERSION=v0.10.0-a ./full_build.sh`, then `VERSION=v0.10.0-b ...`), boot the first with `image/run-qemu.sh` and upload the second
`.update` on the Updates page.

## 8. Moving a device that was written before this

The layout changed (one root partition before, now A, B and data). A device written from an earlier image has to be written again once;
its settings can be carried over with Backup and restore. From here on, updates do the work.

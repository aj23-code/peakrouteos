# PeakRoute OS

A router operating system for x86 machines: Debian 13 with one static Go daemon, `peakroute`, that serves the web interface and writes
the configuration of the services under it (nftables, dnsmasq, hostapd, etc.) Status: alpha.

- **Install:** download `peakroute-os-<version>.img.gz` from [Releases](../../releases), `gunzip -k` it and write it to a disk of at least 8 GB
  (`sudo dd if=peakroute-os-<version>.img of=/dev/sdX bs=4M conv=fsync status=progress`). Plug a computer into any port and open the welcome page.
  Everything is set up there; there is no installer and no console setup. Root is locked: the administrator you make is the only login.
- **Update:** System → Updates. A new version is written into the second of two system slots while the router keeps routing, started once
  on trial and kept only when you press Keep (otherwise it goes back after ten minutes). Settings live on their own partition and are not
  touched. Details: [docs/updates.md](docs/updates.md).
- **Build:** `./full_build.sh` makes the image and the update bundle (needs `sudo`, `debootstrap` and friends: [docs/image.md](docs/image.md)).
  `VERSION=` names the build, otherwise it comes from git; the release workflow builds a tagged version.
- **Why the image is about 6.6 GB** for about 1.1 GB of system: two A/B root slots and a data partition; the empty space compresses to
  nothing. See [docs/image.md](docs/image.md).
- **Docs:** [docs/](docs/) has a page for each part; [NOTES.md](NOTES.md) is the engineering notes and [TODO.md](TODO.md) the work list.

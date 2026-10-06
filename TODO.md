# PeakRoute OS: Status and Worklist

Last reviewed: 2026-10-05, at v0.7.0-alpha plus uncommitted Devices page work.
Effort: S = under an hour, M = one session, L = multi-session.

## What's here today

**Image and install**
- Debian 13 (Trixie) minimal live ISO built by `buildISO.sh` (debootstrap in a container, squashfs, GRUB).
- Built-in disk installer, `peakroute-install` (UEFI only).
- Console first-boot wizard (`first_boot.sh`): Router or AP mode, WAN and LAN port selection, LAN IP, DHCP range. Writes `/etc/network/interfaces`, the dnsmasq config, `nftables.conf` and the JSON configs.

**Router base (Router mode)**
- nftables: default-drop input and forward, LAN to WAN allowed, stateful tracking, IPv4 masquerade.
- dnsmasq for DHCP and DNS.
- IPv4 forwarding and sysctl hardening.

**Daemon (`peakroute`, single static Go binary, HTTPS on 443)**
- Web first-run admin setup, login (bcrypt, cookie sessions, rate limiter) and security headers.
- Self-signed TLS cert, generated and auto-renewed at startup.
- Dashboard (read-only): CPU, memory, disk, conntrack, interface counters.
- DHCP lease list with per-client usage.
- Traffic history in pure-Go SQLite, with 1h, 24h and 30d charts.
- systemd journal log viewer.
- Devices page (uncommitted).

**Built but not connected**
- `config.Manager` staged/commit model and `network/generator.go` (writes `/etc/network/interfaces`). Nothing calls `StartProvisioner`.
- `auth.DecodeJSONStrict` and `SyncPhysicalInterfaces` have no callers.

**Not built yet**
- All configuration editing from the UI (no write endpoints).
- 21 of 26 sidebar entries have no page: routing, wireless, firewall and NAT, DNS, VPN, system, diagnostics.

## Tier 0: Security holes (do first)

- [ ] 1. **Authenticate `/api/v1/cert/upload`.** Add a session check and require POST. Validate that the cert and key parse and match (`tls.X509KeyPair`) before writing, and write via temp files and rename. (S) `main.go:152`, `cert.go:139`
- [ ] 2. **Escape DHCP hostnames in `innerHTML`.** Share one `escapeHtml` helper in `app.js`. Fix `leases.js:27` and `telemetry.js:230`, then audit the `traffic.js` templates. (S)
- [ ] 3. **Harden first-run.** Reject empty or short passwords and validate the username. Print the web UI URL and IP at the end of the console wizard. (S) `main.go:369`, `first_boot.sh`
- [ ] 4. **Remove the baked-in root password and tty1 root autologin.** The installer should prompt for a root password. Make sure autologin is removed even if the wizard exits early. (M) `buildISO.sh`

## Tier 1: Structure (unblocks the rest)

- [ ] 5. **`requireAuth` middleware** wrapping every `/api/v1/*` route. Remove the five copy-pasted checks. Add method checks. (S)
- [x] 6. **`.gitignore` and dev-data.** Done 2026-10-05: `.gitignore` covers build output, ISO, runtime state, certs and DBs, and `-dev` mode now keeps all its state in `./dev-data/` instead of the repo root.
- [x] 7. **Repo cleanup.** Done 2026-10-05: history rewritten with `git filter-branch` (ISOs, `build/`, dev state and certs removed). `.git` went from 2.1 GB to 256 KB, fsck is clean, all 9 tags kept (hashes changed). ISOs are built manually with `full_build.sh` and go in GitHub Releases, not in git. Pre-rewrite backup: `/media/alec/DATA/peakroute_os_backup_2026-10-05` (delete once things are verified).
- [ ] 8. **One source of truth for config.** Go through `config.Manager` for all reads and writes. Stop re-reading JSON per request in handlers and stats. (M)
- [ ] 9. **Wire up staged and commit.** Call `StartProvisioner`, reload networking after writing, and add commit and discard endpoints. Remove or use the dead code. Reconcile `generator.go` with `first_boot.sh` on when `br0` is created. (L)

## Tier 2: Correctness (duplicate calls, bad data)

- [ ] 10. **Split sampling from reading.** Only the 1s poller updates `lastIfaceStats` and calls `RecordSample`. `GetSystemStats` reads the poller's latest snapshot. Move CPU sampling into the poller to drop the 100ms sleep per request. (M) `system_stats.go:88-147`, `:373`
- [ ] 11. **Traffic DB hygiene.** Record only WAN, LAN and bridge interfaces. Run retention at startup as well as daily. Create the DB with `auto_vacuum=INCREMENTAL`. (S)
- [ ] 12. **Fix `writeAtomic`.** Unique temp file, fsync the file, rename, fsync the directory. (S) `config.go:342`
- [x] 12b. **Build on Ubuntu without containers.** Done 2026-10-05: `buildISO.sh` now runs `debootstrap` natively (no docker/podman) and fetches a checksum-pinned Debian keyring, because Ubuntu 24.04's keyring cannot verify Trixie. `BUILD_DIR` can be overridden. Not yet tested end-to-end (needs `sudo apt install debootstrap xorriso mtools` first).
- [x] 13. **Fix host-side commands in `buildISO.sh`.** Done 2026-10-05: removed the stray `rm -rf /etc/systemd/system/getty@tty1.service.d/` and `systemctl daemon-reload`, which ran as root against the build host. The image keeps its autologin override for the live ISO, and `first_boot.sh` removes it on the installed system.
- [ ] 14. **First-boot script fixes.**
  - Use `CURRENT_HOSTNAME`, not `$HOSTNAME`.
  - Range-check IPv4 octets and validate the DHCP range against the subnet.
  - Add `isc-dhcp-client` or switch to `udhcpc`, and stop hiding failures behind `|| true`.
  - Fill in `network.json` properly for AP mode.
  - Use consistent mode casing (`router` vs `ROUTER`). (M)
- [ ] 15. **Installer safety.** Exclude the live USB from the disk list. Show size and model, and require typing the device name to confirm. (S)

## Tier 3: Auth and session hardening

- [ ] 16. Constant-time login: always run bcrypt, with a dummy hash when the user is not found. (S)
- [ ] 17. Remove `SanitizeString` from usernames. (S)
- [ ] 18. Use `SessionTimeoutMinutes` from config. Add an expired-session janitor. (S)
- [ ] 19. Make logout POST-only and add CSRF tokens (before write endpoints ship). (M)
- [ ] 20. Drop `'unsafe-inline'` from the CSP, which means moving the `onclick=` handlers into JS. (M)
- [ ] 21. Run the daemon as a dedicated user with `AmbientCapabilities=`, plus a small privileged helper. Do this after #9 shows which privileged operations are needed. (L)

## Tier 4: Polish

- [ ] 22. Replace the `AF_PACKET` per-device capture with nftables per-client counters or conntrack accounting. Cover every LAN port or the bridge. Persist totals across restarts. (L)
- [ ] 23. Cert: add SANs (hostname and LAN IP), fix the "Local CA" naming, renew while running. (S)
- [ ] 24. Use `MemAvailable` for memory. Pick the right CPU thermal zone. (S)
- [ ] 25. Make sure journald logs persist across reboots. (S)
- [ ] 26. Hide or disable sidebar entries that have no pages yet. (S)
- [ ] 27. Add tests (generator, config manager, limiter, session expiry) and CI running `go vet` and `go test`. (M)

## Suggested batches

1. **Batch 1 (about a day):** 1, 2, 3, 5, 6.
2. **Batch 2:** 8, 10, 11, 12.
3. **Batch 3, build and image:** 4, 13, 14, 15. Then 7 as a single sitting at the end.
4. **Project on its own:** 9.

## Longer-term gaps (needs design decisions first)

IPv6 (NAT and ICMPv6 input rules), time sync (NTP), an update mechanism for the OS and daemon, a watchdog, a recovery path (SSH or console tool), Secure Boot and BIOS boot, and the missing UI pages above.

## Open decisions

- History rewrite or fresh repo (#7)?
- Which privileged operations will the UI need (#21)?

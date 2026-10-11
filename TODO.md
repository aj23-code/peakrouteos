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

- [x] 1. **Authenticate `/api/v1/cert/upload`.** Done 2026-10-05: behind `requireAuth`, POST-only, validates that the cert and key parse, match and are unexpired, and installs both via temp files and rename. (Expired-cert rejection not exercised in testing.)
- [x] 2. **Escape DHCP hostnames in `innerHTML`.** Done 2026-10-05: one shared `escapeHtml` in `app.js` is used by the leases, top-clients, interface-table, traffic-card and devices renderers. The inline `onclick` in the devices table was replaced with a data attribute and a delegated listener. Verified in a browser: a lease named `<img/src="x"/onerror=...>` ran script before the fix and renders as plain text after.
- [x] 3. **Harden first-run.** Done 2026-10-05 for the web setup page: username must be 3-32 chars (letters, numbers, `.`, `_`, `-`, starting with a letter or number), password 8-72 bytes, and a confirm-password field must match. Enforced on the server (400 with the form re-shown) and mirrored client-side. First unit tests added (`internal/auth/validate_test.go`). Still to do: print the web UI URL/IP at the end of the console wizard (deferred with the console wording pass).
- [ ] 4. **Remove the baked-in root password and tty1 root autologin.** The installer should prompt for a root password. Make sure autologin is removed even if the wizard exits early. (M) `buildISO.sh`

## Tier 1: Structure (unblocks the rest)

- [x] 5. **`requireAuth` middleware** (done 2026-10-05; method checks still to do) wrapping every `/api/v1/*` route. Remove the five copy-pasted checks. Add method checks. (S)
- [x] 6. **`.gitignore` and dev-data.** Done 2026-10-05: `.gitignore` covers build output, ISO, runtime state, certs and DBs, and `-dev` mode now keeps all its state in `./dev-data/` instead of the repo root.
- [x] 7. **Repo cleanup.** Done 2026-10-05: history rewritten with `git filter-branch` (ISOs, `build/`, dev state and certs removed). `.git` went from 2.1 GB to 256 KB, fsck is clean, all 9 tags kept (hashes changed). ISOs are built manually with `full_build.sh` and go in GitHub Releases, not in git. Pre-rewrite backup: `/media/alec/DATA/peakroute_os_backup_2026-10-05` (delete once things are verified).
- [x] 8. One source of truth for config. Done 2026-10-06: `config.Manager` is the only reader/writer. Login, the dashboard, stats and the traffic tracker read from memory (verified with `strace`: zero config-file opens across 25 requests), first-run setup goes through the atomic `Manager.SetupAdmin` (10 concurrent setups produce exactly one account), build metadata is written to `sys.json` only when it changed, and the tracker reads the LAN interface from the manager instead of a hardcoded `/etc` path. Also fixed in the Manager: staged/live slice aliasing, memory changing before a failed save, and listeners receiving live pointers. `Manager` accessors return copies; mutators work on a copy, save, then commit to memory.
- [ ] 9. Wire up staged and commit (the translator pipeline). Design agreed 2026-10-06: edits go to `network.json.staged`; a banner appears in every view while changes are uncommitted; the Staged Changes page lists a field-by-field diff plus a plan (which files change, which services restart); commit validates, writes `network.json`, then a translator renders the system files from the JSON and restarts only the services whose rendered file changed; a 60-second commit-confirm reverts automatically unless confirmed from the new address. First editor page: LAN settings (IP, netmask, DHCP range). Note: the change channels are lossy (buffer of 10, a send is dropped when full), so the applier must not depend on them alone; apply synchronously under commit and keep a version/status. Steps:
  - [x] 9a. Model, validation and diff (pure functions): `DHCPConfig` (nil = not managed by the JSON yet), `NetworkConfig.Validate()` (strict, per-field errors; interface names and addresses must be safe to put in root-owned config files), `DiffNetwork`, `Manager.StagedChanges`. Done 2026-10-06.
  - [x] 9b. Translator, render and plan (`internal/translator`). Done 2026-10-06: `Render` produces the interfaces, nftables, sysctl and dnsmasq files from the JSON (router mode; static WAN and upstream DNS supported; AP mode returns `ErrUnsupportedMode`), re-validating and re-checking every value before it goes near a template; `PlanChanges` compares with what is on disk and returns the files that differ plus the service actions in order (sysctl, networking, nftables, dnsmasq; restarting networking also restarts a managed dnsmasq). Output is byte-for-byte what `first_boot.sh` writes (the test reads the heredocs out of the script), so a wizard-configured box has an empty plan on its first commit. DHCP not managed by the JSON (nil) leaves the existing dnsmasq file alone and adds a note. The old unused `internal/network/generator.go` was removed. Also done: stage files now disappear when an edit is reverted, and a no-op stage file is cleaned up at startup.
  - [x] 9c. Apply, status and endpoints. Done 2026-10-06: CSRF (#19), router defaults and DHCP import, the apply engine (`translator.Applier`, pre-checks, rollback), the commit/confirm/revert service (`internal/pipeline`, snapshot revert, persisted 60 s deadline, `Resume`) and the endpoints `GET /api/v1/config/{status,staged}`, `POST /api/v1/config/{commit,confirm,revert,discard}`, wired into `main.go` (real `ExecRunner` on the appliance; `LogRunner` plus a sandbox root in `-dev`). Still to do from the original 9c note: update `first_boot.sh` to write the DHCP section into `network.json` (image phase, deferred; the daemon already imports an existing range).
  - [x] 9d. UI: the banner. Done 2026-10-06: a sticky bar above every view (`config_banner.js`) showing uncommitted changes, applying, the 60 s confirm countdown with Confirm / Revert now, lost-contact guidance and outcomes, plus a native review dialog (diff, files, services, validation errors, confirm warning with the new address). Verified in a real browser (all states, pinning on scroll, phone width, hostile text). Still open: a dedicated Staged Changes *page* (the nav entry `sys-staged` is still a placeholder) once the sidebar layout is decided, and a responsive layout for the rest of the dashboard.
  - [x] 9e. First editor pages: Ports (assign each port WAN, LAN or unassigned) and Networks (LAN address, netmask and DHCP). Done 2026-10-08: backend in `internal/netapi` (`GET/PUT /api/v1/ports`, `GET /api/v1/networks`, `GET/PUT /api/v1/networks/{id}`, staging only, validated per page, refused while a change is applying) and the two pages (`ports.js`, `networks.js`) replacing the old read-only Devices table. Checked in a browser at desktop and phone width, through save, review, commit and confirm.
  - [x] 9f. Internet page and WAN health. Done 2026-10-08: `GET /api/v1/uplinks`, `GET/PUT /api/v1/uplinks/{id}` (DHCP or static address, gateway, DNS; staging only) and the Internet page (`internet.js`): connection settings plus a latency chart (5m, 1h, 24h, 30d) from `internal/wanhealth`, which pings the upstream DNS server every 5 s and keeps per-minute history in `wan_health.db`. Checked in a browser (desktop and phone width) with synthetic 30-day history and a flaky fake link; real ICMP checked in a user and network namespace. Follow-ups the same day: a gateway series with a where-it-breaks diagnosis; uptime (24 h, 30 d) and an outage list; WAN usage (rate now, volume over 24 h and 30 d).
  - [x] 9g. Routes page. Done 2026-10-08: static IPv4 routes (`routes` in `network.json`, validated, diffed in the review, rendered by the translator as an `if-up.d` script that tags its routes with `proto 77`), `GET/PUT /api/v1/routes` with the live routing table, and the Routes page (`routes.js`). A route change restarts nothing, a route the kernel refuses fails the commit and rolls back. Checked in a browser at desktop and phone width through save, review, commit and confirm.
  - [x] 9h. Several local networks and VLANs. Done 2026-10-08: extra networks (VLAN, address, DHCP, internet and isolation switches) and per-port native/tagged membership in `network.json`; one bridge per network built with the new `bridge-utils` and `vlan` packages (**added to `buildISO.sh`, so the ISO needs a rebuild**); per-network firewall rules (a new network has no route to the internet until it is allowed, and may talk to other networks only if neither is isolated); `POST/PUT/DELETE /api/v1/networks` and native/tagged on `/api/v1/ports`; the Local Networks, Ports and Overview pages. Checked with the real `ifupdown`, `bridge-utils` and `vlan` in a network namespace and with `nft -c` and `dnsmasq --test`. **To check on a box with several ports:** a tagged VLAN end to end through a managed switch, and DHCP from each network.
  - [x] 9i. Wireless page (detection and capabilities). Done 2026-10-08: `internal/wireless` (adapters from sysfs, capabilities parsed from `iw`), `GET /api/v1/wireless`, the Wireless page (greyed out with a message when no adapter is found), and the wireless packages and firmware added to `buildISO.sh` (**ISO rebuild needed**). 2026-10-10: Wi-Fi adapters no longer appear on the Ports page. 2026-10-10: Wi-Fi networks (an SSID per Local Network, band steering) with hostapd and the country setting: config model, translator and service, and the API are done (UI next). Still to do: radio modes (access point, or station for a point-to-point link or wireless uplink, which needs `wpasupplicant`), and AP mode (limited GUI). See NOTES 7g.

## Tier 2: Correctness (duplicate calls, bad data)

- [x] 10. **Split sampling from reading.** Done 2026-10-06: `PollSystemSample` (1s poller) is the only code that samples `/proc`, records history and computes CPU %; `GetSystemStats` just reads the latest snapshot. Stats requests dropped from ~121 ms to ~20 ms, CPU % now covers a 1s window instead of 100 ms, the poller no longer re-reads `network.json` every second, and a first reading no longer records a bogus 0 rate. Regression tests in `internal/system/system_stats_test.go` (verified to fail if the old behaviour returns).
- [x] 11. **Traffic DB hygiene.** Done 2026-10-06: history is recorded for physical interfaces only (what the UI charts), flushed every 60 s instead of 10 s (the buffer is also flushed on shutdown), retention runs at startup as well as daily, and the DB uses incremental auto_vacuum (existing DBs are converted once on startup). Measured idle writes to storage: 118 MB/day before, 59 MB/day after.
- [x] 11b. **Aggregate to per-minute rows before writing.** Done 2026-10-06: samples are averaged per interface per minute in RAM (`traffic_aggregate.go`) and one row per minute is persisted (the in-progress minute is rewritten each flush so charts lag by at most ~60 s). Charts are unchanged: 1h/24h/30d use 60 s-or-coarser buckets, and a test proves identical `GetHistory` output for per-second vs per-minute rows. About 60x fewer rows; idle writes now ~32 MB/day (118 originally, 59 after #11). Existing databases keep their old 1 s rows until the 30-day purge removes them (a slight skew in the one boundary bucket until then). Further reduction would mean a longer flush interval, at the cost of losing more history on power loss.
- [x] 12. **Fix `writeAtomic`.** Done 2026-10-06: new `internal/fsutil` (`WriteFileAtomic`, plus `StageFile`/`CommitFile` for swapping several files together) writes a uniquely named temp file, fsyncs it, renames it, then fsyncs the directory, and cleans up on failure (verified with `strace`). Now used by all config saves, the interfaces generator, the TLS cert upload and the `configured` flag. Not yet done: sweeping stale `.*.tmp` files left by a crash mid-write, and the self-signed cert generation (`GenerateSelfSignedCert`) still writes non-atomically (see #23).
- [x] 12b. **Build on Ubuntu without containers.** Done 2026-10-05: `buildISO.sh` now runs `debootstrap` natively (no docker/podman) and fetches a checksum-pinned Debian keyring, because Ubuntu 24.04's keyring cannot verify Trixie. `BUILD_DIR` can be overridden. Not yet tested end-to-end (needs `sudo apt install debootstrap xorriso mtools` first).
- [x] 12c. **Auto-start the installer on the live ISO.** Done 2026-10-05: new `peakroute-installer.service` runs `peakroute-install` on tty1 only when `/run/live` exists (before the autologin shell). Ctrl+C drops to a shell. The installer removes the unit from the installed system. Needs a rebuild and a QEMU boot test.
- [x] 13. **Fix host-side commands in `buildISO.sh`.** Done 2026-10-05: removed the stray `rm -rf /etc/systemd/system/getty@tty1.service.d/` and `systemctl daemon-reload`, which ran as root against the build host. The image keeps its autologin override for the live ISO, and `first_boot.sh` removes it on the installed system.
- [ ] 14. **First-boot script fixes.**
  - Use `CURRENT_HOSTNAME`, not `$HOSTNAME`.
  - Range-check IPv4 octets and validate the DHCP range against the subnet.
  - Add `isc-dhcp-client` or switch to `udhcpc`, and stop hiding failures behind `|| true`.
  - Fill in `network.json` properly for AP mode.
  - Use consistent mode casing (`router` vs `ROUTER`). (M)
- [ ] 15. **Installer safety.** Exclude the live USB from the disk list. Show size and model, and require typing the device name to confirm. (S)

## Tier 3: Auth and session hardening

- [x] 16. Constant-time login. Done 2026-10-06: `auth.Authenticate` always performs exactly one bcrypt comparison (against a same-cost dummy hash when the user doesn't exist) and visits every user without stopping early. Measured on a live server: unknown username 3.5 ms vs wrong password 65 ms before (a 62 ms leak), 66 ms vs 67 ms after.
- [x] 17. Remove `SanitizeString` from usernames. Done 2026-10-06: deleted the function (it protected nothing and could silently alter a valid name); login now only trims surrounding whitespace.
- [x] 18. Session timeout from config plus an expired-session janitor. Done 2026-10-06: the idle timeout comes from `session_timeout_minutes` in `auth.json` (default 15, clamped to 1 minute..24 h, via `Manager.SessionTimeout`), and a janitor drops timed-out sessions every minute. Also fixed a latent bug: the session cookie had a fixed `Expires`, so browsers deleted it 15 minutes after login however active you were; it is now a browser-session cookie and the sliding timeout is enforced server-side. There is no UI/API to change the timeout yet (it needs a write endpoint, see #9).
- [x] 19. CSRF tokens and POST-only logout. Done 2026-10-06: every session has its own random CSRF token (independent of the cookie), embedded in the dashboard as `<meta name="csrf-token">`; `auth.RequireAuth` requires it in the `X-CSRF-Token` header on every state-changing `/api/v1` request (403 otherwise, 401 without a session); `apiFetch` attaches it automatically (use `apiFetch`, not bare `fetch`, for POST/PUT/DELETE); logout is a POST form with a hidden token and GET returns 405. Verified in a real browser: apiFetch POST accepted, bare fetch POST 403.
- [ ] 20. Drop `'unsafe-inline'` from the CSP, which means moving the `onclick=` handlers into JS. (M)
- [ ] 21. Run the daemon as a dedicated user with `AmbientCapabilities=`, plus a small privileged helper. Do this after #9 shows which privileged operations are needed. (L)

## Tier 4: Polish

- [ ] 22. Replace the `AF_PACKET` per-device capture with nftables per-client counters or conntrack accounting. Cover every LAN port or the bridge. Persist totals across restarts. (L)
- [ ] 23. Cert and file hygiene: add SANs (hostname and LAN IP), fix the "Local CA" naming, renew while running. Also from #12: write the self-signed cert and key atomically with `fsutil` (`GenerateSelfSignedCert` still writes non-atomically), and sweep stale `.*.tmp` files that a crash mid-write can leave in `/etc/peakroute` and the cert directory (remove them at startup, e.g. anything matching `.*.tmp` older than a minute). (S)
- [ ] 24. Use `MemAvailable` for memory. Fix the CPU temperature: `getCPUTemperature` reads `thermal_zone0`, which on many machines is `acpitz`, a constant placeholder (it reads a fixed 25 C on the Dell dev laptop). The real CPU sensor there is the `x86_pkg_temp` thermal zone or hwmon `coretemp` ("Package id 0"). Pick by zone `type` (`x86_pkg_temp`, `coretemp`, `k10temp`, `cpu_thermal`, `soc_thermal`) and fall back to the hottest plausible zone; report "unavailable" instead of 0 when there is no sensor. (S)
- [ ] 25. Make sure journald logs persist across reboots. (S)
- [x] 26. Sidebar: no dead menu entries, and a responsive layout. Done 2026-10-08: the menu was reorganized (OPNsense-style groups, two levels, quick search, friendly names, planned pages greyed out; NOTES.md 7b) and the whole dashboard is now one responsive site (desktop sidebar; on tablets and phones a top bar with a slide-out menu; NOTES.md 7c). The planned pages themselves are still to be built.
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

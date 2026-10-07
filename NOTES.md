# PeakRoute OS: Working Notes

Living notes for the eventual documentation. Updated with every meaningful change, so it
records what the system does, why it is built that way, and what was learned on the way.
`TODO.md` is the work list; this file is the knowledge. Last updated: 2026-10-06.

> Status: alpha. A router appliance OS (Debian base) with a Go daemon and web UI. The daemon is
> a monitoring dashboard plus a safe configuration pipeline that is being built (see section 7).

## 1. What it is

- **Image:** a Debian 13 (Trixie) minimal live ISO built by `buildISO.sh` (native `debootstrap` on
  Ubuntu/Debian, checksum-pinned Debian keyring, squashfs, GRUB). A built-in installer
  (`peakroute-install`) starts automatically on the live ISO and installs to disk (UEFI only).
- **First boot:** `internal/scripts/first_boot.sh` is a console wizard (router or AP mode, WAN and
  LAN ports, LAN IP, DHCP range) that writes `/etc/network/interfaces`, the dnsmasq DHCP file,
  `/etc/nftables.conf`, the forwarding sysctl, and `network.json` / `sys.json`.
  The installer and wizard are going to be revisited; the daemon does not depend on their wording.
- **Daemon:** one static Go binary (`peakroute`) serving the web UI over HTTPS (443; 8443 in dev),
  with embedded templates and static files.

## 2. Repository layout

| Path | Purpose |
|---|---|
| `main.go` | wiring: flags, config manager, routes, handlers for setup/login/logout |
| `internal/config` | the configuration model, `Manager` (single owner of config), validation, diff |
| `internal/translator` | renders system files from `network.json`, plans what must change |
| `internal/auth` | sessions, CSRF, login (`Authenticate`), validators, rate limiter, security headers |
| `internal/fsutil` | crash-safe atomic file writes |
| `internal/system` | stats (CPU, memory, interfaces), traffic history DB, journal logs |
| `internal/network` | physical interface discovery, DHCP leases, per-device traffic tracker |
| `internal/cert` | self-signed TLS cert generation and certificate upload |
| `web/` | HTML templates, CSS, JS (no framework; one module per view) |
| `buildISO.sh`, `full_build.sh` | image build (`full_build.sh` builds the binary then the ISO) |

## 3. Running it

- **Development:** `go run . -dev` (or build, then `./peakroute -dev`). Serves HTTPS on `:8443` with a
  self-signed certificate (browsers warn). All state goes in `./dev-data/` (gitignored). The first
  visit shows the first-run setup page.
- **Production:** state lives in `/etc/peakroute/` (`auth.json`, `network.json`, `sys.json`,
  `configured`, `traffic_history.db`, `certs/`). The systemd unit runs the daemon as root today
  (running unprivileged is TODO #21).
- **Dev mode must never change the machine it runs on.** The apply step (section 7) writes into a
  sandbox folder and only logs the commands it would run.
- Tests: `go test -race ./...`. Linux only (reads `/proc` and `/sys`).

## 4. Configuration (`internal/config`)

- **Files** (in the config directory): `auth.json` (users, session timeout; mode 0600),
  `network.json` (active network config), `network.json.staged` (pending candidate),
  `sys.json` (hostname, operation mode, build info), `configured` (flag: first-run setup is done).
- **`Manager` is the single owner.** State is loaded once at startup and then read from memory.
  Reads return copies (so a caller cannot mutate live config). Every change works on a copy,
  saves to disk, and only then updates memory, so a failed save never leaves memory ahead of disk.
  Change notifications carry private copies. The notification channels are lossy (buffer of 10),
  so nothing may rely on them alone.
- **First-run setup** is `Manager.SetupAdmin`: creates the admin and the `configured` flag atomically
  under the lock (flag written last and durably). Only one of several concurrent requests wins.
- **Build metadata** (`peakroute_build`, `arch`, `kernel`) is written to `sys.json` only when it changed.
- **Staged config:** `StageNetwork` edits a copy of the active (or already staged) config. If the
  result equals the active config the stage file is removed, so the pending state always agrees
  with the diff. A matching leftover stage file is dropped at startup.
- **Validation** (`NetworkConfig.Validate`) is strict and returns per-field errors. Values end up in
  root-owned config files, so: interface names are `[A-Za-z0-9][A-Za-z0-9._-]{0,14}`, addresses are
  plain IPv4, netmasks are contiguous /8 to /30, host addresses must be inside their subnet and not
  the network/broadcast address, LAN and WAN subnets must not overlap, the DHCP range must be inside
  the LAN and exclude the router's own address, lease times are at least 2 minutes (dnsmasq minimum).
- **DHCP section:** `network.json` may hold a `dhcp` object. `nil` means "not managed by the JSON yet"
  (installs configured by the wizard), which is different from `enabled: false`.
- **Diff:** `DiffNetwork` lists user-visible differences in a stable order. The detected hardware list
  (`interfaces`) is deliberately excluded: plugging in a NIC is not a pending change.
- **Atomic writes** (`internal/fsutil`): unique temp file in the target directory, fsync, rename, fsync
  the directory; the temp file is removed on failure. Used for every config save, the interfaces file,
  the cert upload and the `configured` flag.

## 5. Security model

- **Login:** bcrypt; `auth.Authenticate` always performs exactly one bcrypt comparison (a same-cost
  dummy hash for unknown users), so response time does not reveal which usernames exist.
  Login is rate-limited to 5 attempts per IP per 2 minutes.
- **Sessions:** random 128-bit token in an HttpOnly, Secure, SameSite=Strict cookie. It is a
  browser-session cookie (no fixed expiry); the idle timeout is enforced server-side and slides with
  activity (`session_timeout_minutes` in `auth.json`, default 15, clamped 1 minute to 24 hours).
  A janitor drops expired sessions every minute. Sessions live in memory (a daemon restart logs
  everyone out).
- **CSRF:** each session has a separate random token. State-changing API requests must send it in the
  `X-CSRF-Token` header (the page reads it from `<meta name="csrf-token">`; `apiFetch` adds it).
  Logout is a POST form with a hidden token. Missing/invalid token gives 403.
- **API auth:** every `/api/v1/*` route except `login` and `setup/apply` goes through
  `auth.RequireAuth` (401 without a session, 403 for state-changing requests without the token).
- **Output escaping:** values rendered with `innerHTML` go through the shared `escapeHtml` in `app.js`.
  (DHCP hostnames come from LAN clients; dnsmasq normally filters them, but the UI does not rely on it.)
- **First-run setup page:** username 3-32 characters, password 8-72 bytes with confirmation, enforced
  on the server.
- **TLS:** self-signed certificate generated at startup (no SANs yet, see TODO #23). A custom
  certificate can be uploaded (authenticated, validated as a matching unexpired pair, installed atomically).

## 6. Statistics and traffic history

- **One poller samples `/proc` once a second** (`system.PollSystemSample`): interface counters and
  rates, CPU %. Request handlers only read its latest snapshot. (Earlier, every dashboard refresh also
  recorded a history sample and slept 100 ms; fixed.)
- **History DB** (`system/traffic_graphDB.go`, SQLite via the pure-Go `modernc.org/sqlite`):
  physical interfaces only (what the charts show); samples are averaged per interface per minute in RAM
  (`traffic_aggregate.go`) and written once a minute (the in-progress minute is rewritten each flush);
  30-day retention, purged at startup and daily; incremental auto-vacuum (legacy DBs are converted once).
  The charts only use 60-second or coarser buckets, so per-minute rows give identical charts
  (a test proves it).
- **Disk wear (idle daemon, measured with `/proc/<pid>/io`):** 118 MB/day originally, 59 after
  physical-only plus 60 s flush, 32 MB/day with per-minute rows. SQLite writes whole 4 KB pages per
  flush, so row count is not the only lever. Reading `/proc`, `/sys` or config JSON does not wear
  flash (virtual files / page cache); writes do.
- **Per-device usage** (`network/per_devicetraffic.go`) captures packets in userspace on the first LAN
  interface. It will not scale to gigabit and misses extra LAN ports (TODO #22).

## 7. The staged/commit pipeline (TODO #9, in progress)

Agreed design (2026-10-06):

1. Edits go to `network.json.staged`; a banner shows in every view while there are real uncommitted
   changes (keyed off the diff, not the existence of a file).
2. The Staged Changes page lists a field-by-field diff plus a plan: which files change and which
   services restart.
3. Commit validates the staged config, writes `network.json`, then the **translator** renders the system
   files from the JSON and restarts only the services whose file changed.
4. A **60-second commit-confirm** reverts automatically unless confirmed (needed for changes that
   can lock the admin out: networking or firewall). Confirming after an IP change means logging in
   again at the new address (cookies are per host).

Built so far:

- **9a** `Validate`, `DHCPConfig`, `DiffNetwork`, `Manager.StagedChanges`.
- **9b** `internal/translator`:
  - `Render(cfg, mode)` produces `/etc/network/interfaces`, `/etc/nftables.conf`,
    `/etc/sysctl.d/99-peakroute-forwarding.conf`, `/etc/dnsmasq.d/peakroute-dhcp.conf`, and (static WAN
    with its own DNS) `/etc/dnsmasq.d/peakroute-upstream.conf`. Router mode only (AP returns
    `ErrUnsupportedMode`). It re-validates and then re-checks every interpolated value is a plain token.
  - **The output matches `first_boot.sh` byte for byte** (including its blank line for a single LAN
    port). The tests read the wizard's heredocs out of the script, so they cannot drift apart, and a
    wizard-configured box has an empty plan on its first commit.
  - The LAN lives on `br0` only when there are two or more LAN ports (same as the wizard).
  - `PlanChanges` compares rendered files with disk (contents only) and orders actions:
    sysctl apply, networking restart, nftables reload, dnsmasq restart. Restarting networking also
    restarts a managed dnsmasq (it drops the LAN address dnsmasq is bound to).
  - A `nil` DHCP section leaves the existing dnsmasq file alone and adds a note to the plan.

Decisions for the remaining steps (9c to 9e):

- **Default LAN in router mode: 192.168.1.1/24**, DHCP enabled `.100` to `.200`, lease 12h. Applied by
  the daemon only where the JSON leaves values blank. An existing dnsmasq range (wizard installs) is
  imported instead of overwritten. The installer/wizard will be revisited later.
- **Applying** runs after the HTTP response (networking restarts drop connections), through an
  injectable command runner and filesystem so tests need no root. Config files are written
  atomically, checked (`nft -c`, `dnsmasq --test`) before any service is touched, and rolled back if a
  check or a service action fails.
- **Dev mode** uses a sandbox root plus a logging runner and never runs real commands.
- **Revert survives a daemon restart:** the pending confirm (previous config + deadline) is persisted.
- First editor page: LAN settings (LAN IP/netmask and DHCP range, lease, on/off). DHCP-only is the
  safer first slice (a dnsmasq restart cannot lock you out); to be confirmed.

## 8. HTTP endpoints (current)

| Method and path | Auth | Notes |
|---|---|---|
| `GET /first_run`, `POST /api/v1/setup/apply` | none, only while unconfigured | creates the admin |
| `GET /login`, `POST /api/v1/login` | none | rate limited |
| `POST /logout` | session + CSRF token (form field) | GET returns 405 |
| `GET /`, `/dashboard` | session | server-rendered shell; embeds the CSRF token |
| `GET /api/v1/system/stats` | session | from the poller snapshot, no disk reads |
| `GET /api/v1/system/traffic?iface=&range=` | session | history; ranges `1h` (also used for 5m), `24h`, `30d` |
| `GET /api/v1/system/logs?limit=` | session | systemd journal |
| `GET /api/v1/system/devices` | session | physical NICs |
| `GET /api/v1/dhcp/leases` | session | dnsmasq leases with per-client usage |
| `POST /api/v1/cert/upload` | session + `X-CSRF-Token` | multipart `cert` and `key` |

## 9. Decisions log

| Date | Decision | Why |
|---|---|---|
| 2026-10-05 | Rewrote git history to drop ISOs, `build/`, dev state and certs | `.git` was 2.1 GB with a corrupt blob; no remote existed. ISOs go to GitHub Releases |
| 2026-10-05 | Build natively with debootstrap on Ubuntu, pinned Debian keyring 2025.1 | Ubuntu's keyring cannot verify Trixie; the container is no longer needed |
| 2026-10-05 | Live ISO auto-starts the interactive installer (still asks before erasing) | convenience; Ctrl+C drops to a shell |
| 2026-10-05 | Software first, image/installer work last | user priority; image work is costly to test |
| 2026-10-06 | One owner for config (`Manager`), memory as the read path | consistency, fewer failure modes, fewer reads |
| 2026-10-06 | Per-minute history rows, 60 s flush, physical NICs only | cut flash writes without changing any chart |
| 2026-10-06 | Staged changes keyed off the diff, stage file removed when reverted | the banner must reflect real changes |
| 2026-10-06 | Translator must match the wizard byte for byte | first commit on an existing box must change nothing |
| 2026-10-06 | 60-second commit-confirm with auto-revert | protection against locking out the admin |
| 2026-10-06 | Router default LAN 192.168.1.1/24 | installer setup will be revisited later |

## 10. How things are tested (worth documenting)

- **Mutation checking.** For each new behaviour the code is deliberately broken in a scratch copy and a
  test must fail. Several tests were strengthened when a mutant survived.
- **Golden tests against the wizard script** (translator) and against the old behaviour (chart
  equivalence for per-minute rows).
- **Live checks:** `strace` to prove no config files are opened per request, `/proc/<pid>/io` for disk
  writes, timing comparisons of old and new builds (login timing, stats latency), concurrent setup race.
- Time is injectable (`SessionManager.now`) and filesystems/command runners are interfaces, so logic is
  tested without root or real time.

## 11. Gotchas learned

- The project drive is ext4 mounted `nosuid,nodev`; `debootstrap` normally copes; `BUILD_DIR` can move
  the build elsewhere.
- Browsers refuse the self-signed cert in the in-app preview; use a normal browser at `https://localhost:8443`.
- `gofmt` complains about some older indentation in untouched files; leave it unless editing the file.
- Interface names are not stable across hardware: the CPU temperature code must not assume
  `thermal_zone0` (on the dev laptop it is a fixed 25 C `acpitz` placeholder; TODO #24).
- Session cookies are per host, so after the LAN IP changes the admin must log in again at the new address.

## 12. Documentation to write later

- Install and first boot (wizard, ISO, installer)
- Web UI tour and admin tasks (staging, review, commit, confirm, revert)
- Configuration reference (`network.json`, `auth.json`, `sys.json`, validation rules and errors)
- Security overview (auth, CSRF, session timeout, TLS)
- API reference
- Troubleshooting (lockout recovery, logs, reverting by hand)
- Architecture and contributor guide (this file is the seed)

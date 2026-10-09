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

## 1b. Scope and direction

- **Now: standalone mode.** One PeakRoute box, administered through its own web UI. Anything controller-related
  (an SDN controller that adopts and manages many boxes, machine API tokens, device adoption, fleet config
  push) is explicitly **out of scope** until much later. Do not build it, and do not add TODO items for it.
- **Lean toward it where it costs nothing.** Rules of thumb that keep a future controller simple:
  config stays a set of declarative JSON documents (`network.json`, ...) that the device reconciles to;
  the user-facing model is made of objects (a **Network** with its address range and DHCP together, rather
  than separate "LAN" and "DHCP" screens); operations stay API-shaped (review, commit, confirm, revert are
  plain endpoints, not UI-only logic); and browser-only concerns (cookies, CSRF) stay at the HTTP edge so
  another way to authenticate can be added later without touching the pipeline.

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
  `configured`, `traffic_history.db`, `wan_health.db`, `certs/`). The systemd unit runs the daemon as root today
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

- **9c, `internal/pipeline.Service`** (built, with the endpoints in section 8) runs the workflow:
  - `Review()`: changes, per-field validation errors, the plan, notes, whether confirmation is needed.
  - `Commit()`: refuses if busy or nothing is staged, validates, renders and plans, makes the staged config
    active (`network.json`), then applies **in the background** (so the HTTP response is sent before
    networking restarts). A commit whose plan matches the system just saves.
  - **Confirmation is only demanded when the plan touches networking or nftables** (the changes that can lock
    the admin out). A DHCP-only change applies and is done. For the others the service waits in
    `awaiting_confirm`, saves `confirm.json`, and arms a 60 s timer.
  - `Confirm()` stops the countdown and removes the record (and refuses, leaving the countdown running, if the
    record cannot be removed, since a restart would otherwise revert a change the user accepted).
  - Deadline or `Revert()`: restores the **snapshot of the exact previous files** (not a re-render of the
    previous JSON), restores the previous `network.json`, puts the rejected edit back as the pending
    candidate, and removes the record only after the revert was attempted.
  - **Failure paths:** a failed apply rolls back files and services, restores the previous config and keeps the
    edit pending; if the safety net cannot be armed (the record cannot be saved) the change is undone at once;
    a timer from an earlier window can never revert a later one (ids).
  - `Resume()` at startup: an expired window reverts immediately, a live one continues for the time left.
    `Close()` stops timers but leaves the record for the next start.
  - `Status()` is what the banner polls: `has_changes` follows real differences, never a file's existence.
  - Time and timers are injectable, so the 60 seconds are tested without waiting.

Verified end to end against a real dev server (real login and CSRF): review, commit (403 without the
token, 202 with), the countdown, confirm; and a **SIGKILL in the middle of the 60 seconds**: after the
restart the countdown continued (46 s left, not 60) and the deadline reverted the sandbox files,
`network.json`, and put the edit back in the pending changes.

- **9d (banner), `web/static/scripts/config_banner.js`** (built): a bar above every view, driven only by
  `GET /api/v1/config/status` (all state lives on the server, so it survives reloads and daemon restarts).
  - *Uncommitted changes* (real differences, not a file): "N uncommitted changes" with **Review & commit** and
    **Discard**.
  - *Review dialog* (native `<dialog>`, independent of any page layout): the diff (old struck through, new
    in green), the files to be written, the services affected in plain language, per-field validation errors
    (Commit disabled), notes, and, when confirmation is needed, a warning with the seconds allowed and, if
    the LAN address changes, the exact new `https://` address to reconnect at.
  - *Applying*: brief in-progress bar. *Awaiting confirmation*: prominent amber bar with a live countdown
    (ticks locally, resynced by every 1 s poll) and **Confirm** / **Revert now**.
  - *Lost contact* (the poll fails, as after a LAN address change): says where to reconnect (the address
    is remembered in `sessionStorage` when the commit is made, forgotten once the state is idle again).
  - *Outcome*: failures and reverts stay until dismissed (persisted per outcome timestamp); good news
    ("The change was confirmed") shows for about 8 seconds.
  - All server text goes through `escapeHtml` (checked with hostile strings). POSTs use `apiFetch` (CSRF).
  - Layout: the banner sits in `.content-column` above `<main>` and is `position: sticky; top: 0`, so it is
    pinned while the page scrolls. Below 720 px it is `position: fixed` across the viewport and
    `--config-banner-h` (kept current by a ResizeObserver) pushes the page down, because the dashboard's
    fixed 280 px sidebar otherwise squeezes it into a sliver.

Decisions for the remaining steps (9c to 9e):

- **Default LAN in router mode: 192.168.1.1/24**, DHCP enabled `.100` to `.200`, lease 12h. Done
  (`translator.AdoptExisting`, run once at startup through `Manager.AdoptNetwork`): it only fills what the
  JSON leaves blank and never overwrites. A DHCP section that is missing is imported from an existing
  `peakroute-dhcp.conf` (so a wizard box keeps its real range, verified end to end: after adoption the
  translator's plan for a wizard box is empty); with no file it defaults (only for a /24, where `.100`-`.200`
  fits); a file that cannot be understood (several ranges, tags, a range outside the LAN, a bad lease,
  unreadable) leaves DHCP unmanaged rather than guessing. AP mode is untouched. Adoption writes
  `network.json` only if something changed, is not a commit (nothing is applied) and never shows up as an
  uncommitted change. The installer/wizard will be revisited later; the daemon does not depend on it.
- **Dev mode system root:** `systemRoot` is `""` on the appliance and `./dev-data/sysroot` in `-dev`, so a
  dev instance reads (and, once apply exists, writes) a sandbox instead of the host's real `/etc`.
- **Applying** (`translator.Applier`, built) takes a `Desired` and a `Plan` and: backs up every file that will
  change; writes them atomically; **checks the new configuration before any service is touched**
  (`ifquery --list --all`, `nft -c -f /etc/nftables.conf`, `dnsmasq --test`; a rejected check restores the
  files and stops); runs the service actions in order (`sysctl -p`, `systemctl restart networking`,
  `systemctl reload nftables`, `systemctl restart dnsmasq`); and if an action fails restores the files and
  re-runs the actions so services return to the previous configuration. Rollback problems are reported
  in the result, never swallowed, and the rollback uses a fresh context so a cancelled apply (shutdown)
  still puts things back. Applies are serialised. Everything goes through two interfaces so tests need no
  root: `FileSystem` (`RootFS` maps `/etc/...` into a directory) and `Runner` (`ExecRunner` runs real
  commands with a fixed `PATH` and no inherited environment, output capped, per-command timeouts;
  `LogRunner` only logs and is what `-dev` uses). The HTTP layer will run applies after the response is
  sent (networking restarts drop connections).
- **Dev mode** uses a sandbox root plus a logging runner and never runs real commands (built and verified:
  the dry-run runner logs `[dry-run] would run: ...` and the sandbox files appear under
  `dev-data/sysroot/`). To exercise the flow before the editor page exists, put a candidate in
  `dev-data/network.json.staged` (and the active config in `network.json`) before starting the daemon.
- **Revert survives a daemon restart:** the pending confirm is persisted (`confirm.json`, mode 0600: deadline,
  previous config, the rejected candidate and a file snapshot).
- First editor page: LAN settings (LAN IP/netmask and DHCP range, lease, on/off). DHCP-only is the
  safer first slice (a dnsmasq restart cannot lock you out); to be confirmed.

## 7a. Editing endpoints (ports and networks)

`internal/netapi` serves the editors. Rules every editing endpoint follows:

- **Saving only stages a draft.** Nothing touches the system until the draft is committed through the
  pipeline (banner, review, commit, 60 s confirm). Saving values already in effect, or putting an edit back,
  leaves no draft (the stage file disappears).
- **Each endpoint validates only the fields it owns.** The Networks page checks the address, netmask and DHCP
  fields; the Ports page checks the WAN and LAN port names. A half-configured box can therefore be edited a
  page at a time, and the *whole* configuration is checked when the draft is committed (the review dialog
  shows what is still wrong).
- **Edits are refused with 409 while a change is being applied or waits for confirmation**
  (`pipeline.Service.Idle`), since a revert would overwrite a newer draft.
- **Object-style, generic names** in the API (`ip`, `netmask`, `dhcp.range_start`, `ports`), mapped from the
  config file's names (`lan_ip`, `lan_subnet`, ...) in one table (`errorField`). Errors come back as
  `{"error": "...", "errors": [{"field": "ip", "message": "..."}]}` so a form can mark the right input.
- **Port roles are saved all at once**, so there is never an invalid in-between state (two WANs, no LAN). The
  saved LAN port list follows the hardware order, not the order of the request. A configured port whose
  hardware is missing is still listed (`present: false`) so it can be reassigned.
- Requests must be `application/json`, at most 64 KB, with no unknown fields.

## 7d. Internet connection and its health (built 2026-10-08)

**Settings** (`netapi/uplinks.go`, same rules as 7a): DHCP or static (IP, netmask, gateway, DNS). Saving
only stages. Choosing DHCP clears the static fields so no stale values stay in `network.json`. The
shared validation lets a static gateway and DNS be empty (the wizard allows it); the editor requires both
because a static address without them cannot reach anything. The LAN/WAN subnet overlap is reported on
both `lan_subnet` and `wan_subnet`, so whichever page you are on can show it next to its field. The
`live` block describes the *active* config's port (not the draft's) from the kernel: `/sys` link state,
the first IPv4 address, the default route in `/proc/net/route`, and the resolvers.

**Health** (`internal/wanhealth`):
- **What is probed:** the first upstream DNS server: the configured one for a static address, otherwise the
  first non-loopback server in `/etc/resolv.conf` (on systemd-resolved machines, the real servers in
  `/run/systemd/resolve/resolv.conf`; a local stub is not "upstream"). The target is looked up on every
  probe, so a change of provider or a commit is followed. Nothing is probed until a WAN port exists.
- **How:** an ICMP echo (a real ping) every 5 s with a 2 s timeout. An unprivileged "ping" datagram socket
  is tried first, then a raw socket (root/`CAP_NET_RAW`). If neither is allowed (a developer running as a
  normal user) it falls back to timing a small DNS query (`. NS`) to the same address, and the page says so.
  **Caveat:** some networks redirect all outgoing DNS to their own resolver, which answers for any
  address (the dev machine's network does: even 192.0.2.1 answers in ~50 ms), so the DNS fallback can
  look healthy while the link is down. The ICMP path does not have this problem. On the appliance the
  daemon runs as root today; when it is moved to a normal user (TODO #21) it needs `AmbientCapabilities=CAP_NET_RAW`
  (or `net.ipv4.ping_group_range`) to keep real pings.
- **State:** `online` if any of the last probes answered, `offline` after 3 failures in a row (15 s),
  `unknown` until the first result, `not_configured` without a WAN port or resolver. A probe cancelled
  by shutdown is not counted as lost.
- **Storage:** probes are folded into one row per target per minute in RAM (sent, lost, RTT sum/min/max)
  and written once a minute in one transaction (the same wear reasoning as the traffic history);
  `wan_health.db`, 30-day retention purged at start and daily at 03:00, incremental auto-vacuum. Closing
  the daemon writes the pending minute. A power cut costs at most the last minute.
- **Reading:** the 5m range uses the last 120 raw probes kept in RAM (so it starts empty after a
  restart); 1h, 24h and 30d use 1-minute, 15-minute and 1-hour buckets, as on the traffic page. The
  newest minutes come from RAM and only older ones from the database, so a minute is never counted twice
  whether or not it has been written yet. Average latency is weighted by answered probes, so lost probes
  do not drag it down; a bucket where everything was lost has no latency (null) and shows as a red column.
  Loss under 1 % in a bucket is not drawn, so a stray lost packet does not paint the whole 30-day chart.
- **Chart** (`internet.js`): canvas, average line with a best-to-worst band, red columns for loss, gaps
  where no probes ran, hover tooltip, drawn at device pixel ratio. It refreshes every 5 to 60 s depending
  on the range.

## 7b. Navigation (decided 2026-10-08)

An OPNsense-style sidebar (grouped by function, quick search, clear two-level navigation) holding
UniFi-style object pages. At most two levels: top-level entries, and groups with pages in them.

| Entry | Page | Built |
|---|---|---|
| Overview | system health, interface counters, top clients | yes (`overview`) |
| Clients | DHCP leases and per-client usage | yes (`dhcp`) |
| Traffic | per-port throughput history | yes (`traffic`) |
| Network: Internet, **Networks**, Ports, Routes | WAN uplink; each network with its address range *and* DHCP; physical ports; static routes | Internet (`internet`), Networks (`networks`) and Ports (`network-physical`) |
| Firewall: Rules, Port forwarding, Connections | | planned |
| Services: DNS, VPN, Dynamic DNS | | planned |
| System: Settings, Administrators, Certificates, Backup and restore, Updates, Logs, Diagnostics | | Logs only (`logs`) |

- **Planned pages are listed but greyed out and not clickable** (a `<span class="nav-item nav-soon">`, no
  `data-view`), because the whole product will be built before it is used. To hide them instead, add
  `.nav-soon { display: none; }` to the stylesheet (the groups then only show real pages).
- **Names are confirmed** (friendly, not technical) and live only in `dashboard.html`: friendly (Internet, Networks, Ports, Clients)
  rather than technical (WAN, Interfaces, Devices, DHCP). The view ids behind them did not change.
- **Quick search** filters entries (a group's own name matches all its pages), opens matching groups, shows
  "No matches", and on clearing opens the group of the page you are on. **Accordion:** opening a group by
  clicking closes the others; groups the page opens itself (navigation, search) are handled separately so the
  two do not fight.
- **Networks** is the first editor page (9e): one card per network (router address, ports, DHCP), so VLANs
  later are just more cards.
- Pending changes are not a menu entry: the banner and review dialog cover them.

### Editor pages (built 2026-10-08)

- **Ports** (`ports.js`, view `network-physical`): one row per port with a Role select (Internet (WAN), LAN,
  Unassigned), link state, speed, MAC and driver. The old read-only Devices table and its
  `/api/v1/system/devices` endpoint are gone; this page replaces them. Choosing WAN on one port moves any
  other WAN port to Unassigned (the server allows exactly one). The page applies the same rules as the server
  (one WAN, at least one LAN) so Save is only enabled for a valid choice. Rows say `Unsaved (was X)` for
  edits not yet sent and `Draft` for saved-but-uncommitted ones; a configured port whose hardware is gone
  shows `Hardware missing`. One Save sends all roles (`PUT /api/v1/ports`).
- **Networks** (`networks.js`): one card per network (today only `lan`): router address, netmask, the ports
  (read-only chips that link to Ports through the generic `data-goto` handler in `app.js`), DHCP toggle, range
  and lease time. Field errors from the API (`errors[].field`) are shown under the matching input; anything
  without an input is shown in the message box. If the DHCP section is not managed by the config yet
  (`dhcp: null`) and the person leaves it off and empty, the PUT omits `dhcp` so it stays unmanaged. Enter
  in a text field saves.
- **Both pages**: saving only writes the draft and then calls `ConfigBanner.refresh()` so the banner
  updates at once. While a change is applying or awaiting confirmation (`editable: false`) every input is
  disabled with an explanation. The Ports page polls every 5 s for link changes but never while the person
  has unsaved edits (it would overwrite them). The review dialog's warning adds a line when port roles
  change: the cable you are connected through may stop working, plug into a LAN port to confirm.

## 7c. Responsive layout (decided 2026-10-08)

One site, not separate desktop and mobile sites: the same HTML5 page adapts with CSS media queries, so
every future page works on every screen automatically.

- **Wider than 900 px (desktop):** the sidebar sits beside the content, unchanged.
- **900 px and narrower (tablet, phone):** a slim top bar (menu button and the current page's name) pinned
  to the top; the sidebar becomes a slide-out drawer (`body.nav-open`) with a dimmed backdrop. It closes on
  the backdrop, the close button, Escape, or choosing a page, and growing the window past the breakpoint
  drops the open state. The page uses the full width with tighter spacing.
- **Accessibility:** the menu button carries `aria-controls`, `aria-expanded` and a label that flips between
  "Open menu" and "Close menu"; focus moves into the drawer on open and back to the button on close; the
  closed drawer is `visibility: hidden`, so keyboards cannot reach it; menu entries, banner buttons and dialog
  buttons are at least 44 px tall on phones.
- **The banner** sticks right under the top bar (`top: var(--appbar-h)`); the earlier fixed-position
  workaround and its height-publishing script were removed.
- **Under 520 px (phones):** banner buttons stack full width and the review dialog tightens.
- Tables already scroll inside their card (`.table-container`); the stat grids use `min(260px, 100%)` so a
  column never exceeds the screen.
- **Charts:** a chart is only drawn when its container has a width, and all charts are redrawn when the
  Traffic view is shown. Previously a chart drawn while its view was hidden measured 0 wide and fell back to a
  hard-coded 800 px (for instance after the window was resized on another page), leaving it too wide until
  the next 10 s refresh. The canvas also has `max-width: 100%`.
- Verified in a real browser at desktop, tablet (768) and phone (375) widths: every view fits without sideways
  scrolling, all four ways of closing the drawer, navigation, the pinned banner, the Traffic charts and the
  review dialog.

## 8. HTTP endpoints (current)

| Method and path | Auth | Notes |
|---|---|---|
| `GET /first_run`, `POST /api/v1/setup/apply` | none, only while unconfigured | creates the admin |
| `GET /login`, `POST /api/v1/login` | none | rate limited |
| `POST /logout` | session + CSRF token (form field) | GET returns 405 |
| `GET /`, `/dashboard` | session | server-rendered shell; embeds the CSRF token |
| `GET /api/v1/system/stats` | session | from the poller snapshot, no disk reads |
| `GET /api/v1/system/traffic?iface=&range=` | session | history; ranges `1h` (also used for 5m), `24h`, `30d` |
| `GET /api/v1/uplinks`, `GET /api/v1/uplinks/{id}` | session | the internet connection (today one, id `wan`): `port`, `mode` (dhcp or static), `ip`, `netmask`, `gateway`, `dns`, `pending`, and `live` (what is in effect: `up`, `address`, `gateway`, `dns`) |
| `PUT /api/v1/uplinks/{id}` | session + CSRF | `{"mode": "dhcp"}` or `{"mode": "static", "ip", "netmask", "gateway", "dns": []}`; stages a draft. Switching to DHCP clears the static values. A static address also requires a gateway and one DNS server |
| `GET /api/v1/uplinks/{id}/health?range=5m\|1h\|24h\|30d` | session | `status` (state, target, method, latest latency), `summary` (sent, lost, loss %, avg/min/max ms) and parallel arrays `timestamps` (ms), `avg_ms`, `min_ms`, `max_ms` (null when every probe in the point was lost), `loss_pct` |
| `GET /api/v1/system/logs?limit=` | session | systemd journal |
| `GET /api/v1/dhcp/leases` | session | dnsmasq leases with per-client usage |
| `POST /api/v1/cert/upload` | session + `X-CSRF-Token` | multipart `cert` and `key` |
| `GET /api/v1/config/status` | session | `state` (idle, applying, awaiting_confirm), `has_changes`, `change_count`, `confirm.seconds_left`, `last` outcome; polled by the banner |
| `GET /api/v1/config/staged` | session | the review: `changes`, `valid`, per-field `errors`, `plan` (files and service actions), `notes`, `needs_confirm`, `confirm_seconds` |
| `POST /api/v1/config/commit` | session + CSRF | 202 once applying has started (poll status); 400 with per-field `errors`; 409 if busy or nothing staged |
| `POST /api/v1/config/confirm` | session + CSRF | keep the change; 409 if nothing is waiting |
| `POST /api/v1/config/revert` | session + CSRF | undo the change waiting for confirmation now (202) |
| `POST /api/v1/config/discard` | session + CSRF | drop the staged changes; 409 while applying or awaiting confirmation |
| `GET /api/v1/ports` | session | physical ports with hardware details, `role` (wan, lan, unassigned) from the draft, `active_role`, `pending`, `present` (false: configured but the hardware is gone), `editable` |
| `PUT /api/v1/ports` | session + CSRF | `{"assignments": {"eth0": "wan", "eth1": "lan", "eth2": "unassigned"}}` stages the roles as one atomic change; ports not mentioned keep their role; the result must have exactly one WAN and at least one LAN |
| `GET /api/v1/networks`, `GET /api/v1/networks/{id}` | session | the networks (today one, id `lan`): `ip`, `netmask`, `ports`, `bridged`, `dhcp` (null if not managed yet), `pending` |
| `PUT /api/v1/networks/{id}` | session + CSRF | `{"ip", "netmask", "dhcp": {"enabled", "range_start", "range_end", "lease_time"}}` stages the address and DHCP; `dhcp` omitted leaves DHCP unchanged |

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
| 2026-10-06 | Router default LAN 192.168.1.1/24, adopted at startup | installer setup will be revisited later; wizard boxes keep their real DHCP range |
| 2026-10-06 | CSRF token per session, header for the API, POST-only logout | commit/discard are the first write endpoints |
| 2026-10-06 | Dev mode works on a sandbox root and never touches the host's /etc | developers run `-dev` on their own machines |
| 2026-10-06 | Check new config before touching services; roll back files and services on any failure | a bad config must never leave a router half-configured |
| 2026-10-06 | Revert from a snapshot of the real previous files, persisted for the confirm window | works even if the previous config cannot be rendered, and survives a power cut |
| 2026-10-06 | Only networking/firewall changes need the 60 s confirmation | a DHCP-only change cannot lock the admin out |
| 2026-10-06 | A failed or reverted change keeps the user's edit as the pending candidate | fix it instead of retyping it |
| 2026-10-06 | The banner and review dialog do not depend on any page or sidebar layout | the user had not yet decided the sidebar/settings organization |
| 2026-10-08 | Sidebar: OPNsense-style grouping and search, UniFi-style object pages, two levels, planned pages greyed | prosumer/home/lab audience; sysadmin maintainer; clearest navigation |
| 2026-10-08 | Stay in standalone scope; lean toward a future SDN controller only where free | the SDN controller is much later; do not build it now |
| 2026-10-08 | Friendly menu names confirmed (Internet, Networks, Ports, Clients) | easier for a home or lab audience |
| 2026-10-08 | One responsive site instead of separate desktop and mobile sites | one codebase; every new page works on every screen |
| 2026-10-08 | First editor slice is LAN address plus DHCP, with port roles (WAN, LAN, unassigned) on the Ports page | the address and DHCP mean little without saying which ports are which |
| 2026-10-08 | Editing endpoints validate only their own fields and are refused while a change is applying | partial configuration stays editable; a revert must not overwrite a newer draft |
| 2026-10-08 | Ports and Networks pages replace the read-only Devices table; Save writes a draft only, never the system | one pipeline for every change; the banner is the single place to commit |
| 2026-10-08 | WAN health pings the upstream DNS server (ICMP, DNS query only as a fallback), per-minute history in its own SQLite file | what Meraki's appliance status shows; the resolver is what clients actually depend on; same flash-friendly storage as traffic |
| 2026-10-08 | Static WAN needs a gateway and a DNS server in the editor, though the shared validation allows them empty | an address alone cannot reach the internet, and the health chart needs a DNS server to measure |
| 2026-10-06 | Success messages show ~8 s; failures and reverts stay until dismissed | problems must not be missed, good news should not nag |

## 10. How things are tested (worth documenting)

- **Mutation checking.** For each new behaviour the code is deliberately broken in a scratch copy and a
  test must fail. Several tests were strengthened when a mutant survived.
- **Golden tests against the wizard script** (translator) and against the old behaviour (chart
  equivalence for per-minute rows).
- **Live checks:** `strace` to prove no config files are opened per request, `/proc/<pid>/io` for disk
  writes, timing comparisons of old and new builds (login timing, stats latency), concurrent setup race.
- Time is injectable (`SessionManager.now`) and filesystems/command runners are interfaces, so logic is
  tested without root or real time.
- **Real ICMP without root:** `unshare -rn` gives a user+network namespace in which the process has
  `CAP_NET_RAW`; bring `lo` up inside it and run the compiled test binary (`go test -c`) to exercise
  `TestPingICMPLoopback`, which is skipped otherwise. Writing `0 0` to `/proc/sys/net/ipv4/ping_group_range`
  inside the namespace exercises the unprivileged datagram path instead of the raw one (both verified).
- **Chart checks without a real outage:** a scratch copy with a fake "flaky" pinger and a synthetic 30-day
  history written straight into `wan_health.db`, with the static files served from disk so the JS/CSS can be
  edited without rebuilding.

## 11. Gotchas learned

- The project drive is ext4 mounted `nosuid,nodev`; `debootstrap` normally copes; `BUILD_DIR` can move
  the build elsewhere.
- Browsers refuse the self-signed cert in the in-app preview; use a normal browser at `https://localhost:8443`.
- `gofmt` complains about some older indentation in untouched files; leave it unless editing the file.
- Interface names are not stable across hardware: the CPU temperature code must not assume
  `thermal_zone0` (on the dev laptop it is a fixed 25 C `acpitz` placeholder; TODO #24).
- Session cookies are per host, so after the LAN IP changes the admin must log in again at the new address.
- **Do not call a locking `Manager` method while holding the Manager's lock.** Go mutexes are not
  re-entrant: `AdoptNetwork` originally ran its callback under the lock and the callback read the Manager,
  which hung the daemon at startup (unit tests missed it because their callbacks never touched the
  Manager; a live run found it). It now runs the callback with no lock held and refuses to save if the
  config changed meanwhile (`ErrConfigChanged`). Always smoke-test startup against a real run.
- Test helpers that dereference a result (`info.Mode()`) must check the error first: a nil dereference panics
  the whole test binary and hides every other failure (found while mutation-testing the pipeline).
- A mutant that does not compile proves nothing: when a mutation test prints a build failure, redo it.
- `position: sticky` binds to the nearest ancestor that is a scroll container, even one that never scrolls.
  `.main-content` has `overflow-y: auto` while the page itself scrolls, so a sticky element inside it never
  stuck (and sat 32 px low because sticky respects the container's padding). Put sticky things outside such
  containers. Found by measuring in a real browser, not by reading the CSS.
- The browser preview used for testing only advances CSS transitions and media-query events when it renders a
  frame, so a measurement taken right after a resize or an animation can show a half-finished state. Take a
  screenshot (it forces a frame) and measure again before concluding something is broken.
- There is no JavaScript test runner in the repo (no Node available on the dev machine). Front-end behaviour was
  verified in a real browser: the banner's `describe()` logic with 20 assertions, every state, hostile text,
  scrolling, view switching, a phone width, and a stopped server. A runner would be worth adding later.
- For POST/PUT/DELETE from the browser use `apiFetch` (it adds the CSRF header), never bare `fetch`.

## 12. Open questions for the maintainer

- 9e first slice: DHCP only (a dnsmasq restart cannot lock anyone out) or LAN address plus DHCP.
- Where the Staged Changes page, if any, should live (the banner and dialog already cover the workflow).

## 13. Documentation to write later

- Install and first boot (wizard, ISO, installer)
- Web UI tour and admin tasks (staging, review, commit, confirm, revert)
- Configuration reference (`network.json`, `auth.json`, `sys.json`, validation rules and errors)
- Security overview (auth, CSRF, session timeout, TLS)
- API reference
- Troubleshooting (lockout recovery, logs, reverting by hand)
- Architecture and contributor guide (this file is the seed)

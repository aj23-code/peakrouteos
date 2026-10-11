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

**Settings** (`netapi/uplinks.go`, same rules as 7a): DHCP or static (IP, netmask, gateway, DNS); PPPoE, VLAN, MTU, MAC clone and the DHCP host name are described in *The complete WAN page* below. Saving
only stages. Choosing DHCP clears the static fields so no stale values stay in `network.json`. The
shared validation lets a static gateway and DNS be empty (the wizard allows it); the editor requires both
because a static address without them cannot reach anything. The LAN/WAN subnet overlap is reported on
both `lan_subnet` and `wan_subnet`, so whichever page you are on can show it next to its field. The
`live` block describes the *active* config's port (not the draft's) from the kernel: `/sys` link state,
the first IPv4 address, the default route in `/proc/net/route`, and the resolvers.

**Health** (`internal/wanhealth`):
- **Two series.** `dns` is the upstream DNS server (the internet side) and `gateway` is the router or
  modem directly in front of this one (the configured gateway for a static address, otherwise the
  default route in `/proc/net/route`). Showing both tells you where it breaks: gateway fine but DNS
  gone means the modem, provider or line; both gone means the cable or the modem. The status carries
  that as a `diagnosis` sentence (also when the DNS server answers but the gateway does not, which
  usually means a modem that ignores pings). A series keeps its history when the address behind it
  changes. The gateway is **only probed with a real ping**: many gateways run no DNS server, so the
  DNS-query fallback would report an outage that is not there (state `unavailable` with an explanation).
  The two probes run one after the other each cycle (2 s timeout each, inside the 5 s interval).
  `wan_health.db` rows were first keyed by address; opening an older file renames that column to
  `series` and treats the rows as DNS history.
- **What is probed (the DNS series):** the first upstream DNS server: the configured one for a static address, otherwise the
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
- **State:** `online` once a probe answers, `offline` after 3 failures in a row (15 s),
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
- **Uptime and outages** (`availability.go`), from the DNS series' per-minute rows (RAM for the newest,
  database for the rest, RAM wins for a minute that is in both). A minute is *down* when every probe in
  it was lost and at least 3 were sent (the same 3 as the offline rule); *up* if anything answered;
  minutes with fewer than 3 probes (daemon starting or stopping) or no data at all say nothing and are
  not counted, so uptime is a share of *observed* minutes and a gap in the data does not count as
  downtime. An outage is a run of consecutive down minutes (a missing minute splits it); `ongoing` only
  for the last one, and only if it reaches the present. The gateway series never counts: a modem that
  ignores pings must not make outages. The result is cached for 20 s because the page asks every few
  seconds and a 30-day scan is wasted work.
- **Usage card** (`internet.js`, no new endpoint): the port with role `wan` in `/api/v1/system/stats`
  (polled every 2 s) gives the current download and upload rate; the 24 h and 30 d volumes come from
  `/api/v1/system/traffic` (reloaded every minute and when the WAN port changes) as the sum of each
  bucket's average rate times its length (900 s and 3600 s). Time the daemon was not running has no
  buckets, so the totals are floors. Without a WAN port the card says to pick one on the Ports page.
- **Chart** (`internet.js`): drawn in the **same style as the Traffic chart** (same padding, palette, smooth
  curves and gradient fill under each line, crosshair with a dot per line, and a card tooltip with a coloured
  dot per row): the DNS average in blue and the gateway in green, red columns for DNS loss, gaps where no
  probes ran. Best and worst latency are in the tooltip and the figures above the chart rather than a band.
  Drawn at device pixel ratio; refreshes every 5 to 60 s depending on the range.

### The complete WAN page (built 2026-10-10)

**What it does.** Connection type (**DHCP**, **static**, **PPPoE**), plus VLAN ID, MTU, MAC address to present, the host name
sent with DHCP, and DNS servers that replace the provider's. IPv4 only: nothing about IPv6 exists anywhere in the router yet.

- **Model** (`config/wan.go`, flat fields in `network.json`): `wan_mode` (`dhcp`/`static`/`pppoe`), `wan_vlan` (0: none),
  `wan_mtu` (0: the usual 1500, or 1492 for PPPoE), `wan_mac`, `wan_hostname`, `wan_pppoe` (`username`, `password`, `service`)
  and `wan_dns`. `WanLink()` is the interface the connection runs over (`eth0`, or `eth0.835` with a VLAN) and `WanDevice()` the
  one that carries the address and the traffic (the link, or `ppp0` for PPPoE). **Everything that used to read `WanInterface`
  for traffic now reads `WanDevice()`**: the firewall, the masquerade rule, the other networks' internet rule, the gateway the
  health chart pings. `WanInterface` stays the physical port (the Ports page, the traffic charts).
- **Validation:** VLAN 1 to 4094 (and the port name must still fit 15 characters with `.vlan` added); MTU 576 to 9000 (to 1492
  with PPPoE); MAC lower case, not a group address, not all zeros; host name an RFC 1123 label and only with DHCP; PPPoE needs a
  user name and password, plain printable ASCII without quotes or backslashes (they go into a quoted option file), up to 64
  characters; PPPoE settings are refused with another type. DNS servers (up to three) are checked for every type: required with a
  static address, optional (an override) otherwise.
- **How it is written** (`translator/wan.go`; **plain DHCP and static configurations render byte-for-byte as before**):
  - *Options on the port's stanza:* `hwaddress ether` (MAC clone), `hostname` (read by udhcpc, which the image uses), and an MTU
    other than the usual one. **ifupdown's DHCP method has no `mtu` option and would silently ignore it**, so DHCP gets
    `post-up ip link set dev $IFACE mtu N`; a static address uses the native `mtu`. A VLAN interface cannot be larger than its
    port, so a jumbo MTU over a VLAN also raises the port first (`pre-up`).
  - *VLAN:* a VLAN interface (`eth0.835`, `vlan-raw-device eth0`) holds the connection; the vlan package brings the port up. A
    cloned MAC goes on the VLAN interface, which is what the provider sees.
  - *PPPoE:* a link stanza (`inet manual`; ifupdown brings it up by itself) and a `ppp` stanza (`provider peakroute-wan`, which
    runs `pon`). The login is `/etc/ppp/peers/peakroute-wan`, **mode 0600, only present while the connection is PPPoE**: `plugin
    rp-pppoe.so`, `nic-<link>`, `user`, `password`, `noauth`, `unit 0` (so the session is always `ppp0`), `mtu`/`mru`,
    `defaultroute`, `usepeerdns`, `persist` with `maxfail 0` and `holdoff 10` (dial again forever, ten seconds apart), and LCP
    echo to notice a dead line. The image needs the `ppp` package (added to `buildISO.sh`; **ISO rebuild needed**).
  - *MSS clamping:* when the connection's MTU is below 1500 (PPPoE, or one set by hand) the firewall lowers the TCP maximum segment
    size to MTU minus 40 in both directions (`oifname`/`iifname <wan> tcp flags syn tcp option maxseg size > N ... set N`), placed
    before the accept rules. Without it pages stall on full-size packets. Nothing is added at 1500 or above.
  - *DNS:* servers set by hand are written to the dnsmasq upstream file (`no-resolv` and `server=`) for **every** type, not only
    static. The interface stanza's `dns-nameservers` stays static-only. With PPPoE and nothing set, pppd's `usepeerdns` (the
    ppp package's `0000usepeerdns` hook) updates resolv.conf and dnsmasq follows it.
- **API** (`netapi/uplinks.go`): `PUT /api/v1/uplinks/wan` takes `mode` and, optionally, `vlan`, `mtu`, `mac`, `hostname`, `dns`
  and `pppoe` (`username`, `password`, `service`). Fields left out stay as they are; `0`, `""` or `[]` clear. Changing the type
  drops what belongs to the old one (static values for DHCP and PPPoE; the host name for static and PPPoE; the PPPoE login for
  the others). **Leaving static does not turn its DNS servers into an override** unless the request sends some. **The PPPoE
  password is write-only**: it is accepted, saved in `network.json` (0600) and written to the peer file, and never returned
  (`password_set` says whether one is saved; leaving it out keeps it). The review shows "PPPoE password: (none) -> set" or
  "set -> changed". Error fields: `vlan`, `mtu`, `mac`, `hostname`, `dns`, `pppoe.username`, `pppoe.password`, `pppoe.service`.
- **Live** (`GET /api/v1/uplinks`, `live`): the *active* configuration's device, MAC and MTU as the kernel reports them, with the
  address, gateway and DNS. A PPP interface reports its state as "unknown", so it counts as up when the kernel says it is up and
  it has an address. A PPPoE session has no gateway address: the provider's end of the link (`ip addr`'s `peer`) is shown, and is
  what the health chart pings as the gateway.
- **Page** (`internet.js`): the Connection card has the three types as cards, the fields of the chosen type, one DNS field whose
  wording follows the type, and an *Advanced* section (VLAN, MTU, MAC) that opens by itself when any is in use. The MTU
  placeholder shows the usual value for the type. A saved password shows as "unchanged".
- **Checked with real tools:** the real `ifupdown`, `vlan` and `iproute2` in a network namespace (static over a VLAN with a MAC and
  MTU came up as written and went down cleanly; the DHCP, jumbo-MTU and `ppp` stanzas were run with `ifup --no-act` and produced
  the expected commands, including `pon peakroute-wan`); the real `nft -c` on every variant (a test); the real `pppd` parsing the
  peer file (a test, with a bad option as the control; the lines the PPPoE plugin reads are checked separately, because the
  plugin needs kernel support to start and this machine's `pppd` stops there). The real `/dev/ppp`/`pppoe` kernel path and a real
  PPPoE server have **not** been tried: needs a box that can load the `pppoe` module.
- **Not built:** IPv6 (including a delegated prefix for the LAN), 802.1p VLAN priority (some providers want it on DHCP packets),
  RFC 4638 "baby jumbo" frames (a PPPoE MTU of 1500), IPoE/DS-Lite/MAP-E tunnels, a second internet connection (failover or
  load balancing), and a button to renew the lease or redial.

## 7h. Firewall (started 2026-10-11)

The design, the order rules are applied in, the reasons (per-interface rules written "from, to" rather than zones; the
Local Networks switches kept as a fixed policy layer; an always-on anti-lockout) and the build plan are in
**`docs/firewall.md`**, which every firewall commit updates; it is meant to become the user documentation. Built so far:

- **Stage 1, model** (`config/firewall.go`): `firewall.rules` and `firewall.forwards` in `network.json`, validation (errors on
  `firewall.rules[i].field`, `firewall.forwards[i].field`), review lines in words, shadow detection.
- **Stage 2, ruleset** (`translator/firewall.go`): rules and forwards written into `/etc/nftables.conf` at fixed places; with none
  the file is byte-for-byte unchanged. Checked with real packets between network namespaces
  (`firewall_behaviour_test.go`, no privileges needed), the real `nft -c`, and exact-text tests.
- **Stage 3, API** (`netapi/firewall.go`, `network/firewall_counters.go`): rules and forwards (add, change, delete, switch off), reordering one tab, the fixed policy lines per tab, hit counters read from `nft -j`, and a guard so a Local Network used by a rule or forward cannot be deleted. Endpoints are in the document.
- **Stage 4, Rules page** (`firewall_rules.js`, Firewall menu, Rules): described in the document (laid out in the order it is applied: locked **System rules** (the safety lines) first, then the user's rules numbered by priority, then the locked **Defaults** that the Local Networks switches produce; decided 2026-10-11: system rules always before user rules, switch effects stay defaults so a Block can override them). Checked in the browser: add with errors, add, reorder, the "never matches" warning, switch off, edit, delete, and the review lines and plan the draft produces.
- **Stage 5, Port forwarding page** (`firewall_forwards.js`, Firewall menu): described in the document; checked in the browser (errors, add, a range greys out the device port, the review lines).
- **Stage 6, Connections page** (`network/conntrack.go`, `netapi/connections.go`, `firewall_connections.js`): the kernel's connection table (from /proc or the `conntrack` tool, parsed from real output), with names, NAT, data and filters. The image gets the `conntrack` package (**ISO rebuild needed**).
- **Stage 7, logging** (`network/firewall_log.go`, `netapi/firewall_log.go`, the *Logged traffic* card on the Rules page): rules with log on write kernel log lines tagged `fw:<id>`; the Logs page shows them raw and the Rules page in words. The kernel log format is parsed from the LOG target's documented format (a real line could not be captured in a network namespace: the kernel only logs from the main one).
- **The firewall's seven stages are done.** What remains is real-hardware testing (the ISO needs a rebuild for `ppp`, `conntrack`, hostapd and the wireless firmware), screenshots for the user documentation, and the items in section 12 of the document.

## 7i. DNS (started 2026-10-11)

The design, the two sides (what the router asks upstream, what each network's devices are told), the local domain, local names
and domain forwarding, and the build plan are in **`docs/dns.md`**, which every DNS commit updates. Decisions with the user:
dnsmasq stays the engine (no Unbound: no DNS over TLS or DNSSEC for now); the upstream servers are edited on the DNS page
(one list, the old Internet-page override); the local domain is empty until it is set under System, Settings. Built so far:

- **Stage 1, model** (`config/dns.go`): `domain`, `dns.hosts`, `dns.forwards`, `lan_dns_servers`, and `dns_servers` on each extra
  network, with validation (lower-case DNS names, usable IPv4 servers, limits of 200 names and 50 forwards, no forward inside
  the local domain) and review lines in words.
- **Stage 2, files** (`translator/dns.go`): each network's DHCP answer names its own DNS servers or the router; one more file,
  `/etc/dnsmasq.d/peakroute-dns.conf`, holds the domain (`domain=`, `local=/.../`, `expand-hosts`), `host-record=` lines and
  `server=/<domain>/<server>` lines, and exists only while there is something to say. With nothing set every file is unchanged.
  Checked with the **real dnsmasq answering real queries** in a namespace (`TestTheRealDnsmasqAnswersAsTheFileSays`) and
  `dnsmasq --test`.
- Not yet: the API, the DNS page (Services, DNS), the domain field on Settings, the lookup test. See the build plan.

## 7e. Static routes (built 2026-10-08)

**Model.** `NetworkConfig.Routes` is a list of `{name, destination, gateway, metric, disabled}` (JSON keys
as written; `disabled` and `metric` are omitted when unset, so a route is enabled by default). Only IPv4,
only "via a gateway" routes. It is part of `network.json`, so it goes through the same draft, review,
commit and confirm steps as everything else.

**Validation** (`validateRoutes`, errors on `routes[i].destination|gateway|metric|name`, or `routes` for
the count): the destination must be a network in CIDR form with no host bits set (the error says what the
network is); `/0` is refused because the default route comes from the internet connection; a network that
is already directly connected (inside the LAN or the static WAN subnet) is refused; the gateway must be a
host address of the LAN or the static WAN subnet, not the router's own address (with a DHCP uplink the WAN
subnet is unknown until it is up, so any gateway is accepted and a wrong one is caught when the route is
applied); the same destination may be listed once per metric; metric 0 to 9999 (0 = default); names are
at most 40 plain characters; at most 64 routes. Turned-off routes are validated too.

**How it reaches the system.** The translator writes one script, `/etc/network/if-up.d/peakroute-routes`
(mode 0755, service `routes`): `ip route flush proto 77`, then one `ip route replace ... proto 77` per
enabled route. Protocol 77 tags the translator's own routes, so it removes exactly those and never a
route the kernel or the DHCP client added. Why a hook script and not lines in `/etc/network/interfaces`:
changing a route then needs no networking restart (no interruption, no DHCP restart), and ifupdown runs
every `if-up.d` script when an interface comes up, so the routes return after a reboot or a networking
restart. The script exits 0 when ifupdown runs it (a route that cannot be added yet must not hold an
interface back) and takes `strict` from the commit step, where a route the kernel refuses fails the
commit and the whole change is rolled back. When the last route is removed the file is removed too, and
the apply step flushes protocol 77 instead (`routesApplyCommand`); the check step skips a missing file.
A route's name is never written to any file, so a rename alone changes nothing on the system.
- **Plan and confirm:** the `routes` service sits after `networking` in the order and is an `apply`.
  A routes-only commit shows just that action, but it **does** need the 60 second confirmation: a wrong
  route can cut off an admin who reaches the router from another network.
- **Review dialog:** one line per route added, removed or changed ("Static route to 10.20.0.0/16 (metric
  100): (none) to via 192.168.1.50, metric 100"). A route is the same route while destination and metric stay.
- **What the image provides** (`buildISO.sh`): `ifupdown` (the hooks and `networking.service`),
  `iproute2` (`ip`) and dash. The WAN DHCP client is `udhcpc`; there is no systemd-resolved or
  NetworkManager, so `/etc/resolv.conf` holds the provider's resolvers (what the WAN health check reads).
- **API** (`netapi/routes.go`): GET returns the draft and the routing table in effect (`/proc/net/route`
  parsed by `network.ParseRoutingTable`, classified and ordered by `netapi.ClassifyRoutes`); PUT replaces the
  whole list atomically, like the Ports page (one save, one draft), validating only the routes (a half
  set-up box can still have routes edited). `ownedErrors` now matches `routes[i].x` against the owned field
  `routes`.
- **Page** (`routes.js`): one bordered row per route (name, network, through, metric, an On switch and
  Remove), Add route, Reset and Save to draft; rows that are completely empty are ignored on save, errors are
  shown under the field they belong to (mapped back to the right row even when blank rows were skipped),
  and editing a field clears its own error. Under it, a read-only **Routing table** card with Default,
  Connected, Static and Other tags, refreshed every 10 s but never over what is being typed. On a phone
  each route is a stacked card.
- **Tested** with the real shell against temporary files (the check and apply commands, and the script
  being lenient for ifupdown and strict for the commit), plus 25 mutants of the validation, diff, render and apply code.

## 7f. Several local networks and VLANs (in progress, started 2026-10-08)

**What was decided with the maintainer:** UniFi-style ports (each LAN port has a *native* network, untagged,
and any number of *tagged* networks); a new network is **blocked from the internet until you allow it**
(OPNsense/pfSense style: no rule, no access) and **isolated from the other networks by default**, with a
switch for each. The default network (the one the wizard creates) keeps working exactly as before.

**Config model** (`internal/config/networks.go`; the default network stays in `lan_*`/`dhcp` so existing
files, the wizard and every golden test are untouched):
- `networks`: a list of `{id, name, vlan 2..4094, ip, netmask, dhcp, internet, isolated}`. `internet` and
  `isolated` are always written (no `omitempty`) so reading the file never has to guess; a hand-edited
  entry missing `internet` has no internet. `lan_isolated` is the default network's own isolation switch
  (internet is always on for it).
- `port_networks`: `{port: {native, tagged}}` for LAN ports, where `native` is a network id (`""` or
  `"lan"` is the default network) and `tagged` lists extra networks. A LAN port with no entry carries
  just the default network. The default network is never tagged.
- **Validation** (errors on `networks[i].field`, `networks[i].dhcp.range_start`, `port_networks.eth1.native`
  and so on): id lower-case and unique, name plain and unique (not "lan" or "default"), VLAN 2..4094 and
  unique, address/netmask valid, **no subnet overlaps** the LAN, a static WAN or another network, DHCP
  checked like the default network's (`validateDHCP` is now shared), at most 16 networks, ports must be
  LAN ports and the networks they name must exist. With extra networks the default network's DHCP must be
  managed (non-null): the DHCP file is written for all networks and would otherwise overwrite whatever
  serves the default one.
- **Review dialog:** one line per network added, removed or changed (a rename is one change), one per LAN
  port whose networks changed, and one for the default network's isolation.

**Translator** (`translator/networks.go`; with no extra networks every file is byte-identical to before):
- **Interfaces, the standard Debian way** (ifupdown with the `bridge-utils` and `vlan` packages, both added
  to the image in `buildISO.sh` on 2026-10-08, so **the ISO must be rebuilt to get them**; no `pre-up` tricks):
  the default network is untagged exactly as before (the LAN port that carries it, or `br0` when several
  ports do, or an empty `br0` with `bridge_ports none` when none does). Every other network has its **own
  bridge `br<vlan>`** (`br20`) with the router's address on it, holding the ports that carry it untagged
  and a **VLAN interface** (`eth1.20`, declared with `vlan-raw-device eth1`) for each port that carries it
  tagged. Separate bridges keep the networks apart at layer 2; the firewall decides what crosses. A
  VLAN interface is named `port.vlan` and the kernel allows 15 characters, so a long port name cannot carry
  a tagged network (validation says so). This also gives the wizard's multi-port `bridge_ports` the package
  it needs to survive a reboot.
- **DHCP and DNS:** one dnsmasq file lists every network's interface (so a network without DHCP still
  gets DNS) and, in this mode, **tags** each range and its router/DNS options with the network
  (`set:guest` / `tag:guest`), otherwise a guest client would be handed the default network's router. The
  file exists while any network uses DHCP, and the default network's range is left out if its DHCP is off.
- **Firewall** (the heart of the design): the default network keeps full access to the router, as
  always. Every other network may only use the router's DHCP, DNS and ping (no web interface, no ssh).
  A network reaches the internet **only if its Internet switch is on**: that switch is one forwarding rule
  to the WAN and there is no other. Two networks may talk **only if neither is isolated**, in both
  directions (so an open network cannot reach an isolated one, and the isolated one cannot reach out).
  Return traffic is covered by the existing established/related rule. NAT is unchanged (one masquerade on
  the WAN).
- **Plan:** adding a network restarts networking, reloads the firewall and restarts dnsmasq; flipping the
  Internet or isolation switch only reloads the firewall (no interruption); moving a port between networks
  restarts networking (and dnsmasq after it); renaming a network changes nothing on the system (names never
  reach a file). When the number of ports on the default network crosses one (br0 becomes the port itself or
  the other way round) the firewall and DHCP files change too, because they name the interface. Review
  notes warn about a network no port carries and about a default network with no port.
- **Checked against the real tools** (in a throwaway user and network namespace with dummy ports, the
  distribution's own `ifupdown`, `bridge-utils` and `vlan` packages unpacked next to the host's tools,
  `sysfs` remounted so the bridge hooks see the namespace's interfaces): `ifup` builds `eth1.20`/`eth1.30`
  on `eth1`, `br20` with `eth1.20` and `eth2`, `br30` with `eth1.30`, each with its address; `ifdown -a`
  removes all of it and `ifup -a` builds it again, and both exit 0 (what `systemctl restart networking`
  needs; the `vlan` hook prints a harmless "Cannot find device" while tearing a VLAN interface down). The
  firewall passes `nft -c` and the DHCP file `dnsmasq --test`.

**API** (`netapi/networks.go`, `ports.go`): see the endpoint table. Notes: a request validates only the network it
is about (another network's own problems are shown when that one is edited, or at commit), but a problem two
networks share (the same VLAN or name, overlapping subnets) is reported on **both**, so editing either shows
it; deleting is refused while a port still carries the network untagged, because silently moving that port to
the default network would be a security change nobody asked for; ids are `net2`, `net3`... (lowest free).

**Pages** (2026-10-08):
- **Local Networks** (`networks.js`): **one network at a time**, chosen from a dropdown at the top (the default
  network first, each added one as `Name (VLAN n)`, with a bullet when it has a draft or unsaved edits), plus an
  **Add network** button; edits in a network that is not on show are kept in memory while you look at
  another. The card for the chosen network has the address and netmask, (for added networks) the name and VLAN ID, the ports that carry
  it (plain chips untagged, dashed "tagged" chips), the DHCP server, and the two switches: **Allow internet
  access** (off for a new network, with a note that devices will get an address but cannot reach the internet
  until it is allowed; always on and disabled for the default network) and **Isolate from other networks**
  (on for a new network). A new network starts with a suggested VLAN (the next free multiple of 10) and a
  /24 (192.168.<vlan>.0) with a DHCP range, which the person can change. Each card saves on its own; errors
  appear under the field; deleting takes two clicks (the button turns into "Click again to delete" for 4
  seconds) and says why when refused.
- **Ports** (`ports.js`): when there is more than the default network, a LAN port shows an **Untagged
  network** choice and, under "Also carries, tagged", a checkbox per added network (the untagged one is
  never offered as tagged). Every network is shown as `Name (VLAN n)`, the default one as `LAN (VLAN 1)`. Networks are
  never roles: the role list stays Internet (WAN), LAN and Unassigned, and a LAN port is then given its networks.
  The networks are saved with the roles in one request.
- **Overview, Local Networks card:** one block per network with its tags (VLAN, Internet or No internet,
  Isolated, Draft), its ports, router address, how many leases fall inside its subnet and how much of its
  DHCP range is leased.
- Hardware needed to try it: the dev machine has two NICs, so a scratch build can add fake ports; the real
  bridges and VLAN interfaces were checked in a namespace (see above) but not on a box with several ports.

**What a person sees in the review:** one line per network added, removed or changed, one per LAN port
whose networks changed, and the usual file and service list: adding a network restarts networking, the
firewall and the DHCP server, flipping the Internet or isolation switch only reloads the firewall.

## 7g. Wireless, and PeakRoute on access points (started 2026-10-08)

**Direction (from the maintainer):** PeakRoute should also run on **access points**, with the same codebase.
`operation_mode` in `sys.json` already has `ROUTER` and `AP`; in AP mode the GUI is a **limited** one (no
firewall, routes, WAN, DHCP or several local networks; the Wireless page and the basics stay). Nothing of AP mode
is built yet except this page and the packages; the translator still refuses any mode but ROUTER.

**Image** (`buildISO.sh`, **the ISO needs a rebuild to get this**): `hostapd` (the access point daemon), `iw`
(reads what an adapter can do), `wireless-regdb` (regulatory rules per country), `rfkill`, and Wi-Fi firmware
from Debian's `non-free-firmware` section (hence `--components=main,non-free-firmware`): `firmware-iwlwifi`
(Intel), `firmware-atheros` (Qualcomm Atheros), `firmware-mediatek` (mt76) and `firmware-realtek` (which some
wired NICs need too). Together roughly 270 MB uninstalled on top of the 286 MB ISO; trim the ones for hardware
you will never use. The package names were checked against the trixie package lists.

**Detection and capabilities** (`internal/wireless`, `GET /api/v1/wireless`, read only):
- An interface is a Wi-Fi adapter if the kernel gives it a `wireless` folder or a `phy80211` link under
  `/sys/class/net/<name>` (no extra tool needed). Per adapter: name, radio (`phy0`), MAC, driver, link state.
- With `iw` present, `iw phy <phy> info` is parsed (`iw.go`, tested against real output from an Intel AX
  adapter in `testdata/`): the interface modes (an adapter can be an access point if `AP` is among them), and per
  band (2.4, 5 and 6 GHz; others are ignored) the usable widths (20 always, 40 from `HT20/HT40`, 80 with VHT or
  HE, 160 only when the adapter says so; never above 40 in 2.4 GHz) and every channel with its flags: `disabled`
  (not allowed), `no_ir` (can be listened on but **no access point can start there**) and `dfs` (shares
  spectrum with radar; an access point must listen first). `iw reg get` gives the country in effect; `00` is the
  world domain, which is strict: on the dev laptop every 5 GHz channel is closed for access points until a
  country is set. Without `iw` the adapters are still listed and the page says capabilities cannot be shown.
- **The page:** with no adapter it says "No wireless adapter found" and greys out the rest (an `is-disabled`
  wrapper, no pointer events); otherwise one card per adapter (badge "Can be an access point" or "Cannot be an
  access point", the facts, then each band's widths and open channels, DFS ones marked, and a count of the
  channels that are closed). It checks again every 10 s, so a USB adapter plugged in appears by itself. A
  "Wi-Fi networks" card is shown as **Planned** (greyed unless some adapter can be an access point).

**Wi-Fi adapters are not ports** (2026-10-10): the Ports page no longer lists them (`SystemInterface.Wireless`,
set from the same sysfs test the Wireless page uses) and refuses to give one a WAN or LAN role through
`PUT /api/v1/ports`; they are set up on the Wireless page. An adapter that is *already* configured as a port
(a box set up before this rule, or the dev laptop using Wi-Fi as its uplink) stays listed, tagged "Wi-Fi
adapter", and can only be moved off its role; once it is no longer a port in the active config it drops out.
This rule is about *where roles are chosen*, not a statement that Wi-Fi is always the LAN side:
- **Point-to-point links** (a radio as the client end of a PtP link, or a wireless uplink) must stay possible.
  Design: each radio gets a **mode on the Wireless page**, "access point" or "station" (a PtP/uplink client).
  A station is either the internet uplink (it becomes `wan_interface`) or a member of a Local Network
  (bridged, using 4-address/WDS mode so two PeakRoute boxes can join two sites). Nothing in the config or the
  translator assumes a radio is LAN-side; the guard above only blocks the *Ports endpoint*.
- Station mode needs `wpasupplicant`, which the image does not have yet (hostapd is only the AP side); it
  would be added with that work. The AP side of a PtP bridge is hostapd with `wds_sta=1`.

### Wi-Fi networks: model and rules (stage 1 of the build, 2026-10-10)

Built in stages: (1) config, validation and review lines, (2) translator and the wifi service, (3) API, (4) page.
- **Model** (`internal/config/wifi.go`, in `network.json` under `wifi`, so it goes through the same draft, review,
  commit and confirm steps): `radios`
  (one per adapter used as an access point: `name`, `band` 2.4 or 5 GHz, `channel` (0 = automatic), `width` 20/40/80/160,
  `disabled`) and `networks` (the SSIDs: `id`, `ssid`, `network` (the id of the Local Network it joins, `lan`
  being the default), `security` (`wpa2`, `wpa3` or `wpa2-wpa3`), `passphrase`, `bands`, `band_steering`,
  `hidden`, `client_isolation`, `disabled`). 6 GHz is not offered (it needs WPA3-only and a different set-up).
- **Band steering** is a flag on an SSID that is broadcast on both bands: devices that can use 5 GHz are pushed
  off 2.4 GHz. It needs both bands (validated).
- **Validation** (errors on `wifi.radios[i].x`, `wifi.networks[i].x`, `wifi.coverage`, and `country`):
  the router's country (see below) is required once an adapter and a network are both switched on; an adapter is
  listed once, with a band that allows its width and a channel that exists in the band and **can be the primary
  channel of that width** (40 MHz needs a neighbouring pair, 80 and 160 MHz fixed blocks: `ChannelFitsWidth`,
  `HT40Direction` and `CentreChannel` are the same tables hostapd's settings are computed from); an SSID is
  1 to 32 *bytes*, no control characters, no space at either end, and unique (a duplicate is reported on both);
  the passphrase is 8 to 63 printable ASCII characters; the local network must exist; every band a switched-on
  SSID uses needs a switched-on adapter for it (`wifi.coverage`, a commit-time problem: an SSID can be made before
  its adapter is set up). Open networks are not offered.
- **Review dialog:** one line per adapter and per Wi-Fi network (added, removed, changed; a rename is one
  change), naming the local network. **A passphrase never appears:** a changed one is
  reported as "new password" and a new network as "password set".
- **`network.json` is now written 0600** (it can hold passphrases; it used to be 0644). The staged copy and the
  snapshot kept for a revert (`confirm.json`, already 0600) follow the same rule.
- **Adapter facts the page needs** (`internal/wireless`): `max_ap` (how many Wi-Fi networks an adapter can run at
  once, from its "valid interface combinations": 1 on the dev laptop's Intel adapter) and per band `ht`, `vht`,
  `he` (802.11n, ac, ax).

### Wi-Fi networks: translator and the wifi service (stage 2)

- **One hostapd process for every adapter**, because band steering needs it: the 2.4 GHz side refuses a device
  that has already been seen on the 5 GHz side, and hostapd can only look that up inside one process. The
  translator writes one file per switched-on adapter that has networks to serve, `/etc/peakroute/hostapd/<adapter>.conf`
  (mode 0600: it holds the passphrase), and `peakroute-wifi.service` starts `hostapd` with all of them through
  `/usr/lib/peakroute/hostapd-run` (`internal/scripts/hostapd-run.sh`, copied and enabled by `buildISO.sh`; **the
  ISO needs a rebuild**). The unit only runs while a configuration exists (`ConditionPathExistsGlob`), starts after
  `networking`, and is `Type=forking` with `hostapd -B`, so a configuration hostapd rejects makes the restart, and
  therefore the commit, fail and roll back. The script leaves out a file whose adapter is not present, and fails
  if none is, so a commit cannot report success for an access point that never started. It also runs
  `rfkill unblock wifi`.
- **What a file holds** (`translator/wifi.go`): the radio (`interface`, `country_code`, `ieee80211d`,
  `hw_mode` g or a, `channel` or 0 for automatic, with `acs_exclude_dfs=1` on 5 GHz so an automatic choice avoids
  the radar-wait channels, `ieee80211h=1` on 5 GHz); the generations the adapter supports, **written from the flags
  stored with the radio** (`ht`, `vht`, `he`, read from the adapter when it was set up, because hostapd refuses a
  setting the hardware lacks): HT40 as `[HT40+]` or `[HT40-]` from the channel, VHT and HE width codes with the
  centre channel of the 80 or 160 MHz block; then one section per Wi-Fi network: `ssid`, `bridge=<the local
  network's bridge>`, WPA2 (`WPA-PSK`), WPA3 (`SAE`, management frame protection required) or both (`WPA-PSK SAE`,
  protection optional for the older devices), `ignore_broadcast_ssid`, `ap_isolate`, and 802.11k/v
  (`rrm_neighbor_report`, `bss_transition`) so devices can be told where to roam. The first network on an adapter
  uses the adapter's own interface, the next ones `bss=<adapter>_1`, `_2`...
- **Band steering** is `no_probe_resp_if_seen_on` and `no_auth_if_seen_on` on the 2.4 GHz side of a network that
  is also on 5 GHz, pointing at that network's 5 GHz interface (found per network, so it is right when an adapter
  serves several). Devices that only do 2.4 GHz are never seen on 5 GHz and are unaffected. Both sides use the same
  name and security, which is what lets a device treat them as one network.
- **Bridges:** an access point joins its local network's bridge through `bridge=`. A Wi-Fi network on the
  **default** network therefore forces `br0` even when only one cable port carries it (`LanTarget` and the
  interfaces file follow, and so do the firewall and DHCP files that name the interface).
- **Plan:** `wifi` is a restart, ordered after `networking` and `routes`. Restarting networking removes the bridges
  the access points joined, so **a networking restart also restarts Wi-Fi** (like the DHCP server). A new managed
  directory mechanism removes a `.conf` file there that nothing accounts for (an adapter taken out of the setup, or
  the whole Wi-Fi section removed); other files are left alone. Wi-Fi changes need the 60 second confirmation (they
  drop every Wi-Fi device, an admin's included). The review notes no longer claim a network is uncarried if a Wi-Fi
  network joins it.
- **Checked** with the real `hostapd` (v2.10) in a test that runs it on every kind of file we write and fails on
  anything it says about the configuration (it reads the whole file before it tries the adapter); the test first
  proves it can see a mistake with a bogus setting, and is skipped where hostapd is not installed. What a unit
  test cannot show: whether a particular adapter accepts the channel, width and band-steering settings at runtime.
  That needs the hardware.

**Next, in order:** the config model for Wi-Fi networks (an SSID tied to a Local Network, so a Guest SSID lands
on the Guest VLAN: name, WPA2/WPA3 passphrase, band, channel, width), the translator for hostapd, the country
setting, then AP mode (the limited GUI and a translator that bridges everything to one port).

### Wi-Fi networks: the API (stage 3)

- `netapi/wifi.go`. Every change is staged like the other editors (nothing touches the running box until the
  review and commit). The adapters' capabilities come from an injected `Wireless` function (the same detector the
  Wireless page uses), so the API refuses what the hardware cannot do instead of leaving it to hostapd.
- **Adapter settings** (`PUT /api/v1/wifi/radios/{name}`) are checked against that report: the adapter must exist,
  be able to act as an access point and have readable capabilities; the band must be one it has; the width one of
  the widths it offers for that band; the channel one that is not `disabled`. A channel marked `no_ir` or DFS is
  *allowed*, because whether it can start an access point depends on the country, which may not be chosen yet; the
  commit-time check (config `validateWifi`) is where it is refused. `ht`/`vht`/`he` are copied from the band's
  capabilities into the stored radio, so the translator never needs the hardware.
- **The number of Wi-Fi networks per adapter** is capped by `max_ap` from the adapter's interface combinations
  (stated by `iw`); adding a network or changing its bands that would exceed it is refused with a message on `bands`.
- **Errors are scoped** to the thing being edited (`errorsForWifi`): a problem with another Wi-Fi network, the
  country or "no adapter serves this band" does not block saving this one; the commit gate still catches them.
- **Passwords** are returned to the logged-in session (the form has a reveal button) but never appear in the
  review lines. On `PUT`, leaving `passphrase` out keeps the stored one.
- A Local Network cannot be deleted while a Wi-Fi network joins it.

### Wireless page: the Wi-Fi editors (stage 4)

- The page (`wireless.js`) has three parts, top to bottom: a note (the country in use, with a link to Settings, or a
  warning that none is set; the regulatory domain in effect), **Wi-Fi networks** (a dropdown with one network on show, like Local Networks, plus *Add Wi-Fi network*),
  and one card per adapter. An adapter that can be an access point gets an **Access point** section: a switch, the
  band, the channel (Automatic or one of the channels the adapter reports) and the width (what the adapter offers
  for that band). Changing the band resets the channel to Automatic and the width to the usual choice (20 MHz on
  2.4 GHz, up to 80 MHz on 5 GHz). Channels the country in effect has closed are still listed, marked as closed:
  the country being edited is only a draft, so the page cannot know yet.
- **The country is not set on this page** (decided 2026-10-10): it is a router-wide setting, kept in `network.json` as
  top-level `country` (so it still goes through review and commit) and edited on **System, Settings**
  (`settings.js`; `GET /api/v1/settings`, `PUT /api/v1/settings/country`). Wi-Fi only *uses* it: `country_code` in
  the hostapd files, and the commit is refused (error on `country`) when Wi-Fi is on without one. The review shows it
  as "Country", not as a Wi-Fi line. A change of country alone touches no file and restarts nothing: the review
  says the system already matches. Anything else that needs a regulatory country later can read the same setting.
  Not done: setting the kernel's regulatory domain (`iw reg set`) when no Wi-Fi is running, so the channels listed
  on the Wireless page only open up once Wi-Fi has started with the country.
- One adapter serves **one band at a time** (the stored radio has a single band); a dual-band adapter is used for
  2.4 GHz *or* 5 GHz. Serving both bands needs two adapters. A Wi-Fi network that ticks a band no adapter is
  switched on for shows a warning, and the commit refuses it (`wifi.coverage`).
- A Wi-Fi network has: name, password (hidden, with a Show button), security (WPA2 + WPA3, WPA3 only or WPA2 only),
  the Local Network it joins, bands, band steering (only offered when both bands are ticked, switched on when the
  second band is ticked), switched on, hidden name and client isolation. Typing does not redraw the card (the focus
  would be lost); the 10 s refresh only redraws when the list of adapters changed.
- With no adapter the page is greyed with its message; with adapters but none able to be an access point, the Wi-Fi
  networks card says so and cannot add a network.
- There is no list of connected devices here: that is what the Clients page is for (see below).

### Clients: wired or wireless, and online or offline (added 2026-10-10)

- **Wi-Fi stations:** `iw dev` lists the interfaces; every one in AP mode is asked `iw dev <if> station dump`
  (`wireless/stations.go`). hostapd's extra networks are interfaces of their own (`wlan0_1`...), so each station
  carries its own network name, and the band comes from the interface's channel frequency. A device that is still
  in the password exchange (`authorized: no`) is left out. A short cache (`StationCache`, 2 s) stands between the
  pages and `iw`.
- **Who is here** (`internal/presence`): a lease outlives the device (it is held until it expires), so the lease
  list alone cannot say whether a device is online. The `Tracker` does a round every 20 s: devices on a Wi-Fi
  station list are online; every other lease is asked with **ARP** (`ARPProber`, an `AF_PACKET` raw socket: a
  "who has this address?" broadcast on the interface whose subnet holds the address, two tries of 600 ms). ARP is
  used instead of ping because every connected device must answer it, while phones and firewalled PCs do not
  answer a ping. A reply only counts if it comes from the leased IP **and** the leased MAC. A device counts as
  online for 75 s after its last answer.
- **What is remembered** (`client_presence.json` in the config directory, 0600, forgotten after 60 days): when each
  device was last seen and how it was connected (Wi-Fi with network and band, or wired). That is what lets an offline
  device keep its Wi-Fi or Ethernet icon and a "last seen" time. A newly seen device or a changed connection is
  saved at once; when only times moved the file is rewritten every 5 minutes (flash storage), and once more on
  shutdown.
- **Lease API** (`GET /api/v1/dhcp/leases`) gains `connection`, `ssid`, `band`, `status` and `last_seen_epoch`.
  `status` is `online`, `offline` or `unknown`. **`unknown` is used whenever the answer cannot be given**: ARP needs
  `CAP_NET_RAW` (the daemon has it as root; the dev server usually does not), so without it only Wi-Fi devices
  are known online and the rest are `unknown`, never `offline`. Before the first round nothing is called offline.
- **Clients page:** a Status column first (a green dot and Online, or a grey dot, Offline and "Last seen 2 hours
  ago"; offline rows are dimmed and listed after the online ones), the Connection icon column (Wi-Fi in blue, with
  network and band in its tooltip, or an Ethernet plug; a dash when unknown), and the header counts
  "N online, M offline".
- **Connection when the Wi-Fi side cannot be read** (`iw` missing but an adapter present): never "wired" for a
  device that answers; shown as unknown.
- **Not built:** naming the switch port a wired device is on (needs the bridge forwarding table), kicking a device
  off, per-device limits, 802.11k/v/r roaming hints, and showing the status on the Overview's client table.

## 7b. Navigation (decided 2026-10-08)

An OPNsense-style sidebar (grouped by function, quick search, clear two-level navigation) holding
UniFi-style object pages. At most two levels: top-level entries, and groups with pages in them.

| Entry | Page | Built |
|---|---|---|
| Overview | system health, interface counters, top clients | yes (`overview`) |
| Clients | DHCP leases and per-client usage | yes (`dhcp`) |
| Traffic | per-port throughput history | yes (`traffic`) |
| Network: Internet, **Local Networks**, Ports, Wireless, Routes | WAN uplink; each local network with its address range *and* DHCP; physical ports; static routes | Internet (`internet`), Local Networks (`networks`), Ports (`network-physical`), Wireless (`wireless`) and Routes (`routes`) |
| Firewall: Rules, Port forwarding, Connections | | planned: design in `docs/firewall.md` |
| Services: DNS, VPN, Dynamic DNS | | planned |
| System: Settings, Administrators, Certificates, Backup and restore, Updates, Logs, Diagnostics | | Logs only (`logs`) |

- **Planned pages are listed but greyed out and not clickable** (a `<span class="nav-item nav-soon">`, no
  `data-view`), because the whole product will be built before it is used. To hide them instead, add
  `.nav-soon { display: none; }` to the stylesheet (the groups then only show real pages).
- **Names are confirmed** (friendly, not technical) and live only in `dashboard.html`: friendly (Internet, Local Networks, Ports, Clients)
  rather than technical (WAN, Interfaces, Devices, DHCP). The view ids behind them did not change.
- **Quick search** filters entries (a group's own name matches all its pages), opens matching groups, shows
  "No matches", and on clearing opens the group of the page you are on. **Accordion:** opening a group by
  clicking closes the others; groups the page opens itself (navigation, search) are handled separately so the
  two do not fight.
- **Local Networks** (view id `networks`; called Networks until 2026-10-08) is the first editor page (9e): one card per network (router address, ports, DHCP), so VLANs
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

### Overview page (redesigned 2026-10-08)

Top to bottom: the header (no status badge: the old fixed "System Online" pill was removed because it only
existed on this page, and the Internet card already shows the state); the **device card**; then **Internet** and **Network** cards side by side (one column on a narrow
screen); then the system resource cards (CPU, memory, disk, firewall states) and the Top Clients table.
The old Network Interfaces table is gone: per-port counters live on Traffic, roles and link state on
Ports.
- **Device card** (the former system-details bar, now at the top): one card, the hostname as its title
  and a vertical list of specs under it (PeakRoute version, uptime, load average, processor,
  architecture, kernel), a label on the left and the value beside it. The DNS resolvers are not shown:
  `/etc/resolv.conf` lists a local stub on a developer machine (and will list dnsmasq on the appliance),
  and the upstream servers are on the Internet page. The element ids (`hostname-val`, `app-version-val`,
  ...) did not change, so `telemetry.js` fills it as before.
- **Internet card** (everything about the internet, its "Details" link goes to the Internet page): state,
  latency, gateway latency, uptime 24 h and the diagnosis sentence when there is one; the WAN port, link,
  address, gateway and live download/upload rates; and a one-hour sparkline (latency line, red columns for
  lost minutes, min to max under it). Health comes from the health endpoint every 10 s (`overview_cards.js`).
- **Local Networks card** ("Details" goes to the Local Networks page): the LAN ports, router address as
  CIDR, client count and how much of the DHCP range is leased ("34 of 101", orange from 90 %). Addresses
  come from `/api/v1/uplinks` and `/api/v1/networks` every 30 s. The WAN rates and the client count are not
  fetched again: `telemetry.js` already polls stats and leases every second and passes them to
  `OverviewCards.onStats/onLeases`.
- `formatMs`, `formatPercent` and `formatRate` are shared helpers in `app.js` (Overview and Internet).
- **Gotcha when testing:** a dev server that serves the static files from disk makes browsers cache
  scripts heuristically; reload with the cache bypassed (or `fetch(url, {cache: 'reload'})`) or the page
  keeps running the old script. The real daemon serves embedded files, which carry no modification time.

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
| `GET /api/v1/wireless` | session | `adapters` (`name`, `phy`, `mac`, `driver`, `state`, `capabilities_known`, `ap_capable`, `modes`, `bands`: `name`, `widths`, `channels` with `number`, `mhz`, `disabled`, `no_ir`, `dfs`), `iw_available`, `country`, `country_known` |
| `GET /api/v1/routes` | session | `routes` (the draft: `name`, `destination`, `gateway`, `metric`, `enabled`), `pending`, `editable`, and `table`: the kernel's routing table in effect (`destination`, `gateway`, `interface`, `metric`, `kind`: default, connected, static (one of ours, matched against the *active* config) or via) |
| `PUT /api/v1/routes` | session + CSRF | `{"routes": [...]}` replaces the whole list in the draft (`enabled` omitted means on); field errors come back as `routes[2].gateway` and so on |
| `GET /api/v1/uplinks`, `GET /api/v1/uplinks/{id}` | session | the internet connection (today one, id `wan`): `port`, `mode` (`dhcp`, `static` or `pppoe`), `ip`, `netmask`, `gateway`, `dns`, `vlan`, `mtu`, `usual_mtu`, `mac`, `hostname`, `pppoe` (`username`, `service`, `password_set`: never the password), `device`, `pending`, and `live` (what is in effect: `up`, `address`, `gateway`, `dns`, `device`, `mac`, `mtu`) |
| `PUT /api/v1/uplinks/{id}` | session + CSRF | `mode` and, as needed, static `ip`, `netmask`, `gateway`, `dns`; `vlan`, `mtu`, `mac`, `hostname`; `pppoe` (`username`, `password`, `service`); stages a draft. Fields left out stay; the old type's settings are dropped. A static address also requires a gateway and one DNS server; the PPPoE password is write-only |
| `GET /api/v1/uplinks/{id}/health?range=5m\|1h\|24h\|30d` | session | `status` (the DNS series' `state`, `target`, `source`, `rtt_ms`, plus `method`, `gateway` (the same fields for the gateway) and a `diagnosis` sentence), `availability` (`uptime_24h`, `uptime_30d` in percent or null, `outage_count`, `outages`: newest first, at most 20, each `start` ms, `seconds`, `ongoing`; independent of the charted range), and two series, `dns` and `gateway`, each with `summary` (sent, lost, loss %, avg/min/max ms) and parallel arrays `timestamps` (ms), `avg_ms`, `min_ms`, `max_ms` (null when every probe in the point was lost), `loss_pct` |
| `GET /api/v1/system/logs?limit=` | session | systemd journal |
| `GET /api/v1/dhcp/leases` | session | dnsmasq leases with per-client usage, how each device is connected (`connection`: `wifi`, `wired` or `unknown`; `ssid` and `band` for Wi-Fi) and whether it is here (`status`: `online`, `offline` or `unknown`; `last_seen_epoch`, 0 if never) |
| `POST /api/v1/cert/upload` | session + `X-CSRF-Token` | multipart `cert` and `key` |
| `GET /api/v1/config/status` | session | `state` (idle, applying, awaiting_confirm), `has_changes`, `change_count`, `confirm.seconds_left`, `last` outcome; polled by the banner |
| `GET /api/v1/config/staged` | session | the review: `changes`, `valid`, per-field `errors`, `plan` (files and service actions), `notes`, `needs_confirm`, `confirm_seconds` |
| `POST /api/v1/config/commit` | session + CSRF | 202 once applying has started (poll status); 400 with per-field `errors`; 409 if busy or nothing staged |
| `POST /api/v1/config/confirm` | session + CSRF | keep the change; 409 if nothing is waiting |
| `POST /api/v1/config/revert` | session + CSRF | undo the change waiting for confirmation now (202) |
| `POST /api/v1/config/discard` | session + CSRF | drop the staged changes; 409 while applying or awaiting confirmation |
| `GET /api/v1/ports` | session | physical ports with hardware details, `role` (wan, lan, unassigned) from the draft, `active_role`, `pending`, `present` (false: configured but the hardware is gone), `editable`; for LAN ports `native` (the id of the untagged network) and `tagged` (ids); and `networks`: what a LAN port can carry (`id`, `name`, `vlan`, `default`) |
| `PUT /api/v1/ports` | session + CSRF | `{"assignments": {"eth0": "wan", "eth1": "lan", "eth2": "unassigned"}, "networks": {"eth1": {"native": "lan", "tagged": ["net2"]}}}` stages the roles as one atomic change; ports not mentioned keep their role, and a LAN port not named under `networks` keeps its networks; the result must have exactly one WAN and at least one LAN; an entry that says only the default network is dropped; a port that stops being a LAN port loses its networks |
| `GET /api/v1/firewall` | session | `tabs`, `rules` (with `text`, `pending`, `shadowed_by`), `forwards`, `policy` (the fixed lines per tab), `editable` |
| `GET /api/v1/firewall/connections` | session | the connection table: `known`, `count`, `max`, `accounting`, `total`, `shown`, `connections` (see `docs/firewall.md`); `?limit=` 1 to 5000 |
| `GET /api/v1/firewall/counters` | session | `known`, `counters` by rule or forward id: `packets`, `bytes` |
| `POST /api/v1/firewall/rules`, `PUT`/`DELETE /api/v1/firewall/rules/{id}`, `PUT /api/v1/firewall/order` | session + CSRF | rules and their order within a tab (see `docs/firewall.md`) |
| `POST /api/v1/firewall/forwards`, `PUT`/`DELETE /api/v1/firewall/forwards/{id}` | session + CSRF | port forwards |
| `GET /api/v1/settings` | session | `country` (the draft), `country_pending`, `countries` (`code`, `name`), `editable` |
| `PUT /api/v1/settings/country` | session + CSRF | `{"country": "US"}`; an empty string clears it (committing Wi-Fi without a country is refused) |
| `GET /api/v1/wifi` | session | `country` (read only: it is set under settings), `radios` (`name`, `band`, `channel` (0: automatic), `width`, `enabled`, `ht`, `vht`, `he`, `pending`), `networks` (`id` `wifi1`..., `ssid`, `passphrase`, `network`, `security`, `bands`, `band_steering`, `hidden`, `client_isolation`, `enabled`, `pending`), `editable` |
| `PUT /api/v1/wifi/radios/{name}` | session + CSRF | `{"band", "channel", "width", "enabled"}` checked against the adapter's capabilities; `DELETE` stops using the adapter |
| `POST /api/v1/wifi/networks` | session + CSRF | `{"ssid", "passphrase", "bands": [...], "network", "security", "band_steering", "hidden", "client_isolation", "enabled"}`; left out: security `wpa2-wpa3`, network `lan`, enabled, band steering on when both bands are chosen; answers 201 |
| `PUT /api/v1/wifi/networks/{id}`, `DELETE ...` | session + CSRF | fields left out stay as they are (also the passphrase); `DELETE` stages the removal and answers with the whole Wi-Fi setup |
| `GET /api/v1/networks`, `GET /api/v1/networks/{id}` | session | the networks, the default one (id `lan`) first: `name`, `default`, `vlan` (1 for the default), `ip`, `netmask`, `ports` (carried untagged), `tagged_ports`, `bridged`, `dhcp` (null if none), `internet`, `isolated`, `pending` |
| `POST /api/v1/networks` | session + CSRF | adds a network: `{"name", "vlan", "ip", "netmask", "dhcp": {...}, "internet", "isolated"}`; left out, `internet` is false and `isolated` true and there is no DHCP; the id (`net2`, `net3`...) is chosen by the server; answers 201 with the network |
| `PUT /api/v1/networks/{id}` | session + CSRF | `{"ip", "netmask"}` always, and `name`, `vlan`, `dhcp`, `internet`, `isolated` only if they change (a field left out stays); the default network's name, VLAN and internet access cannot be changed |
| `DELETE /api/v1/networks/{id}` | session + CSRF | stages the removal and answers with the list; refused for the default network and while a port still carries the network untagged (a network that is only tagged is taken off those ports) |
| `GET /api/v1/routes` | session | `routes` (the draft: `name`, `destination`, `gateway`, `metric`, `enabled`), `pending`, `editable`, and `table`: the kernel's routing table in effect (`destination`, `gateway`, `interface`, `metric`, `kind`: default, connected, static (one of ours, matched against the *active* config) or via) |
| `PUT /api/v1/routes` | session + CSRF | `{"routes": [...]}` replaces the whole list in the draft (`enabled` omitted means on); field errors come back as `routes[2].gateway` and so on |
| `GET /api/v1/uplinks`, `GET /api/v1/uplinks/{id}` | session | the internet connection (today one, id `wan`): `port`, `mode` (`dhcp`, `static` or `pppoe`), `ip`, `netmask`, `gateway`, `dns`, `vlan`, `mtu`, `usual_mtu`, `mac`, `hostname`, `pppoe` (`username`, `service`, `password_set`: never the password), `device`, `pending`, and `live` (what is in effect: `up`, `address`, `gateway`, `dns`, `device`, `mac`, `mtu`) |
| `PUT /api/v1/uplinks/{id}` | session + CSRF | `mode` and, as needed, static `ip`, `netmask`, `gateway`, `dns`; `vlan`, `mtu`, `mac`, `hostname`; `pppoe` (`username`, `password`, `service`); stages a draft. Fields left out stay; the old type's settings are dropped. A static address also requires a gateway and one DNS server; the PPPoE password is write-only |
| `GET /api/v1/uplinks/{id}/health?range=5m\|1h\|24h\|30d` | session | `status` (the DNS series' `state`, `target`, `source`, `rtt_ms`, plus `method`, `gateway` (the same fields for the gateway) and a `diagnosis` sentence), `availability` (`uptime_24h`, `uptime_30d` in percent or null, `outage_count`, `outages`: newest first, at most 20, each `start` ms, `seconds`, `ongoing`; independent of the charted range), and two series, `dns` and `gateway`, each with `summary` (sent, lost, loss %, avg/min/max ms) and parallel arrays `timestamps` (ms), `avg_ms`, `min_ms`, `max_ms` (null when every probe in the point was lost), `loss_pct` |
| `GET /api/v1/system/logs?limit=` | session | systemd journal |
| `GET /api/v1/dhcp/leases` | session | dnsmasq leases with per-client usage, how each device is connected (`connection`: `wifi`, `wired` or `unknown`; `ssid` and `band` for Wi-Fi) and whether it is here (`status`: `online`, `offline` or `unknown`; `last_seen_epoch`, 0 if never) |
| `POST /api/v1/cert/upload` | session + `X-CSRF-Token` | multipart `cert` and `key` |
| `GET /api/v1/config/status` | session | `state` (idle, applying, awaiting_confirm), `has_changes`, `change_count`, `confirm.seconds_left`, `last` outcome; polled by the banner |
| `GET /api/v1/config/staged` | session | the review: `changes`, `valid`, per-field `errors`, `plan` (files and service actions), `notes`, `needs_confirm`, `confirm_seconds` |
| `POST /api/v1/config/commit` | session + CSRF | 202 once applying has started (poll status); 400 with per-field `errors`; 409 if busy or nothing staged |
| `POST /api/v1/config/confirm` | session + CSRF | keep the change; 409 if nothing is waiting |
| `POST /api/v1/config/revert` | session + CSRF | undo the change waiting for confirmation now (202) |
| `POST /api/v1/config/discard` | session + CSRF | drop the staged changes; 409 while applying or awaiting confirmation |
| `GET /api/v1/ports` | session | physical ports with hardware details, `role` (wan, lan, unassigned) from the draft, `active_role`, `pending`, `present` (false: configured but the hardware is gone), `editable` |
| `PUT /api/v1/ports` | session + CSRF | `{"assignments": {"eth0": "wan", "eth1": "lan", "eth2": "unassigned"}}` stages the roles as one atomic change; ports not mentioned keep their role; the result must have exactly one WAN and at least one LAN |

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
| 2026-10-08 | The gateway is measured as a second series, only with real pings | tells ISP problems from local ones; a DNS query to a gateway is not a valid test |
| 2026-10-08 | Uptime is a share of observed minutes, and an outage is whole minutes of total loss on the DNS series | missing data (daemon down) must not read as downtime, and a gateway that ignores pings must not read as an outage |
| 2026-10-08 | "Networks" renamed "Local Networks" (menu, page title, Overview card); all WAN facts live in the Overview's Internet card | the Network group also holds Internet, so "Networks" was ambiguous; each card should lead to the page about its subject |
| 2026-10-08 | Static routes are applied by an if-up.d script tagged proto 77 rather than lines in /etc/network/interfaces, and still need the 60 s confirmation | no networking restart for a route change, routes survive reboots and restarts, only our own routes are ever removed; a wrong route can still lock out an admin on another network |
| 2026-10-08 | Overview: one device card first (hostname and a vertical list of specs, no DNS), then Internet and Network cards; the interfaces table is gone | the landing page should say what this box is, then whether it is working and what is connected |
| 2026-10-08 | WAN usage reuses the stats and traffic endpoints instead of a new one | the data is already collected and served; one less thing to keep in step |
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

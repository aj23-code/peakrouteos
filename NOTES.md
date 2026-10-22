# PeakRoute OS: Working Notes

Living notes for the eventual documentation. Updated with every meaningful change, so it
records what the system does, why it is built that way, and what was learned on the way.
`TODO.md` is the work list; this file is the knowledge. Last updated: 2026-10-20.

> Status: alpha. A router appliance OS (Debian base) with a Go daemon and web UI. The daemon is
> a monitoring dashboard plus a safe configuration pipeline that is being built (see section 7).

## 1. What it is

- **Image:** a raw disk image, `peakroute-os.img`, built by `buildImage.sh` (Debian 13 Trixie, native `debootstrap` on
  Ubuntu/Debian, checksum-pinned Debian keyring) and written to the machine's disk like OpenWrt's (`dd`). **No installer, no live
  ISO, no console setup** (decided 2026-10-12): the first thing seen is the welcome page of the web interface. See 7t and
  `docs/image.md`. Booting works by UEFI and legacy BIOS.
- **First start:** the daemon builds the setup network (192.168.77.1, 7r) and the person sets everything up on the welcome page
  (`docs/setup.md`); the administrator made there is the console login too. Root is locked (no default password; no password of any kind logs in as root).
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
| `internal/network` | physical interface discovery, DHCP leases, per-device usage (nftables counters, `usage.go`) |
| `internal/cert` | the web interface's certificate: built-in (own web authority) or uploaded; renewal; the Certificates API |
| `web/` | HTML templates, CSS, JS (no framework; one module per view) |
| `buildImage.sh`, `full_build.sh`, `image/` | the disk image (`full_build.sh` builds the binary then the image; `image/overlay` holds the files of the image, `image/run-qemu.sh` tries it in a virtual machine) |

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
- **First-run setup** is the wizard of `internal/setup` (7r). The account and the `configured` flag are made by
  `Manager.SetupAdmin` atomically under the lock (flag written last and durably); only one of several concurrent
  requests wins. `Manager.MarkConfigured` is the flag alone, for a first-run restore.
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
  on the server (the administrator page of the wizard).
- **TLS:** the certificate is made by the router's own web authority for its names and addresses (docs/certificates.md), or uploaded; served without a restart.

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
- **Clients menu and DHCP page (2026-10-21):** *Clients* (the lease list, `view-dhcp`, `leases.js`, unchanged) is in the **Network** menu, after Ports, and a new *DHCP* page (`view-dhcp-server`, `dhcp_server.js`, `DHCPServerModule`) is in **Services**, beside DNS (first built as a group of its own, moved the same day), because the DHCP server's settings were only reachable inside each network's card on Local Networks. The DHCP page has one card per network (default LAN and each VLAN) with the on/off switch, first and last address and lease time, saved to the draft through the same `PUT /api/v1/networks/{id}` as Local Networks (the page sends the network's address, netmask, isolation, name, VLAN and internet flag back as they are, plus its `dhcp` object; errors map from `dhcp.range_start` etc. to the fields). Both pages edit the same settings; each reloads from the server on opening. No new endpoint. Static address reservations do not exist yet (the old Clients subtitle claimed "static assignments"): the DHCP page is where they would go. Checked: the page and script are served, the PUT is accepted and a range outside the subnet is refused on the right field (against `-dev`); not looked at in a browser.
- **Network on the Clients page (2026-10-21):** each lease from `GET /api/v1/dhcp/leases` has a `network` (the name of the local network whose subnet holds its address: the default is "LAN", the others by their names; empty when none), filled by `network.LabelNetworks` from the *committed* configuration in `main.go`, and shown as a chip beside the connection icon. A draft network shows once committed. Tested with several subnets (a /22 included).
- **Clients page header (2026-10-21):** the card title "All Active DHCP Leases" is gone (the page and menu already say it); the header row holds the online/offline count (no line under it any more: devices with an address of their own are listed as Static, see below) (heading size: green "N online", grey "N offline"; words as well as colours) and, when two or more networks have devices, a network filter (`lease-network`, kept across the 3-second refresh; its options are rebuilt only when the set of networks changes so an open list is not closed).
- **`-dev` presence (2026-10-21):** a laptop cannot ARP-probe the devices (raw socket) and the leases are samples, so in `-dev` the presence tracker's `Probe` is replaced by a stand-in that says every device but the last answers; the Clients page then shows both Online and Offline. Production is unchanged.
- **Reserved addresses (2026-10-21; docs/dhcp.md):** a router-side fixed address for a device (dnsmasq `dhcp-host=mac,ip[,name]`). `config.Reservation{MAC, IP, Name}` in `NetworkConfig.Reservations` (`network.json` `reservations`); validation in `config/reservations.go` (MAC normalised and not a group address and unique, IP a host address inside one of the networks, not the router's, unique, **that network's DHCP server on**, name a DNS label, unique; fields `reservations[i].mac|ip|name`); `diffReservations` (keyed by MAC); rendered after the ranges by `writeReservations` in both dnsmasq renderers (no reservations: the file is unchanged), `ensureSafeReservations` rechecks; a change restarts only dnsmasq. `GET`/`PUT /api/v1/dhcp/reservations` replace the whole draft list like the routes (`netapi/reservations.go`, network named by subnet). `ClientLease` gained `reserved` and `reservation` (from the *committed* config, `network.LabelNetworks`). UI: a "Reserved addresses" card on the DHCP page (pick a connected device, add by hand, per-row errors, draft tag) and a Reserve link on each Clients row (`DHCPServerModule.reserve`). A network edit that would strand a reservation (its DHCP server switched off, its subnet moved, the network deleted) is refused when it is made, naming the address and saying to change it on the DHCP page first (`withReservations` in `netapi/networks.go`; the delete lists the blockers like the ports and rules do). Checked against `-dev` end to end (save, review, commit writes the lines, leases flagged); not looked at in a browser, not on hardware.
- **Reserve / Unreserve on the Clients page (2026-10-21):** each row says "(reserved)" after the address (the committed state), with an **Unreserve** link that takes the device out of the draft list (`PUT /api/v1/dhcp/reservations` without it; a message box above the card says what happened); a change that is only in the draft shows in the same brackets: "(reserved, draft)" for one just added, "(reserved, removed in draft)" and "(reserved, address changed in draft)". The server marks each lease with `reserve_change` (`network.MarkReservationChanges`, active against draft). A device with none shows the **Reserve** link. **Reserve** opens the DHCP page with a new row added; the Reserved addresses card is below the network cards, so the page scrolls the row to the middle of the screen (smooth), outlines it in blue until it is saved, and puts the cursor in the first empty field (name, hardware address, address) or on Save when the row is complete.
- **The page is in the address (2026-10-21):** `loadView` (app.js) pushes `#<view>` (e.g. `#dhcp-server`) when a page is opened, so a refresh, a bookmark and the browser's Back and Forward buttons come back to it; the first load uses the page the address names, else the home page (overview, or the access point's page). Only pages that exist and are in the menu count (`allowedView`; Traffic is allowed on a router though it has no menu entry): anything else falls back to home. A bare `#` (a `href="#"` link that did nothing) is ignored; `popstate` and `hashchange` both go through one handler that leaves the page already showing alone. Checked in the built-in browser (see below).
- **Looking at the pages in a browser (2026-10-21):** the built-in browser refuses the dev server's self-signed certificate and the Chrome extension was not connected, so a 30-line stand-in (an HTTP proxy on `localhost:8080` in front of `https://localhost:8443`, certificate unchecked, `Secure` dropped from the cookie, the scratch folder only) was used. Seen and working: login, Overview with the Traffic history link, `#dhcp` (Clients: network chips, online/offline count, filter, Reserve, "(reserved, draft)", Unreserve), Reserve opening the DHCP page scrolled to the new outlined row, Save to draft (Draft tag, banner), address-bar navigation. Fixed from what was seen: links in subtitles were dark blue on dark (`.subtitle a`), the outline touched the explanation, the save message was off screen at the top. Screenshots lag the action by a second or two: wait before judging.
- **Static devices on the Clients page (2026-10-21):** besides the leases the list has the devices in the router's IPv4 neighbour table (`network.ReadNeighbours`: `ip -j neigh show`, completed entries only) that have no lease (`network.StaticClients`: in one of the router's networks, not the router, hardware address and address not in the leases), marked `static` with `expiry_text` "Static" and no host name; `main.go`'s `clientList` joins them and also feeds the presence tracker's targets so their Online/Offline is probed like the others. A neighbour whose interface (`translator.NetworkOfInterface`) is another network than its address's gets `seen_on` (amber "Seen on X": the wrong VLAN or a wrong fixed address). `LeaseHandler` now takes a source function. `-dev` has two sample neighbours (one seen on the wrong network). A device the router has not exchanged ARP with lately is not in the table, so a quiet static device can drop off and come back.
- **Service limits instead of a privilege split (2026-10-21, TODO #21):** `peakroute.service` has `NoNewPrivileges`, `ProtectKernelModules`/`Logs`/`ControlGroups`/`Clock`, `RestrictRealtime`/`SUIDSGID`/`Namespaces`, `LockPersonality`, `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK AF_PACKET`, `SystemCallArchitectures=native` and a `CapabilityBoundingSet` that removes the capabilities a router never uses. Reasons and what was left out (a `ProtectSystem=strict` filesystem sandbox, which needs a test boot) are in docs/image.md. Checked with `systemd-analyze verify` and `security --offline` (9.4 -> 6.2); not booted. Children inherit the limits (`ip`, `nft`, `dnsmasq`, `openvpn`, `wg`, `iw`, the shell scripts): address families and capabilities were chosen to include everything they use (`AF_PACKET` for the ARP probe and the DHCP server, netlink for `ip`, `iw`, `nft`). If a feature stops working on a device after this, `journalctl -u peakroute` and the first thing to loosen is `RestrictAddressFamilies` or `CapabilityBoundingSet`.
- **Disk card lists each partition (2026-10-21):** the Overview's disk bar showed only `/` (the slot, about 3 GB on the image, which says nothing about the space the router really has). `system.Stats` has `disks` (`getDisks`): **Root** (`/`) and **Data** (`/data`, only when it is a file system of its own: a different `statfs` id than `/`, so a development machine shows just the root), each with its size, use and a bar (`telemetry.js`, `#disk-list`); `disk` is kept as the root. The Top Clients table calls a device with no host name "Static device" when it is a static one. Checked in the browser with the single-partition case (the laptop has no `/data`); the two-partition case is only unit tested (`disks_test.go`).
- **The WireGuard check before a commit (2026-10-22, found by the first CI run of the release workflow):** `wgCheckCommand` ran `wg-quick strip "$f" | wg setconf wgchk /dev/stdin || exit 1`, and in a pipeline only the last command's result counts: a tunnel file wg-quick could not read or understand gave `wg setconf` empty input, which it accepts, so the broken file passed. It now takes wg-quick's output first (`conf=$(wg-quick strip "$f") || exit 1`) and pipes it on. `wgcheck_test.go` tests the rule with stand-in `wg-quick`, `wg` and `ip` (checked: it fails against the old script), and the test with the real tools (`vpn_e2e_test.go`) skips when the user namespace of the machine cannot read a file it has just written (the Ubuntu 24.04 runners restrict them: `apparmor_restrict_unprivileged_userns=1`, as this laptop does, which has no `wg-quick` and so skipped the test and never showed it).
- **The counting table loads itself (2026-10-22, found on the router after the first update):** usage stayed at 0 B because `/etc/nftables.conf` is only rewritten by a commit that changes something, and the file the older version had written has no `peakroute_acct` table, so `nft list table` failed and nothing was read. Same cause as the old `inet dhcp` stanza staying until the next commit: **a new version's rendered files reach a router only with its next change**. Now `UsageTracker.Ensure` (set in `main.go`: writes `translator.AccountingRuleset(cfg)` to `/run/peakroute/usage.nft` and runs `nft -f`; the ruleset starts with `add table` and `delete table`, so it replaces whatever there is) is called when the table is missing, at most once a minute, and the poll reads it straight away; an access point and a router with no internet port load nothing. A reload of the firewall that flushes the ruleset is healed the same way within a minute. Tests: a fake runner (loaded once, not every poll) and the real kernel (the table is deleted, the tracker loads it, a connection is counted). **Open design point:** nothing re-renders the system files after an update, so any future change to a rendered file needs the same treatment (a self-loading step) or a "the system files are out of date: apply" offer; considered, not built.
- **Nicknames for devices (2026-10-22; docs/dhcp.md):** `internal/clientnames` (`Store`: hardware address -> nickname, `client_names.json` in the configuration directory, 0600, written atomically before the change counts; `CleanName`: trimmed, at most 40 characters, no control characters; at most 2000; `HandleName` for `PUT /api/v1/clients/{mac}/name {"name"}`, empty removes). No draft and no commit: it is a label and changes nothing in the system. Writing is refused for read-only accounts by the session layer (state-changing methods need the admin role). `ClientLease` has `nickname` (filled in `main.go`'s lease handler). The Clients page has a pencil per row and an inline input (Enter saves, Escape cancels; the 3-second redraw is paused while typing, and `init` clears the editing state), shows the device's own name under a nickname, and the Overview's Top Clients and the Reserve prefill (a valid DNS label from the nickname, else the host name) use it. Column headers say "Name". Checked in the browser (rename, redraw paused, saved file 0600, Overview). **Not in the backup** (`backup.Collect` lists its files explicitly; adding one needs the manifest section and the restore path, not done).
- **Traffic page (2026-10-21):** no longer a top-level sidebar entry (the user saw no use for it there). The page (`view-traffic`, `traffic.js`: per-interface download/upload charts, 5 minutes to 30 days) is unchanged and is reached by a "Traffic history" link on the Overview's internet card and on the Ports page (`data-goto="traffic"`, as the firewall logs are reached). The Internet page keeps its own usage table. To bring the entry back, restore the `nav-top` link in `dashboard.html`.
- **Per-device usage** (`network/usage.go`; TODO #22, rebuilt 2026-10-21): counted by the kernel, read by the daemon. The nftables file ends with `table inet peakroute_acct` (`translator.renderAccounting`): four sets (`up4`, `down4`, `up6`, `down6`; `flags dynamic,timeout; timeout 10m; size 8192; counter`) and a forward-hook chain at priority 10 (after the firewall, so a dropped packet is not counted; before the masquerade, so a device still has its own address) that adds the sender of traffic going to the internet connection and the receiver of traffic coming from it; each element carries its own byte counter. `UsageTracker.Poll` (every 10 s) runs `nft -j list table inet peakroute_acct` and `ip -j neigh show`, finds each address's hardware address in the neighbour table and adds what the counter grew by (a counter lower than last time is a new one: the whole ruleset is flushed at every reload, and elements expire when idle; an address nobody knows yet is not advanced, so its bytes wait). Totals and the counters last seen are saved to `client_usage.json` (0600) about once a minute and at shutdown, so a daemon restart loses nothing and does not count the kernel's counters twice. It replaces the AF_PACKET capture (one raw socket per interface in the daemon): it covers every port, bridge and VLAN, counts IPv6, needs no capability, and survives restarts. What it counts is the traffic routed between a local network and the internet connection: traffic between local networks, to the router itself and between devices on one network is not (docs/dhcp.md does not promise it). Tests: the parsing and delta rules with a fake runner (`usage_test.go`), the ruleset text and that an access point has none (`translator/usage_test.go`), and real packets in the firewall tests' namespaces (`usage_e2e_test.go`: a device is counted, a blocked one is not, a second look adds nothing, a reload of the ruleset does not lose or double count). The earlier fix of the same day (interfaces from the ARP table) is gone with the capture.
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
  still puts things back. An undo does not re-run `sysctl` or the time zone one-liner when their file did not exist before the
  change (they read it and would report it missing: seen on the first boot of the image, where the first apply failed in
  networking and the undo listed two more errors that meant nothing). A failed `networking`, `nftables` or `dnsmasq` action
  adds the last 25 lines of the unit's journal to the step, because systemd's own message says nothing about the cause.
  Applies are serialised. Everything goes through two interfaces so tests need no
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

**What it does.** Connection type (**DHCP**, **static**, **PPPoE**), plus VLAN ID, MTU, MAC address to present and DNS
servers that replace the provider's (the DNS servers moved to the DNS page, and the host name sent with DHCP moved to System, Settings on 2026-10-11: it is now the router's `hostname`, see 7i). IPv4 only: nothing about IPv6 exists anywhere in the router yet.

- **Model** (`config/wan.go`, flat fields in `network.json`): `wan_mode` (`dhcp`/`static`/`pppoe`), `wan_vlan` (0: none),
  `wan_mtu` (0: the usual 1500, or 1492 for PPPoE), `wan_mac`, `wan_pppoe` (`username`, `password`, `service`)
  and `wan_dns`. `WanLink()` is the interface the connection runs over (`eth0`, or `eth0.835` with a VLAN) and `WanDevice()` the
  one that carries the address and the traffic (the link, or `ppp0` for PPPoE). **Everything that used to read `WanInterface`
  for traffic now reads `WanDevice()`**: the firewall, the masquerade rule, the other networks' internet rule, the gateway the
  health chart pings. `WanInterface` stays the physical port (the Ports page, the traffic charts).
- **Validation:** VLAN 1 to 4094 (and the port name must still fit 15 characters with `.vlan` added); MTU 576 to 9000 (to 1492
  with PPPoE); MAC lower case, not a group address, not all zeros; (the router's host name, an RFC 1123 label, is checked for every type and only *sent* with DHCP); PPPoE needs a
  user name and password, plain printable ASCII without quotes or backslashes (they go into a quoted option file), up to 64
  characters; PPPoE settings are refused with another type. DNS servers (up to three) are checked for every type: required with a
  static address, optional (an override) otherwise.
- **DHCP does not need a lease to apply (2026-10-20, found on the first boot of the image on hardware):** `inet dhcp` makes ifupdown
  run `udhcpc -n`, so a WAN with no lease in about 8 seconds (uplink unplugged, provider slow) was a failed `ifup`, a failed
  `networking.service`, and the whole commit rolled back, LAN changes included. The WAN's DHCP is now `inet manual` with
  `up udhcpc -b -R -p /run/udhcpc.$IFACE.pid -i $IFACE [-x hostname:NAME]` and a `down` that kills it (`dhcpHooks`; after the
  other options, so the cloned MAC and the MTU are set first). The port comes up at once, the client forks to the background and keeps
  asking, so a cable plugged in later gets its lease. Checked with busybox `udhcpc` in a namespace: with no server it exits 0 and
  leaves the pid file. The access point's bridge `br0` got the same treatment the next day (`dhcpClientHooks`, listed before the recovery address `post-up`; the client's lease script (`/etc/udhcpc/default.script`, read from the Debian package) flushes addresses only when the client starts or stops, so the recovery address survives later leases). A Wi-Fi uplink is the WAN stanza, so it has it too.
- **How it is written** (`translator/wan.go`; static configurations render as before, DHCP as described above):
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
- **API** (`netapi/uplinks.go`): `PUT /api/v1/uplinks/wan` takes `mode` and, optionally, `vlan`, `mtu`, `mac`, `dns`
  and `pppoe` (`username`, `password`, `service`). Fields left out stay as they are; `0`, `""` or `[]` clear. Changing the type
  drops what belongs to the old one (static values for DHCP and PPPoE; the PPPoE login for
  the others). **Leaving static does not turn its DNS servers into an override** unless the request sends some. **The PPPoE
  password is write-only**: it is accepted, saved in `network.json` (0600) and written to the peer file, and never returned
  (`password_set` says whether one is saved; leaving it out keeps it). The review shows "PPPoE password: (none) -> set" or
  "set -> changed". Error fields: `vlan`, `mtu`, `mac`, `dns`, `pppoe.username`, `pppoe.password`, `pppoe.service`.
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
- **The firewall log store (2026-10-11, agreed plan in TODO 9l).** `internal/fwlog`: the daemon follows `journalctl -k -f --grep=fw:` (resuming after the saved `__CURSOR`, kept in the same database; a cursor journalctl refuses is dropped) and saves the parsed packets in `firewall_log.db` (SQLite, WAL, written in batches every 2 s). Retention is 7 days or 100,000 entries, whichever first; the same kind of packet within a minute (rule, interfaces, addresses, protocol, destination port; not the source port) is one row with a count and first/last time. `GET /api/v1/firewall/log` is paged (`before`, `next`) and filtered (`rule`, `action`, `network`, `hours`, `q`; the first three resolved to rule ids from the rules in effect), `log.csv` downloads the same selection with formula-looking cells quoted. `network.ReadFirewallLog` (journalctl `-n`) is gone. The **Firewall, Logs** page (`web/static/scripts/firewall_log.js`: filters, 10 s refresh with Pause, Show more, CSV link, device names from the leases) replaces the *Logged traffic* card on the Rules page, which now only points to it. Fixed on the way: clicking an upstream radio button on the DNS page threw a console error (a null `closest`). Tested with a fake journal source and fake journalctl scripts (arguments, kill on close, refused cursor); a logged packet from the real kernel was not captured (the kernel only logs from the main network namespace), so the end to end path still needs a real machine.
- **System logs and the journal (2026-10-11).** The System Logs page is now *System logs* (the live journal, with a pointer to Firewall, Logs). `buildISO.sh` writes a journald drop-in: persistent, 100 MB, 10 MB files, one month (TODO 25). It only takes effect in the next image; the live ISO's RAM root cannot keep a journal across reboots anyway. Syslog forwarding is still to do (TODO 9l).
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
- **Stage 3, API** (`netapi/dns.go`, `settings.go`): the upstream servers, what each network is told, local names, forwarded domains and the domain on Settings, with notes about a network's chosen servers. The upstream servers (the old Internet-page `dns`) now also refuse loopback and duplicate servers. Endpoints are in the document.
- **Stage 4, page** (`web/static/scripts/dns.js`, Services, DNS): the upstream servers (the provider's, or servers of your own; a static connection forces its own), what each network is told (this router, or other servers), local names and forwarded domains as lists with an editor, each saved as a draft; the local domain is shown and linked to Settings. The Internet page keeps the DNS field only for a static address (otherwise it points to the DNS page and no longer sends `dns`); Settings gained a Local domain card. A domain that would contain a forwarded domain is refused on Settings by naming that forward. Checked in a browser against a sandbox build (errors, adding, editing, switching off, two-click delete, review lines).
- **Stage 5, lookup test** (`netapi/dns_lookup.go`; first a card on the DNS page, since 2026-10-11 the **DNS lookup** card on the new **System, Diagnostics** page, `diagnostics.js`, with its description; the DNS page links to it): `GET /api/v1/dns/lookup?name=` asks the router's dnsmasq on 127.0.0.1:53 (Go resolver with a custom dial, the name made absolute so the router's own search list cannot change it; an IPv4 address does a reverse lookup) and returns status, answers and milliseconds. It tests what is running, not the draft. The fake DNS server in the tests answers, refuses, stays silent and records the exact question; a real dnsmasq with `interface=` lines was confirmed to answer on loopback. `API.DNSAddr` points tests (and the sandbox) elsewhere.
- The DNS page's two cards are titled **Upstream DNS** (where the router asks) and **Local DNS** (what each network is told), with the plain-words descriptions kept under the titles (2026-10-11).
- Not yet: polish and screenshots (stage 6).
- **The host name moved to System, Settings (2026-10-11).** The Internet page's "Host name to send" is now the router's `hostname` (`network.json` key `hostname`, was `wan_hostname`; `PUT /api/v1/settings/hostname`; review line "Host name"). It is checked and kept whatever the connection type (changing to static or PPPoE no longer clears it) and is only written (`hostname` on the DHCP stanza) with DHCP addressing; the uplink API no longer takes it. It is not yet the machine's own name (`sys.json` still holds the unchangeable device name shown on Overview): making the two one setting is an open question. Settings and DNS pages now stack their cards with a shared gap (`.card-stack`).

## 7j. Administrators (built 2026-10-11)

Design, rules and API are in **`docs/administrators.md`**. In short: System, Administrators (`admins.js`, `auth/admins.go`) manages
the accounts in `auth.json` with two roles, **admin** and **readonly**; changes take effect at once (they are not staged like the
network configuration).

- **Roles are enforced in `RequireAuth`** (`auth/csrf.go`): a read-only session gets 403 "This account is read-only." for anything that
  changes state, except `POST /api/v1/me/password`. The role is read from the configuration at every request
  (`SessionManager.Lookup` now returns it), so a demotion counts at once, and **a session whose account was removed is no longer a
  session**. An unknown or empty role counts as read-only. The dashboard shows who is signed in (and a *read-only* tag).
- **Rules the router enforces:** at least one administrator always (`changeAccount`, also against two administrators acting at once);
  not your own removal or role change; your own password only with the current one (rate limited, 5 per 2 minutes per account);
  other accounts' passwords by an administrator without it; names unique whatever the capitals; at most 20 accounts. A password
  change ends the account's other sessions (`SessionManager.RevokeUser`); an administrator's reset or a removal ends all of them.
- **Tests:** `auth/admins_test.go` drives the real handlers behind `RequireAuth`; every deliberate break of the rules was caught (five that the first round missed got their own tests).
  The older session and CSRF tests now create the accounts they sign in with (a session needs its account to exist).
- Not built: more roles, two-factor, RADIUS/LDAP, past sign-ins, a console tool to recover when every administrator has forgotten the
  password (TODO, longer-term gaps).

## 7k. Settings round two (started 2026-10-11)

Design and rules are in **`docs/settings.md`** (updated by each commit). Plan: host name as the machine's name, time zone and NTP,
syslog forwarding, a Power card (restart, shut down), and a regrouped page. Built so far:

- **Host name.** `hostname` is now the machine's name: `/etc/hostname` is written (service `hostname`, action `hostname -F /etc/hostname`,
  first in the apply order, restarts nothing else) and the Overview shows it. `send_hostname` (default off) decides whether a DHCP
  connection also tells the provider (before, any set name was sent). An empty name leaves the machine alone; sending needs a name.
  `/etc/hosts` is deliberately not edited (dnsmasq would serve its entries to the LAN); the image gets `libnss-myhostname` instead.
- **Time** (`config/settings.go`, `translator/settings.go`, `network/timestatus.go`). `timezone` (from the generated `config/timezones.go`: zone.tab's 418 zones plus UTC, each with its country, so the page can suggest the chosen country's zones), `ntp_disabled`, `ntp_servers` (up to four names or addresses; empty is `pool.ntp.org`). Applied by two new services: `timezone` (`/etc/timezone` plus a one-liner that points `/etc/localtime` at the zone and fails on a zone this machine lacks) and `ntp` (a drop-in for `systemd-timesyncd`, present only while not default, with `enable`+`restart` or `disable --now`; last in the order, after DNS). The page shows the live clock from `timedatectl`. The two shell one-liners are tested by running them for real against a sandbox directory with a stub `systemctl`. Artifacts must stay grouped by service (the applier relies on it), so the new files come last in `Render`. The image gets `systemd-timesyncd` and `tzdata`.
- **Power** (`auth.AdminAPI.HandlePower`, `POST /api/v1/system/power`): restart or power off, administrators only, refused while a change is being applied or awaits confirmation (`Idle` is the pipeline's `Idle`), answered first and carried out two seconds later with `systemctl reboot|poweroff` through the same runner as the applier (so `-dev` only logs it). The page asks twice, then waits for the router to go away and come back and reloads itself. The Settings page is now four groups: General, Time, Logging, Power.
- **Factory reset** (2026-10-21; `auth.AdminAPI.HandleFactoryReset`, `POST /api/v1/system/factory-reset {"password"}`): administrators only, the caller's own password again (rate limited with `Attempts`), refused while a change is applied or pending. `main.go` leaves `/data/factory-reset` (only when `/run/peakroute/state` is `persistent`; `-dev` only logs) and reboots after two seconds; the initramfs script (`factory_reset_if_asked`) removes `/data/overlay` and then the file, before the layers are mounted (they cannot be emptied in use; a power cut in between repeats it). The device then starts like a new one (setup network 192.168.77.1). Slots and the boot loader are untouched. The function is run for real in `image/factoryreset_test.go` (taken out of the real script). **Needs the new initramfs**, so an image or update built after this change; an older image writes the flag, restarts, and erases at the first start that has the new initramfs. The card is on the Settings page under Power: a button, then a password box; after the answer the page says to open https://192.168.77.1 and does not wait for the router. Checked: handler tests, the daemon's real routes in `-dev` (CSRF, wrong and right password); not checked in a browser (the built-in one refuses the self-signed certificate), and not on hardware.
- **Syslog forwarding** (`internal/syslogfwd`, `config/settings.go`). `syslog` in `network.json`: up to three servers (name or IPv4, port, `udp` or `tcp`), `system` and `firewall` switches. Not a system service: the daemon sends itself. `Forwarder.Update` (fed every 5 s with the configuration *in effect*) starts, keeps or stops one sender per server (a goroutine with a 2,000-message queue: oldest dropped when full, TCP reconnects with 1 s doubling to 30 s and re-sends the message it was holding) and a `journalctl -f -n 0` follower that runs only while the system log is wanted; firewall packets arrive from the firewall log reader through `fwlog.Collector.OnPacket` (every packet, not the folded rows; packets older than two minutes are skipped so a restart does not replay the journal). RFC 5424, TCP lines end in LF, no TLS (not planned yet). Firewall messages carry the rule's name and action; the journal's own facility and severity are kept. `Statuses()` shows per server sent, dropped, connected and the last error on the Settings page, and `POST /api/v1/syslog/test` (Diagnostics, Syslog test) sends one message to every server in effect. Checked end to end against a real UDP listener in the sandbox (a real journal entry and the test message arrived with the router's host name). The mutation check found a hang-prone harness (a mutant that wrote invalid UTF-8 killed the script that restores the files) and four weak spots that now have tests.

## 7l. Dynamic DNS (built 2026-10-11)

Design, services, rules and API are in **`docs/ddns.md`**. Services → Dynamic DNS keeps names pointed at the internet connection's
address. In short: `ddns` in `network.json` (up to ten entries: provider `dyndns2`/`duckdns`/`cloudflare`/`custom`, name, server, user,
password or token, custom https URL with `{ip}`/`{hostname}`/`{username}`/`{password}`, source `wan` or `web`); the daemon does the
updating itself (`internal/ddns`: a 30 s loop over the configuration *in effect*, one function per service, an HTTP client that only
follows https redirects), so a commit that changes only this has an empty plan.

- **Rules:** a connection address that is private or in `100.64.0.0/10` is never published (the page says so; the `web` source asks
  api.ipify.org then icanhazip.com over https instead); an unchanged name is told again after a week; trouble that may pass backs off
  1, 2, 4... up to 30 minutes; trouble that needs a change (bad login, no such name, abuse) stops the name until it is edited or *Update
  now* is pressed. A changed entry starts afresh (its state is keyed by all its fields).
- **Secrets are write-only** (stored 0600 in `network.json`; the API says `password_set`; the review says *set* or *changed*; no status or
  log line holds one). A provider change drops the fields that belong to the old provider. Cloudflare: zone found by taking labels off
  from the left (never the bare TLD), record changed keeping TTL and proxy setting, or added.
- **Tests:** fake services over real TLS (`internal/ddns/ddns_test.go`), API tests (`netapi/ddns_test.go`), config tests; mutation
  checks on all three. A harness lesson: the checking script must decode the test output leniently, or a mutant that writes bad text
  can crash it and leave a file broken.
- Not checked against a real service.

## 7m. VPN (built 2026-10-11: WireGuard and OpenVPN)

Design, rules and API are in **`docs/vpn.md`**. Services, VPN has two tabs: **Site to site** and **Client VPN** (remote access to the
router: router is the server, a file per device). Both engines on both tabs. `vpn` in `network.json`: `sites` (up to 8; `engine`
`wireguard` or `openvpn`), `wg_remote` (WireGuard server and up to 100 `clients`), `ov_remote` (OpenVPN server and up to 100
`clients`: name, certificate **serial**, address) and `ca` (the built-in authority).

- **Zones.** A VPN that is switched on is a firewall zone like a Local Network (`vpn-ra`, `vpn-ora`, `vpn-<id>`): `NetworkConfig.VPNZones()`
  feeds `FirewallTab` (kind `vpn`), the rule `from`/`to` checks and the translator's nft pairs (`nftVPNRules`: the ports by protocol on
  the WAN, DNS and ping to the router, internet only when switched on, member pairs open unless isolated, MSS clamp 1380).
  `nftablesNetworkRules` was rewritten so Local Networks and VPNs are members of one set of pairs. A site's interface is
  `VPNSite.Interface()` (`wgs1`, `ovs1`; `wgra`, `ovra`).
- **WireGuard files.** wg-quick files in `/etc/wireguard` (0600, managed directory) and one sync script (`peakroute-wg-sync`, sha256
  fingerprints in `/run/peakroute-wg`, touches only `wgra`/`wgs<number>`) run by the `wireguard` service and by the daemon at start.
  The pre-check loads every file into a scratch interface (`wg setconf`); `wg-quick strip` alone does **not** validate keys.
- **OpenVPN files.** `internal/pki` is the authority (ECDSA P-256; authority 20 y, server 10 y, devices 5 y; CRL; tls-crypt key).
  One file per tunnel with everything inline in `/etc/openvpn/{server,client}/pr-*.conf` (managed by prefix), one file per device in
  `/etc/openvpn/ccd/` (a managed directory with `Any`: no suffix; `ccd-exclusive`, so no file means refused), systemd instances of the
  package's `openvpn-server@`/`openvpn-client@` units started by `peakroute-ovpn-sync` (service `openvpn`, after `wireguard`). A
  server's file carries a hash of its device files so that a change restarts it. **A device's certificate is only good on its own
  server** (`verify-x509-name`: `ra-` prefix for remote access, `<id>-peer` for a tunnel). OpenVPN will not take a /30 for a server's
  network (`--server` wants /29 or wider), hence `10.98.N.1/29`. A client tunnel imports a file (`internal/ovpn`: strict, takes only
  the blocks and the address; `route-nopull`). Nothing is pre-checked (OpenVPN cannot read a file without running it): the renderer
  re-encodes every PEM block (`pki.Normalize`) and `ensureSafeOVPN` checks every address.
- **Keys and secrets.** A device's or peer's private key is made on the router, shown once in the file and not kept (only the serial
  and the end date). Revocation: remove, new file or removing the server puts the serial on the CRL (`revoke`, regenerates it; ECDSA
  signatures differ every time, so a CRL is made only when something is revoked). A certificate that has run out must not make the
  configuration unsavable: validation checks who signed and for what, not the dates (the page warns). WireGuard pre-shared keys
  are typed or made in the page; every secret is write-only (*set*/*changed* in the review).
- **Status.** WireGuard: `wg.Reader` runs `wg show all dump` (cached 2 s). OpenVPN: `ovpn.Reader` reads the servers' status files
  (`/run/openvpn-server/status-<name>.log`, version 2, written every 10 s) and a client tunnel is Connected while its interface is up.
- **Remote devices reaching a remote site** need the other router to list the remote access network among its networks (cryptokey
  routing for WireGuard, plain routing for OpenVPN); documented in `docs/vpn.md`.
- **Tests.** Unit tests for the model, translator, API, pki and the file reader/writer; **two real end-to-end labs**
  (`translator/vpn_e2e_test.go` for WireGuard and `openvpn_e2e_test.go`: routers and devices in network namespaces with the real tools
  and the generated nft rules: tunnels, remote access, policy, isolation, removal; for OpenVPN also the ccd files, the revocation list,
  the wrong kind of certificate, the status file and the script). The WireGuard tools are extracted from the Debian packages for the
  test (skipped when missing); `openvpn` is the host's. Mutation checks on config, translator and API. Lab lessons: `unshare -rmn` plus
  `nsenter` into namespaces made with `unshare -n sleep`, move links with `ip link ... netns PID`, `wg-quick` runs as the (user
  namespace) root, `in` is a shell keyword, a stand-in `systemctl` runs openvpn in the background, and `user nobody` has to be taken
  out of the files in the lab (no such user inside a one-user namespace).
- **QR codes** for a WireGuard device's file: `web/static/scripts/qr.js` draws them in the page (the file never leaves it). Same algorithm as `internal/qr`; the Go tests read codes back with an independent decoder (format and version words, Reed-Solomon check of every block, known capacities) and pin SHA-256 fingerprints that the script must match (checked in the browser). Not scanned with a real phone yet. OpenVPN files are too big for one (858 bytes at most).
- Not done: IPv6, renewing the server certificate, real hardware, a scan test with a phone.

## 7n. Certificates (built 2026-10-11)

Design and rules are in **`docs/certificates.md`**. System, Certificates (the entry that was greyed out). `internal/cert`: a `Manager`
keeps the web interface's certificate. **Built-in**: signed by a **web authority of its own** (ECDSA P-256, `ca.crt`/`ca.key` next to
`tls.crt`), separate from the VPN's (the user's decision: not shared). Good for localhost, host name, host.domain, the address of every
Local Network, loopback and the extra names (`extra-names.json`, up to 10). `Ensure` runs at start and every 12 h (main.go) and
makes the certificate again when it is missing/damaged, not signed by our authority (the old self-signed one is migrated at once),
within 30 days of expiry, or lacks a name or address the router now has; the authority is only made again when its files are broken.
**Custom**: an upload (checked before anything changes, key staged and swapped with the certificate, `custom` marker written first) is
never touched by `Ensure`. The server uses `tls.Config.GetCertificate` (`ListenAndServeTLS("", "")`), so changes apply without a restart.
Not part of the draft/commit pipeline (applies at once, like passwords). `Wanted()` is fed from the configuration *in effect*. The page
(`certificates.js`) also shows the OpenVPN authority and its certificates (from `/api/v1/vpn`) and **renews the OpenVPN server
certificate** (`POST /api/v1/vpn/ca/renew-server`, through the draft because the servers restart). The VPN page keeps only a pointer.
Tests: `internal/cert` (verified against the authority for every name and address, migration, renewal, names, custom kept and replaced
on request, refused uploads leave the pair alone, the API), netapi renewal. Checked in the browser. The old `/api/v1/cert/upload` route is
gone (replaced by `/api/v1/certificates/upload`).

## 7o. Diagnostics (built 2026-10-11)

Design and rules are in **`docs/diagnostics.md`**. System, Diagnostics: ping, traceroute, a TCP connection test, the DNS lookup and the
syslog test. `internal/diag` runs ping and `tracepath` as **jobs** (`Runner`: fixed programs and arguments, no shell, a validated target,
at most three at once, 500 lines, a deadline, stoppable, the last 20 kept; the page polls every 0.7 s) because the server's 10 s write
timeout rules out one long response; the connection test answers at once (`TryConnect`, 4 s). Sources are resolved server-side from the
configuration in effect (internet connection's address, Local Networks' router addresses, up VPN interfaces). `tracepath` has no source
option, so a traceroute cannot be sent from one network's address (the `traceroute` package would be needed). Starting a test is a POST
(a read-only account cannot start one). Lessons: with `exec.Cmd` writing into an `io.Pipe`, `cmd.Wait` must run in its own goroutine
that closes the pipe, or the reader never sees the end. Tests: the runner with a fake program (lines, stop, limits, forgetting, failure
words, bounded output), the real `ping` on 127.0.0.1, the connection test against real listeners, the API with a fake runner; checked in
the browser against real `ping` and `tracepath`.

## 7p. Bug batch (2026-10-11)

- **CPU temperature** (`system/cputemp.go`): chosen by what a sensor says it is (thermal zone types, then hwmon chips such as `coretemp`
  with its "Package" label, `k10temp`), then the hottest zone that is not a known non-CPU (acpitz placeholder, nvme, wifi, battery...),
  else none: `cpu_temp_celsius` is `null` and the overview shows n/a. Memory uses `MemAvailable`.
- **Stale temp files** (`fsutil.SweepStale`): `.name.123.tmp` files older than a minute are removed at start from the daemon's, the
  certificates' and every directory the configuration writes to.
- **CSP**: `script-src 'self'` (the last inline handler became `data-goto`); `style-src` still allows inline styles.
- **Checks**: `./check.sh` (gofmt, vet, tests) and `.github/workflows/ci.yml` (**run by hand only since 2026-10-22**, `workflow_dispatch`: it was red after v0.9.5-alpha with a failure whose log could not be read without a login and that does not reproduce locally, and the release workflow already runs `check.sh` before building; installs wireguard-tools, openvpn, nftables so that the end-to-end tests run;
  they skip themselves without the tools or user namespaces). No git remote yet.

## 7q. Backup and restore (built 2026-10-12)

Design, format and rules are in **`docs/backup.md`**. System, Backup and restore (the last greyed-out entry but Updates). `internal/backup`:
`format.go` (always-encrypted file: header in the clear and authenticated, scrypt N=2^16 and XChaCha20-Poly1305, passphrase 10 to 256,
cost bounded when reading), `bundle.go` (gzipped tar with a manifest of sizes and SHA-256; strict `Decode`: whitelist of paths, no links
or duplicates, size and hash per file, schema version `SchemaVersion` refused when newer; `Collect` reads the *committed* settings,
administrators and certificates, history through `VACUUM INTO` of a read-only connection so a database in use is copied consistently),
`restore.go` (`Service`: `Inspect` keeps an opened backup 10 minutes under a token; reports with the usual `DiffNetwork` lines; `Restore`
validates everything first, keeps a plain copy of the router's own state in `backups/` (newest three, 0700, not downloadable), applies
certificates, administrators and the settings last as the draft via `StageNetwork`, with the earlier steps undone from the copy if a later
one fails; `keepOwnPorts` for restoring onto other hardware; history is staged in `restore-pending/` and swapped in by `ApplyPending` before
the daemon opens its databases), `api.go` (one key derivation at a time). The web authority comes across and the web certificate is made
again for the new router's names by `Certs.Ensure`. `SessionManager.RevokeAll` signs everyone out after the administrators change. Tests
cover the file (every byte covered by the seal, hostile cost parameters), the bundle (forged archives), the whole flow between two routers
(byte-identical administrators and authority, draft then commit gives the same `network.json`, history swapped in), partial restores,
different ports and Wi-Fi adapters, refusal before any change, undo and the three-copy limit, custom certificates, and the API. Checked in
the browser. A lesson: `\'` inside a single-quoted JS string in a heredoc ends the string; the page did not load until it was fixed.

## 7r. First-run wizard (built 2026-10-12)

Behaviour and rules are in **`docs/setup.md`**. Replaces the one-form first-run page. `internal/setup` (`Service`): `HandleInfo` (wired
ports with `Here` = the port(s) the browser's address belongs to, found by `HerePorts` from the interface addresses and bridge members, and
`DefaultRoute` from `/proc/net/route`; suggestions by `suggest`), `HandleCheck` (`build` lays the form over the router's settings, converts a
netmask given as `/24`, runs `NetworkConfig.Validate` and maps the field names to the form's; errors are filtered to the pages up to the one
asked), `HandleApply` (under one mutex: build, `StageNetwork`, `SetupAdmin`, undo the draft if the account fails, sign in), the restore
handlers (`backup.Service.HandleOpen`/`HandlePreviewChoice` reused; `RestoreFirstRun` forces the administrators in, refuses a backup without
them and one whose ports are missing and not chosen, makes no pre-restore copy, then `MarkConfigured`), and `Guard`, which answers 409 once
the router is set up. Every answer ends up in `network.json.staged`, `auth.json` and the `configured` flag (a test reads them back with a new
`Manager`). The page is `first_run.html` with `first_run.js` (all state in one object, one `render()` per page, `data-f` bound inputs; the
wizard has its own small helpers because the dashboard scripts are not loaded). After finishing, `sessionStorage['pr_open_review']` makes
`config_banner.js` open the review dialog on the first dashboard. The administrator is also the console login: `internal/console` (`System`: `useradd --create-home --groups sudo`, then `chpasswd -e` with the bcrypt hash on stdin; refuses names the system uses as account or group and accounts outside uid 1000-59999; `Dev` only logs), `AuthConfig.ConsoleUser`, kept in step by `AdminAPI.setPassword` (console first, so a failure changes neither), protected in `changeAccount`, rebuilt after a first-run restore (`consoleAfterRestore`); a configured router that restores keeps its own `ConsoleUser`. The way in before anything is set up is the setup network (`internal/bootstrap`: bridge `brsetup` over every wired port, 192.168.77.1/24, a DHCP-only dnsmasq of its own with no router or DNS option; `Needed` = no committed LAN interface; `Reconcile` starts or stops it; the pipeline's `Before` (stop) and `Settled` (reconcile) hooks run around every commit and revert, after network.json is put back on failure). `HerePorts` narrows a bridge to the member the client is plugged into (`/proc/net/arp`, `bridge fdb show`). Tested with the real tools in a namespace (`TestTheSetupNetworkWorksForReal`: veth ports, the real dnsmasq, `busybox udhcpc` gets a lease and pings the router, teardown frees the ports; dnsmasq is run with `--no-daemon` there because it cannot change user inside the namespace). Tests: `internal/setup` (every answer saved, other connection modes, netmask
forms, per-step checks, the connected port protected, failed attempts leave nothing, one winner among eight at once, closed after setup,
unknown fields refused, restore choices that cut the browser off) and `internal/backup/firstrun_test.go` (restore onto a blank router with
ports chosen; saved on disk; token single use). Checked in the browser against an empty data directory, with a stand-in list of ports (the
laptop has one): new router end to end, errors per field, the review opening, and a restore with missing ports.

## 7s. Access point mode (built 2026-10-12)

Behaviour and rules are in **`docs/access-point.md`**. What the device *is* follows the configuration: `network.json` `ap` (`APConfig{enabled,
ports, mode, ip, netmask, gateway, dns}`); `NetworkConfig.IsAP()` / `OperationMode()` replace the `operation_mode` of `sys.json` (now
display-only history); the dashboard's mode and menu come from the committed settings. `Validate` branches at the top (`validateAP`: ports,
address, country, Wi-Fi, settings, syslog; the router's own fields are not looked at, so leftovers cannot block it). `translator/ap.go`
(`renderAccessPoint`, chosen in `Render` by `cfg.IsAP()`): br0 with dhcp or static, `bridge_ports` = `APBridgeMembers()` (cable ports then
Wi-Fi links), the recovery address as a `post-up`, a short nftables input policy (checked with the real `nft`: it caught `udp sport 67
dport 68`, which needs `udp dport`), `ip_forward=0`, removals of every router-only file, the VPN renderers run with no VPN to remove their
files, hostapd and wpa_supplicant as on a router with `bridgeOf` returning br0 for everything. `UplinkStation` is false for an access
point. `bootstrap.Needed` is false for one (the bridge keeps the setup address). API: `GET/PUT /api/v1/ap` (`netapi/ap.go`): becoming an
access point puts every cable port in the bridge; going back fills what a never-router lacks (`routerFill`); a switch there and back leaves no
draft (the diff is empty). UI: System, Operation mode (`ap.js`); the router-only menu entries are inside `{{if eq .SystemMode "ROUTER"}}`.
The wizard has the third choice, an `ap` page and an optional Wi-Fi page for both (`internal/setup`: `buildAP`, `addWifi`, `wifiFor` picks the
bands from the adapters' reports); the Wi-Fi network joins the default network and a dual-band single adapter serves 2.4 GHz. Tests:
config (validation, review), translator (every rendered shape, router to AP and back as plans, the real `nft`), netapi (the endpoint),
setup (the flow, checks per page, the connected port protected, Wi-Fi making), bootstrap. Checked in the browser (wizard end to end, the
page, commit of the mode in the sandbox: files written, menu cut down). Not possible here: a real bridge with a Wi-Fi member.

## 7t. The disk image (built 2026-10-12; first real boot reached the welcome page)

Rules and how to use it: **`docs/image.md`**. `buildImage.sh` replaces `buildISO.sh`, `first_boot.sh` and the installer (all removed; the
tests that compared the translator with `first_boot.sh`'s heredocs went with them, the scenarios stayed as ordinary render tests).
Same packages as the ISO (the dependencies of everything the daemon uses stay covered) minus `live-boot` and `live-config`, plus what a
disk needs: `initramfs-tools`, `udev`, `kmod`, `grub-pc-bin` (legacy BIOS), `sudo` (the console administrator), `procps` (`sysctl` and
`pkill` are used by the daemon and were only there by accident), `gdisk`. Layout (GPT): 1 MiB BIOS boot, 128 MiB EFI system (FAT32,
`PRESP`), root ext4 (`peakroute-root`). Found by label and UUID, so it can be written to any disk. `image/overlay` is copied over the
Debian tree (services, sysctl, journald limits, fstab, grub defaults with `rootwait` and both consoles, initramfs `MODULES=most` so the
drivers are not the build machine's, the console banner that says where the setup page is). **Security:** root is locked (`passwd -l`; there is no default password in
the image), nothing is signed in on the console, `systemd-firstboot` is masked, `machine-id` is empty so every device makes its own. First
start: `peakroute-growroot.service` (`/usr/lib/peakroute/growroot`: move the backup GPT with `sgdisk -e`, stretch partition 3 into the free
space with `sfdisk -N`, `partx -u`, `resize2fs`; once, marker `/var/lib/peakroute/rootgrown`; tested here against a disk image in a file with
the real `sfdisk` and `sgdisk`). `image/run-qemu.sh` boots the image in QEMU (UEFI or BIOS, on a bigger virtual disk, the web interface
forwarded). The real tools caught one mistake while writing it: the BIOS boot partition's type GUID. The first real run (the user's, with `sudo`) hit two faults that are fixed: the project drive was mounted `nodev` (debootstrap cannot work there: the build folder
moves to `/var/tmp` then) and a `set +e` inside `cleanup` disabled the script's stop-at-first-error. The rebuilt image was written to an NVMe disk in a real
machine and **booted to the welcome page** (the setup network works on real hardware). It then **grew the root partition to the whole disk and routed** (router setup, commit and routing worked on real hardware). Still to check on it: the rest of
the wizard flows (access point, extender, link, restore), Wi-Fi, VPN and the other pages.

## 7u. Updates: A/B slots and the update bundle (built 2026-10-20)

Design and rules: **`docs/updates.md`** (read it first). Decisions with the user: **A/B root partitions** (the device is a live router), releases
**from GitHub or an uploaded file**, **checksum only** for trust (a signature entry is left room for in the bundle). What changed:

- **Disk layout 1** (`buildImage.sh`, `docs/image.md`): GPT with BIOS boot, ESP (`PRESP`, 128 MiB), **slot A and slot B** (ext4, labels
  `peakroute-root-a/-b`, partition names the same; B is empty in a fresh image) and **data** (`peakroute-data`, grows to the disk). The
  boot loader's files, `grub.cfg` (`image/grub/grub.cfg`) and `grubenv` are on the **ESP** (`grub-install --boot-directory=/boot/efi`), so
  both slots are equal and an update never touches GRUB. `grubenv` has `slot` (kept) and `trial` (tried once: GRUB clears it as it reads
  it, so an unconfirmed trial is a single start). GRUB finds the slot by label and passes `root=LABEL=...`, `peakroute.slot=`,
  `peakroute.trial=1` and `panic=10`; it uses `/vmlinuz` and `/initrd.img` (Debian's links at the top of the file system).
- **Read-only root, state on the data partition.** The initramfs script `image/overlay/etc/initramfs-tools/scripts/local-bottom/peakroute-state`
  (with a hook that adds `overlay` and `e2fsck`) mounts the data partition at `/data` and lays overlays on `/etc`, `/var`, `/home` and
  `/usr/local` (upper layers in `/data/overlay/<name>`); if the data partition cannot be used it lays tmpfs layers and writes `volatile` to
  `/run/peakroute/state` (the page warns). The daemon binary moved to `/usr/lib/peakroute/peakroute` so a copy in the upper `/usr/local`
  can never shadow the slot's. The daemon writes `/usr/local/sbin/peakroute-*-sync` scripts, which is why `/usr/local` is a layer too.
  `growroot` became `growdata` (`peakroute-growdata.service`, marker `/var/lib/peakroute/datagrown`, grows partition 5 of the disk /data is on).
  `image/growdata_test.go` is the old growroot test moved (five partitions now). **A device written from an older image cannot update**
  (different layout): one more write of the image, settings carried over with Backup and restore.
- **One root file system image, two outputs.** `buildImage.sh` makes `rootfs.img` once (ext4, loop-mounted, copied, `e2fsck`ed); `dd`
  puts it in slot A and it is gzip-compressed into the **bundle** `peakroute-os-<version>.update` (tar: `manifest.json` first, then
  `rootfs.img.gz`; manifest: format, version, built, arch, layout, `rootfs_size`, `rootfs_sha256`, `payload_sha256`, notes, `reset`) with a
  `.sha256` beside it. `/usr/lib/peakroute/release` (version, built, layout, arch, Debian) is written into the system. `full_build.sh`
  **The version has one source, `config.BuildVersion`** (linker flag from `full_build.sh`): the Overview, the Updates page, backups, the release
  file in the image and the bundle all show it (checked 2026-10-20: no version number is typed anywhere else). `full_build.sh` takes it from
  `VERSION=` or from git: the tag when HEAD is tagged and clean, else `<last tag>-dev.g<hash>[.dirty]`; `GetBinaryVersion` shows a named
  build exactly as named and adds the commit only for an unnamed `go build`. The release workflow passes the tag. `full_build.sh`
  passes `VERSION`, `UPDATE_REPO` (the fixed source, linker flag `config.DefaultUpdateRepo`) and `UPDATE_NOTES`.
- **`internal/update`**: `version.go` (semver order, `v0.9.5-alpha` < `v0.10.0`), `bundle.go` (read the tar, `WriteSlot`: wipe the head of
  the slot, stream gzip into the partition hashing both the compressed and the raw bytes, refuse anything that does not match the manifest,
  `Compatible` for arch/layout/slot size), `github.go` (newest release by version number with a `.update` asset and a checksum, from the
  `.sha256` asset or GitHub's `digest`; https only; `FindRelease`), `service.go` (`BootState` from `/proc/cmdline` and `grub-editenv list`;
  `receive` shared by upload and download; `finishSlot` = `e2fsck -f -p` (exit 1 is fine), `resize2fs`, then **`tune2fs -L` last**, so a
  slot that was not written to the end has no label and can never be started; `Restart`, `Keep`, `GoBack`; `update.json` in the config
  directory holds settings, last check, the version in each slot and a 30-entry history), `api.go`, `sim.go`. The ESP is mounted read-only
  and remounted writable only for the `grub-editenv set`. Restart makes a copy of the settings first (`backup.Service.SaveCopy`, new
  exported wrapper). **Trial:** `Restart` sets `trial=<other>` and reboots; the new version shows a bar on every page and the Updates card
  with a 10-minute countdown; **Keep** sets `slot=<running>`; **Go back** just reboots (the kept slot starts), or from a kept version sets a
  trial of the other slot. `image/overlay/usr/lib/peakroute/trialwatch` (+ `peakroute-trialwatch.service`, only when the kernel was told
  `peakroute.trial=1`) reboots if the grubenv still names another slot after `PEAKROUTE_TRIAL_SECONDS` (600), so even a dead daemon is undone.
  Installing is refused while a version is on trial (the other slot is the way back), while a configuration change awaits confirmation, and
  while another update runs. The upload and download handlers lift the server's 10-second limits for themselves
  (`http.NewResponseController`). Endpoints: `GET /api/v1/updates` and POST `check`, `install`, `cancel`, `upload`, `restart`, `keep`,
  `revert`, PUT `settings` (a read-only account is refused by `auth.RequireAuth` for all but the GET).
- **Page** (`updates.js`, System, Updates): trial card, "ready" card with a two-click restart, versions, check for updates, install from a
  file (the manifest is read in the browser from the first 64 KiB so the version and notes show before sending; upload with
  `XMLHttpRequest` for progress), go back, settings (pre-releases, daily look; **the GitHub repository is fixed at build time, `UPDATE_REPO`, and is not a setting**, decided 2026-10-20), history; a "restarting" card that waits for the
  router and re-signs-in; and `watch()` on every page: an amber dot on the menu entry for a newer release or a trial, and a bar while a
  version is on trial. The versions of the system (PeakRoute OS, built, Debian, kernel, slot) are on this page.
- **Development run:** `peakroute -dev` uses `update.NewSimulated` (`dev-data/update-sim`: two 64 MiB slot files, a text `grubenv`, a
  restart that only re-reads it, a sample update file `sample-v99.0.0-sim.update` and a damaged one). The whole flow (upload, ready,
  restart, trial bar, keep, go back from a kept version, go back from a trial, damaged file) was clicked through in a real browser; the
  browser pane refuses self-signed HTTPS, so a small plain-HTTP proxy on another port was used (not in the repo).
- **Releases** (`.github/workflows/release.yml`): a pushed `v*` tag runs the checks, `full_build.sh`, and attaches `peakroute-os-<tag>.update`
  (+ `.sha256`) and `peakroute-os-<tag>.img.gz` (+ `.sha256`) to a GitHub release; a tag with a hyphen is a pre-release. GitHub itself
  has no update mechanism: a release is only files on a tag, and the device does the rest. A release file may be at most 2 GiB.
- **Not run for real yet** (needs `sudo` and a boot): the new `buildImage.sh` (partitioning, GRUB on the ESP for UEFI and BIOS, the
  initramfs script and the overlays on a read-only root, the resulting slot boot), `grub.cfg` (syntax checked with `grub-script-check`),
  the real `e2fsck`/`resize2fs`/`tune2fs`/`grub-editenv` on a real partition, and the release workflow. Test in QEMU: build twice (a second
  `VERSION=`), boot the first image (`image/run-qemu.sh`), upload the second `.update` on the Updates page, restart, keep. Likely trouble
  spots: GRUB's prefix for the removable EFI build with the boot directory on the ESP, `root=LABEL=` and the read-only root (anything
  that writes outside `/etc`, `/var`, `/home`, `/usr/local`, `/tmp`, `/run`), and `overlay` in the initramfs.

### Access point mode, the second round (2026-10-12)

- **Home page** (`netapi/apstatus.go`, `GET /api/v1/ap/status`, `ap_home.js`): address, gateway and resolvers from the system (`hostinfo.go`: `/proc/net/route`,
  resolv.conf, the bridge's addresses), ports from sysfs, Wi-Fi from `iw`: `iw dev` (channel, width, `txpower`), `survey dump` (noise and the
  time busy: `busyPercent` is the change since the previous look, kept per adapter, or the total for the first look and after a channel
  change), `iw dev X link` for a Wi-Fi link (connected, signal, rates), the station list per adapter (its first network is on the adapter's name,
  the others on `name_1`...). The menu and the first page follow `config.ShownMode` (the settings in effect, or the draft while nothing has
  ever been applied); the config banner loads the page again when `/api/v1/ap` says the shown mode changed. A loop found on the way: comparing the
  *active* mode with the page's reloaded forever right after setup, because nothing had been applied yet.
- **`netapi.RouterOnly`** wraps the router pages' endpoints in `main.go` (Internet, Local Networks, Ports, Routes, Firewall, DNS, Dynamic DNS, VPN):
  409 for an access point.
- **VLAN on a Wi-Fi network** (`WifiNetwork.VLAN`, 2-4094, access point only; a router refuses it): `renderInterfacesAP` writes `<port>.<id>` VLAN
  interfaces and a `br<id>` bridge for each VLAN in use, `wifiBridge` points hostapd's `bridge=` at it. Checked with the real hostapd.
- **Wizard:** the welcome page offers router, access point, extender (needs two adapters), point-to-point link (near end hosts a WDS network on
  5 GHz where possible, far end joins) and restore. `Form.Mode` is `router`, `ap`, `extender`, `ptp_host` or `ptp_client`; `addLink` builds the
  station or the hosting radio; `wifiFor` leaves out an extender's link adapter; `GET /api/v1/setup/wifi/scan` lists networks to join. A mistake found
  with the browser: a client-only field (`ptp_end`) in the payload was refused by the strict decoder.
- **The whole Wireless page is there in access point mode**; only the Local Network picker is a VLAN ID, and the *internet connection* role of a
  Wi-Fi client is not offered (the API refuses it too: an access point has no internet connection; the link role joins the main Wi-Fi).

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
  as "Country", not as a Wi-Fi line. The country is also the **kernel's regulatory domain** (2026-10-12):
  `/etc/modprobe.d/peakroute-regdom.conf` (`options cfg80211 ieee80211_regdom=US`, read when the module loads at every start) and the
  action `apply regdomain` (`iw reg set US`; `00` when the file is gone; nothing if `iw` is missing), placed after networking and before
  Wi-Fi in the apply order. So a change of country alone runs that one command and restarts nothing else, and the channels on the Wireless
  page open up as soon as it is committed, not only once Wi-Fi has started. Access points write it too. `translator.Render` now takes the
  mode only as a check ("" or the same as the configuration says: `cfg.OperationMode()`); the pipeline passes "".
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

### Wi-Fi client adapters: the internet over Wi-Fi, and point-to-point links (built 2026-10-12)

An adapter is either an access point or a **station** (a client of another access point); both are in the same Wireless page card, and
an adapter cannot be both in use (a switched-off one does not count). `WifiConfig.Stations` (`config/wifi.go`: `WifiStation{name, ssid,
security, passphrase, hidden, role, network, disabled}`), at most 2; WPA2, WPA3 or both, like the access point side.
- **Role `uplink`** is the router's internet connection: saving it makes the adapter `wan_interface` (a cable port that was it is left
  unassigned; a LAN port of that name is dropped) and refuses a connection that uses a VLAN or PPPoE (the validation says the same:
  `wan_vlan`, `wan_mode`). Switching it off or removing it gives the internet port back to the first cable port that is not a LAN port
  (refused with a message if none is free). An uplink that is not `wan_interface` is **not in use** (`UplinkStation`): nothing is rendered
  for it, and the page says which port is the internet port.
- **Role `bridge`** joins a local network like a cable port, over the air: a point-to-point link to another PeakRoute router (the far
  end has *Accept point-to-point links* on a Wi-Fi network: hostapd `wds_sta=1`, which makes the stations' 4-address interfaces join the
  network's bridge) or a wireless extender. The station has its own stanza (`iface wlan1 inet manual`, `pre-up iw dev $IFACE set 4addr
  on`, `wpa-conf ...`) and is a member of the bridge: `defaultPorts` and `networkMembers` list it after the cable ports (so `LanTarget`
  is `br0` even with one cable port), `carriedBy` counts it, and a network that a link joins cannot be deleted.
- **How it is joined:** ifupdown's own wpa hook (`wpasupplicant` package, added to the image) starts wpa_supplicant from the stanza's
  `wpa-conf /etc/peakroute/wpa/<adapter>.conf` (0600, a managed directory, `translator/wifi_station.go`). The network name is written in
  hexadecimal (nothing in a name can break the file), WPA3 as `key_mgmt=SAE` with `sae_password` and management frames required, both as
  `WPA-PSK SAE`; `scan_ssid=1` for a hidden one; `country=` from the router-wide country. A change to a file restarts networking (that
  reads it again; the DHCP server follows, as always).
- **API:** `PUT/DELETE /api/v1/wifi/stations/{adapter}`, staged like the rest; the adapter must exist and (if the hardware can be read)
  have `managed` among its modes. `GET /api/v1/wifi` gains `stations` (each with `in_use`) and `wan_port`. The review shows "Wi-Fi
  connection wlan0: joins Cafe, WPA2/WPA3, internet connection, password set" and never a password.
- **Checked** with the real `wpa_supplicant` (v2.10; reads the whole file before it looks for the adapter, with a control that proves it
  can see a bad setting) and the real `hostapd` for the WDS setting, both skipped where not installed. Not possible without hardware:
  whether a given adapter does 4-address mode, WPA3 as a client or a link at all.
- **Scan for networks** (`wireless/scan.go`, `GET /api/v1/wifi/scan?adapter=`): `iw dev X scan` (the adapter is switched up first if it is down), falling back to `scan dump` (the kernel's remembered results, said so on the page) where a scan is not allowed; the strongest access point per name and security, strongest first; open, WEP and enterprise networks are listed but cannot be picked; names that are empty or hidden and 6 GHz are left out; \xNN escapes are read. An adapter that serves an access point in the settings in effect is refused (409). The parser is tested against a hand-made file of every kind and the real `iw` format on the machine, when there is one.
- **The page is three collapsible sections** (2026-10-12, decluttered): *Wi-Fi networks: wireless router or access point* (open: the usual indoor Wi-Fi, with a plain explanation, the Wi-Fi networks and the adapters that host them), *Point-to-point links and wireless uplink* (closed unless a client or an accepted link is set up: what outdoor point-to-point radios do, in three uses with the steps, and the Wi-Fi client of each adapter) and *Adapters in this device* (closed: the facts, bands and channels). Each has a one-line summary when closed. The `Accept point-to-point links` switch of a Wi-Fi network sits in its own collapsed *Point-to-point link* part, open only if on. Same data and API as before; `renderAdapters` fills the three places (`wifi-aps`, `wifi-links`, `wifi-adapters`).
- **Not built:** choosing a particular access point (BSSID) or band, using the radio as client and
  access point at once (needs the driver's combinations), roaming between several access points of one name.

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
| Firewall: Rules, Port forwarding, Connections, Logs | | all four built (`firewall-rules`, `firewall-forwards`, `firewall-connections`, `firewall-log`); design in `docs/firewall.md` |
| Services: DNS, VPN, Dynamic DNS | | all three built (`dns`, `vpn`, `ddns`) |
| System: Settings, Administrators, Certificates (`certificates`), Backup and restore (`backup`), Updates, System logs, Diagnostics | | Settings (`settings`), System logs (`logs`; the Logs page renamed 2026-10-11, firewall traffic moved to Firewall, Logs), Diagnostics (`diagnostics`: ping, traceroute, connection test, DNS lookup, syslog test) |

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
| `GET /first_run`, `/api/v1/setup/*` (info, check, apply, restore/open, restore/preview, restore/apply) | none, only while unconfigured (409 after) | the first-run wizard, docs/setup.md |
| `GET /login`, `POST /api/v1/login` | none | rate limited |
| `POST /logout` | session + CSRF token (form field) | GET returns 405 |
| `GET /`, `/dashboard` | session | server-rendered shell; embeds the CSRF token |
| `GET /api/v1/system/stats` | session | from the poller snapshot, no disk reads |
| `GET /api/v1/system/traffic?iface=&range=` | session | history; ranges `1h` (also used for 5m), `24h`, `30d` |
| `GET /api/v1/wireless` | session | `adapters` (`name`, `phy`, `mac`, `driver`, `state`, `capabilities_known`, `ap_capable`, `modes`, `bands`: `name`, `widths`, `channels` with `number`, `mhz`, `disabled`, `no_ir`, `dfs`), `iw_available`, `country`, `country_known` |
| `GET /api/v1/routes` | session | `routes` (the draft: `name`, `destination`, `gateway`, `metric`, `enabled`), `pending`, `editable`, and `table`: the kernel's routing table in effect (`destination`, `gateway`, `interface`, `metric`, `kind`: default, connected, static (one of ours, matched against the *active* config) or via) |
| `PUT /api/v1/routes` | session + CSRF | `{"routes": [...]}` replaces the whole list in the draft (`enabled` omitted means on); field errors come back as `routes[2].gateway` and so on |
| `GET /api/v1/uplinks`, `GET /api/v1/uplinks/{id}` | session | the internet connection (today one, id `wan`): `port`, `mode` (`dhcp`, `static` or `pppoe`), `ip`, `netmask`, `gateway`, `dns`, `vlan`, `mtu`, `usual_mtu`, `mac`, `pppoe` (`username`, `service`, `password_set`: never the password), `device`, `pending`, and `live` (what is in effect: `up`, `address`, `gateway`, `dns`, `device`, `mac`, `mtu`) |
| `PUT /api/v1/uplinks/{id}` | session + CSRF | `mode` and, as needed, static `ip`, `netmask`, `gateway`, `dns`; `vlan`, `mtu`, `mac`; `pppoe` (`username`, `password`, `service`); stages a draft. Fields left out stay; the old type's settings are dropped. A static address also requires a gateway and one DNS server; the PPPoE password is write-only |
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
| `GET /api/v1/dns` | session | `upstream`, `domain`, `networks`, `hosts`, `forwards`, `editable` (see `docs/dns.md`) |
| `PUT /api/v1/dns/upstream`, `PUT /api/v1/dns/networks/{id}` | session + CSRF | the servers the router asks / a network is told to use |
| `POST /api/v1/dns/hosts`, `PUT`/`DELETE /api/v1/dns/hosts/{id}`, `POST /api/v1/dns/forwards`, `PUT`/`DELETE /api/v1/dns/forwards/{id}` | session + CSRF | local names and forwarded domains |
| `PUT /api/v1/settings/hostname` | session + CSRF | `{"hostname": "my-router"}`; empty clears it. Sent to the provider with DHCP addressing; kept (not sent) with static and PPPoE |
| `PUT /api/v1/settings/domain` | session + CSRF | `{"domain": "lan"}`; empty clears it |
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
| `GET /api/v1/uplinks`, `GET /api/v1/uplinks/{id}` | session | the internet connection (today one, id `wan`): `port`, `mode` (`dhcp`, `static` or `pppoe`), `ip`, `netmask`, `gateway`, `dns`, `vlan`, `mtu`, `usual_mtu`, `mac`, `pppoe` (`username`, `service`, `password_set`: never the password), `device`, `pending`, and `live` (what is in effect: `up`, `address`, `gateway`, `dns`, `device`, `mac`, `mtu`) |
| `PUT /api/v1/uplinks/{id}` | session + CSRF | `mode` and, as needed, static `ip`, `netmask`, `gateway`, `dns`; `vlan`, `mtu`, `mac`; `pppoe` (`username`, `password`, `service`); stages a draft. Fields left out stay; the old type's settings are dropped. A static address also requires a gateway and one DNS server; the PPPoE password is write-only |
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
| 2026-10-20 | The disk image is about 6.6 GB for about 1.1 GB of system: two 3 GiB root slots, 129 MiB of boot partitions, a 256 MiB data partition | A/B updates need a second slot, and slots cannot be resized after the disk is written, so they are generous; the empty space compresses to nothing in the release download. Laying the disk out at the first start (a ~1.4 GB image) and slimming the system are deferred until the layout has been booted for real (`docs/image.md`, "Why the image file is about 6.6 GB") |
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

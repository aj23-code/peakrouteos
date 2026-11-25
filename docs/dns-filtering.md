# DNS filtering (AdGuard Home)

Per network, the router can answer DNS through **AdGuard Home**, which blocks the names on its lists (ads, trackers, malware) and keeps a
query log and statistics. It runs on the router itself. Routers only: an access point has no DNS role and does not carry it.

## What a person sees

- **DNS page, Local DNS**: each network's "Devices are told to use" has a third choice beside *This router* and *Other servers*:
  **This router, blocking ads and trackers (AdGuard Home)**. It is a draft like every other change: AdGuard Home starts when the first
  network that filters is committed, and stops when the last one stops. The three choices exclude each other, which is the rule that a
  network that filters cannot also be told to use other DNS servers (the commit is refused otherwise, on that field).
- **DNS page, Ad and tracker blocking** (a card at the top, shown once the image has AdGuard Home or a network uses it): whether
  AdGuard Home is running, and *Open AdGuard Home*, which opens its own interface (lists, rules, allow-lists, query log) in a new tab on port
  3000. That interface has no accounts of its own: the router's sign-in stands in front of it, and a read-only account can look but not change
  anything. Changes made there apply at once and are not part of the draft; AdGuard Home keeps them in its own file.
- The option is also in the API of Local Networks (`filter` on a network), which the pages do not use.

## Names in AdGuard Home

AdGuard Home shows a device by the first name it can find: a client of its own, then the reverse lookup of the address, which the
router's DNS answers from the DHCP leases (the host name the device gave). The nicknames from the Clients page are not in either, so the
daemon gives them to AdGuard Home (`internal/filtering/clients.go`, every 30 seconds while AdGuard Home runs):

- each nickname becomes a **persistent client** named with it, with the addresses its device has now and its hardware address; two devices
  with one nickname share one client (names are unique in AdGuard Home);
- when a device's address changes, the client's addresses are updated and **everything else a person set on it in AdGuard Home (blocked
  services, its own upstreams) is kept**; when a nickname is removed or renamed, its client goes;
- only the clients made this way are ever changed or removed (their names are remembered in `adguard_clients.json`); a client a person
  made in AdGuard Home, even one named like a nickname, is left alone;
- a nickname whose device has no address now keeps its client as it was; a device with no nickname keeps showing its host name.

Checked with the real AdGuard Home: a client added through its API with the device's address is shown by name in its query log.

## How it works

```
device on a filtering network ──53──► firewall (nat prerouting: redirect to :5335) ──► AdGuard Home (0.0.0.0:5335)
                                                                                        │ not blocked
                                                                                        ▼
                                                              dnsmasq on 127.0.0.1:53 (the router's own DNS)
```

- The firewall redirects UDP and TCP port 53 arriving on each filtering network's interface to port 5335, where AdGuard Home listens on
  every address. The sender is kept, so AdGuard Home sees which device asked (its per-device statistics and rules work). A device that
  asks another server on port 53 (a hard-coded 8.8.8.8, say) is caught the same way. DHCP is unchanged: devices are still told the router.
- AdGuard Home's upstream is the router's own DNS server, dnsmasq, over the loopback (`interface=lo` is added to dnsmasq's file while any
  network filters). So **local names, forwarded domains, the DNS overrides and the choice of upstream servers all still apply** on a
  filtering network, and device names (reverse lookups) come from the DHCP leases.
- The input chain lets a filtering network's devices reach port 5335 (after the redirect that is where the packet is); other networks may
  not. AdGuard Home's web interface listens on 127.0.0.1:3001 only. The daemon offers it on port 3000 over HTTPS (the router's own
  certificate) while any network filters, after checking the session; a change from the browser must carry an `Origin` of that same page, and
  the router's session cookie is not passed on. The default network reaches port 3000 because it may reach everything on the router; the
  others do not.
- **Only plain DNS is caught.** Apps with their own encrypted DNS (DNS over HTTPS or TLS) ask AdGuard Home nothing and are not filtered.
  Blocking those is a separate, later choice.

## Files and services

| What | Where |
|---|---|
| The switch | `network.json`: `filter: true` on an added network, `lan_filter: true` for the default one (not written when off) |
| The marker | `/etc/adguardhome/peakroute-enabled`, written by the translator while any network filters (service `filtering`, verb *apply*) |
| The service | `peakroute-adguard.service` (in the image, not enabled; its start condition is the marker). `DynamicUser`, its own state folder `/var/lib/adguardhome`, no capabilities, a read-only system |
| First configuration | `/usr/lib/peakroute/adguard-seed`, run before each start, writes `AdGuardHome.yaml` **only if it is missing**. After that the file is AdGuard Home's own |
| The program | `/usr/lib/peakroute/AdGuardHome`, put there by `buildImage.sh` (below) |

The action reads the marker: present, `systemctl enable` and `start`; absent, `disable --now`. A router whose image has no AdGuard Home
(older than this feature) fails the commit with "AdGuard Home is not in this system image: update to a version that has it".

The seed: web interface on `127.0.0.1:3001` with no accounts, DNS on `0.0.0.0:5335` with the rate limit off (the default of 20 questions a
second per device throws away part of a busy browser's), upstream, bootstrap and private reverse lookups all `127.0.0.1:53`, a day of query
log and statistics (the data partition is not for keeping a history). AdGuard Home fills in the rest, including its default block list.

## The image

`buildImage.sh` downloads the **newest stable AdGuard Home** release at build time, so every router image carries the current one
(`ADGUARD_VERSION=v1.0.0 ./buildImage.sh` builds with a named release instead). The download is checked against the `checksums.txt`
published beside it; that catches a damaged or swapped file, but both come from the same GitHub release, so it is no check on GitHub
itself. The version is written into `/usr/lib/peakroute/release` (`PEAKROUTE_ADGUARDHOME`) and its licence (GPLv3) is kept in
`/usr/share/doc/peakroute/`. About 32 MB on disk (12 MB compressed). Updates to AdGuard Home arrive with the router's own updates, not by
AdGuard Home updating itself (`--no-check-update`).

## Checked, and not

Checked here with the real AdGuard Home v1.0.0 (a user-space run): the seed is accepted and migrated; with no accounts the interface is
open on the loopback; a rule blocks a name (answered 0.0.0.0) and a name that is not blocked is answered by the upstream; 60 questions in
quick succession are all answered. The translator's output loads into a real kernel's nftables (a throw-away network namespace), and the
build's checksum line works on the real `checksums.txt`. **Not checked:** the redirect with real devices on a real router, the systemd unit's
sandbox on the image, and the build script itself (it needs root).

## Left out for now

- Blocking DNS over HTTPS and DNS over TLS to other servers on the filtered networks.
- IPv6 (the router does not route it yet).
- Per-network lists or per-device rules from the router's own pages; they are made in AdGuard Home's interface.
- Pi-hole: its installer, PHP stack and its own dnsmasq do not fit a read-only image next to our dnsmasq.

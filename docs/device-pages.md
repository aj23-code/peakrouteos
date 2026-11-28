# Device pages, history, LLDP and SNMP (plan, 2026-11-28; phases 1 and 2 built 2026-11-28)

Goal: click a device's name in the Clients list and get **a page of its own** (like Meraki's client page): who it is, where it is
connected (the Wi-Fi network and signal, or the **switch and port** it is plugged into), what it has used over time and what it is doing
now. The switch and port come from **LLDP** (what the router's own ports see) and **SNMP** (what managed switches and access points
know). Standalone mode only, router only.

## 1. Where we start (checked in the code)

- **Per device the router knows now** (`network.ClientLease`): hardware address, address, host name, nickname, lease, **a byte total** (all
  time, from the counting table, `network.UsageTracker`), how it is connected (`wifi` with network name and band, `wired`, `unknown`),
  its network, its reservation, whether it is online and when it was last seen (`presence.Tracker`, a 20 s round: the Wi-Fi station list,
  and an ARP probe for the rest).
- **There is no history**: the total only grows; nothing keeps a rate over time, and the presence tracker keeps only the last answer.
- **PeakRoute AP already reports itself:** `GET /api/v1/ap/status` on the access point answers with its ports, radios, Wi-Fi networks
  and the **devices connected now** (`wireless.Station`: signal, rates and so on); the router adopts the AP (`translator/adopt.go`), so it
  holds the way to reach it. The U6+ has 256 MB of RAM (about 90 MB free, the daemon takes 20), which is what any addition to the AP has
  to fit in.
- **Wi-Fi stations** (`wireless.ParseStations`, `iw dev X station dump`) give signal, rates and bytes for the devices on the router's own
  radios, not for the access points (the U6+ AP has its own daemon).
- **Live connections** of a device are in the connection table (`/api/v1/firewall/connections`), and DNS names it asked for are in
  AdGuard Home's query log when filtering is on (`internal/filtering` already talks to AdGuard Home per client).
- **Nothing speaks LLDP or SNMP.** No SNMP library is in `go.mod`.

## 2. The device page

Opened from the Clients list (the name is a link; `#device/<mac>` so it can be bookmarked). Sections, each only when it has something to show:

1. **Identity:** names (nickname, host name), hardware address and its maker (OUI table), addresses (IPv4, and a static or reserved note),
   network, and the actions that are on the list now (nickname, reserve, remove an offline device).
2. **Connection:** online or offline, since when, last seen; **Wi-Fi** (network, band, signal in dBm, rates, how long) from the station
   dump; **wired**: the router port or, with phase 3, "**Switch name, port 12**, 1 Gbit/s, VLAN 20, PoE".
3. **Usage:** totals, rates now, and graphs of upload and download over 1 h, 24 h, 30 d (phase 1's history store), with the busiest
   hours; a note for a device that has been quiet.
4. **Presence history:** a strip of when it was online, and the last changes (joined, left, moved to another port or network).
5. **Now:** its connections right now (from the table, the busiest first) and, with filtering on, what it asked DNS for lately and
   what was blocked (AdGuard Home's log for that client).
6. **Firewall:** the rules that mention it and its recent log hits, with a link to the rule.

## 3. Phases

1. **Device page and per-device history (no new protocol).** A history store like `wan_health.db` (`device_history.db`, SQLite):
   every minute the byte counters of each device are sampled into a row (device, minute, bytes up, bytes down, online); older than a day
   they are kept as 5-minute rows, older than 30 days dropped (the retention is a setting). Presence changes are events in the same
   store. New API: `GET /api/v1/clients/{mac}` (everything in section 2) and `GET /api/v1/clients/{mac}/history?range=`. The page and
   its graphs (the same canvas code as the Internet page). *Stores bytes and presence only: not the places a device went.*
2. **LLDP for the router's own ports.** Run `lldpd` (a Debian package; it also tells neighbours what this router is) and read its
   neighbour table with `lldpcli -f json show neighbors`: for each router port, the device at the other end (name, port, description,
   capabilities: a switch, an access point). A *Neighbours* card on the Ports page, and the device page's "connected to" for a device on
   a port whose neighbour is a switch we cannot read (it says which switch, not which port). Off by default; one switch on the Ports
   page (it sends and listens, so it is a choice).
3. **SNMP for the switches and access points.** The administrator adds a **device to watch** (address, SNMP version and credentials,
   a name); the router polls it every minute or two and learns:
   - **who it is** (`SNMPv2-MIB`: name, description, uptime) and its **ports** (`IF-MIB`: name, alias, up or down, speed, errors, byte
     counters: a graph per port), **PoE** where `POWER-ETHERNET-MIB` exists;
   - **which hardware addresses are on which port** (`Q-BRIDGE-MIB` forwarding table, `BRIDGE-MIB` where that is all it has): this is
     how a device's page says "switch 2, port 12, VLAN 20";
   - **which ports lead to other switches** (`LLDP-MIB` remote table): a port that has a switch behind it carries many hardware
     addresses and is not where a device is plugged in; the device is put on the port **nearest to it** (the port that sees its
     address and has no switch behind it).
   **PeakRoute APs are polled through their own API** (section 4b), so a Wi-Fi client's page names its AP, network, band and signal.
   Joined with the router's own ports (phase 2) this gives a map of what is plugged into what. A *Network devices* page lists the
   watched devices, their ports and their state; a change (a port down, a device moved to another port) is an event in the history.
4. **Nice to have, when the rest is used:** per-device bandwidth rank on the Overview, an alert when a watched switch stops answering,
   Wake-on-LAN from the device page, a *block this device* action (a firewall rule from the page), the same page for a device on an
   access point: this is in phase 3 (section 4b).

## 3a. Phase 1 as built

- **The page:** `#device/<hardware address>`, opened by clicking a device's name (or its address, when it has no name) in the Clients list
  (`leases.js`, `client-link`); the Clients menu entry stays lit; an access point has no such page. `web/static/scripts/device.js`,
  `view-device` in `dashboard.html`, routing in `app.js` (`deviceFromAddress`). Sections: *Device* (names, hardware address and its maker,
  address with the *Static* and *Seen on* notes, network, reservation, lease), *Connection* (online or last seen; Wi-Fi network, band,
  signal with a word for it, channel, link speed and time connected from the router's own radios; "the switch port is not known yet" for a
  cable), *Usage* (received and sent over 1 h, 24 h or 30 d, busiest download and upload, bars you can hover), *When it was here* (a strip
  of online and offline stretches and the last changes), *Connections now* (the device's open connections, from the connection table).
- **History** (`internal/devhistory`, `device_history.db` in the configuration folder, SQLite): `UsageTracker` now keeps what each device
  **sent and received** apart (`Directions()`, in `client_usage.json` as `up` and `down`: devices counted before this version have no
  split, so their history starts now); a sampler (every minute, router only) writes the difference to `usage_min` (a row a minute) and
  records changes of presence (online or offline, or moved to another Wi-Fi network, band or network) in `presence`; rows older than 25 h
  are folded into `usage_5m`, everything older than 30 days is dropped (`DefaultRetention`; not a setting yet). **Bytes and presence
  only: never where a device went.** Removing a device from the Clients list removes its history.
- **API:** `GET /api/v1/clients/{mac}` (the device and its last 20 changes), `GET /api/v1/clients/{mac}/history?range=1h|24h|30d` (points of
  1 min, 5 min or 1 h, totals, peaks and the online stretches), `GET /api/v1/firewall/connections?ip=` (the connections of one device).
  `internal/devices` serves the first two next to the existing `DELETE /api/v1/clients/{mac}` (remove) and `PUT .../name`.
- **The maker** comes from the IEEE registry (`internal/oui`, the Debian package `ieee-data` in the image: `oui.csv`, `mam.csv`,
  `oui36.csv`; the rest of the package is deleted from the image). A *private* address (the "locally administered" bit: phones and
  laptops make one up to be harder to follow) names no maker and is said to be private.
- **Not yet in phase 1:** DNS lookups of the device (AdGuard Home's log for the client) and the firewall rules and log hits that mention it
  (section 2, items 5 and 6 of the page); the switch port (phases 2 and 3); a setting for the retention.
- Tested: the store (buckets, folding, retention, events, removal, reopening), the sampler (starting point, deltas, a counter that went
  down, a device that appears, presence changes, a restart), the API (names, Wi-Fi, bad addresses, ranges, totals, peaks, stretches, no
  history), the routes together on a real mux, the vendor lookup, the connection filter; the page's script ran in a browser against its real
  markup with mocked answers, and the address handling of `app.js` with good and bad addresses.

## 3b. Phase 2 as built (LLDP)

- **The switch:** `lldp` in `network.json` (a draft like everything else), the *Neighbours* card on the Ports page (`neighbours.js`,
  `GET/PUT /api/v1/lldp`). Off by default. `lldpd` is in the image (installed, not running, like avahi); switching it on writes
  `/etc/lldpd.d/peakroute.conf` (`translator/lldp.go`: `configure system interface pattern <the LAN ports>`, a system description and the
  port id as the interface name) and a commit enables and restarts `lldpd.service`; off removes the file and stops it. **Only the LAN
  ports**, never the internet port and not Wi-Fi.
- **Neighbours** (`internal/lldp`): `lldpcli -f json show neighbors details`, read through helpers that cope with lldpd's JSON changing shape
  (one thing is an object, several are a list; a system with no name has no name key; one address is a string, several a list). For each
  router port: the neighbour's name, chassis id, port id and description, system description, addresses, the capabilities it has on
  (Bridge, Router, Wlan...), the port's VLAN, whether it spoke LLDP or CDP.
- **Where a device is** (`network.ReadBridgeFDB`, `bridge -j fdb show`, asked at most every 5 s): the port of the router's bridge that has
  learned the device's hardware address. The device page's *Port* line says that port and, when LLDP has heard a neighbour on it, "behind
  *switch-hall*, its port *Port 7*". A device behind a switch is on the port the switch is plugged into: the switch's own port for it is
  phase 3 (SNMP). A Wi-Fi device is not placed on a cable port.
- **What the neighbours see:** the router announces itself (its name, "PeakRoute OS") on the LAN ports, so a managed switch lists it.
- Tested: the parser on one neighbour, several, a nameless CDP neighbour, odd shapes and garbage; the bridge table parser; the
  renderer (LAN ports only, never the internet port, an unsafe port name refused), the plan (one action), the command, the review; the
  API (draft only, asked only while in effect, lldpd not answering); the device page's attachment; the card and the port line in a browser
  with mocked answers (which found and fixed a checkbox that was reset on every redraw). **lldpd itself has not run on a router here.**

## 4. SNMP: what it takes

- **A library, or our own:** `github.com/gosnmp/gosnmp` (BSD-2, widely used, v1, v2c and v3) is the usual choice; the other way is a small
  v2c-only client of our own (GET, GETNEXT, GETBULK in BER, a few hundred lines, no new dependency, no v3). Recommendation: gosnmp, checked
  by `govulncheck` like everything else, so that v3 (authentication and encryption) is available; **v2c sends the community in clear text**:
  the page says so, and v3 is the default offered.
- **Secrets:** communities and v3 keys are stored like the PPPoE password (never sent back to the page, never in the review of changes,
  in a file of mode 0600, in a backup the way the other secrets are, `docs/backup.md`).
- **The poll is read-only** (SNMP GET only); the router never sets anything on a switch. Polls have a timeout and a cap on the walk, run
  one device at a time, and a device that does not answer is marked unreachable and retried more slowly.
- **Where the router reaches them:** the switches are on a local network of the router, so no firewall change is needed (outgoing is
  allowed); a switch on another network needs a rule that lets the router through, which the page checks and says.
- **What switches give:** most managed switches (UniFi, TP-Link JetStream, Netgear, MikroTik, Cisco, HP/Aruba, Zyxel) answer `IF-MIB`,
  `BRIDGE-MIB` and often `Q-BRIDGE-MIB` and `LLDP-MIB`; unmanaged and "smart" switches usually do not. Access points (UniFi) often give
  `IF-MIB` and the station table in a vendor MIB: only the generic MIBs are used at first, vendor extras later by request.

## 4a. One way to watch a device, two ways to talk to it

Decided with the maintainer (2026-11-28): **PeakRoute devices are read through their own API; anything else through SNMP**, and both
stay. In the code that is one idea, a **watched device** (name, address, kind, last poll, reachable or not) with a **driver** that fills in
what the pages need and nothing else knows which driver it was:

| Driver | For | Gives |
|---|---|---|
| `peakroute-ap` | PeakRoute APs (adopted) | radios, Wi-Fi networks, stations with signal and rates, ports, uptime, config state |
| `snmp` | other switches, access points, anything with SNMP | system, ports (name, status, speed, counters, PoE), forwarding table (which hardware address is on which port), LLDP neighbours |
| later: a PeakRoute switch, and the SDN controller | what the standalone router does not manage | the same record; the pages and the device page do not change |

A driver returns one record: *ports* (name, up, speed, VLAN, neighbour, the hardware addresses learned on it), *clients* (hardware address,
port or radio, signal where there is one) and *health* (uptime, errors). The device page and the Network devices page read the record, so the
SDN controller, when it comes, is one more driver and not a rewrite. Nothing here assumes the controller: the standalone router is the
only mode that is built.

## 4b. PeakRoute AP in this

The access points run PeakRoute AP, so they need not be treated like third-party switches:

- **The router reads an AP through its own API, not SNMP** (phase 3 gains an *AP* kind of watched device, added when the AP is adopted):
  the status the AP already serves has the stations (signal, rates, network and band) and the radios, with nothing new to run on the AP
  and the same sign-in the adoption set up. A Wi-Fi client's page then says "**AP hall, Home 5 GHz, -58 dBm**" and the Network devices page
  lists the AP with its radios, channel, clients and uptime; a client that moves between two APs shows as a change in its history.
  Missing fields (channel utilisation, per-station bytes and retries) are added to the AP's status, which is cheaper than a new protocol.
- **LLDP on the AP** (`lldpd`, about 1-2 MB): the AP announces its name to the switch it is plugged into, so the switch's own LLDP table
  (and so our phase 3) names the port "PeakRoute AP hall". The AP's hardware address is in the switch's forwarding table too, so the page
  can say "AP hall is on switch 2, port 7".
- **An SNMP agent on the AP** is a separate and optional question, for **other tools** (LibreNMS, Zabbix, Grafana) that want to monitor it
  without our API. Two ways: Debian's `snmpd` (net-snmp: the interface table with the port and radios' counters, system, CPU, memory and
  disk; a small `pass_persist` script for a PeakRoute AP table of stations under a private enterprise number), or an agent inside the AP
  daemon (no extra process, but v3 on the server side is ours to write). Costs: `snmpd` is a few MB of resident memory on a board with 90
  MB free, and an open UDP port: so **off by default, v3 only, answering only the router's address** (and any address the administrator
  adds), switched on from the AP page. Suggested: after phase 3, and only if you use such a tool.

## 5. Questions for the maintainer

1. **Which switches and access points do you have** (make and model)? That decides which MIBs have to work first and what to test against.
2. **How long to keep a device's history**, and may it be per-device bytes and presence only (no destinations)? Plan: 1-minute rows for a
   day, 5-minute rows for 30 days, a setting to change it; roughly 15-20 MB for 100 devices.
3. **SNMP v3 as well as v2c from the start?** Plan: yes (gosnmp). v2c-only would be a smaller first slice.
4. **LLDP: run `lldpd`, or only read?** `lldpd` also announces the router to the switches (they then list it by name), which most people
   want; it is one more daemon on the image.
5. **An SNMP agent on the AP, for other monitoring tools?** Plan: later and optional (section 4b); the router does not need it.
6. **Where does the device page live?** Plan: a view of its own (`#device/<mac>`) reached from the Clients list, the Overview's
   top-devices list and the firewall log, with a back link, not a pop-up.

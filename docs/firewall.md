# PeakRoute OS firewall

Status (2026-10-11): **all seven stages built** (the model, the ruleset, the API, the Rules, Port forwarding and Connections pages, and
logging). What is deliberately left out is listed in section 12. This file was written first and every stage's commit updates it. It is meant to become the firewall chapter of the user documentation, so it
is written for a person who runs the router, with the developer details in the sections marked *Internals*.

Contents: 1 What exists today, 2 The model (and why not zones), 3 Rules, 4 Order of evaluation, 5 Port forwarding,
6 Connections, 7 Logging and counters, 8 Staying out of trouble, 9 Recipes, 10 Internals, 11 Build plan, 12 Not planned.

---

## 1. What exists today

The router already has a firewall, generated from the configuration into `/etc/nftables.conf`. Nobody writes rules
yet; these switches produce all of it:

| Where | Setting | Effect |
|---|---|---|
| Local Networks | **Internet access** (per network) | the network may start connections to the internet |
| Local Networks | **Isolate from other networks** (per network) | no traffic to or from other networks (two networks talk only if neither is isolated) |
| (always) | default network | may use everything on the router, including the web interface |
| (always) | other networks | may only use the router's DHCP, DNS and ping |
| (always) | the internet | may start nothing: replies to our connections only |

Everything is **stateful**: once a connection is allowed, its replies and related packets (such as ICMP errors)
come back automatically, and packets that belong to no connection are dropped.

## 2. The model

### Per-interface rules, written as "from, to"

Rules are attached to the place traffic **comes from**: one of your Local Networks, or the Internet. Each rule also
says where the traffic is **going**: anywhere, this router, the internet, or another Local Network. The Rules page
shows one list per source (LAN, Guest, IoT..., Internet), the way pfSense and OPNsense do, and each list is read top to
bottom: **the first rule that matches decides**. The source is picked from a dropdown at the top of the page (like Local
Networks), not from tabs, because there can be up to 17 networks; in this document and in the API the lists are still called
"tabs".

A new network has no rule that lets it anywhere, so it starts isolated from everything except the router's DHCP and
DNS, which is what the Local Networks page already promises.

### Why not zones

A zone model (group networks into zones, then write a policy between zones) is what UniFi does now. It earns its
keep with many networks that behave the same. PeakRoute has at most 17 networks, mostly each with its own purpose,
so a zone would usually hold one network and add a layer to learn without removing anything. Writing the rule next
to the network it protects is easier to read and to audit.

What zones are good for, grouping, is covered without them: a rule can name **one network, the internet, this
router, or an address / address range**, and *groups of addresses and ports* (aliases, section 12) can be added later
without changing how rules are written or stored. Because a rule already names its source and its destination as
"endpoints" (not as interface names), the same rules could later be pushed to switches or access points by a
central controller, which is where the project wants to be able to go.

### The router's own traffic

Traffic the router starts itself (updates, DNS lookups, pings from the diagnostics page) is not filtered.

## 3. Rules

A rule has these fields.

| Field | Values | Notes |
|---|---|---|
| name | text, up to 60 characters | shown in the list, in logs and in the generated file |
| enabled | on / off | off keeps the rule without applying it |
| from | a Local Network, or **Internet** | which tab the rule is on; fixed when the rule is made |
| action | **Allow**, **Block**, **Reject** | Block drops silently; Reject answers (TCP reset, ICMP "port unreachable"), so the sender fails at once instead of waiting |
| protocol | any, TCP, UDP, TCP and UDP, ICMP | ports only with TCP / UDP / both |
| source | any, or an IPv4 address or range inside the "from" network | e.g. one device, or `192.168.20.0/28` |
| to | **any**, **this router**, **Internet**, or a Local Network | "any" means the router and everything behind it |
| destination | any, or an IPv4 address or range | combined with "to"; e.g. to *LAN*, destination `192.168.1.20` |
| ports | `80`, `80,443`, `8000-8100`, or a mix (up to 15 entries) | destination ports; the source port is not matched |
| log | on / off | writes matching packets to the system log |

Checks (the same ones everywhere: page, API and the file writer): every field is validated, ports are 1 to 65535
and ranges go upward, addresses must be IPv4, "from internet to internet" and "from a network to itself" are refused,
and there can be at most 200 rules.

Some combinations are accepted but flagged, because they can never do anything useful: a rule that no packet can
reach because an earlier rule already decides the same traffic is marked **shadowed** in the list.

## 4. Order of evaluation

For traffic **through** the router (the forward path), the first matching line below decides. For traffic **to** the
router (the input path) the same order applies with the lines marked *router*.

1. **Basics** (not editable): replies and related packets of an allowed connection are allowed; packets that belong
   to no connection are dropped.
2. **DHCP**: requests from any Local Network to the router are always allowed (a network without DHCP could not work
   otherwise and a rule that blocked it would only break things).
3. **Anti-lockout** *(router)*: the **default network** may always reach the router's web interface and SSH. This
   exists so that no rule can lock you out of your own router. It can be switched off in Settings once that page has
   the option.
4. **Port forwards** (section 5): the traffic they forward is allowed.
5. **Your rules**, from the tab the traffic comes from, top to bottom.
6. **Network policy (the defaults)**: what the switches on Local Networks mean, shown under the rules on each tab as fixed lines:
   - *Allow to the internet (set on Local Networks)*, if that network's **Internet access** is on
   - *Allow to the other Local Networks that are not isolated: LAN, IoT (set on Local Networks)*, one line naming the networks it can reach: a
     network is reachable when neither it nor this one is isolated; when none is, the line says that they are all isolated
   - *Allow the router's DNS and ping* (other networks), or *Allow everything on the router* (the default network)
7. **Everything else is blocked.** This line is also shown, at the bottom of every tab, so that "why doesn't this
   work?" always has a visible answer.

Your rules come before the network policy on purpose: *Block* rules ("this camera never reaches the internet") must
win over the Internet switch, and *Allow* rules ("this printer may be reached from LAN") work from anywhere in the
list. The switches on Local Networks keep working exactly as they do today and need no rules.

## 5. Port forwarding

A forward sends connections that arrive from the internet on one of the router's ports to a device inside.

| Field | Notes |
|---|---|
| name, enabled | as for rules |
| protocol | TCP, UDP or both |
| external port(s) | `443`, or a range `5000-5010` |
| to | an IPv4 address inside one of your Local Networks |
| internal port(s) | same count as the external ones; empty means the same numbers |
| allowed sources | any, or up to 10 addresses / ranges (so a forward can be limited to your office) |

A forward is written as two things that always travel together: the address translation, and the line in step 4
that lets the translated traffic through. You never write the second one, and it cannot be forgotten.

A forward only applies to traffic coming in on the internet connection. Devices inside that use the router's public
address to reach the forwarded service (called *NAT reflection* or *hairpin*) are **not** handled in the first
version; use the device's inside address or a local DNS name.

## 6. Connections

A read-only page of the connections currently passing through the router: protocol, state, source, destination
(with the device names from DHCP), age, and how much has been transferred, with a filter box and the totals. It
shows what the firewall is actually dealing with, and lets you see at a glance what a rule did. It needs the
connection tracking table, which the kernel keeps for the stateful rules anyway; byte counters are switched on for it.

## 7. Logging and counters

- Every rule carries a **hit counter** (packets since the rules were last loaded), shown in the rule's line; reloading the
  rules (any commit that changes the ruleset) starts the counts again.
- A rule with **log** on writes a line to the kernel log for every packet it matches, tagged `fw:<id>`, limited to five per
  second per rule so that a scan cannot fill the disk (`fw:fw2 IN=br30 OUT=eth0 SRC=10.30.0.40 DST=93.184.216.34 ... PROTO=TCP
  SPT=40000 DPT=443`). The raw lines are in the system journal, so **System, System logs** shows them as they are too.
- **Firewall, Logs** shows them **in words**, newest first: when (and, for a repeated packet, until when), the rule and what it
  does, from, to (with device names from DHCP where known), protocol and how many times. It looks the rule up in the
  configuration that is running, so a rule that was deleted since shows by its id, marked *rule no longer there*; a rule that is
  only in the draft cannot have logged anything. Filters: **search** (address, port, interface or protocol), **rule**, **what the
  rule does** (allowed, blocked, rejected), **traffic from** (a Local Network or the internet) and **when** (last hour, 24 hours,
  7 days, everything kept). It refreshes every 10 seconds (**Pause** stops that), loads 200 entries and *Show more* for the rest,
  says how much the log holds, and **Download CSV** saves what the filters select. The **Rules** page only says which rules are
  *Logged* and points there.
- **The firewall log is kept by the router itself, not by the journal.** The system journal is shared with everything else and
  is size limited, so a busy rule must not be able to push the system's own messages out, and the journal rotating must not
  take the firewall log with it. The daemon follows the kernel lines that carry a rule's tag (`journalctl -k -f --grep=fw:`) and
  saves each packet, parsed, in `firewall_log.db` next to the other databases:
  - **What is kept:** **7 days, or 100,000 entries, whichever comes first** (about 15 to 25 MB at the most); the oldest go first.
    The limit is on entries, not packets, because of the next point.
  - **Repeats are folded.** The same kind of packet (same rule, interfaces, addresses, protocol and destination port, whatever the
    source port) that is seen again within a minute of the last time is counted in the same entry: a port scan is one row that
    says *seen 342 times*, with the first and the last time, and does not push everything else out.
  - **Written in batches** every couple of seconds in one transaction (one write per packet would wear flash for nothing). A
    power cut costs at most the last few seconds.
  - **It carries on where it stopped.** The journal position of the last saved packet is kept, so after a restart nothing is lost
    (as far as the journal still has it) and nothing is saved twice. A position journalctl refuses is dropped and reading starts
    again from the start of what the journal keeps. Without a journal (no `journalctl`) it tries again every 30 seconds and the
    page says the log cannot be read.
- `GET /api/v1/firewall/log` serves it, newest first, a page at a time: `known`; `entries` (`id`, `time` and `last` in Unix
  milliseconds, `count`, `rule`, `name`, `action`, `text`, `in`, `out`, `src`, `sport`, `dst`, `dport`, `proto`); `next` (the
  `before=` for the next page, 0 for the end); `total`, `oldest`, `keep_days` and `keep_entries`. The filters, all optional:
  `limit` (1 to 1000, default 200), `before` (an entry id), `rule` (one rule by id), `action` (`allow`, `block` or `reject`:
  the rules that do that), `network` (the rules for traffic from a Local Network by id, or `wan`), `hours` (1 to 168), and `q`
  (text in an address, interface, protocol or port number). `rule`, `action` and `network` narrow each other and are matched
  against the rules in effect; a rule that is no longer there can only be asked for by its id.
- `GET /api/v1/firewall/log.csv` downloads everything the same filters select (not a page): first and last seen in UTC, times,
  rule, action, interfaces, addresses and ports, protocol. A cell that starts like a formula (`=`, `+`, `-`, `@`) is written
  with a leading quote so that a spreadsheet does not run it (rule names are typed by people).

## 8. Staying out of trouble

- **Nothing changes until you commit**, like every other page: rules are saved as a draft, shown in the review, and
  applied together.
- **Firewall changes count as risky**: after committing there are 60 seconds to confirm; if you cannot reach the page
  any more, the change is undone by itself.
- The file is **checked with `nft -c` before it is applied**, and the service is only reloaded if the check passes.
- The **anti-lockout** line (section 4) cannot be removed by a rule.
- The review shows rules in words ("Block: IoT to LAN, TCP 9100, 631"), not as nftables syntax.

## 9. Recipes

*These become real screenshots and steps once the page exists.*

1. **Keep guests off your LAN, allow the internet.** Local Networks: Guest has Internet access on and Isolation on.
   No rule is needed.
2. **Let the LAN use a printer on the IoT network.** On the **LAN** tab add *Allow, to IoT, destination 192.168.30.20,
   TCP 9100*. Replies come back on their own. It works even though IoT is isolated, because your rules come before the
   network policy; nothing else on IoT becomes reachable.
3. **A camera that must never reach the internet.** On the camera's network tab add *Block, to Internet, source
   192.168.30.40*, above everything.
4. **Open a web server to the world.** Port forwarding: TCP 443 to 192.168.1.50.
5. **Answer pings from the internet.** Internet tab: *Allow, to this router, ICMP*.
6. **Only the office may reach the forwarded server.** Port forwarding: set *allowed sources* to the office's address.

## 10. Internals

**Storage.** `network.json` gets a top-level `firewall` object, written only when there is something in it:

```json
"firewall": {
  "rules": [
    {"id": "fw1", "name": "Printer", "from": "lan", "action": "allow", "protocol": "tcp",
     "to": "iot", "destination": "10.30.0.20", "ports": "9100,631"},
    {"id": "fw2", "name": "Camera", "from": "iot", "action": "block", "protocol": "any",
     "source": "10.30.0.40", "to": "internet", "log": true}
  ],
  "forwards": [
    {"id": "pf1", "name": "Web", "protocol": "tcp", "external_ports": "443", "to_ip": "192.168.1.50",
     "internal_ports": "8443", "sources": ["203.0.113.7", "198.51.100.0/24"]}
  ]
}
```

Rules name networks by id (`lan`, `net2`...) and the internet as `wan`, never by interface, so a renamed port or a VLAN on
the internet connection needs no rule edits. The list order is the rule order. Disabled rules are kept and not written to
the ruleset. The code is `internal/config/firewall.go` (model, validation, review text, shadow detection) and
`internal/translator/firewall.go` (the ruleset).

**What the ruleset looks like.** With no rules and no forwards the file is exactly what it was before the firewall pages
existed (a test compares the bytes). With rules, lines are *added at fixed places*; here is a real one (the rules of the
test configuration: LAN `eth1`, Guest `br20`, IoT `br30`, internet `eth0`):

```
    chain input {
        type filter hook input priority filter; policy drop;
        iifname "lo" accept
        ct state established,related accept                       # 1 basics
        ct state invalid drop
        # always allowed, whatever the rules say: DHCP, and the default network's way into this router
        iifname { "eth1", "br20", "br30" } udp dport 67 accept     # 2 DHCP
        iifname "eth1" tcp dport { 22, 443, 8443 } accept          # 3 anti-lockout
        # firewall rules, in order
        # fw3: Allow ICMP, from the internet to this router
        iifname "eth0" meta l4proto icmp counter accept comment "fw:fw3"      # 5 your rules
        # fw4: Reject TCP and UDP 53, from Guest to anywhere
        iifname "br20" meta l4proto { tcp, udp } th dport 53 counter reject comment "fw:fw4"
        iifname "eth1" accept                                     # 6 network policy (unchanged)
        iifname "br20" udp dport { 53, 67 } accept
        ...
    }
    chain forward {
        ...
        # port forwards: the traffic they translate is let through   # 4
        iifname "eth0" ip daddr 192.168.1.50 meta l4proto tcp th dport 8443 ct status dnat counter accept comment "fw:pf1"
        # firewall rules, in order
        # fw1: Allow TCP 9100,631, from LAN to IoT (10.30.0.20)
        iifname "eth1" oifname "br30" ip daddr 10.30.0.20 tcp dport { 9100, 631 } counter accept comment "fw:fw1"
        iifname "eth1" oifname "eth0" accept                      # 6 network policy
        ...
    }
table ip nat {
    chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
        # pf1: TCP 443 from the internet to 192.168.1.50:8443, only from 203.0.113.7, 198.51.100.0/24
        iifname "eth0" ip saddr { 198.51.100.0/24, 203.0.113.7 } meta l4proto tcp th dport 443 counter dnat to 192.168.1.50:8443 comment "fw:pf1"
    }
```

(The numbers and `#` notes on the right are only for this document.) Points worth knowing:

- A rule going **to this router** is written once, in the input chain; one going **through** (to the internet or to a
  network) once, in the forward chain; **to anywhere** twice, one in each.
- Every rule has a `counter` and `comment "fw:<id>"`. The id is the only thing from the configuration that is used as a
  tag; the readable sentence is a `#` comment line above, made of plain ASCII (anything else becomes `?`). **Rule names are
  not written to the file at all.**
- A **reject** of TCP sends a reset (`reject with tcp reset`); of anything else the default `reject`, which sends an ICMP
  "unreachable". Both make the sender fail at once; a **block** is silence.
- **log** adds `log prefix "fw:<id> " limit rate 5/second`.
- The DHCP and anti-lockout lines (steps 2 and 3) appear **only when at least one rule is switched on**, so the file stays
  as it was for everyone who does not use rules. The anti-lockout ports are SSH (22) and the web interface (443, or 8443 in
  development).
- A port forward is a translation (`dnat`) in the `ip nat` table's new `prerouting` chain, plus a line in the forward chain
  that accepts exactly the translated traffic (`ct status dnat`, the inside address and port). A single port is rewritten to
  the internal port; a range keeps its numbers. Both lines follow `WanDevice()`, so they work over a VLAN or PPPoE.
- A forward is only valid for an address on one of the Local Networks (not the router's own, not a network or broadcast
  address); two enabled forwards may not claim the same external port with the same protocol (TCP and UDP are separate;
  "both" claims both).
- Rules that can never be reached (an earlier rule on the same tab already covers everything they match) are detected
  (`NetworkConfig.Shadows`) for the page to flag; they are still written, harmlessly.

**The API** (`internal/netapi/firewall.go`; all need a session, changes also the CSRF token, and are refused with 409 while
a change is being applied; everything is saved as a draft):

| Request | What it does |
|---|---|
| `GET /api/v1/firewall` | the **tabs** (`lan`, each network's id, `wan`), the **rules** (every field plus `text`, the rule in words; `pending`, differs from what is running; `shadowed_by`, the earlier rule that makes it unreachable), the **forwards**, the **policy** (the fixed lines of every tab: `before` the rules and `after` them, each with a `kind` of `allow`, `block` or `off`), and `editable` |
| `GET /api/v1/firewall/connections?limit=` | the connection table (`known`, `count`, `max`, `accounting`, `total`, `shown`, and `connections`: protocol, state, expiry, source and destination with ports, `source_nat`, `dest_nat` and `dest_nat_port`, bytes and packets both ways); the busiest first; `limit` 1 to 5000, default 1000 |
| `GET /api/v1/firewall/log` | the packets that rules with logging matched, newest first, a page at a time, filtered, each with its rule (see section 7) |
| `GET /api/v1/firewall/log.csv` | the same as a CSV download (section 7) |
| `GET /api/v1/firewall/counters` | `known` and `counters` by id (`fw1`, `pf1`): packets and bytes since the ruleset was last loaded; read from `nft -j list ruleset` |
| `POST /api/v1/firewall/rules` | adds a rule at the end of the list: `name`, `from`, `action`, `to` are required; `protocol` defaults to `any`, `enabled` to on |
| `PUT /api/v1/firewall/rules/{id}` | changes a rule; fields left out stay. `from` cannot change (a rule stays on its tab) |
| `DELETE /api/v1/firewall/rules/{id}` | removes it |
| `PUT /api/v1/firewall/order` | `{"from": "lan", "ids": [...]}` puts one tab's rules in that order; the ids must be exactly that tab's rules; the other tabs' rules keep their places |
| `POST /api/v1/firewall/forwards`, `PUT` / `DELETE /api/v1/firewall/forwards/{id}` | the same for port forwards (`sources` left out stays, `[]` clears it) |

Ports are accepted with spaces (`80, 443`) and stored without. Errors name the field (`name`, `to`, `ports`, `to_ip`, ...)
and only concern the rule being edited: another rule's problem does not stop you saving this one. Ids are `fw1`, `fw2`... and
`pf1`...; the lowest free one is used. A Local Network cannot be deleted while a rule uses it (as the source or the
destination) or a forward sends to a device on it. The fixed lines are written by the API (not the page) so that the words
always match what the ruleset does.

**The Rules page** (`web/static/scripts/firewall_rules.js`, in the menu under Firewall, Rules):

- The dropdown at the top chooses where the traffic comes from; it shows how many rules each source has.
- The page is laid out in the order the firewall really applies it, top to bottom: **System rules** (grey, with a lock, no
  number, cannot be edited: replies to connections that were started, DHCP, and for the default network the anti-lockout) are
  always checked first and nothing of yours can override them; then **Your rules**, **numbered by priority** from 1 downward
  (the first that matches decides; moving a rule with the arrows renumbers them); then the **Defaults** (also locked: what the
  Internet and isolation switches on Local Networks mean, ending in "Block everything else"), which decide whatever none of your
  rules matched.
- *Why the defaults are not in front of your rules:* they are defaults, not rules the system imposes. If they were checked first,
  an Allow they contain (for instance "Allow to the internet") would win over a Block of yours, and you could not keep one
  device off the internet while its network has Internet access on. Only the safety lines, which exist so that nothing can lock
  you out or break addressing, are in front.
- Each rule shows its name, what it does (Allow green, Block red, Reject amber), a **Draft** tag until it is committed, a
  **Logged** tag, the rule in words, and its **hit count** (packets since the rules were last loaded; a dash for a rule that is not
  running yet; refreshed every 5 seconds without redrawing the list). A rule that no traffic can reach shows an amber
  "Never matches: the rule X above already decides all of this traffic".
- Controls: move up and down (saved at once as a new order), a switch to turn a rule on or off, **Edit**, and **Delete** (two
  clicks, the second within four seconds). **Add rule** opens the editor below the list.
- The editor asks for a name, what to do, where the traffic is going (the internet, anywhere, this router, or another network;
  not "the internet" when the traffic comes from the internet), the protocol, the ports (only for TCP, UDP or both), optional
  from and to addresses, whether to log it and whether it is on. Mistakes appear under the field they belong to.
- Nothing is applied by the page: every save writes the draft and the bar at the top offers Review and commit.

**The Port forwarding page** (`firewall_forwards.js`, in the menu under Firewall): one line per forward with its on/off switch,
its sentence ("TCP 443 from the internet to 192.168.1.50:8443, only from 203.0.113.7"), a hint with the address to use from
outside ("Reach it at 203.0.113.10:443"; it is the internet address in effect, shown when the router has one), the hit count,
**Edit** and **Delete** (two clicks). The editor asks for a name, the protocol, the port on the internet side (one port or one
range), the device to send to (the field suggests the devices that hold a DHCP lease), the port on the device (empty keeps
the number; **greyed out for a range, which keeps its numbers**), and optional allowed addresses.

**The Connections page** (`firewall_connections.js`, in the menu under Firewall; read only): the connections passing through the
router right now, the busiest first. Each line has the protocol, **From** (the device that started it, with its DHCP name when
it has one, and "seen outside as ..." when the router rewrote its address), **To** (and "sent on to NAS 192.168.1.60:8443 (port
forward)" when a forward redirected it), the state (Established, Connecting, Closing, Closed waiting, Waiting for a reply,
Active), how long until it is forgotten, and the data it carried in each direction. There is a search box (addresses, ports,
device names, states), a protocol filter, a Pause button for reading, and a line saying how many connections are tracked and
how full the table is (the busiest 2000 are loaded; the page shows 200 at a time).

*Where it comes from:* the kernel's connection tracking table, read from `/proc/net/nf_conntrack` where the kernel offers it
and otherwise with the `conntrack` tool (the image installs the `conntrack` package: **Debian's kernel may not have the /proc file**;
the Ubuntu kernel this was developed on does not). Both formats are parsed by one function and tested against real output. The
byte counts need the kernel's accounting (`nf_conntrack_acct`), which the daemon switches on the first time the page is opened;
it only counts connections that start afterwards, and the page says so. On a machine where the table cannot be read (not
root, no module) the page says that instead of an empty table.

**Review lines.** One line per rule or forward added, removed or changed ("Allow TCP 9100,631, from LAN to IoT
(10.30.0.20)" with the old text beside it), a rename is one change, and one line "Firewall rule order" when the order of
rules that exist before and after differs.

**Checks, in order of how much they prove.**

1. *Real packets.* `TestTheRulesDecideWhatRealConnectionsDo` builds three network namespaces joined by virtual cables (a LAN
   client and a guest client, the router with the generated ruleset loaded into a real kernel, and a server playing the
   internet) and tries real TCP and UDP connections and pings: that an allow opens only what it names, that a block beats
   the Internet switch, that reject refuses at once while block is silent, that the first matching rule wins, that
   switched-off rules do nothing, that source and destination narrow a rule, that a rule can reach the router but not
   the anti-lockout ports, that "anywhere" covers the router and what is behind it, and that port forwards translate,
   keep ranges, honour allowed sources and leave everything else closed. It needs no privileges (`unshare -rn`) and skips
   itself where namespaces or the needed kernel modules are missing.
2. *The real `nft -c`* accepts the ruleset in every variant (with and without rules, forwards, and over PPPoE).
3. *Exact text* of the whole ruleset, and of each kind of rule line, is compared in tests.
4. *Safety at the last moment:* even if the validation were bypassed, the writer refuses ids, addresses, ports and network
   names that are not plain.

## 11. Build plan

Each stage is its own commit with tests and updates this document.

1. **Model** (done): rules and forwards in the configuration, validation, review lines, shadow detection.
2. **Ruleset** (done): the translator writes the rules; checked with `nft -c` and with real packets between namespaces.
3. **API** (done): list, add, change, delete, reorder and switch rules and forwards; hit counters.
4. **Rules page** (done): a list per network and the Internet, chosen from a dropdown; the fixed policy lines, add / edit / reorder / switch / delete, hit counts.
5. **Port forwarding page** (done).
6. **Connections page** (done): the connection table with names, NAT, data, search and protocol filter.
7. **Logging** (done): the firewall log store and the **Firewall, Logs** page (2026-10-11; before that a *Logged traffic* card on the Rules page and the raw lines in the journal). Still to do before the user documentation: screenshots in this document.

## 12. Not planned (yet)

Address and port groups (aliases) · NAT reflection · time schedules · per-rule bandwidth limits · IPv6 (the whole router
is IPv4 for now) · filtering the router's own outgoing traffic · zones (see section 2) · importing and exporting rule
sets · a rule "wizard" · GeoIP blocking.

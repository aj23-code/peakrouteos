# PeakRoute OS firewall

Status: **design (2026-10-11), not built yet.** This file is written first and kept up to date as each stage is
built; every stage's commit updates it. It is meant to become the firewall chapter of the user documentation, so it
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
says where the traffic is **going**: anywhere, this router, the internet, or another Local Network. The Firewall page
has one tab per source (LAN, Guest, IoT..., Internet), the way pfSense and OPNsense do, and each tab is read top to
bottom: **the first rule that matches decides**.

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
6. **Network policy**: what the switches on Local Networks mean, shown under the rules on each tab as fixed lines:
   - *Allow to the Internet*, if that network's **Internet access** is on
   - *Allow to other networks*, if neither network is isolated
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

- Every rule carries a **hit counter** (packets and bytes since the rules were last loaded), shown in the list.
- A rule with **log** on writes a line to the system log (`peakroute-fw`), readable on the Logs page, with the rule's
  name, the interface, and the addresses and ports. Logging is limited to a few lines per second per rule so that a
  scan cannot fill the disk.

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

*(Filled in as it is built. The planned layout:)*

- **Storage:** `network.json` gets a top-level `firewall` object: `rules` (an ordered list) and `forwards`. Rule ids
  are `fw1`, `fw2`...; forward ids `pf1`...
- **Interface names:** rules name networks by id (`lan`, `net2`) and the Internet as `wan`; the translator turns them
  into interfaces (`LanTarget`, `br20`, `WanDevice()`), so a renamed port or a VLAN on the WAN needs no rule edits.
- **nftables layout:** the existing `inet filter` table keeps its `input` and `forward` chains; `inet filter` gets the
  rules in the order of section 4, each with `counter` and `comment "fw:<id> <name>"` so the page can read the hit
  counts back (`nft -j`). Port forwards add a `prerouting` chain to the `ip nat` table. With no rules and no forwards
  **the file is byte-for-byte what it is today** (tested).
- **Rejects** use `reject with tcp reset` / `reject with icmp port-unreachable`.
- **Testing:** the file is checked with the real `nft -c`; behaviour is tested with real packets between network
  namespaces (a client, the router and a server): what each rule must let through and keep out is asserted by
  actually trying to connect.

## 11. Build plan

Each stage is its own commit with tests and updates this document.

1. **Model**: rules and forwards in the configuration, validation, review lines, shadow detection.
2. **Ruleset**: the translator writes the rules; checked with `nft -c` and with real packets between namespaces.
3. **API**: list, add, change, delete, reorder and switch rules and forwards; hit counters.
4. **Rules page**: a tab per network and the Internet, the fixed policy lines, add / edit / reorder / switch.
5. **Port forwarding page**.
6. **Connections page** (connection table, filters).
7. **Logging** and the Logs page filter; polish; screenshots in this document.

## 12. Not planned (yet)

Address and port groups (aliases) · NAT reflection · time schedules · per-rule bandwidth limits · IPv6 (the whole router
is IPv4 for now) · filtering the router's own outgoing traffic · zones (see section 2) · importing and exporting rule
sets · a rule "wizard" · GeoIP blocking.

# A second internet connection: failover and load balancing (plan, 2026-11-27: nothing of this is built yet)

Goal: a router that can have **two internet connections** (WAN and WAN 2), watches each one, and either **fails over** from the
first to the second when the first stops working, or **balances** the networks' traffic over both, switched on and managed like
everything else here (drafts, review, commit, confirm). Standalone mode only. IPv4 only (docs/ipv6.md is shelved).

The model is Meraki's: every uplink is probed **through itself**, an uplink is down only when its probes fail together and
repeatedly, and traffic goes back to the preferred uplink only after it has been healthy for a while.

## 1. Where we start (checked in the code)

- **One internet connection is assumed everywhere.** `NetworkConfig.WanDevice()` is used in about 21 places: the interfaces file,
  nftables (forward, NAT masquerade, input), the usage counters, the health check, SQM (`SQM_DEV`), DDNS, the setup network. Config is
  flat (`wan_interface`, `wan_mode`, `wan_ip`, `wan_gateway`, `wan_dns`, `wan_vlan`, `wan_mtu`, `wan_mac`, `wan_pppoe`, ...).
- **The API already has the shape**: `netapi.Uplink` (id, name, port, mode, address, DNS, VLAN, MTU, MAC, PPPoE, device, live status)
  and `GET/PUT /api/v1/uplinks/{id}`, with the comment "a second uplink would be another of these"; the only one is id `wan`.
  The Ports page assigns a role (`RoleWAN`) to a port.
- **The health check watches one line** (`internal/wanhealth`): every 5 s it pings the upstream DNS server and the gateway
  (raw ICMP, `ping.go`; an HTTP fallback in `http.go` when ICMP cannot be used), keeps per-minute history in `wan_health.db`, and says
  online / offline (3 probes in a row failed) and **where it breaks** (gateway answers but nothing beyond, and so on). Its probes
  follow the default route, so they cannot say which of two lines is down: they have to be **bound to a device**.
- **The default route is the interface stanza's**: DHCP (`udhcpc` hook), static `gateway`, or pppd's `defaultroute`. Static routes
  have a `metric` field (`render.go`). Nothing manages the default route at run time.
- **Marks already in use**: the firewall counting table, and SQM's per-network class in bits 16-23 of the connection mark
  (`0x00ff0000`, `docs/sqm.md`). The balancing marks need a byte of their own (section 5).
- **Firewall zone `wan`** (`FromWAN`) is one zone; with two uplinks it must mean "either".

## 2. Choices made with the maintainer (2026-11-27)

- The monitor is **Meraki-like**: probes through each uplink, down only when every probe fails, a recovery time before failing back.
- **Probe targets start as the uplink's current upstream DNS servers (and its gateway), and the administrator can add more.**
- **Two modes when there are two uplinks: failover, or load balanced.**

## 3. The model

An **uplink** is what `Uplink` already is, plus how it is watched. There are at most **two**: `wan` (first) and `wan2`. A port is given
the role *WAN* or *WAN 2* on the Ports page. Config keeps the first uplink's flat `wan_*` fields (nothing to migrate, an old backup
restores unchanged) and adds the second as a nested object with the same fields, so one accessor, `Uplinks()`, returns both as a list of
`{ID, Link, Device, Kind, Gateway, DNS, ...}` and every place that says `WanDevice()` loops over it or asks for "the uplinks that are up".

Settings (new, on the Internet page, shown when there is a second uplink):

| Setting | Values | Default |
|---|---|---|
| mode | **failover**, **load balanced** | failover |
| preferred uplink (failover) | WAN, WAN 2 | WAN |
| fail back to the preferred uplink | on, off | on |
| recovery time (before failing back) | 10 s - 1 h | 2 min |
| per uplink: probe targets | the automatic ones, plus any number of added ones (an address, or a name that is looked up) | automatic |
| per uplink: also use public fallback targets | on, off | on |
| per uplink: probe every | 2 - 60 s | 5 s |
| per uplink: down after | 2 - 20 failed rounds | 3 (so about 15 s) |
| per uplink: weight (load balanced) | 1 - 100 | 1 |

## 4. Watching an uplink

- **Each probe goes out through its own uplink**: `SO_BINDTODEVICE` to the uplink's device (`ppp0` for PPPoE, `eth0.100` for a VLAN) and
  the uplink's address as the source, so the answer comes back the same way regardless of the routing table.
- **Targets, per uplink**: (1) its **gateway**, (2) its **upstream DNS servers as they are now** (the ones the connection got from
  DHCP/PPPoE or that were set; re-read every round, because they change), (3) the **targets the administrator added**, (4) with
  *public fallbacks* on, 1.1.1.1 and 9.9.9.9. The Internet page lists them with their last result.
- **A target only counts once it has answered.** Many providers' DNS servers do not answer ping; a target that has never answered since
  the uplink came up is shown as "does not answer ping" and is **left out of the decision**, so it cannot make a healthy line look down.
  If nothing answers, the HTTP/DNS probe in `http.go` is tried.
- **Down** = every target that counts failed for *down after* rounds in a row. The gateway is evidence for the diagnosis ("the
  gateway answers but nothing beyond it does: your provider's side is down"), not part of the decision, so a provider whose gateway
  never answers a ping does not matter. **Up** = any counting target answers; the uplink is then **recovering** for
  the recovery time before failback or before it joins the balance again.
- **History** (`wan_health.db`) is kept per uplink (a series key per uplink: existing history is the first uplink's); the page shows
  both lines, the outage list names the uplink, and each state change is a line in the system log.

## 5. Routing

- **Failover.** Both uplinks have their default route in the main table, the preferred one with the lower metric. The daemon
  (a reconcile loop like SQM's, started also by the interface hooks) **removes the default route of an uplink that is down** and
  puts it back when it is up and past its recovery time (or immediately, for the secondary). A DHCP renewal that puts the route back
  is undone by the next round (at most the probe interval). Connections that were on the failed line break; new ones use the other.
  The router's own traffic follows the main table.
- **Load balanced.** Each uplink has its own routing table (100, 101) with a default route through its gateway. nftables marks **a new
  connection** as it arrives from a network (`numgen random mod <sum of weights>` mapped to an uplink, weights by the setting, only
  uplinks that are up), stores the mark in the **connection mark** (so the whole connection, and its replies, keep it), and
  `ip rule fwmark <mark> lookup <table>` sends it out of that uplink. The balancing mark uses its own byte of the connection mark
  (`0xff000000`; SQM keeps `0x00ff0000`), so the two do not touch. When an uplink goes down it leaves the number map at once (new
  connections avoid it) and its tables' routes are removed, and its connections fail like in failover. The router's own traffic follows
  the main table (the preferred uplink, with failover behaviour).
- **Answering the right way.** A connection that **arrives** on an uplink (a port forward, a VPN server, a ping to the router) must be
  answered through the same uplink: the input and prerouting rules mark it by the interface it came in on, and the same rule table
  routes the replies (plus `ip rule from <uplink address> lookup <table>` for the router's own answers).
- **NAT**: masquerade on both uplinks' devices. **Firewall**: the zone *internet* means both; nothing from either is let in.
- **Does not balance**: one connection never uses two lines, and a site that ties a session to the visitor's address sees a change
  when a connection moves; a *keep the same uplink for a destination* option is left for later.

## 6. What else has to follow the uplinks

| Part | What changes |
|---|---|
| SQM (docs/sqm.md) | one shaping tree per uplink, each with its own speeds; the settings belong to the uplink. In failover the unused one is idle. |
| Usage counters (`peakroute_acct`) | count by whichever uplink device is the outgoing/incoming one; totals add |
| DDNS | updates from the address of the uplink in use (failover) or the preferred uplink (balanced), and when it changes after a failover |
| Setup network, VPN servers, port forwards | listen on / forward from both uplinks; replies as above |
| Backup and restore | the second uplink's settings and the failover settings are in `network.json` |
| Overview and Internet pages | both uplinks' state, which one carries the traffic, a failover banner with the time and reason |
| Wireless (the AP/cellular case) | a USB tethering dongle or a second router shows up as a port; nothing special |

## 7. Phases

1. **Two uplinks, configured but only by metric.** The Ports role *WAN 2*, `Uplinks()`, interface stanzas, NAT and firewall for both,
   the Internet page with both, and routes by metric (the preferred has the lower metric). No health yet: this is already a manual
   failover. Tests: render, validation, an old config unchanged.
2. **Watching.** Bound probes, the target list (automatic and added), counting rules, per-uplink history and page. Tests: fake pinger;
   real kernel in network namespaces (two upstreams, one made to drop).
3. **Failover.** The reconcile loop, recovery time, failback, log and banner. Tests in namespaces: drop the first uplink, traffic
   moves in the probe time; bring it back, traffic returns only after the recovery time.
4. **Load balanced.** The tables, rules, number map, weights, reply symmetry. Tests in namespaces: two uplinks of different speeds,
   many connections split by weight, a port forward answered on its own uplink, an uplink dropped.
5. **Everything that follows** (section 6), the docs, NOTES.md; then check on the real router with two lines.

## 8. Open questions

1. **Does the second uplink have to be on its own physical port?** Plan: yes. A second uplink as a VLAN on the first's port (a provider
   that hands over two circuits on one cable) is possible with the same model (`wan_vlan`) and is left for later.
2. **Balance weights by hand, or from the speeds in the SQM settings?** Plan: by hand, with the speeds as the suggestion on the page.
3. **Should the router's own traffic (updates, DNS, NTP) also be balanced?** Plan: no, it follows the main table.
4. **A failover to a metered line** (a cellular dongle): worth a *use only when the other is down* mode (that is failover) and a
   warning that updates and the speed test would also use it. Not built in the first version.
5. **Probe through a PPPoE uplink that is down** (the interface does not exist): shown as "down, no link", not an error.
6. **What failback does to long connections** (a video call moving back): the recovery time and the option to turn failback off are the answer.
7. **IPv6** when it is built: each uplink with its own prefix and a failover of the advertised prefix is a separate problem (docs/ipv6.md).

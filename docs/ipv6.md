# IPv6 (plan, 2026-11-25: nothing of this is built yet)

Goal: a router whose networks have working, **firewalled** IPv6 next to IPv4, switched on and managed like everything else here
(per network, drafts, review, commit, confirm), without breaking an IPv4-only router that does nothing with it. Standalone mode only.

## 1. Where we start (checked in the code)

- **Forwarding is already on for IPv6** (`net.ipv6.conf.all.forwarding = 1` in the image's `99-peakroute.conf`), but **nothing else is**:
  no address on the WAN, no delegated prefix, no router advertisements, no DHCPv6. With forwarding on the kernel also ignores router
  advertisements on every interface unless `accept_ra = 2`, so the WAN cannot even autoconfigure today.
- The firewall is one `inet` table, so **every forward and input rule already applies to both families**: *Allow internet access* and
  *Isolate* will mean the same for IPv6 with no new switch. What is missing is ICMPv6 (the router's input chain drops neighbour
  discovery and router advertisements from the WAN; only the access point's file allows them) and the things that are IPv4-only by
  nature: NAT, port forwards (DNAT), the DNS redirect for filtering (`ip nat`), the rules page (IPv4 addresses).
- The per-device usage table already has IPv6 sets (`up6`/`down6`); the Clients, Diagnostics, DNS, DDNS and VPN pages and the validators
  say "IPv4 only".
- The WAN is built by ifupdown (`manual` stanzas whose hooks run `udhcpc`; PPPoE through pppd). dnsmasq already serves DHCP and DNS for
  every network (and can do router advertisements and DHCPv6, which is why it is the LAN side of this plan).

## 2. What an ISP gives, and what we do with it

| ISP gives | Needed |
|---|---|
| a global address by **RA/SLAAC** (and a default route from the RA) | the router listens for RAs on the WAN (`accept_ra = 2`) |
| a **delegated prefix** by **DHCPv6-PD** (usually /56, /60 or /64; it can change) | a DHCPv6 client asking for it (`ia_pd`), carving **/64s** for the networks |
| PPPoE with IPv6 | `+ipv6` in pppd (IPV6CP) plus the two lines above on `ppp0` |
| a fixed address and a routed prefix (business lines) | *static* IPv6 in the WAN settings |
| IPv4 only, or IPv4 over CGNAT / DS-Lite / 6rd | **out of scope** (no native IPv6 to hand out) |

The WAN-side client is **dhcpcd** (Debian trixie 10.1: DHCPv6 IA_NA and IA_PD, RA, hooks). Not `wide-dhcpv6-client` (2008), not
`isc-dhcp-client` (deprecated), and not systemd-networkd (it would take over what ifupdown manages). It runs for the IPv6 part only
(`ipv6only`; udhcpc keeps IPv4), with `ia_pd` assigning one /64 per network to the network's interface, so lifetimes, renewals and
renumbering are the client's job, not a script of ours.

## 3. The design

### The LAN side: dnsmasq, with prefixes that may change
- The delegated prefix is **dynamic**, and our files are rendered from configuration. So the files do not contain the prefix: a network
  gets `dhcp-range=::,constructor:<iface>,ra-stateless,ra-names` (dnsmasq builds the range from whatever /64 the interface has, and
  follows it when it changes) and `enable-ra`. Devices autoconfigure by **SLAAC**; **stateless DHCPv6** carries the extras; the DNS server
  is advertised in the RA (RDNSS). `ra-names` makes SLAAC devices resolvable by name.
- **Which /64 goes to which network**: a stable *subnet id* per network (the default network is 0, an added one gets the lowest free
  number when it is made, kept in the file). A /56 holds 256, a /60 holds 16, a /64 holds one; a network that does not fit gets no
  IPv6 and the review says so.
- **Per network switch, "IPv6"** (Local Networks), next to Internet / Isolate / mDNS / NTP: devices here get IPv6 addresses. Off until
  asked; the proposal is that ticking the WAN's IPv6 asks which networks.
- **ULA** (a private `fd00::/8` prefix for each network, advertised beside the global one so that internal addresses do not change when
  the ISP renumbers) is wanted but is **phase 2**.

### The WAN side
- A new choice in the Internet page: **IPv6: off / automatic / fixed**. *Off* is the default and writes nothing, so an existing router is
  unchanged. *Automatic* = RA + DHCPv6 (address and prefix); *fixed* = address, prefix length, gateway, DNS, and the routed prefix.
- Kernel settings per interface go where the interface is declared (`accept_ra = 2` and `autoconf = 1` on the WAN, **`accept_ra = 0`
  on every LAN network**, so that a router never autoconfigures from a device that sends RAs), and the defaults are set off, so an
  interface made later is safe.
- A prefix-length hint for the request (default: ask for a /56 and take what is given) as an advanced setting.

### The firewall (one `inet` table: most of it is already right)
- **Default deny inbound.** Established/related only. IPv6 has no NAT, so being reachable is a rule decision, never an accident.
- **ICMPv6 is not optional.** Input from the WAN: neighbour discovery and router advertisements (from link-local sources, hop limit 255),
  DHCPv6 replies (UDP 547 to 546 from `fe80::/10`: required for the prefix) and echo (rate limited). Forward: packet-too-big,
  destination-unreachable, time-exceeded and parameter-problem (RFC 4890), or path-MTU discovery breaks. Input from other networks: ND,
  MLD, echo, DHCPv6 (UDP 547); the default network already has everything.
- **MSS clamp** for IPv6 as for IPv4 (checked: the current one is for IPv4 only).
- **Rules and pinholes** (the Rules page speaks IPv4 addresses): in phase 2, a rule can name an IPv6 address, and *Allow incoming to
  this device* becomes the IPv6 counterpart of a port forward. Until then nothing is reachable from the internet over IPv6, which is
  the safe default.
- **DNS filtering must not be leaked around.** Devices on a filtering network that get IPv6 are told the router's v6 address as their
  resolver, which would reach dnsmasq directly and skip AdGuard Home. So the redirect gets an `ip6 nat` counterpart, AdGuard Home
  listens on `::` too, and the usage counters already cover both.

### What each page does with IPv6 (phase 1 = status only)
Internet: the WAN address and delegated prefix shown; Local Networks: each network's /64; Overview: IPv6 connectivity. Clients (the
addresses a device uses, from the neighbour table by MAC), Diagnostics (ping6/traceroute6), WAN health, DDNS (AAAA), DNS (AAAA local
names) and the VPNs are **phase 2 or 3**.

## 4. Phases

1. **Phase 1, a working dual-stack router.** The WAN's *off / automatic*; dhcpcd and its hooks; per-network switch and subnet ids;
   dnsmasq RA and stateless DHCPv6; sysctls; the ICMPv6, DHCPv6 and MSS rules; the `ip6 nat` DNS redirect and AdGuard on `::`; status
   on the Internet, Local Networks and Overview pages; the image gets `dhcpcd-base`; backup and review lines.
2. **Phase 2, daily use.** Fixed IPv6; PPPoE; ULA; rules with IPv6 addresses and *Allow incoming*; Clients show addresses; Diagnostics;
   WAN health; DDNS and local DNS AAAA.
3. **Phase 3.** Stateful DHCPv6 (addresses by number, reservations), a prefix-size hint in the UI, VPN over IPv6, the access point
   learning its own IPv6.

## 5. How it will be tested, and what cannot be

- **Here (user and network namespaces, real tools):** the rendered nftables file loads; dnsmasq's RA and stateless DHCPv6 are seen by a
  second namespace (SLAAC address appears, default route from the RA); the firewall's behaviour for real IPv6 packets (inbound dropped, ICMPv6 and
  established allowed, isolation) with the same two-namespace method as the IPv4 tests; the DNS redirect over IPv6 against the real
  AdGuard Home; golden tests for every rendered file; and a **fake ISP** (a DHCPv6 server that hands out a prefix) for the PD client if one
  can be run unprivileged, otherwise the hook and assignment logic against recorded lease data.
- **Not here:** a real ISP. Prefix hint behaviour, DUID/IAID rules, rapid commit, renumbering, PPPoE IPv6, and whatever else a
  particular provider does. The first real test is the maintainer's router, so phase 1 ships with IPv6 **off by default** and a clear
  way back.

## 6. Questions for the maintainer (they change the design)

1. **Your ISP and connection** (name, cable/fiber/DSL/PPPoE, and whether it advertises IPv6 or a prefix delegation). That decides
   whether DHCPv6-PD is the path or something else is.
2. **Default for a network when the WAN's IPv6 is switched on:** off until ticked per network (safest, my proposal), or on for every
   network that already has internet access.
3. **Inbound policy:** default deny with explicit *Allow incoming* rules in phase 2 (my proposal), or allow-by-network switches.
4. **Phase 1 scope:** is "automatic only, no static/PPPoE until phase 2" acceptable for a first release?

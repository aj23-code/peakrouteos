# PeakRoute OS VPN

Status (2026-10-11): WireGuard built (site to site and remote access). OpenVPN is the next stage (section 9). This file is a living
document: every commit that changes the page updates it, and it is meant to become the VPN chapter of the user documentation.

---

## 1. What it is

The VPN page (Services, VPN) has two tabs:

- **Site to site**: a tunnel between this router and another router (a branch office, a friend's house, a cloud server). The
  networks behind each router can reach each other. Several tunnels are possible (up to 8).
- **Client VPN**: remote access *to this router*. Laptops and phones connect from anywhere and reach the networks here as if they were
  at home. This router is the server; you add each **device** and give it a small file to import into the WireGuard app. One server,
  up to 100 devices.

Both use **WireGuard** (a kernel feature on Debian 13; the tools are `wireguard-tools`). OpenVPN follows (section 9). Like everything
else, changes are saved as a draft and start when committed.

## 2. How it fits the firewall

Every VPN is its own network ("zone") in the firewall, like a Local Network:

- the **remote access server** is the zone `vpn-ra` ("Remote access");
- each **site tunnel** is the zone `vpn-<id>` (`vpn-s1`, named after the tunnel).

Each has its own tab on Firewall, Rules, with *your rules* for traffic coming from it, and can be named as a destination in other
rules. A VPN zone only exists while it is switched on. The fixed lines (grey, cannot be changed) are the same ideas as for a Local
Network:

| Switch (on the VPN page) | Meaning |
|---|---|
| **May use this router's internet connection** | the VPN's traffic may go out to the internet (and is masqueraded like any other) |
| **Isolated from the Local Networks** | the VPN cannot reach the Local Networks (or the other VPNs) and they cannot reach it |

Without isolation a VPN and a Local Network (or another VPN) may talk both ways; with it, not at all, and your own rules can still
open specific things. A VPN may always use this router's **DNS and ping** and nothing else of the router itself; the web interface and
SSH are not reachable from a VPN unless a rule on that VPN's tab says so. The tunnels' UDP ports are open on the internet connection.

TCP connections through a tunnel are asked to use segments that fit it (MSS clamp 1380).

## 3. Site to site

Each side of a tunnel needs: its own private key (made by the router, never shown), the other router's **public key**, the networks
behind the other router, and a small address in the tunnel's own network.

1. Add a tunnel on this router: a **name**, the **networks behind the other router**, the **public key of the other router** (from
   the other router), the **address in the tunnel** (the page suggests `10.99.N.1/30`; the other router takes the next address,
   `10.99.N.2/30`), and optionally the **address of the other router** (`name:port`, when this router should start the connection).
2. Save to the draft. The tunnel's row has **Settings for the other router**: this router's public key, where to connect, the
   other router's address in the tunnel and the networks to send here. Enter those on the other router (a PeakRoute router has the
   same page; another WireGuard device needs `Endpoint`, `PublicKey`, `AllowedIPs` and an address).
3. Commit. When both sides are set, they connect: the row says **Connected** with the last handshake time and traffic.

Rules the page enforces (messages say what is wrong):

- a name of 1 to 32 letters, digits, spaces or `. _ ( ) + -`, not used by another tunnel;
- the other router's key is a valid WireGuard key, is not this router's own, and is not used by another tunnel;
- the tunnel address is an IPv4 address with a prefix from /24 to /31, and the tunnel's little network overlaps nothing else;
- the **networks behind the other router** are IPv4 networks (at least one, up to 16) that must **not overlap** any network of this
  router (the default network, Local Networks, the internet connection when static, static routes) or another VPN;
- each tunnel has its own UDP listen port (default 51820, 51822, ...; 51821 is the remote access server's);
- the endpoint, when set, is a name or IPv4 address with a port; keep-alive is 0 to 600 seconds (25 is set for you when this router
  calls the other one, so that a connection through NAT stays open).

**Which side calls?** Either may. A side with an **address of the other router** starts the connection; the one without waits.
A router with a changing or private address should be the caller. The other side learns the address from the first packet.

**Pre-shared key** (optional): an extra secret both sides must have. It is *not* shown again once saved, so *Make one* creates a key
in the page (shown once) for you to enter on the other router too, or paste the one the other router made.

**Changing the key of this router** (*New key for this router*) means the other router must be given the new public key.

## 4. Client VPN (remote access)

1. **Set up** the server: the **address devices connect to** (a name, ideally a Dynamic DNS name, or an address of this router that
   can be reached from the internet; the port is added), the **port** (UDP, default 51821; forward it from a router in front), and
   the **network for the devices** (default `10.8.0.0/24`; this router takes the first address, devices get the rest).
2. Choose what devices can reach and use: **networks devices can reach** (empty: all the networks of this router, the VPN
   networks included), **send all of the device's traffic through this router** (a full tunnel; needs the internet switch),
   **use this router for DNS**, the two zone switches.
3. **Add a device**: a name; the router makes its keys and shows the **file** (the private key is in it, shown only once and not
   kept) to **copy or download**. Import it in the WireGuard app (*Add tunnel, Import from file*; on a phone, send the file to it,
   a QR code is not built yet). A device with its own keys (*Public key of the device*) gets a file with a placeholder for its private
   key.
4. Commit. The device connects; its row says **Connected** with the last handshake and traffic.

Per device: **Rename**, switch off and on, **New file** (new keys; the old file stops working once committed) and **Remove**. Device
names are unique; each device has a fixed address in the remote access network (`10.8.0.2`, `.3`, ...).

**New server key** makes every device's file out of date (each needs a new file).

Devices reach another site's networks through this router when that is wanted: those networks are in what devices send to the
router. The router on the other end must also have the remote access network in the networks it sends back here (add `10.8.0.0/24`
to the networks behind this router on its side of the tunnel), because WireGuard only passes traffic for addresses it was told about.

## 5. Where the files go

| Path | Content |
|---|---|
| `/etc/wireguard/wgra.conf` | remote access server (0600) |
| `/etc/wireguard/wgs<N>.conf` | each site tunnel (0600), made for wg-quick |
| `/usr/local/sbin/peakroute-wg-sync` | the script that brings the tunnels in line with the files (rewritten on every commit that has tunnels) |
| `/etc/dnsmasq.d/peakroute-vpn.conf` | lets the DNS server answer on `wgra` when devices are told to use it |

The directory belongs to PeakRoute OS: a `*.conf` in it that is not wanted is deleted at commit. The sync script only touches
interfaces called `wgra` or `wgs<number>`, takes down those that are no longer wanted, brings up those that are, and restarts one
only when its file changed (a fingerprint is kept in `/run/peakroute-wg`). The daemon runs it again when it starts, so tunnels
come back after a reboot.

Before anything is applied a check loads every file into a scratch interface with `wg setconf` (a bad key or address fails the
commit, which rolls back).

## 6. Secrets

Private keys and pre-shared keys are stored in `network.json` (0600) and in the 0600 WireGuard files, **never shown again**: not by
the API, not in the review (*set* or *changed*), not in logs. A device's private key exists only in the file made for it (and in
the page until it is closed): the router keeps the device's *public* key.

## 7. API (all need a signed-in administrator; changes need the administrator role)

| Call | Does |
|---|---|
| `GET /api/v1/vpn` | the page: tunnels, remote access with devices, how each is going (from `wg show all dump`), this router's networks, suggestions |
| `POST /api/v1/vpn/sites` | add a tunnel (this router's key is made) |
| `PUT /api/v1/vpn/sites/{id}` | change (a field left out stays; `new_key`; `preshared_key` a key or `generate`); `DELETE` removes |
| `PUT /api/v1/vpn/remote` | set up or change the server; `DELETE` removes it with its devices |
| `POST /api/v1/vpn/remote/clients` | add a device (`name`, optionally `public_key`, `preshared`); the answer has the file |
| `PUT/DELETE /api/v1/vpn/remote/clients/{id}` | rename, switch, `new_key` (answer has the new file), remove |
| `GET /api/v1/vpn/remote/clients/{id}/file` | the file for a device with its own keys (a placeholder for the private key) |

## 8. Status

A peer is **Connected** with a handshake in the last three minutes, **Idle** after that (WireGuard connects again when there is
traffic), **Waiting** when it has never connected. A tunnel that is only in the draft shows no status.

## 9. Still to do

- **OpenVPN** (stage B): site to site and remote access, with a **built-in certificate authority** that makes and signs the server and
  client certificates and gives `.ovpn` downloads; the same two tabs, the same zones and rules.
- A **QR code** for a device's file.
- IPv6 (everything here is IPv4 for now).
- A test on real hardware after an ISO rebuild (`wireguard-tools` is added to `buildISO.sh`).

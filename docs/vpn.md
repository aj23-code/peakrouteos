# PeakRoute OS VPN

Status (2026-10-11): built, with WireGuard and OpenVPN, for site to site and for remote access. This file is a living
document: every commit that changes the page updates it, and it is meant to become the VPN chapter of the user documentation.

---

## 1. What it is

The VPN page (Services, VPN) has two tabs:

- **Site to site**: a tunnel between this router and another router (a branch office, a friend's house, a cloud server). The
  networks behind each router can reach each other. Several tunnels are possible (up to 8).
- **Client VPN**: remote access *to this router*. Laptops and phones connect from anywhere and reach the networks here as if they were
  at home. This router is the server; you add each **device** and give it a small file to import into the WireGuard app. One server,
  up to 100 devices.

Both can use **WireGuard** (a kernel feature on Debian 13; the tools are `wireguard-tools`) or **OpenVPN** (the `openvpn` package, with
certificates made by the router itself: sections 9 to 11). WireGuard is the simpler and faster choice; OpenVPN is for devices and
sites that speak only that. Like everything else, changes are saved as a draft and start when committed. A router can run both
engines at the same time, each remote access server and each tunnel being a zone of its own.

## 2. How it fits the firewall

Every VPN is its own network ("zone") in the firewall, like a Local Network:

- the **WireGuard remote access server** is the zone `vpn-ra` ("Remote access");
- the **OpenVPN remote access server** is the zone `vpn-ora` ("Remote access (OpenVPN)");
- each **site tunnel**, of either engine, is the zone `vpn-<id>` (`vpn-s1`, named after the tunnel).

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

TCP connections through a tunnel are asked to use segments that fit it (MSS clamp 1380). The ports the tunnels listen on are open on
the internet connection, each with its own protocol: UDP for WireGuard and for OpenVPN over UDP, TCP for OpenVPN over TCP. A tunnel
that only calls the other router (WireGuard with no listening need, an OpenVPN client) opens nothing.

## 3. Site to site (WireGuard)

This section describes WireGuard; OpenVPN tunnels are in section 9.

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

## 4. Client VPN (remote access, WireGuard)

This section describes WireGuard; the OpenVPN remote access server is in section 9 and works the same way for the person using the page.

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
| `GET /api/v1/vpn` | the page: tunnels of both engines, both remote access servers with devices, the authority, how each is going, this router's networks, suggestions |
| `POST /api/v1/vpn/sites` | add a tunnel (`engine`: `wireguard` by default, or `openvpn` with a `role`) |
| `PUT /api/v1/vpn/sites/{id}` | change (a field left out stays); WireGuard: `new_key`, `preshared_key` a key or `generate`; OpenVPN: `new_file` (server), `file` (client) ; `DELETE` removes (and revokes the other router's certificate) |
| `PUT /api/v1/vpn/remote` | set up or change the WireGuard server; `DELETE` removes it with its devices |
| `POST /api/v1/vpn/remote/clients` | add a WireGuard device (`name`, optionally `public_key`, `preshared`); the answer has the file |
| `PUT/DELETE /api/v1/vpn/remote/clients/{id}` | rename, switch, `new_key` (answer has the new file), remove |
| `GET /api/v1/vpn/remote/clients/{id}/file` | the file for a WireGuard device with its own keys (a placeholder for the private key) |
| `PUT /api/v1/vpn/ov-remote` | set up or change the OpenVPN server (the first time it makes the authority, when there is none, and the control channel key); `DELETE` removes it and revokes its devices |
| `POST /api/v1/vpn/ov-remote/clients` | add an OpenVPN device (`name`): the router makes its certificate and key; the answer has the file |
| `PUT/DELETE /api/v1/vpn/ov-remote/clients/{id}` | rename, switch, `new_file` (a new certificate, the old one revoked; answer has the file), remove (revokes) |

The engines' settings are not mixed: a request that names OpenVPN settings for a WireGuard tunnel, or the other way round, is refused.

## 8. Status

A WireGuard peer is **Connected** with a handshake in the last three minutes, **Idle** after that (WireGuard connects again when
there is traffic), **Waiting** when it has never connected. An OpenVPN device or peer is **Connected** (with where from, since when and
how much it moved) or **Not connected**; an OpenVPN client tunnel is Connected while its interface is up. A tunnel that is only in
the draft shows no status.

## 9. OpenVPN

OpenVPN is the second engine of both tabs. It is a separate way of doing the same things, with the same zones and rules; what
differs is how the two sides know and trust each other (certificates instead of keys) and a few limits.

**Site to site.** The two routers play different parts, because the certificates come from one of them:

- the router that **waits** ("This router waits") makes a file for the other. When you add the tunnel the page asks for the **address
  the other router connects to** (a name or an address of this router that can be reached from the internet, and the port), the
  protocol, the port to listen on, and the tunnel's own network (a **/29 or wider**, suggested `10.98.N.1/29`; OpenVPN will not take
  less; this router takes the first address and the other router the next). The router makes a certificate for the other router and
  shows you the **file** to give it, once: it holds the other router's private key, which this router does not keep;
- the router that **connects** ("This router connects") imports that file (pasted, or chosen from disk). It takes the address, the
  protocol, the keys and (when the file has them) the networks behind the other router from it; the networks can be given by hand.
  It writes its own file from those and nothing else: a script, a plugin or any other line in the imported file is ignored.

Both sides list **the networks behind the other router**, as with WireGuard. The one that connects takes only what is listed (it
ignores routes and DNS the other side might push); the one that waits is told the networks behind the other router by that same
list (it routes them to the tunnel). The file is also an ordinary OpenVPN client file, so another product can be the other side (it then adds its own routes).
**New file for the other router** makes a new certificate (the old one stops being accepted when this is committed).

**Remote access.** The same as WireGuard's, with a protocol choice (UDP or TCP, so that it can get through networks that block UDP) and
the default network `10.9.0.0/24`. Each device has its own certificate and a fixed address (`10.9.0.2`, ...). What a device is told
(the networks to send through the tunnel, the router as DNS server, all traffic) is pushed to it when it connects. The file is
imported into the OpenVPN app (OpenVPN Connect on a phone or computer; on Linux `openvpn --config file.ovpn`). A device whose file
the router cannot make (no address to connect to yet) is refused before anything is kept.

**What is not the same.** An OpenVPN server does not show who is merely configured but silent as "Waiting": a device is connected or
not. A change to a server's devices (adding, removing, switching off, a new file) restarts that server, which drops the devices
that are connected for a few seconds (they reconnect by themselves); it is the only way to be sure a removed device is cut off at
once. An OpenVPN tunnel's port cannot be shared with another tunnel, and the same port number cannot be used by WireGuard.

## 10. The certificate authority

OpenVPN needs certificates, and making them by hand is what puts people off. The router is its own **certificate authority**. It is
made the first time something needs it (the first OpenVPN server or tunnel that waits) and shown at the bottom of the Client VPN tab.

| What | Valid for | Notes |
|---|---|---|
| the authority | 20 years | ECDSA P-256; its key never leaves `network.json` |
| the server certificate | 10 years | one for all the OpenVPN servers of this router; used only to serve |
| a device's or a peer router's certificate | 5 years | made with the device; used only to connect; its name tells which server may accept it |

- **A device's certificate only works on the server it was made for**: remote access devices are named `ra-<id>` and the server
  accepts only those; the other router of a site tunnel is named `<tunnel id>-peer` and that tunnel's server accepts only that name.
- **Revocation.** When a device is removed, gets a new file, or its server is removed, its certificate goes on a **list of
  certificates that are no longer accepted**, and the list is made again and given to the servers. A device is also refused
  because it has no device file any more (the first line of defence); the list is the second, for a certificate that was copied.
  The page shows how many are on the list.
- **Running out.** A certificate that is within 60 days of running out, or has, is marked on the page (a device row, the tunnel it
  is for, the server certificate). A device gets a new file with **New file**; a router that connects needs a new file from the other
  router. A certificate that has run out never stops the router from saving other changes.
- **Secrets.** The authority's key, the server's key, the control channel keys and the key of a tunnel that connects are stored in
  `network.json` (0600) and in the 0600 files in `/etc/openvpn`, and are never shown again: not by the API, not in the review
  (*set* or *changed*), not in logs. A device's key exists only in the file made for it (and in the page until it is closed). The
  authority and what it made must be part of a backup.
- The control channel of every server and tunnel is protected with a shared key (`tls-crypt`), so that a server does not even answer
  a packet from someone who does not have the file. Data uses AES-256-GCM, AES-128-GCM or ChaCha20-Poly1305, TLS 1.2 or later.

## 11. Where the OpenVPN files go

| Path | Content |
|---|---|
| `/etc/openvpn/server/pr-ra.conf` | the remote access server (0600; the certificates, key, revocation list and control channel key are inside) |
| `/etc/openvpn/server/pr-s<N>.conf` | a site tunnel where this router waits (0600) |
| `/etc/openvpn/client/pr-s<N>.conf` | a site tunnel where this router connects (0600) |
| `/etc/openvpn/ccd/ra-<id>`, `/etc/openvpn/ccd/s<N>-peer` | one file per device, and per tunnel's other router: its address, and for a tunnel the networks behind it. A device **without** a file is refused |
| `/usr/local/sbin/peakroute-ovpn-sync` | the script that brings the services in line with the files (rewritten on every commit that has OpenVPN tunnels) |
| `/run/openvpn-server/status-<name>.log` | who is connected (written by the server every 10 seconds; the page reads it) |

The directories belong to PeakRoute OS: a `pr-*.conf` in the two server and client directories, or any file in `ccd`, that is not wanted
is deleted at commit. Each tunnel is a unit of the package's own service files (`openvpn-server@pr-ra`, `openvpn-client@pr-s2`...).
The script restarts a unit only when its file changed or it is not running, stops the ones that are no longer wanted, and
only touches `pr-ra` and `pr-s<N>`. The daemon runs it again when it starts, so tunnels come back after a reboot. A server's file holds
a fingerprint of its device files, so that changing one restarts the server. A tunnel is not checked before the commit the way
WireGuard's files are (OpenVPN cannot read a file without using it): what is written is checked by the router instead, strictly (every
block is a block, every address an address).

## 12. Still to do

- A **QR code** for a device's file (WireGuard and OpenVPN).
- IPv6 (everything here is IPv4 for now).
- Renewing the **server certificate** and the authority before they run out (20 and 10 years: for now it would mean starting again).
- A test on real hardware after an ISO rebuild (`wireguard-tools` and `openvpn` are added to `buildISO.sh`); the real tools were
  tried in network namespaces (WireGuard and OpenVPN, with the real `wg-quick` and `openvpn`), not through systemd.

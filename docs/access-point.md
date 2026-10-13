# Access point mode

PeakRoute can run as a **router** (the usual way) or as an **access point**: a device that joins a network another router runs, and
adds cable ports and Wi-Fi to it. It is chosen in the first-run wizard (*Set up as an access point*) or later on **System, Operation
mode**, and it is a draft like every other change: review, commit, confirm.

## What an access point is

- **One network.** The cable ports you choose, the Wi-Fi networks and any Wi-Fi link are all members of one bridge (`br0`). A device
  plugged into the access point is on the other router's network and gets its address from that router's DHCP server.
- **Its own address** is on the same bridge: from the other router's DHCP server (the other router lists the device under its host
  name, which is how to find it), or fixed (address, netmask, gateway, DNS servers).
- **A recovery address.** `192.168.77.1/24`, the address of the setup network, is kept on the bridge beside the real one. A computer
  that was set up through the setup network keeps working there, so the change can be confirmed even when the address from DHCP is not
  known yet; a computer set to an address in 192.168.77.0/24 reaches the device there at any time.
- **Nothing is routed, translated or served.** No NAT, no DHCP or DNS server, no extra networks (VLANs), no VPN, no firewall rules of
  your own, no static routes, no Dynamic DNS, no internet connection of its own (a Wi-Fi uplink means nothing here). IP forwarding is off.
  The firewall only protects the device itself: its web interface (443), ping, and the replies to what it asked for. Everything of
  a router that is in the settings stays in the file, unused, so that going back to a router finds it again.
- **Wi-Fi works as on a router** (Wireless page): every Wi-Fi network joins the one bridge, whichever local network a leftover setting
  names. An access point can also hold a **link** to another access point over Wi-Fi (Wi-Fi client, *a link into a local network*): with
  no cable at all that is a wireless extender.
- The pages that only a router has (Clients, Traffic, Internet, Local Networks, Ports, Routes, Firewall, Services) are not in the menu.

## The home page

In access point mode the first page is **Access point** (not the router's Overview), and the menu is cut down to what an access point has:
Access point, Wireless, and System (Settings, Operation mode, Administrators, Certificates, Backup and restore, System logs, Diagnostics).
The home page shows, refreshed every 5 seconds:

- **The device:** name, how long it has been up, processor, memory, load, software version, and whether changes are waiting.
- **On the network:** whether it is on the network (the bridge is up and has an address; "waiting for an address" when DHCP has not answered),
  its address, gateway and name servers, the setup address, and each cable port (cable plugged in, speed) and Wi-Fi link (connected, signal,
  speed).
- **Wi-Fi:** for each adapter hosting Wi-Fi: whether it runs, the channel and width in use, transmit power, **channel use** (how much of
  the time the channel is busy, measured between two looks; above 70% is marked red), the noise, and how many devices are connected with
  their average signal; and each Wi-Fi network with its bands, VLAN and device count.
- **Connected devices:** every device on the Wi-Fi: address, network, band and channel, signal (excellent to poor), speed each way, traffic
  and how long it has been connected.

Right after the first-run setup nothing is applied yet, so the page says so and the menu already shows the access point's (the mode shown
follows the settings in effect, or, before anything was ever applied, the draft). When a change that switches the mode is applied the
page loads again by itself. The interface of the pages a router has (Internet, Local Networks, Ports, Routes, Firewall, DNS, Dynamic DNS,
VPN) answers an access point with "This device is an access point: that belongs to a router" instead of staging settings nothing uses.

## VLANs on Wi-Fi

A Wi-Fi network of an access point can have a **VLAN ID** (2 to 4094; empty is the untagged network, the one the cable ports are on). It
is set where the Wi-Fi network is made (Wireless page, and the wizard's Wi-Fi page); there is no Local Network to name, because an access
point has none. How it is built is how the router builds its tagged networks: each VLAN used by a Wi-Fi network that is switched on gets a
bridge `br<ID>` with no address, holding the VLAN interface of every cable port (`eth0.20`: the cable ports are a trunk for the VLANs
in use) and the Wi-Fi networks on that VLAN (hostapd `bridge=br20`). The access point's own address stays on the untagged bridge. Several
Wi-Fi networks can share a VLAN, and an extender with no cable gets the bridge with no ports. A router refuses a VLAN ID on a Wi-Fi network
(it joins a Local Network, which has its own).

## What it writes

`/etc/network/interfaces`: `br0` as `inet dhcp` (with the host name as the DHCP name) or `inet static`, `bridge_ports` the cable ports
and links, and `post-up ip addr add 192.168.77.1/24 dev br0 label br0:setup`; `/etc/nftables.conf`: a short input policy (loopback,
established, ping, DHCP replies, 443; drop the rest) and nothing forwarded; `net.ipv4.ip_forward=0`; and **removes** every file only a
router has (the DHCP, DNS and VPN dnsmasq files, the routes script, the PPPoE login, the WireGuard and OpenVPN files). The hostapd and
wpa_supplicant files are written as on a router.

## Switching

- *Router to access point:* every cable port joins the bridge unless you choose. The router's settings stay in the file.
- *Access point to router:* the router settings that are there are used; a device that has never been a router gets the wizard's defaults
  (the first cable port is the internet connection and the others the local network, 192.168.1.1/24 with DHCP), and needs two cable ports.
- Both need the 60 second confirmation (the network is rebuilt). A device that cannot be reached after the change goes back by itself.

## In the wizard

The welcome page asks what the device is: **a router**, **an access point**, **a wireless extender**, **a point-to-point link** (it then asks
which end), or a restore. All but the router are this mode.

- *Access point:* region, the network (cable ports, DHCP or a fixed address), Wi-Fi (if an adapter can host a network: one name and
  password, and an optional VLAN ID; one adapter serves 2.4 GHz, two adapters cover both bands with band steering), the administrator,
  and the review.
- *Wireless extender* (needs two Wi-Fi adapters): **join the main Wi-Fi** (an adapter, a scan to pick the network, the password), the
  network (cable ports are optional), and **repeat the main Wi-Fi** on the other adapter with the same name and password (so devices
  move between the two by themselves), then the administrator.
- *Point-to-point link:* the **near end** hosts the link (an adapter, a link name and password, on 5 GHz where the adapter can, and
  accepting 4-address stations); the **far end** joins it (an adapter, a scan, the password). Both need the cable port that carries the
  network behind that end. Set the near end up first.

The port you are plugged into must be one of the bridge's ports, or the page would be lost when the settings are applied.

## Interface

| Method and path | Notes |
|---|---|
| `GET /api/v1/ap` | the mode in the draft and in effect, the access point's network, the cable ports, the recovery address |
| `PUT /api/v1/ap` | `{mode: "router"\|"ap", ports, address_mode, ip, netmask, gateway, dns}`: stages the change |

## Not done

- Which cable ports carry a VLAN: all of them, as a trunk (a port for one tagged network only is not offered).
- Radio health is read with `iw dev X survey dump`, which the daemon may do as root; on a machine where it is refused the page says
  nothing about channel use. The first reading is the average since the adapter started, the next ones are since the previous look.
- Telling the main router apart from a loop: spanning tree is off, so two cables from the access point into the same switch loop.
- Real hardware has not been tried: bridging a Wi-Fi interface, `post-up` on the bridge and the DHCP name are checked only against
  their syntax.

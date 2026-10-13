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

The welcome page offers *Set up as an access point*: region, then the network (cable ports, DHCP or a fixed address), Wi-Fi (if an
adapter can host a network: one name and password; one adapter serves 2.4 GHz, two adapters cover both bands with band steering), the
administrator, and the review. The port you are plugged into must be one of the bridge's ports, or the page would be lost when the
settings are applied.

## Interface

| Method and path | Notes |
|---|---|
| `GET /api/v1/ap` | the mode in the draft and in effect, the access point's network, the cable ports, the recovery address |
| `PUT /api/v1/ap` | `{mode: "router"\|"ap", ports, address_mode, ip, netmask, gateway, dns}`: stages the change |

## Not done

- The API endpoints of the router's pages are still there in access point mode (the menu hides them); a change made through one only
  stages router settings that nothing uses.
- A VLAN-tagged SSID (several networks on one access point).
- Telling the main router apart from a loop: spanning tree is off, so two cables from the access point into the same switch loop.
- Real hardware has not been tried: bridging a Wi-Fi interface, `post-up` on the bridge and the DHCP name are checked only against
  their syntax.

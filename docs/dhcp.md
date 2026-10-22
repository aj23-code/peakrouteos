# DHCP and reserved addresses

Contents: 1 The DHCP page, 2 Reserved addresses, 3 What is written, 4 Internals, 5 Not planned.

## 1. The DHCP page

**Services > DHCP.** One card for each local network (the default LAN and every VLAN): the DHCP server's switch, the first and last address
it hands out and the lease time. These are the same settings as on the Local Networks page (each network's own card has them too); both
pages save to the draft and both are committed from the bar at the top. The devices that have an address are listed on
**Network > Clients**, with the network each is on.

## 2. Reserved addresses

A reserved address is the router-side way to give a device a fixed address, and the better one: the address is kept in one place, cannot
clash with the addresses handed out to other devices, and a device that is reset to its defaults gets it back. The device is known by its
hardware (MAC) address and always gets that address from the DHCP server of the network the address is in, whichever of that network's
ports or Wi-Fi it comes in on. It takes the address the next time it asks (when it reconnects or renews its lease, or is restarted).

- Add them under **Reserved addresses** on the DHCP page: pick a connected device (its current address is kept), or add one by hand.
  On the Clients page a **Reserve** link on a device opens the same page with it filled in. A device that has one shows **Reserved**
  (and, if it still holds another address until it asks again, *will move to ...*).
- A row has an optional **name** (the host name the router's DNS knows the device by), the **hardware address** (any of `aa:bb:...`
  or `aa-bb-...`, written the first way) and the **address**. The network shown beside it is the one the address is in.
- The rules, checked when it is saved and again when committed: the hardware address is a real device address (not a group address),
  listed once; the address is a host address inside one of your local networks, not that network's own address, not its network or
  broadcast address, and used once; **the DHCP server of that network must be on**; the name is a host name (letters, digits and hyphens),
  used once. An address inside the dynamic range works too: the server never gives a reserved address to anyone else.
- A change is committed like any other (it restarts only the DHCP server, for a moment). If a network is deleted or its DHCP server is
  switched off while a device still has an address reserved in it, the commit is refused and names the address.
- The Clients page shows *Reserved* from what is **committed**; a reservation in the draft is shown in the review and on the DHCP page.

## 3. What is written

`/etc/dnsmasq.d/peakroute-dhcp.conf` gets one line per reservation after the ranges: `dhcp-host=<mac>,<address>[,<name>]`. A router
with no reservations gets exactly the file it always had.

## 4. Internals

`config.Reservation` (`internal/config/reservations.go`: type and `validateReservations`), the list is `NetworkConfig.Reservations`
(`network.json` key `reservations`), diffed by `diffReservations` (one change per device), rendered by `writeReservations`
(`translator`, with `ensureSafeReservations` as a second look), served by `GET`/`PUT /api/v1/dhcp/reservations` (`netapi/reservations.go`:
the whole list is replaced, like the routes; only its own errors block a save). `network.LabelNetworks` marks the leases `reserved`.
The page is `web/static/scripts/dhcp_server.js`, the link on the Clients page `leases.js`.

## 5. Not planned

Reserving an address for a device by name or by client identifier instead of the hardware address (a device that changes its hardware
address, such as a phone with a private address per network, needs one reservation per network) · IPv6 · importing a list.

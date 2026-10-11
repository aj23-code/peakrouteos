# PeakRoute OS DNS

Status: **design (2026-10-11), not built yet.** This file is written first and kept up to date as each stage is built; every
stage's commit updates it. It is meant to become the DNS chapter of the user documentation.

Contents: 1 What exists today, 2 Two sides: what the router asks, what the devices are told, 3 What each network is told,
4 The router's own DNS server, 5 The local domain, 6 Local names, 7 Domain forwarding, 8 Staying out of trouble, 9 Recipes,
10 Internals, 11 Build plan, 12 Not planned.

---

## 1. What exists today

DNS on the router is **dnsmasq**, which also runs the DHCP server. Today it works like this and nothing about it can be
changed from the web interface except the upstream servers:

- Every network's DHCP answers tell its devices to use **the router** for DNS (the router's address on that network).
- The router answers those questions itself and passes what it does not know on to its **upstream servers**: the ones the
  internet connection hands out (DHCP or PPPoE), or, with a static address, the ones you entered.
- The Internet page has one field, *DNS servers*, which is required with a static address and optional otherwise: set, those
  servers replace the provider's.
- A non-default network may ask the router for DNS and ping and nothing else on the router (see the firewall document); the default
  network may use everything.

## 2. Two sides

There are two separate questions, and the page keeps them apart:

| Side | Question | Setting |
|---|---|---|
| **Public side** (upstream) | Who does the router ask when it does not know a name? | The provider's servers (the default), or servers you choose |
| **Local side** (per network) | Who are the devices on this network told to ask? | This router (the default), or other servers you choose |

They are independent. A network can be told to use a server in your own network (a Pi-hole, a company DNS server) while the
router itself still asks the provider; or every network can use the router, which asks a public resolver you picked.

## 3. What each network is told

Every Local Network has a *DNS for this network* choice:

- **This router** (default): DHCP tells the devices the router's address. They get the router's local names, the DHCP device
  names, the domain forwarding, and the upstream answers.
- **Other servers**: DHCP tells the devices up to three addresses you enter, in this order, instead of the router. The router
  does nothing for DNS on that network except that it still answers if a device asks it directly.

Things to know:

- It only changes what DHCP **advertises**. A device with a fixed address, or one that uses its own DNS settings (many phones
  and browsers do), ignores it. The router cannot force anything.
- A network without a DHCP server of the router's own has nothing to advertise: the choice is shown but has no effect, and the
  page says so.
- If the servers are on **another** network, the firewall must let the devices reach them (a rule on the network's Rules page).
  The page warns when a server you enter is not on the same network.
- The default network is a network like the others here.

## 4. The router's own DNS server

**Upstream servers** (edited on the DNS page; the Internet page keeps only the static-address case, where they are required):

- *The provider's servers* (default). The page shows the ones in use right now.
- *These servers instead*: up to three IPv4 addresses, used for everything the router does not answer itself.

It is the same setting that was on the Internet page; there is only one list. DNS over TLS and DNSSEC are not offered (dnsmasq
cannot do them; see section 12).

The router's DNS service always answers on every network (unchanged: DNS and ping are the only things a non-default network may
ask of the router). Choosing "other servers" for a network does not stop it.

## 5. The local domain

A router-wide name such as `home.example.com` or `lan`, **empty until you set it** (decided 2026-10-11), set on **System, Settings**
next to the country: it is a device setting, and the DNS page only shows it. Set, it does three things:

- DHCP tells devices the domain, so they can use short names (`laptop` finds `laptop.lan`).
- The router answers for the domain **itself** and never asks the upstream servers about names inside it (so a typo does not
  leak to the internet).
- Local names (section 6) and the device names from DHCP resolve under it.

## 6. Local names

A list of **name to address** entries the router answers itself: the heart of *split DNS* (see the firewall document, port
forwarding: use the inside address from inside the network).

| Field | Notes |
|---|---|
| name | a host name or a full name: `nas`, or `home.example.com` |
| address | an IPv4 address |
| on / off | off keeps the entry without applying it |

A short name (no dot) also resolves with the local domain added when one is set (`nas` and `nas.lan`). Reverse lookups (address to
name) work as well. At most 200 entries; a name may be listed once.

*Using it for a service you port-forward:* add `home.example.com` with the server's inside address. Devices inside get the inside
address; everyone else gets the public one from public DNS. The name and its HTTPS certificate stay the same everywhere.

## 7. Domain forwarding

*Ask this server about that domain.* Example: `corp.example.com` goes to `10.0.0.5`. Questions for the domain and everything
under it go to the servers listed (up to three), instead of the upstream servers. At most 50.

## 8. Staying out of trouble

- Nothing changes until you commit, like every other page; DNS changes only restart dnsmasq (a moment without DNS and DHCP
  answers, no effect on traffic that is flowing).
- The file is checked with `dnsmasq --test` before it is applied.
- A name cannot be listed twice, a forwarded domain cannot be listed twice, and every address and name is checked.

## 9. Recipes

*These become real screenshots and steps once the page exists.*

1. **Use a Pi-hole for the Home network.** DNS page, Home: *Other servers*, `192.168.1.5`. The Pi-hole's own settings decide what
   it asks upstream.
2. **Reach my server by its name from inside.** Local names: `home.example.com` to `192.168.1.50`.
3. **Use a public resolver instead of the provider's.** Upstream: *These servers instead*, `9.9.9.9, 149.112.112.112`.
4. **A company domain from a VPN gateway.** Domain forwarding: `corp.example.com` to `10.8.0.1`.
5. **Use short device names.** System, Settings: domain `lan`; then `laptop` works.

## 10. Internals

*(Filled in as it is built. The planned layout:)*

- **Storage** in `network.json`: top-level `domain`; `dns` with `hosts` (`id`, `name`, `address`, `disabled`) and `forwards`
  (`id`, `domain`, `servers`); per network `dns_servers` (the default network's is `lan_dns_servers`). Upstream stays in
  `wan_dns`.
- **Files:** the DHCP file gets the per-network `dhcp-option=tag:<net>,option:dns-server,...` with the chosen servers instead of
  the router; a new `/etc/dnsmasq.d/peakroute-dns.conf` holds `domain=`, `local=/<domain>/`, `expand-hosts`, `host-record=`,
  `server=/<domain>/<server>`. **With nothing set, every file is exactly what it is today.**
- **Testing:** the real `dnsmasq --test`; behaviour with the real dnsmasq in network namespaces (a client asks, and what comes
  back is asserted: local names, the domain, forwarding to a second dnsmasq, upstream); the DHCP option with a hand-built DHCP
  request.

## 11. Build plan

Each stage is its own commit with tests and updates this document.

1. **Model**: domain, local names, forwards, per-network servers; validation; review lines.
2. **Files**: the translator writes them; checked with `dnsmasq --test` and with real queries between namespaces.
3. **API**: the DNS settings, the local names, the forwards, per-network choice, the domain on Settings.
4. **DNS page** (Services, DNS) and the domain field on Settings.
5. **Lookup test** on the page: ask the router a name and see the answer and how long it took.
6. Polish; screenshots in this document.

## 12. Not planned (yet)

DNS over TLS and DoH upstream · DNSSEC validation · ad and tracker blocking lists · IPv6 records (AAAA, the whole router is IPv4
for now) · wildcard names · CNAME and other record types (only name-to-address entries) · different upstream servers per network ·
splitting one name into different answers per network (views) · importing /etc/hosts · replacing dnsmasq with Unbound.

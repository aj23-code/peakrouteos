# PeakRoute OS DNS

Status (2026-10-11): **stages 1 to 5 built** (the model, the files dnsmasq reads, the API, the page and the lookup test; polish and screenshots are left). This file was written first and every stage's commit updates it. It is meant to become the DNS chapter of the user documentation.

Contents: 1 What exists today, 2 Two sides: what the router asks, what the devices are told, 3 What each network is told,
4 The router's own DNS server, 5 The local domain, 6 Local names, 7 Domain forwarding, 8 Staying out of trouble, 9 Recipes,
10 The page, 11 Internals, 12 Build plan, 13 Not planned.

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
| **Public side**: *Upstream DNS* | Who does the router ask when it does not know a name? | The provider's servers (the default), or servers you choose |
| **Local side**: *Local DNS* (per network) | Who are the devices on this network told to ask? | This router (the default), or other servers you choose |

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

## 10. The page

**Services, DNS.** Top to bottom; every change is saved to the draft and applied when you commit, like the rest:

1. **Upstream DNS** (where the router asks): *The provider's servers* (the card shows which ones are in use right now) or *These servers instead*
   (up to three). With a fixed address on the internet connection only the second is possible. Save, or Reset to what is saved.
2. **Local DNS** (what each network is told): one block per Local Network with *This router (its address)* or *Other servers* and the
   servers. Advice (no DHCP from the router there, a server on another network, a public server for a network without
   internet) is shown under the block.
3. **Local names**: a list with a switch, Edit and a two-click Delete per entry, and *Add name*. The local domain is shown here
   but changed under **System, Settings** (the link is on the page).
4. **Domain forwarding**: the same, with *Add domain*.

**DNS lookup** is not on this page: it is a tool on **System, Diagnostics** (the DNS page links to it). Type a name (`nas`, `www.example.com`) or an IPv4 address and press *Ask*. The router's own DNS server is
asked, the way a device would ask it, and the page shows *Answered*, *Not found*, *No answer* (the server did not reply in
three seconds: it may be stopped, restarting or waiting for an upstream server) or *Error* (the server could not get an
answer), with the addresses (or names, for an address) and how long it took. The last five questions stay on the page. It
tests what is **running now**; changes still in the draft are not included, so commit first and then ask. A short name is
asked exactly as typed (a device's own search domain is not added); use `nas.lan` to see the domain at work.

The Internet page only shows a *DNS servers* field for a fixed address; otherwise it says that the DNS page decides. Errors
appear under the field they belong to; a draft entry carries a *Draft* tag until it is committed.

## 11. Internals

**Storage** in `network.json`; every key is written only when it has something in it:

```json
"domain": "lan",
"lan_dns_servers": ["192.168.1.5"],
"networks": [ { "id": "net2", "name": "Guest", ..., "dns_servers": ["1.1.1.1", "9.9.9.9"] } ],
"dns": {
  "hosts":    [ {"id": "h1", "name": "nas", "address": "192.168.1.50"} ],
  "forwards": [ {"id": "f1", "domain": "corp.example.com", "servers": ["10.0.0.5"]} ]
},
"wan_dns": ["9.9.9.9"]
```

The code is `internal/config/dns.go` (model, validation, review text) and `internal/translator/dns.go` (the files). Names and
the domain are kept in lower case; a switched-off entry is kept and not written.

**The files.** With nothing set, every file is exactly what it was (a test compares them) and the local file is not written.

- *What a network is told* is in the DHCP file, as before, one line per network:
  `dhcp-option=tag:guest,option:dns-server,1.1.1.1,9.9.9.9` instead of the router's own address. With one network and no tags the
  line is `dhcp-option=option:dns-server,...`. The router stays the *gateway* (`option:router`) either way.
- *The router's own answers* are in `/etc/dnsmasq.d/peakroute-dns.conf`, written only while there is a domain, a local name
  or a forward switched on:

```
# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.
domain=home.example.com
local=/home.example.com/
expand-hosts
host-record=nas,nas.home.example.com,192.168.1.50
host-record=www.example.com,192.168.1.50
server=/corp.example.com/10.0.0.5
server=/corp.example.com/10.0.0.6
```

  `domain=` is what DHCP tells the devices and what is added to simple names; `local=/<domain>/` makes the router the only
  one that answers for the domain (a name inside it that nobody defined is "not found", and is never sent upstream);
  `host-record` answers the name and its reverse lookup; a short name is written both short and with the domain, because
  dnsmasq does not add the domain to host records by itself; `server=/<domain>/<server>` sends a domain, and what is under
  it, to its own servers, one line per server.
- *The upstream servers* are the existing upstream file (`no-resolv` and `server=` lines) written when servers are set by
  hand, for every connection type.
- A change to these files restarts dnsmasq (DHCP and DNS pause for a moment); nothing else is touched.

**The API** (`internal/netapi/dns.go`; all need a session, changes also the CSRF token, and are refused with 409 while a change is
being applied; everything is saved as a draft):

| Request | What it does |
|---|---|
| `GET /api/v1/dns` | `upstream` (`mode`: `provider` or `custom`; `servers`: the ones set by hand; `provider`: what the provider's servers are right now; `static`: the connection has a fixed address, so servers are required; `pending`), `domain` (read only here), `networks` (one per Local Network: `mode` `router` or `custom`, `servers`, `router` (the address it is told otherwise), `dhcp` (the router runs DHCP there), `notes`, `pending`), `hosts`, `forwards` (each with `text`, `enabled`, `pending`), `editable` |
| `PUT /api/v1/dns/upstream` | `{"servers": [...]}` sets the servers the router asks instead of the provider's; an empty list goes back to the provider's (refused with a fixed address). The same list as `dns` on the Internet page |
| `PUT /api/v1/dns/networks/{id}` | `{"servers": [...]}` sets what the network is told to use (`lan` is the default network); an empty list goes back to the router |
| `POST /api/v1/dns/hosts`, `PUT` / `DELETE /api/v1/dns/hosts/{id}` | local names: `name`, `address`, `enabled` (left out stays); names are kept in lower case |
| `POST /api/v1/dns/forwards`, `PUT` / `DELETE /api/v1/dns/forwards/{id}` | forwarded domains: `domain`, `servers`, `enabled` |
| `GET /api/v1/dns/lookup?name=` | asks the router's own DNS server (127.0.0.1, port 53) one question and returns `query`, `kind` (`name` or `address`), `server`, `status` (`answered`, `not_found`, `timeout`, `error`), `answers`, `millis` and `message`. Read only, needs a session only. A name is up to 253 characters of letters, digits, `-`, `_` and `.`; anything else is refused with 400 |
| `PUT /api/v1/settings/domain` | `{"domain": "lan"}` sets the local domain (kept in lower case); empty clears it. Refused when it would swallow a forwarded domain |

*Notes on a network's servers* (in `notes`, advice, not errors): the router runs no DHCP server there, so nothing tells the devices;
a server is on **another** network, so a firewall rule must let this network reach it; a public server for a network that has
no internet access. Ids are `h1`... for names and `f1`... for forwards, the lowest free one. The servers set by hand were
tightened for both lists: up to three, IPv4, not loopback, not unspecified, not multicast or broadcast, each once.

**Safety at the last moment.** Even if the validation were bypassed, the writer refuses a domain or name that is not a plain
DNS name, a forwarded domain with a slash or a newline in it, and any server or address that is not a plain IPv4 address.

**Checks.**

1. *The real dnsmasq answers real queries.* `TestTheRealDnsmasqAnswersAsTheFileSays` runs three dnsmasq processes in a
   throw-away namespace (no privileges): the router's, with the generated local file; a server for a forwarded domain; and a
   general upstream that claims every name with its own address, so that what the router did with each question shows in the
   answer. It asks for a local name in full and short, a name that is a full name, a switched-off name, an unknown name inside the
   local domain (not found, never sent upstream), a forwarded domain and a name below it (answered by the forward's server, not
   the upstream), any other name (the upstream), and a reverse lookup.
2. *The real `dnsmasq --test`* accepts the files (with a control: it refuses a line it does not know).
3. *Exact text* of the files and of the DHCP lines, and the plan: a DNS change restarts only dnsmasq.
4. *The lookup test* is checked against a small fake DNS server (answers, not found, SERVFAIL, silence, reverse lookups, and the
   exact name the router is asked) and by hand against a real dnsmasq started the way the router starts it (`interface=`
   lines, no `bind-interfaces`): it does answer on 127.0.0.1, which the lookup test relies on (the web server's own firewall
   rule accepts everything on the loopback interface).
5. What dnsmasq would tell a DHCP client is *not* checked with a real client (no DHCP client is available to the tests); the
   lines are compared as text and accepted by `dnsmasq --test`.

## 12. Build plan

Each stage is its own commit with tests and updates this document.

1. **Model** (done): domain, local names, forwards, per-network servers; validation; review lines.
2. **Files** (done): the translator writes them; checked with `dnsmasq --test` and with real queries to the real dnsmasq in a namespace.
3. **API** (done): the DNS settings, the local names, the forwards, per-network choice, the domain on Settings.
4. **DNS page** (done, Services, DNS) and the domain field on Settings.
5. **Lookup test** (done), on the Diagnostics page: ask the router a name and see the answer and how long it took.
6. Polish; screenshots in this document.

## 13. Not planned (yet)

DNS over TLS and DoH upstream · DNSSEC validation · ad and tracker blocking lists · IPv6 records (AAAA, the whole router is IPv4
for now) · wildcard names · CNAME and other record types (only name-to-address entries) · different upstream servers per network ·
splitting one name into different answers per network (views) · importing /etc/hosts · replacing dnsmasq with Unbound.

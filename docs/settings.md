# PeakRoute OS settings

Status (2026-10-11): in progress. System, Settings holds the choices that belong to the whole router rather than to one network or
port. Like the network pages, everything here is saved as a **draft** and applied when you commit (the review shows each change in
words), except where a section says otherwise. This file is a living document: every commit that changes the page updates it, and
it is meant to become the settings chapter of the user documentation.

Contents: 1 General (host name, country, local domain), 2 Time, 3 Logging, 4 Power, 5 Internals, 6 Not planned.

---

## 1. General

### Host name

The name of this router, such as `my-router`: letters, digits and hyphens, 1 to 63 characters, not starting or ending with a
hyphen. Set, it is:

- **written to the machine** (`/etc/hostname`, applied with `hostname -F`, nothing else restarts), and
- **shown on the Overview** as the device's name.

Left empty, PeakRoute does not touch the machine's name (it keeps whatever it has; the Overview then shows the name recorded at
first start).

**Tell my internet provider this name** (off by default): when on, a connection that gets its address by DHCP sends the name to the
provider's DHCP server (`hostname` on the connection's interface stanza). A few providers want a particular name; most do not
care, and leaving it off tells them nothing. A connection with a static address or PPPoE keeps the setting but cannot send it.
The switch needs a name (turning it on with an empty name is refused, and clearing the name turns it off).

Before 2026-10-11 the name was a field on the Internet page that was sent to the provider and nothing else; it moved here and
became the machine's name.

### Country

Where the router is used. Wi-Fi follows it (the channels and power the country allows), and Wi-Fi cannot be switched on without it.

### Local domain

A name such as `lan` or `home.example.com`, empty until set. See the DNS document (`docs/dns.md`, section 5).

## 2. Time

*(not built yet)*

## 3. Logging

*(not built yet)*

## 4. Power

*(not built yet)*

## 5. Internals

**Storage** in `network.json`: `hostname`, `send_hostname`, `country`, `domain`. Settings are part of the staged configuration, so
they go through the same draft, review, commit, apply and confirm steps as the network pages; a commit that changes only a setting
shows only that setting in the review and restarts only what it needs.

**API** (`internal/netapi/settings.go`; sessions and the CSRF token needed, administrators only for changes, refused with 409 while
a change is being applied):

| Request | What it does |
|---|---|
| `GET /api/v1/settings` | the draft: `hostname`, `send_hostname`, `hostname_pending`, `country`, `country_pending`, `countries`, `domain`, `domain_pending`, `editable` |
| `PUT /api/v1/settings/hostname` | `{"hostname", "send_hostname"}`; a field left out stays; an empty name clears it (and the sending, unless the request says otherwise) |
| `PUT /api/v1/settings/country` | `{"country": "US"}` |
| `PUT /api/v1/settings/domain` | `{"domain": "lan"}` |

**Applying.** The host name is the `hostname` service: `/etc/hostname` is written and `hostname -F /etc/hostname` makes the running
machine take it, which is the first action of a commit and does not restart anything else. (The image should carry
`libnss-myhostname` so that the machine can resolve its own name without an `/etc/hosts` entry; PeakRoute does not edit
`/etc/hosts`, because dnsmasq would hand such entries to the whole network.)

## 6. Not planned (yet)

*(see the end of each section as it is written)*

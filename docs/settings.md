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

A router has no battery-backed clock it can trust, and a lot depends on the time being right: the firewall log's timestamps, DHCP
leases, certificates. This card sets the **time zone** and the **time servers**.

- **The router's clock** is shown live at the top (in the zone the machine is in right now), with a line saying whether the clock
  has been set from a time server and which one. After the internet connection comes up it takes a minute or so for the first
  synchronisation.
- **Time zone**: any zone of the system's time zone database (about 420, plus UTC), grouped by region; the zones of the country
  chosen above come first. *Not set* leaves the machine's own zone (the choice offered until you pick one; it shows what the
  machine has). The zone is written to `/etc/timezone` and `/etc/localtime` is pointed at it; a zone this machine does not have
  fails the commit and changes nothing.
- **Keep the clock right with time servers (NTP)**: on by default. The time service is `systemd-timesyncd` (the image carries
  it). Switching it off stops and disables the service; the clock then drifts.
- **Time servers**: up to four names or IPv4 addresses (names in lower case; a loopback or unspecified address is refused),
  separated by commas. Empty uses the default, `pool.ntp.org` (the NTP pool project, which any client may use). The router's own
  DNS resolves the names.

A commit that changes only the zone runs one command (`ln -sf`) and restarts nothing; one that changes the servers (or switches
the time off or on) restarts only `systemd-timesyncd`, after the network and DNS changes of the same commit.

## 3. Logging

*(not built yet)*

## 4. Power

*(not built yet)*

## 5. Internals

**Storage** in `network.json`: `hostname`, `send_hostname`, `country`, `domain`, `timezone`, `ntp_disabled`, `ntp_servers` (every key is written only when it has something in it). Settings are part of the staged configuration, so
they go through the same draft, review, commit, apply and confirm steps as the network pages; a commit that changes only a setting
shows only that setting in the review and restarts only what it needs.

**API** (`internal/netapi/settings.go`; sessions and the CSRF token needed, administrators only for changes, refused with 409 while
a change is being applied):

| Request | What it does |
|---|---|
| `GET /api/v1/settings` | the draft: `hostname`, `send_hostname`, `hostname_pending`, `country`, `country_pending`, `countries`, `domain`, `domain_pending`, `timezone`, `timezones` (with their countries), `suggested_timezones` (those of the chosen country), `ntp_enabled`, `ntp_servers`, `ntp_default_servers`, `time_pending`, `time` (the clock as it is now: `now`, `timezone`, `ntp_enabled`, `synchronized`, `server`; null when it cannot be read), `editable` |
| `PUT /api/v1/settings/hostname` | `{"hostname", "send_hostname"}`; a field left out stays; an empty name clears it (and the sending, unless the request says otherwise) |
| `PUT /api/v1/settings/country` | `{"country": "US"}` |
| `PUT /api/v1/settings/domain` | `{"domain": "lan"}` |
| `PUT /api/v1/settings/time` | `{"timezone", "ntp_enabled", "ntp_servers"}`; a field left out stays; blank servers are dropped and names lower-cased; an empty list means the default |

**Applying.** The host name is the `hostname` service: `/etc/hostname` is written and `hostname -F /etc/hostname` makes the running
machine take it, which is the first action of a commit and does not restart anything else. (The image should carry
`libnss-myhostname` so that the machine can resolve its own name without an `/etc/hosts` entry; PeakRoute does not edit
`/etc/hosts`, because dnsmasq would hand such entries to the whole network.)

The time zone is the `timezone` service (`/etc/timezone` written, then a shell one-liner points `/etc/localtime` at the zone and fails when
the zone file is missing); the time servers are `/etc/systemd/timesyncd.conf.d/peakroute.conf`, present only while they are not the
defaults (chosen servers, or the time switched off, marked by a comment line the `ntp` service's command looks for), applied by the
`ntp` service (`enable` and `restart`, or `disable --now`). The zone list is `internal/config/timezones.go`, generated from the time
zone database's `zone.tab`; a test checks every name against the database of the machine it runs on. The clock is read with
`timedatectl show` and `timedatectl timesync-status`.

## 6. Not planned (yet)

*(see the end of each section as it is written)*

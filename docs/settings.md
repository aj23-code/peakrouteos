# PeakRoute OS settings

Status (2026-10-11): built. System, Settings holds the choices that belong to the whole router rather than to one network or
port. Like the network pages, everything here is saved as a **draft** and applied when you commit (the review shows each change in
words), except where a section says otherwise. This file is a living document: every commit that changes the page updates it, and
it is meant to become the settings chapter of the user documentation.

Contents: 1 General (host name, country, local domain), 2 Time, 3 Logging (syslog forwarding), 4 Power (restart, power off), 5 Internals, 6 Not planned.

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

### Syslog forwarding

Sends the router's logs to up to three syslog servers, so that they are kept somewhere else and for longer than the router keeps
them (the firewall log holds 7 days or 100,000 entries; the journal 100 MB).

- **Servers**: a name such as `logs.example.com` or an IPv4 address (not this router itself, and not `localhost`), a port (514
  unless you say otherwise) and **UDP or TCP**. The same server may be listed once per port and protocol.
  - *UDP* sends one datagram per message and cannot know whether it arrived.
  - *TCP* keeps one connection open and sends one line per message (the line ends with LF, which every common receiver
    understands). While the server is away, messages wait in a queue of 2,000 per server and go out in order when it is back; if
    the queue is full the oldest goes (the page counts them as *dropped*). Connecting again is tried after 1 second, then 2, 4,
    and so on up to every 30 seconds.
  - There is **no encryption** yet (no TLS): send logs only to a server on a network you trust.
- **What is sent** (at least one when there is a server):
  - **The system log**: everything the system's journal gets from the moment forwarding starts (the router's services, the kernel,
    the daemon), without the firewall's own lines. Facility and severity are the journal's own.
  - **The firewall log**: every packet a rule with *Log matching traffic* matches, as it happens (not folded like the page does),
    as `rule="Block mail" rule_id=fw2 action=block in=br20 out=eth0 src=10.20.0.5 sport=40000 dst=1.1.1.1 dport=25 proto=TCP`:
    the rule's name and what it does, so a log server can read it without knowing the router. Facility `local0`-style
    `4` (security), severity *warning* for blocked and rejected traffic, *informational* for allowed.
- **Format**: RFC 5424, one line, with the router's host name (or the machine's name) as the origin. Header fields are made safe
  (printable ASCII, no spaces) and the text is one line of at most 1,900 bytes.
- **Live, not a history**: what happened while forwarding was off, or before the router started, is not sent (the router will not
  replay a week of the firewall log when it restarts: packets more than two minutes old are skipped).
- **Running now**: under the servers the page shows how sending to each server in effect is going: connected or not, how many
  messages went out, how many were dropped, and the last error. **System, Diagnostics, Syslog test** sends one test message to every
  server that is running and says what happened to each.
- Like everything here the servers are saved as a draft; they start receiving when you commit (within a few seconds; nothing is
  restarted, the daemon does the sending itself).

## 4. Power

**Restart the router** and **Power off**, for administrators. Each button asks twice (the first click turns it into *Click again to
confirm* for five seconds). They are refused (409) while a configuration change is being applied or waits for confirmation, so a
restart never cuts a change off halfway; what was committed is kept.

- The router answers first and acts two seconds later (`systemctl reboot` or `systemctl poweroff`), so that the page gets its answer.
- After a restart the page waits until the router has gone away and come back, then reloads by itself. After *Power off* it says to
  switch the router back on.
- The request is written to the log (`Restart requested by "alice"`). A read-only account is refused like for every change.
- In `-dev` mode the command is only logged, so a developer's own machine is never restarted.

## 5. Internals

**Storage** in `network.json`: `hostname`, `send_hostname`, `country`, `domain`, `timezone`, `ntp_disabled`, `ntp_servers`, `syslog` (`servers`, `system`, `firewall`) (every key is written only when it has something in it). Settings are part of the staged configuration, so
they go through the same draft, review, commit, apply and confirm steps as the network pages; a commit that changes only a setting
shows only that setting in the review and restarts only what it needs.

**API** (`internal/netapi/settings.go`; sessions and the CSRF token needed, administrators only for changes, refused with 409 while
a change is being applied):

| Request | What it does |
|---|---|
| `GET /api/v1/settings` | the draft: `hostname`, `send_hostname`, `hostname_pending`, `country`, `country_pending`, `countries`, `domain`, `domain_pending`, `timezone`, `timezones` (with their countries), `suggested_timezones` (those of the chosen country), `ntp_enabled`, `ntp_servers`, `ntp_default_servers`, `time_pending`, `time` (the clock as it is now: `now`, `timezone`, `ntp_enabled`, `synchronized`, `server`; null when it cannot be read), `syslog` (`servers`, `system`, `firewall`), `syslog_pending`, `syslog_status` (per server in effect: `sent`, `dropped`, `connected`, `last_error`), `editable` |
| `PUT /api/v1/settings/hostname` | `{"hostname", "send_hostname"}`; a field left out stays; an empty name clears it (and the sending, unless the request says otherwise) |
| `PUT /api/v1/settings/country` | `{"country": "US"}` |
| `PUT /api/v1/settings/domain` | `{"domain": "lan"}` |
| `PUT /api/v1/settings/syslog` | `{"servers": [{"host", "port", "protocol"}], "system", "firewall"}`; replaces the forwarding (a port left out is 514, a protocol left out is udp; hosts lower-cased); no servers turns it off |
| `POST /api/v1/system/power` | `{"action": "reboot"}` or `{"action": "poweroff"}`; administrators only; 409 while a change is being applied or waits for confirmation |
| `POST /api/v1/syslog/test` | sends one test message to every syslog server in effect: `results` (`host`, `port`, `protocol`, `ok`, `message`) |
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

Syslog forwarding is not a system service: the daemon does it (`internal/syslogfwd`). It follows the journal for new entries
(`journalctl -f -n 0`) while the system log is wanted, gets the firewall packets from the same reader that feeds the firewall log
(`fwlog.Collector.OnPacket`), and keeps one sender per server (a goroutine with a queue). Every five seconds the daemon hands the
configuration *in effect* to `Forwarder.Update`, which starts, keeps or stops senders and the journal follower; nothing is
restarted for a change.

## 6. Not planned (yet)

Looked at, because other routers have them, and left out for now:

- **Setting the time by hand** (with the time servers off) and **serving time to the networks** (an NTP server for the LAN, and
  telling DHCP clients about it). The clock follows time servers or it does not.
- **Encrypted syslog (TLS)**, syslog over the VPN, per-server choices of what to send, and **syslog from other sources** (the DNS
  query log, a per-rule choice of server).
- **The web interface's own address, port and certificate**, and allowing the web interface from the internet: certificates will
  have their own page (System, Certificates) and the firewall keeps the web interface to the default network for now.
- **Automatic updates** (the Updates page), **backup and restore** (it comes last, once every setting has its place), **factory
  reset**, **UPnP and NAT-PMP** (a service of its own, if ever), **scheduled restarts**, **a login banner**, **a contact name and
  location** for the device, **e-mail or push alerts**, **LED and fan control**.
- **A choice of language, units and date format.** The page uses the browser's.

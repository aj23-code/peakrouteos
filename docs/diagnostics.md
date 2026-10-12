# PeakRoute OS Diagnostics

Status (2026-10-11): built. System, Diagnostics holds the small tools for finding out why something does not work. They look; they
change nothing, and they ask about what is running now, not about changes still in the draft. This file is a living document: every
commit that changes the page updates it, and it is meant to become the Diagnostics chapter of the user documentation.

---

## 1. The tools

| Tool | What it does |
|---|---|
| **Ping** | sends 1 to 20 small packets to a name or an IPv4 address and says how many came back, how many were lost and the fastest, average and slowest time; the lines appear as ping prints them |
| **Traceroute** | lists the routers a packet passes on its way, one line each, with the time; it shows where a connection stops |
| **Connection test** | tries to open a TCP connection to a port and closes it again: *open*, *refused*, *no answer*, *unreachable* or *no such name*, with how long it took |
| **DNS lookup** | asks this router's own DNS server a name or an address (docs/dns.md) |
| **Syslog test** | sends a test message to every syslog server in effect (docs/settings.md) |

## 2. Ping and traceroute

- They run as **tests** you can watch: the page looks at the router every 0.7 seconds and shows each line as it comes. A test can be
  **stopped**. At most **three** run at once; finished tests are forgotten after 20 more.
- **Send from.** Ping and the connection test can be sent from *Automatic* (the way the routing table says), from the **internet
  connection's** address, from the router's address on any **Local Network**, or from an up **VPN** (its address on the tunnel). That
  tests the router from that side: ping a device from the Guest network's address to see that Guest can reach it. A source that has no
  address right now is refused with the reason. Traceroute always leaves the way the routing table says (`tracepath` takes no source).
- **Limits.** A ping has at most 20 packets and ends by itself (a deadline of the count plus five seconds, and the test stops at the
  count plus ten); a traceroute goes at most 20 hops and is stopped after 90 seconds; the output is kept to 500 lines.
- **What may be typed.** A host name or an IPv4 address (IPv6 is not used yet); nothing that a program could take for an option (a
  target never starts with `-`), no spaces or other characters. Nothing is run through a shell: the programs and their arguments are
  fixed (`ping -4 -n -c N -W 2 -w N+5 [-I source] -- target`, `tracepath -n -4 -m 20 target`).
- **Programs.** `ping` is `iputils-ping` and `tracepath` is `iputils-tracepath`, both in the image already; the page says so when one
  is missing. A `no reply` line in a traceroute is a router that does not answer these probes, which is often normal.
- **Who may.** Starting a test sends traffic, so it is a `POST` and a **read-only account cannot start one** (it can still use the
  DNS lookup, which only reads). The page shows the router's refusal in words.

## 3. The connection test

Opens a TCP connection to `host:port` (from the chosen source address), waits at most four seconds, closes it, and says what happened:

| Result | Meaning |
|---|---|
| **Open** | the connection was accepted: something listens and nothing in the way blocks it |
| **Refused** | the machine answered and refused: nothing listens on that port (the network between is fine) |
| **No answer** | nothing came back in time: a firewall drops it, or the address is not there |
| **Unreachable** | there is no way to reach that address from here (no route) |
| **No such name** | the name could not be turned into an address |

Nothing is sent over the connection. Use it to check a port forward (from outside, by asking a friend, or from another network of the
router), a server, or a firewall rule.

## 4. API

| Call | Does |
|---|---|
| `GET /api/v1/diagnostics` | the places a test can be sent from (with their addresses), which programs there are, the limits |
| `POST /api/v1/diagnostics/jobs` | start a ping or traceroute: `tool`, `target`, `count`, `source` (a place's id); answers with the test |
| `GET /api/v1/diagnostics/jobs/{id}` | how it is going: the lines so far, running or the result, the ping's summary |
| `DELETE /api/v1/diagnostics/jobs/{id}` | stop it |
| `POST /api/v1/diagnostics/connect` | the connection test: `host`, `port`, `source` |

## 5. Still to do

- A traceroute from a chosen network's address (needs the `traceroute` package in the image).
- IPv6 targets.
- Ping with a packet size (to look for the largest packet that gets through) and a "don't fragment" option.
- Showing the routing table and the neighbour (ARP) table here.

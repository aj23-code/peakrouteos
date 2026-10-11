# PeakRoute OS Dynamic DNS

Status (2026-10-11): built. Keeps a public name such as `home.example.com` pointed at the address the internet connection has right
now, even when the provider changes it. The router tells the name's service whenever the address changes. This file is a living
document: every commit that changes the page updates it, and it is meant to become the Dynamic DNS chapter of the user documentation.

---

## 1. What it does

You give the router a **name**, the **service** that holds it and the **login** for that service. The router then:

1. finds the address to publish (section 3);
2. tells the service when that address is not the one it told it last, and also **once a week** when nothing changed, so that
   services that drop idle names (some do after 30 days) do not;
3. shows on the page how each name is going: up to date, in trouble (it tries again), or stopped (it needs something changed).

Names are saved as a draft like everything else and start being kept up to date when the change is committed. The router looks
every 30 seconds whether something needs doing, so a changed address reaches the service within about half a minute (plus the time
the connection takes to come up). It does not need a reboot or a restart of anything: the daemon does the updating itself.

## 2. Services

| Service | What you enter | How the router updates it |
|---|---|---|
| **DynDNS**, **No-IP**, **Dynu**, or **another service that speaks dyndns2** (many do) | the name, the user name, the password (or the service's update key); for *another service* also the update server's name | `GET https://<server>/nic/update?hostname=<name>&myip=<address>` with the login as HTTP basic authentication. The answer words are understood: `good` and `nochg` are done; `badauth`, `nohost`, `notfqdn`, `numhost`, `abuse`, `badagent`, `!donator` stop the name (they need something changed); `dnserr` and `911` are tried again |
| **DuckDNS** | a name under `duckdns.org` and the token from your account page | `GET https://www.duckdns.org/update?domains=<name without .duckdns.org>&token=...&ip=<address>`; `OK` is done, `KO` stops the name |
| **Cloudflare** | the full name and an **API token** that may edit DNS for the zone ("Edit zone DNS") | finds the zone (the name with labels taken off from the left until Cloudflare knows one; never just the top-level domain), looks for the A record, changes it (keeping its TTL and proxy setting) or **adds** it when there is none |
| **Another service** (an address to call) | the name, an https address with `{ip}` in it, and a user name or password where the address asks for them | calls the address with the placeholders filled in (each value escaped). A 2xx answer is done; 4xx stops the name (except 429), 5xx is tried again |

`{ip}`, `{hostname}`, `{username}` and `{password}` are the placeholders of a custom address; `{ip}` is required. The address must be
**https**, with the server's name in lower case and no user name or password inside it, and the server's name cannot itself hold a
placeholder. Recipes for services that are not listed (put the secret in the *password* box and `{password}` in the address, not in
the address itself):

- **Namecheap**: `https://dynamicdns.park-your-domain.com/update?host=@&domain=example.com&password={password}&ip={ip}`
- **FreeDNS (afraid.org)**: `https://sync.afraid.org/u/{password}/?ip={ip}` (the password is the update token)

Only https is used, for the services and for the login: a password is never sent in the clear. Redirects are followed only to
https addresses, at most three.

## 3. Which address

Each name has a choice:

- **The address of the internet connection** (the usual one): what the provider gave this router. If that address is **not
  public** (private ranges, the carrier-grade NAT range `100.64.0.0/10`, link-local, loopback) nobody on the internet could reach it,
  so the router does **not** publish it: the page says so, with the address, and the name waits until the connection has a public
  address. The page also warns above the list when the connection's address is not public.
- **The address an internet service reports**: for a router behind another router or a carrier's NAT. The router asks
  `api.ipify.org` (then `icanhazip.com` if that fails) what address its request comes from, over https, and publishes that (after
  checking that it is a public IPv4 address). The answer is kept five minutes. Reaching this router from outside still needs the
  other router to forward to it: Dynamic DNS only keeps the name right. This choice does contact a third party each time (about
  every five minutes while it is in use), which the other choice does not.

IPv4 only; the whole router is IPv4 for now.

## 4. When it tries, and when it stops

- **Updated** or **already had it**: done. The address is remembered; nothing more is sent until the address changes, or a week has
  passed (`nochg` answers are cheap but the services dislike being told the same thing often).
- **Trouble that may pass** (the service is busy or unreachable, a 5xx answer, 429, `911`, anything unexpected): tried again after 1
  minute, then 2, 4, 8, 16, and every 30 minutes at most, until it works.
- **Needs something changed** (wrong login, no such name, blocked for abuse, an address that is refused): the name **stops** and
  says why on the page. It stays stopped until the entry is edited (and the change committed: an edited entry starts afresh), or you
  press **Update now**, which tries once whatever it was waiting for.
- A name that is switched off is not updated and not listed as running. A name changed in the draft shows *Draft* until the change
  is committed; its status is that of the entry as it is running.
- The state (last address, last result) lives in memory: after the daemon restarts, each name is told its address once. Nothing is
  sent while the connection is down.

**Update now** (on a running name) tells the service the address immediately and shows the answer. It works on the name as it is
running, not on the draft.

## 5. Staying out of trouble

- **Passwords and tokens are write-only.** They are stored in `network.json` (mode 0600, like the PPPoE password), never sent back
  by the API (`password_set` says whether one is saved), never in the review (a change is *set* or *changed*), never in a status
  line or the log; to keep a saved one, leave the box empty when editing.
- A name can be listed once. At most 10 names.
- A name, a server and an address are checked when saved and again before they are used; a login cannot hold a line break.
- Services are only ever called by the router (its own traffic is not filtered by the firewall).

## 6. API

All need a session; changes also the CSRF token, and are refused with 409 while a change is being applied or waits for confirmation;
everything is saved as a draft.

| Request | What it does |
|---|---|
| `GET /api/v1/ddns` | `entries` (`id`, `provider`, `hostname`, `server`, `username`, `url`, `source`, `enabled`, `password_set`, `text`, `pending`, and `status`: `address`, `ok`, `attention`, `message`, `last_try`, `last_success`, `next_try` in Unix milliseconds (0: never or not waiting), null when the name is not running as shown), `servers` (the dyndns2 servers offered by name), `wan_address`, `wan_public`, `editable` |
| `POST /api/v1/ddns` | adds a name: `provider` (`dyndns2`, `duckdns`, `cloudflare`, `custom`), `hostname`, `server`, `username`, `password`, `url`, `source` (`wan` by default or `web`), `enabled` |
| `PUT /api/v1/ddns/{id}` | changes a name; a field left out stays, an empty or missing password stays; what belongs to another kind of service (the server, the address, the user name for a token) is dropped when the service changes |
| `DELETE /api/v1/ddns/{id}` | removes it |
| `POST /api/v1/ddns/{id}/update` | tells the service the address now and answers with the status; 404 when the name is not running |

## 7. Internals

- **Storage** in `network.json`, written only when there is something in it: `"ddns": {"entries": [{"id": "d1", "provider": "duckdns",
  "hostname": "mine.duckdns.org", "password": "...", "source": "wan"}]}`. Code: `internal/config/ddns.go` (model, validation, review lines),
  `internal/ddns` (the updater and one function per service), `internal/netapi/ddns.go` (the API), `web/static/scripts/ddns.js`.
- The updater reads the configuration **in effect** (a draft does nothing until it is committed), and takes the connection's address from
  the same place the Internet page does.
- No file is written and no service restarted for a change: a commit that changes only Dynamic DNS has an empty plan.

## 8. Checks

`internal/ddns/ddns_test.go` runs the updater against fake services over real TLS: the exact requests (paths, query, login, user
agent), every answer word, the weekly refresh, the growing wait and its cap, the stop and the fresh start after an edit, the public
address rule, the address service (kept five minutes, the next one tried), Cloudflare (zone search, change, add, unchanged, every
trouble), custom addresses with escaping, that no status ever holds a secret, and that a redirect to plain http is refused. The
mutation check broke each rule in turn and every break was caught except those that change nothing a user can see.
Not checked: a real service (that needs an account; the first real test is yours).

## 9. Not planned (yet)

IPv6 (AAAA) records · several names in one update (one entry per name) · keeping an address that is not the router's own (a server
behind it) · TTL and proxy choices for Cloudflare records (an existing record keeps its own) · wildcard names · updating a name from
the address of a different interface than the internet connection · e-mail on failure · a log of past updates.

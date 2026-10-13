# First-run setup

A router that has not been set up shows the setup wizard at `/first_run` (every other address redirects there). It is the only page
that needs no sign-in, and it is open **only until the router is set up**: from then on every `/api/v1/setup/*` route answers 409
and the page sends you to the sign-in.

## How you reach it: the setup network

A router that has not been set up has no address anyone could browse to. So, **until settings with a local network are committed**,
the daemon builds a *setup network* (`internal/bootstrap`): all the wired ports are joined in a temporary bridge `brsetup` with the
address **192.168.77.1/24**, and a DHCP server on it hands out `192.168.77.100` to `.199` (1 hour). Plug a computer into any port, let
it take an address, and open `https://192.168.77.1`.

- The address is unusual on purpose: `192.168.1.1` is what most internet providers' routers use, and a router behind one would clash.
- No router and no DNS server are offered in the leases, and there is no DNS on it: a computer that has another connection (Wi-Fi) keeps
  using that one for the internet, and the address is typed.
- It follows the *committed* settings, not the wizard: the wizard only saves a draft, so the setup network stays until the first commit
  (and so does the way in if the router is restarted before the draft is applied). The commit lets go of the ports first, so they can
  join the real bridge, and the setup network **comes back if the change fails or is reverted** (the pipeline's `Before` and `Settled`
  hooks).
- Wi-Fi adapters are left alone (the wireless pages set them up).
- The wizard finds the port you are plugged into from the router's ARP table and the bridge's forwarding table (`bridge fdb`), so only
  that one is protected, not every port of the setup bridge.
- If you plug the internet cable in a port while setting up, the setup DHCP server would answer on that network too: keep the modem out
  until the wizard is finished.

## The pages

**Welcome.** The choice: *Set up as a new router*, or *Restore from a backup*.

**Setting up as an access point** is the same wizard with other pages (docs/access-point.md): region, the network (cable ports, DHCP or
a fixed address), Wi-Fi, the administrator and the review.

**Setting up as a new router**

1. **Region.** Country (sets what the Wi-Fi may do), time zone (the browser's own zone is suggested, and the zones of the chosen
   country come first in the list) and the name of the router (the machine's own name is suggested).
2. **Internet.** The internet port (the one with the default route, or one with a cable, is suggested; Wi-Fi adapters are never
   offered, they are set up on the Wireless page) and how it connects: automatic (DHCP), a fixed address (address, netmask as
   `255.255.255.0` or `/24`, gateway, DNS servers) or PPPoE.
3. **Local network.** The ports of the local network (all the others are ticked), the router's address and netmask, and whether the
   router hands out addresses (DHCP), with the range (it follows the address until typed by hand).
4. **Wi-Fi** (only where an adapter can host a network). One name and password; every adapter that can be an access point serves it: one
   adapter on 2.4 GHz (every device can use it), two or more with the first on 5 GHz and the next on 2.4 GHz and band steering. Channels
   are automatic. The Wireless page does the rest.
5. **Administrator.** User name and password (the rules of `docs/administrators.md`).
6. **Finish.** All the answers on one page; the button does everything.

Every page is checked by the server before the next one opens (`POST /api/v1/setup/check`, which changes nothing), including the
pages before it, so an answer that stops being valid because of a later one is caught. The last button checks everything again.

**Restoring a backup.** The file and its passphrase, then what is in it (`docs/backup.md`): the administrators, the settings (with the
usual list of what they set), the certificates and, if the file has it, whether to bring the history. A backup without administrators
is refused (nobody could sign in). If the backup uses ports this router does not have, or you tick *Choose this router's ports
myself*, you assign the internet port and the local ports from this router's own, and the report is made again for that choice.
*Restore* then brings it all across and leaves you at the sign-in.

## What is saved, and where

Nothing is saved until the last button. Then, in this order, all in the router's JSON files:

| Answers | File |
|---|---|
| Name, country, time zone, ports, internet connection, local network, DHCP | `network.json.staged`, the **draft** |
| The administrator (name, password hash, role) | `auth.json` |
| That the router is set up | the `configured` file, written last |

The settings are the draft, not applied: right after the setup the new administrator is signed in and the **review dialog opens by
itself** (a flag in the browser's session storage), listing what will change and the files that will be written. *Commit* applies it
with the usual confirmation. The reason is the same as everywhere else: if the router's address changes, the page follows it, and the
change is undone if the new address cannot be reached. A power cut between the steps leaves a router that still offers the setup (the
flag is last), and the draft of the earlier try is replaced.

A restore does the same with the backup's own files; it does not sign you in (the accounts are the ones in the backup), and the draft
waits for the first sign-in.

## The console login

The administrator is also the login of the router's own console (a screen and keyboard, or a serial cable): the daemon makes a Linux
account of the same name (`useradd`, home directory, bash, the `sudo` group) and gives it the **password hash the web interface keeps**
(bcrypt, `chpasswd -e` on standard input, which the system's `crypt` understands), so the two can never differ and the password is never
on a command line. Because it is a Linux name too, the wizard asks for 3 to 32 lower-case letters, numbers, `-` or `_`, starting with a
letter, and refuses a name the system already uses (an account or a group). The account is recorded as `console_user` in `auth.json`.

- Changing that account's password in the web interface changes the console's first; if that fails, neither changes.
- It cannot be removed or made read-only (it would leave a console login that the web interface knows nothing about). Moving the
  console login to another administrator is still to do.
- Accounts of the system (user ids outside 1000 to 59999) are never changed or removed by this.
- A restore makes the console account of the backup (or, for an older backup, the first administrator with a usable name); if that
  cannot be done the web interface still works and the page says so. On a router that is already set up, restoring keeps the console
  login of this machine.
- On a developer's machine (`-dev`) nothing of the host's accounts is touched; it only logs what it would do.

## Safety

- **One setup only.** The account and the flag are made under one lock (`Manager.SetupAdmin`): of any number of simultaneous
  attempts exactly one wins, the others get 409. A failed step leaves the router as it was (no account, no draft, not set up).
- **The port you are connected through** (found from the browser's address and the router's own addresses; for a bridge, its member
  ports) cannot be the internet port and must be in the local network, or applying the settings would cut you off. This is checked by
  the server for the new router and for a restore with ports chosen by hand.
- **Restoring is no new way in.** Anyone who can reach the page of a router that is not set up can already make themselves its
  administrator; restoring also needs the backup file *and* its passphrase. Restoring never replaces an existing account, because it
  is refused once the router is set up (a router that is set up restores from System, Backup and restore).
- Unknown fields in a request are refused, not ignored.

## Interface

| Method and path | Notes |
|---|---|
| `GET /api/v1/setup/info` | the ports (wired only, with the one the browser is on), countries, time zones, suggestions, defaults |
| `POST /api/v1/setup/check` | `{through: "region"\|"internet"\|"lan"\|"admin", ...answers}`: the errors, by field, up to that page |
| `POST /api/v1/setup/apply` | all the answers: saves, signs in, answers `{redirect}` |
| `POST /api/v1/setup/restore/open` | multipart `file` and `passphrase`: the report and a token |
| `POST /api/v1/setup/restore/preview` | `{token, wan, lan}`: the report for a choice of ports |
| `POST /api/v1/setup/restore/apply` | `{token, wan, lan, history}` |

## Still to do

- `peakroute.local` (mDNS) and a captive-portal style redirect, so the address need not be typed.
- Wired ports plugged in after the daemon started (a USB network adapter) are not joined to the setup network until it restarts.
- Choosing the update channel and an automatic update window (when Updates exist).
- The image (no installer, no console wizard: it is flashed and the first thing seen is this page): the `sudo` package, root's password
  locked and no autologin, so that this account is the only way in. Verify on a real image that a bcrypt hash in `/etc/shadow` logs in on
  the console.
- Moving the console login to another administrator.

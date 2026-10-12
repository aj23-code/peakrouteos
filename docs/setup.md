# First-run setup

A router that has not been set up shows the setup wizard at `/first_run` (every other address redirects there). It is the only page
that needs no sign-in, and it is open **only until the router is set up**: from then on every `/api/v1/setup/*` route answers 409
and the page sends you to the sign-in.

## The pages

**Welcome.** The choice: *Set up as a new router*, or *Restore from a backup*.

**Setting up as a new router**

1. **Region.** Country (sets what the Wi-Fi may do), time zone (the browser's own zone is suggested, and the zones of the chosen
   country come first in the list) and the name of the router (the machine's own name is suggested).
2. **Internet.** The internet port (the one with the default route, or one with a cable, is suggested; Wi-Fi adapters are never
   offered, they are set up on the Wireless page) and how it connects: automatic (DHCP), a fixed address (address, netmask as
   `255.255.255.0` or `/24`, gateway, DNS servers) or PPPoE.
3. **Local network.** The ports of the local network (all the others are ticked), the router's address and netmask, and whether the
   router hands out addresses (DHCP), with the range (it follows the address until typed by hand).
4. **Administrator.** User name and password (the rules of `docs/administrators.md`).
5. **Finish.** All the answers on one page; the button does everything.

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

- Wi-Fi in the wizard (a name and password for a first access point).
- Choosing the update channel and an automatic update window (when Updates exist).
- The console wizard of the image (the first-boot script) should print the address of this page.

# PeakRoute OS Backup and restore

Status (2026-10-12): built. System, Backup and restore makes one encrypted file with everything needed to set the router up again, and
puts it back. This file is a living document: every commit that changes the page updates it, and it is meant to become the Backup and
restore chapter of the user documentation.

---

## 1. What a backup holds

| Section | Contents | When restored |
|---|---|---|
| **Settings** | `network.json`: networks, firewall, DNS, VPN (all keys, the OpenVPN authority, devices), DDNS passwords, Wi-Fi, host name, time and syslog settings | becomes the **draft**: reviewed and committed from the bar at the top, with the usual confirmation |
| **Administrators** | `auth.json` (accounts, password hashes, roles, session timeout) and the operating mode from `sys.json` | at once; **everyone is signed out** |
| **Certificates** | the web authority and its key, the web certificate and its key, the custom marker and extra names (docs/certificates.md) | at once |
| **History** (off unless asked for) | the traffic, internet quality and firewall log databases | when the router next starts |

The settings in a backup are the ones that are **running** (the committed `network.json`), never changes still in the draft. Not in a
backup: web sessions, DHCP leases, presence, the draft, and everything the router can make again from the settings (every generated
file under `/etc`). The machine's own identity (its build, kernel) is not restored.

## 2. The file

- **Always encrypted, with a passphrase** of 10 to 256 characters that you choose (twice). There is no unencrypted option, because the
  file holds every private key. **A lost passphrase cannot be recovered.**
- Layout: a header in the clear (`PRBACKUP`, file format, scrypt cost, salt, nonce), then the archive sealed with XChaCha20-Poly1305
  under a key made from the passphrase with **scrypt** (N=2^16, r=8, p=1: 64 MiB and about a quarter of a second). The header is
  authenticated too: a changed byte anywhere stops the file from opening (the router says *the passphrase is wrong, or the file is
  damaged*, as the two cannot be told apart). A file may not ask for a cost above 128 MiB.
- The archive is a gzipped tar: `manifest.json` first (format, **schema version**, time, host name, software, sections, and the size
  and SHA-256 of every file, the ports the settings use), then the files. Only the paths a backup may hold are accepted; links, paths
  that are not clean, duplicates, files the manifest does not list or whose size or checksum differ, and anything over 1 GiB unpacked
  are refused.
- Name: `peakroute-<host name>-<date>.prbackup`.
- **Schema version.** It is the version of what is inside (`backup.SchemaVersion`, now 1) and goes up when a change to `network.json` or
  `auth.json` is one an older router could not read. A router **refuses a backup with a higher number** than it knows (it says to update
  first) and reads older ones (the settings are tolerant of fields that were added later).
- Making one is a `POST` (the passphrase never goes in an address) and only for an administrator, because it would hold every key. Only
  one backup is made or opened at a time (the key derivation costs 64 MiB).

## 3. Restoring

1. **Open it.** Choose the file and enter the passphrase. The router decrypts it in memory and shows what is in it: the host name, when
   and by which version it was made, and for each section what restoring it would do. It keeps the opened backup for ten minutes under a
   token, and **changes nothing**.
2. **Check.** The settings are validated against this software (a backup that is not valid here cannot be restored as it is, and the
   page says why), and shown as the same review lines as a commit, against what runs now. The administrators are checked too (valid
   names, hashes and roles, **at least one administrator**, no duplicates), and so are the certificates (each certificate with the key
   that belongs to it).
3. **Choose** which sections to restore. Settings, administrators and certificates are on by default when they are valid; **history
   is off** by default (it replaces the history here). The administrators are their own checkbox: a backup that is old holds the
   passwords of that day.
4. **Restore.** Everything chosen is checked again first; nothing is changed if any of it is wrong. The router then **keeps a copy of
   what it has** (section 5), and applies the sections in an order that can be undone: certificates, then administrators, then the
   settings last, as the draft. If a step fails the earlier ones are put back from the copy. Restoring the settings replaces the draft
   (the page says when there are changes waiting in it); committing it is the usual review, commit and confirm, so a restored
   configuration that locks you out reverts by itself.
5. A restore is refused while a change is being applied or waits for its confirmation.

### A different router

A backup from a router with other ports (`eth0` and `eth1` against `enp1s0` and `enp2s0`) would make no sense as it is. The page then says
which ports the backup uses that this router does not have, and **keeps this router's own port assignments** by default: which port is the
internet connection, which are LAN ports and which networks each carries (references to networks the backup lacks are dropped), the
list of ports, and the Wi-Fi adapters (those this router does not have are left out). Everything else comes from the backup: networks,
addresses, firewall, DNS, VPN keys, certificates, passwords. Assign more ports afterwards on the Ports page. The checkbox can be turned
off, and the page shows again what would change.

### Certificates on another router

The certificate in the backup was made for the router it came from. The web authority comes across, so that the devices that trust it
still do, and the router then **makes its web certificate again** for its own names (the usual check, docs/certificates.md). A custom
certificate that was uploaded stays custom.

### History

The databases are written next to the live ones and swapped in **when the daemon starts** (they are in use now): restart from System,
Settings, Power. Until then the page says so.

## 4. The page

- **Back up:** the passphrase twice (the button stays off until they match and are long enough), *Include the history* (with how big it is
  now), *Download the backup*.
- **Restore from a backup:** the file and the passphrase, then what is in it with a checkbox for each section, the ports option, the
  review lines, the problems, and *Restore the chosen* (it asks to be clicked twice). Afterwards it says what was done and what is left
  to do (commit the settings; sign in again).
- **Copies on this router:** the copies below.

## 5. Copies on this router

Before a restore changes anything the router saves what it has (settings that run, administrators, certificates; **not** the history,
which is too big, so a restored history cannot be undone) in `/etc/peakroute/backups/` (a directory of 0700, files of 0600). The newest
three are kept. They are plain (not encrypted: they hold what is already on this box in the clear) and **cannot be downloaded**.
*Restore this* puts one back the same way a backup is restored (settings as the draft, the rest at once, everyone signed out), and
keeps the state it replaces as a copy in turn, so an undo can be undone. *Delete* removes one.

## 6. API (administrator role for everything but the first)

| Call | Does |
|---|---|
| `GET /api/v1/backup` | the copies, the minimum passphrase length, how big the history is, whether a restore may start now |
| `POST /api/v1/backup/create` | `{passphrase, confirm, history}`: the file |
| `POST /api/v1/backup/open` | multipart `file` and `passphrase`: opens a backup and answers with the report and a token |
| `POST /api/v1/backup/preview` | `{token, keep_ports}`: the report again, with this router's own ports kept or not |
| `POST /api/v1/backup/restore` | `{token, settings, admins, certificates, history, keep_ports}` |
| `POST /api/v1/backup/copies/{id}/restore`, `DELETE /api/v1/backup/copies/{id}` | put a copy back; remove it |

## 7. Still to do

- **Restore on the first-run page** of a new router (no administrator yet): it needs care, because it would be a way in without a password.
- **Automatic copies** after every commit, so that a "go back to last week" is possible.
- **Scheduled backups** to a USB stick or an SFTP server.
- Migration hooks for a schema that changes (none is needed yet).

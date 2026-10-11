# PeakRoute OS administrators

Status (2026-10-11): built. Who may sign in to the web interface, with what role, their passwords, and how long a session may
sit idle. Unlike the network pages, **these changes take effect at once**: they are not part of the draft that is reviewed and
committed (a password change that waited for a commit would be strange, and nothing here can cut the router off from the
network). This file is meant to become the accounts chapter of the user documentation.

## 1. Roles

| Role | Can |
|---|---|
| **Administrator** (`admin`) | everything: every page, every change, and the accounts on this page |
| **Read-only** (`readonly`) | look at every page; change nothing, except its own password |

- The first account is made by the setup wizard and is an administrator.
- A role in the file that is neither of these (a typo, a hand edit) counts as **read-only**: a damaged file must never give more
  than it says.
- What an account may do is read from the configuration **at every request**: demoting an account takes effect on its very next
  click, with the session it already has; removing it ends its sessions at once.
- A read-only account sees the whole interface (it shows who you are in the sidebar, with a *read-only* tag). The buttons are
  there, and pressing one answers *This account is read-only*: the server decides, not the page.

## 2. The page (System, Administrators)

- **Your account**: who you are and your role, and *Change your password* (current password, new one twice). Your other sessions
  are signed out; this one carries on.
- **Accounts** (administrators only): every account with its role, how many sessions it has open, and per account: a *role*
  menu (Administrator or Read-only), *Set password* (for someone who forgot theirs: they are signed out everywhere), *Sign out*
  (end all its sessions) and *Delete* (click twice). *Add account*: user name, role, password twice.
- **Session timeout** (administrators only): minutes of inactivity before a session ends, 1 to 1440 (24 hours), 15 by default.
  It counts for everyone, and for people already signed in from their next click on.

Rules the page cannot get around (they are enforced by the router):

- There is **always at least one administrator**.
- You **cannot remove, or change the role of, the account you are signed in with**, and you set your own password only with the
  current one (an administrator sets *other* accounts' passwords without it).
- User names are 3 to 32 characters (letters, digits, `.`, `_`, `-`; the first a letter or digit) and unique **whatever the
  capitals**: `Bob` and `bob` are one person.
- Passwords are 8 characters to 72 bytes (bcrypt reads no more), typed twice.
- At most 20 accounts.
- Guessing the *current* password on the change form is limited to 5 tries in 2 minutes per account (a stolen session must not be
  a way to test passwords).

## 3. API

All need a session; changes also the CSRF token. Read-only sessions are refused (403) for everything except their own password.

| Request | What it does |
|---|---|
| `GET /api/v1/admins` | `users` (`username`, `role`, `you`, `sessions`; everyone for an administrator, only yourself for a read-only account; yourself first, then by name), `you`, `role` (yours), `can_manage`, `session_timeout_minutes`, `timeout_min`, `timeout_max`, `timeout_default`, `min_password`, `max_password_bytes`, `max_accounts`. Never a password or a hash |
| `POST /api/v1/admins` | `{"username", "role", "password", "confirm"}` adds an account (201) |
| `PUT /api/v1/admins/{username}` | `{"role"}` |
| `DELETE /api/v1/admins/{username}` | removes the account and ends its sessions |
| `POST /api/v1/admins/{username}/password` | `{"password", "confirm"}` sets another account's password and ends its sessions |
| `DELETE /api/v1/admins/{username}/sessions` | ends the account's sessions (for yourself: all the others) |
| `POST /api/v1/me/password` | `{"current", "password", "confirm"}` your own password; your other sessions end |
| `PUT /api/v1/session/timeout` | `{"minutes"}` |

Mistakes come back as `{"error", "errors": [{"field", "message"}]}` with the field named (`username`, `role`, `password`,
`confirm`, `current`, `minutes`), and the page shows them under the box they belong to.

## 4. Internals

- **Storage:** `auth.json` (mode 0600): `session_timeout_minutes` and `users` (`username`, `password_hash` (bcrypt), `role`). All
  changes go through `Manager.UpdateAuth`, which saves atomically and changes nothing when the save fails.
- **Sessions** live in memory (`auth.SessionManager`), so they end when the daemon restarts. A session is valid only while its
  account exists; `RevokeUser` ends an account's sessions (optionally keeping the one making the request).
- **Where the rules are:** `internal/auth/roles.go` (roles), `session.go` (account must exist, role read at each request),
  `csrf.go` (`RequireAuth`: read-only accounts refused for anything that changes state), `admins.go` (the API and
  `changeAccount`, which refuses to leave the router without an administrator even when two administrators act at once).
- **The log** (the journal) records who did what, never a password: `Account "dave" added by "alice" (readonly)`, password
  changes and how many sessions ended, role changes, removals, the timeout.

## 5. Checks

`internal/auth/admins_test.go` runs the real handlers behind `RequireAuth` with a real configuration in a temporary directory:
every refusal above (with the field it names and the proof that nothing changed), sessions that end and sessions that stay, the
limit on guessing, that no password or hash appears in a reply or a log line, the file mode, and that the accounts survive a
restart. The mutation check broke each rule in turn; every break was caught.

## 6. Not planned (yet)

Two-factor sign-in · single sign-on or RADIUS/LDAP · more than two roles (for example *can change firewall rules but not
accounts*) · a list of past sign-ins · locking an account after failed sign-ins (the sign-in form is already rate limited per
address) · API tokens · e-mail password reset (a router has no mail; an administrator sets the password). If every administrator has forgotten
theirs there is no way back from the web interface; a console recovery tool is one of the longer-term gaps in `TODO.md`.

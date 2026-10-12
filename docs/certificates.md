# PeakRoute OS Certificates

Status (2026-10-11): built. System, Certificates is the one place for every certificate on the router: the one of its own web
interface, how to make computers and phones trust it, and the OpenVPN certificates. This file is a living document: every commit that
changes the page updates it, and it is meant to become the Certificates chapter of the user documentation.

---

## 1. The web interface's certificate

The router's web interface is only reachable over HTTPS. It has one of two kinds of certificate:

- **Built-in** (the usual): made by the router itself, for the names and addresses it has, and renewed by itself.
- **Custom**: one you uploaded, for example from a public authority for a name that points at this router. The router never
  replaces or renews it: put a new one in before it runs out. The page warns when it has 30 days or less.

Certificates apply **at once** and are not part of the draft (like an administrator's password): the server picks the certificate on
every new connection, so nothing restarts. A page that is already open keeps the old certificate until it reconnects.

### The built-in certificate

- **Made by a web authority of its own.** The router has a small certificate authority (ECDSA P-256, valid 10 years) for the web
  interface alone, in `/etc/peakroute/certs/ca.crt` and `ca.key` (0600). It signs the router's certificate (ECDSA P-256, valid one year).
  **It is not the VPN's authority**: nothing made for a VPN device is trusted for the web interface, and the other way round.
- **Good for**: `localhost`, the router's host name, the host name with the local domain (`gateway.home.arpa`), the address of every
  Local Network (the router's own address on it), `127.0.0.1` and `::1`, and the **extra names** you add. The internet connection's
  address is not in it (it changes); add it, or a name for it, as an extra name if you reach the router that way.
- **Renewed by itself.** At start and every twelve hours the router checks the certificate and makes it again when it is missing or
  damaged, **has less than 30 days left**, was not made by the web authority (the self-signed certificate of older versions is
  replaced the first time), or **is not good for a name or address the router now has** (a changed host name, a new network, a name
  you added). The authority does not change when that happens, so a device that trusts it keeps trusting the router. A certificate for
  fewer names than before is not replaced (it is still good).
- **Extra names.** Names and addresses you also use to reach the router (up to 10), such as the name from Dynamic DNS. Saving them makes
  the certificate again at once. The page suggests the names it knows: Dynamic DNS names, the VPN servers' addresses and the internet
  connection's address. When a custom certificate is in use the names are kept for when the built-in one is.
- **Make a new one** makes the certificate now (the same authority). With a custom certificate in use the button reads *Use the
  built-in certificate instead*, asks you to click again, and then replaces it.

### Trusting the router (the authority certificate)

On the page this is a section of the Web interface card, labelled *Authority certificate*, below the *Certificate in use* (the web interface's SSL certificate itself).

A browser knows public authorities, not this router's, so it warns about the built-in certificate. **Download the authority's
certificate** (a public file; the key never leaves the router) and install it once on each computer and phone you use: the page
tells how for Windows, macOS, iPhone and iPad, Android, Chrome on Linux and Firefox, and shows the fingerprint to compare. From then on
the router's addresses and names are trusted. Only install it on devices of your own: a device that trusts an authority trusts every
certificate it makes. This is why it is separate from the VPN's.

### Using your own certificate

Upload the certificate and its private key (PEM files). The router checks them **before anything changes**: the file holds a valid
certificate, the key belongs to it, it has not run out and is already valid, and it is a certificate for a server (not an authority). A
chain may follow the router's certificate in the file (the router's certificate must be first). The certificate file is written
only when the key is, and the pair is swapped in together, so a failure leaves the old pair as it was. The page then lists what the
router has that the certificate is not good for (a browser would complain there).

## 2. The VPN's certificates

OpenVPN needs certificates; WireGuard uses keys. The card shows the OpenVPN authority (valid 20 years), the server certificate (10
years), how many certificates are no longer accepted, and when each device's and each client tunnel's certificate runs out, with a
warning inside 60 days. Devices and tunnels are managed on the VPN page (docs/vpn.md, sections 9 and 10).

**Renew the server certificate** makes the OpenVPN servers a new certificate from the authority. It is the one change on this page that
goes through the **draft**: the servers restart when you commit, which briefly disconnects devices (they reconnect by themselves). Devices
are not touched: their certificates are the authority's, not the server's. Renewing the authority itself (20 years) is not built.

## 3. Where things are

| Path | Content |
|---|---|
| `/etc/peakroute/certs/tls.crt`, `tls.key` | the certificate the web interface serves, and its key (0600) |
| `/etc/peakroute/certs/ca.crt`, `ca.key` | the web authority (the key 0600) |
| `/etc/peakroute/certs/custom` | present while a custom certificate is in use (it is never replaced by itself) |
| `/etc/peakroute/certs/extra-names.json` | the extra names |

In `-dev` mode everything is under `dev-data/certs`. These files, the authority's key above all, belong in a backup.

## 4. API (administrator role for changes)

| Call | Does |
|---|---|
| `GET /api/v1/certificates` | the mode, the certificate (names, issuer, dates, fingerprint), what is missing, the extra names, the authority |
| `PUT /api/v1/certificates/names` | set the extra names (checked); the built-in certificate is made again |
| `POST /api/v1/certificates/regenerate` | make the built-in certificate; `replace_custom: true` is needed when a custom one is in use |
| `POST /api/v1/certificates/upload` | multipart `cert` and `key`: install a custom certificate |
| `GET /api/v1/certificates/authority.crt` | the web authority's certificate (made if there is none) |
| `POST /api/v1/vpn/ca/renew-server` | renew the OpenVPN server certificate (draft) |

## 5. Still to do

- Renewing the web authority and the OpenVPN authority before they run out (10 and 20 years).
- Certificates from Let's Encrypt (ACME), when the router is reachable under a public name.
- Showing in the browser's own terms which certificate a connection used (the fingerprint on the page is what to compare).

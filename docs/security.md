# Security

What protects a PeakRoute router, and the checks that watch over it. The details of each part are in the page that owns it; this is the overview.

## What can reach the router

- **From the internet:** nothing. The firewall's input chain drops by default and accepts only loopback, replies to what the router
  started, and the networks below (docs/firewall.md). Pings from the internet are off until "Answer pings from the internet" is ticked.
- **From the default network:** everything (it is the administrator's network).
- **From any other network (guest, IoT, VLANs):** only DNS and DHCP, ping, and what that network's own settings switch on (mDNS, NTP,
  DNS filtering). Never the web interface or the console. `TestAnotherNetworkCannotReachTheRoutersWebInterfaceOrAnythingButWhatItIsGiven`
  pins this.

## The web interface

HTTPS (TLS 1.2 or newer) with the router's own certificate; session cookies are `Secure`, `HttpOnly` and `SameSite=Strict`; every
change from the browser carries a CSRF token; sign-in is limited to 5 tries per 2 minutes per address and per account. AdGuard Home's own
interface listens only on loopback and is reached through the router's sign-in, which also refuses requests from another origin.

## Updates

Releases are **signed** and routers only install what the maker's key signed (docs/updates.md, section 7b).

## Known vulnerabilities

- **Go:** `govulncheck` runs before every release build and every week (`.github/workflows/vulncheck.yml`). It looks for known
  vulnerabilities in the Go standard library and in the packages used that the code can actually reach. A failing run means: raise the Go
  version in `go.mod` (the builds use it) or the package that is named. First run, 2026-11-27: 14 reachable standard-library
  vulnerabilities in go1.27.1 (mostly the HTTP server), fixed by go1.27.4.
- **AdGuard Home** is downloaded at build time (the newest release, checked against the checksums published beside it, which catches a
  damaged download and not a release GitHub itself serves wrongly) and is a separate Go program that `govulncheck` on our code does not
  look inside. So the release workflow scans the program that went into the image (`.github/scripts/scan-adguard.sh`, govulncheck in
  binary mode) and puts the result in the run's summary. It **only reports**: a vulnerability AdGuard has not fixed yet is not something a
  release can wait for. First run, 2026-11-27: the same advisory our own check shows as unreachable (GO-2026-5932, the unmaintained
  `x/crypto/openpgp` package) is reachable in AdGuard Home, with no released fix. AdGuard Home is not reachable from the internet (loopback,
  behind the router's sign-in, DNS only from the networks), which is why this is a report and not a gate. Pinning a version was considered
  and left out: it would trade the unlikely risk of a tampered release for the likelier one of running an old version.
- **GitHub:** code scanning (CodeQL), Dependabot and secret scanning with push protection are on.
- **Debian:** every image build applies the security updates of the day (`trixie-security` and `trixie-updates`, then `dist-upgrade`); a
  router has them once it updates to a release built after. **A weekly check** (`.github/workflows/debian-security.yml`) tells you when
  one is due: each release lists its packages (`peakroute-os-amd64-generic-<tag>.packages`, "name version" per line), and the check compares
  the newest release's list with what Debian holds now (`.github/scripts/debian-updates.sh`, run in a `debian:trixie` container) and opens
  an issue naming what is behind, security ones first. The issue is updated while something is behind and closed when a release has
  caught up. A release from before the lists has nothing to compare: the next one does. It reports; it does not build a release.
  **The access point is covered the same way:** `buildImageU6Plus.sh` applies the security updates and writes `peakroute-ap-arm64-ubnt-u6plus-<version>.packages`, `release-ap.yml` attaches it, and the weekly check compares the newest `ap-v*` release too (its own issue, "...than ap-v..."; the comparison runs against Debian's amd64 archive, whose versions match arm64's except for arm64-only packages, which are skipped). The access point's releases are checked (`check.sh`), scanned (`govulncheck`) and signed like the router's, and it installs only what that key signed.
- **CPU microcode** (`intel-microcode`, `amd64-microcode`, from `non-free-firmware`) is in the router image: the processor's own fixes
  (Spectre and its kin), loaded from the start of the initramfs.
- Run by hand: `go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...`

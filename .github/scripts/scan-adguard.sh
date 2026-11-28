#!/bin/sh
# Looks for known vulnerabilities in the AdGuard Home program that a build has put in the image (govulncheck can read a compiled Go
# program: it says which of the vulnerabilities in the Go standard library and in the packages the program was built with can be reached).
#
#   scan-adguard.sh BINARY
#
# It REPORTS and never fails: a vulnerability that AdGuard has not fixed yet is not something a release can wait for. The result is printed
# (and, in a workflow, added to its summary) so that it can be seen, and the exit status is 0 whatever it found; 2 means it could not run.
set -u
BIN="${1:?usage: scan-adguard.sh BINARY}"
[ -f "$BIN" ] || { echo "scan-adguard: $BIN is not there" >&2; exit 2; }
# govulncheck is built first and run itself (go run would hide its exit status)
if [ -z "${GOVULNCHECK:-}" ]; then
    tools="$(mktemp -d)"
    GOBIN="$tools" go install golang.org/x/vuln/cmd/govulncheck@v1.10.0 || { echo "scan-adguard: govulncheck could not be built" >&2; exit 2; }
    GOVULNCHECK="$tools/govulncheck"
fi

out="$(mktemp)"
trap 'rm -f "$out"' EXIT
# (exit status 3 is "vulnerabilities found")
# shellcheck disable=SC2086
$GOVULNCHECK -mode=binary "$BIN" > "$out" 2>&1
status=$?
if [ "$status" -ne 0 ] && [ "$status" -ne 3 ]; then
    echo "scan-adguard: govulncheck could not run (exit $status):" >&2
    tail -5 "$out" >&2
    exit 2
fi
version="$("$BIN" --version 2>/dev/null | sed 's/.*version //' || true)"
{
    echo "AdGuard Home ${version:-(version unknown)}:"
    if grep -q '^Vulnerability #' "$out"; then
        echo "KNOWN VULNERABILITIES that the program can reach:"
        grep -E '^Vulnerability #|^    Found in:|^    Fixed in:|^  More info:' "$out" | head -60
    else
        echo "no known vulnerabilities that the program can reach."
    fi
} | tee "${SCAN_OUTPUT:-/dev/null}"
exit 0

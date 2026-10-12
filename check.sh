#!/bin/sh
# The checks every change must pass: formatting, vet, and all tests. Run it before committing; the CI workflow runs the same.
# The tests that need real tools (wg, openvpn, unshare, nft) skip themselves when the machine lacks them.
set -eu
cd "$(dirname "$0")"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "gofmt: these files need formatting:" >&2
	echo "$unformatted" >&2
	exit 1
fi
go vet ./...
go test -count=1 ./...

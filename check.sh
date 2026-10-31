#!/bin/sh
# The checks every change must pass: formatting, vet, and all tests. Run it before committing; the release workflow runs it before it builds, and the checks workflow (run by hand) runs it with the end-to-end tools installed.
# The tests that need real tools (wg, openvpn, unshare, nft) skip themselves when the machine lacks them.
set -eu
cd "$(dirname "$0")"
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
	echo "gofmt: these files need formatting:" >&2
	echo "$unformatted" >&2
	gofmt -d $unformatted >&2 || true
	exit 1
fi
go vet ./...

# PeakRoute AP (cmd/peakroute-ap) has no router in it (docs/access-point.md, "PeakRoute AP"): the router's packages must not be in
# its binary. Some are still named by shared code for their types or small helpers (netapi, config, translator, backup); they are
# listed in AP_KNOWN, so that a new one fails here. When one of them is no longer pulled in, take it off the list.
ROUTER_ONLY="clientnames ddns fwlog ovpn presence wanhealth wg"
AP_KNOWN="clientnames ddns fwlog ovpn wg"
ap_deps=$(go list -deps ./cmd/peakroute-ap | sed -n 's#^peakroute/internal/##p')
for p in $ROUTER_ONLY; do
	echo "$ap_deps" | grep -qx "$p" || continue
	case " $AP_KNOWN " in
	*" $p "*) ;;
	*)
		echo "PeakRoute AP pulls in the router's package internal/$p:" >&2
		go list -deps -f '{{.ImportPath}}: {{join .Imports " "}}' ./cmd/peakroute-ap | grep "peakroute/internal/$p\b" | grep -v "^peakroute/internal/$p:" >&2
		exit 1
		;;
	esac
done
for p in $AP_KNOWN; do
	echo "$ap_deps" | grep -qx "$p" || echo "note: PeakRoute AP no longer pulls in internal/$p: take it off AP_KNOWN in check.sh" >&2
done
# the router must keep building with all of it
go build -o /dev/null . ./cmd/peakroute-ap

go test -count=1 ./...

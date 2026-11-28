#!/bin/sh
# Which packages of a release have a newer version in Debian now? Run inside a debian:trixie container (the weekly workflow does):
#
#   debian-updates.sh PACKAGES-FILE      one line "name version" per package, as the release lists them
#
# Prints a line "name installed -> candidate (security|update)" for each package whose newest version in trixie, trixie-updates or
# trixie-security is newer than the one in the release, security ones first, and nothing when the release is up to date. The exit status is
# 0 whatever the answer (the caller reads the output). APT_CACHE and DPKG can name other programs (the tests do).
set -eu
LIST="${1:?usage: debian-updates.sh PACKAGES-FILE}"
APT_CACHE="${APT_CACHE:-apt-cache}"
DPKG="${DPKG:-dpkg}"

sec="$(mktemp)"; upd="$(mktemp)"
trap 'rm -f "$sec" "$upd"' EXIT

while read -r name version; do
    [ -n "$name" ] && [ -n "$version" ] || continue
    cand="$($APT_CACHE policy "$name" 2>/dev/null | sed -n 's/^  Candidate: //p' | head -1)"
    case "$cand" in ""|"(none)") continue ;; esac
    if $DPKG --compare-versions "$cand" gt "$version"; then
        # where the candidate comes from: the security archive, or the point release's updates
        if $APT_CACHE madison "$name" 2>/dev/null | awk -F'|' -v v="$cand" '{gsub(/ /,"",$2); if ($2==v) print $3}' | grep -q 'security'; then
            echo "$name $version -> $cand (security)" >> "$sec"
        else
            echo "$name $version -> $cand (update)" >> "$upd"
        fi
    fi
done < "$LIST"

cat "$sec" "$upd"

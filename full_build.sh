#!/usr/bin/env bash
# Builds the daemon and then the disk image (peakroute-os.img) and the update bundle (peakroute-os-<version>.update): see
# docs/image.md and docs/updates.md.
#
#   ./full_build.sh                              build both
#   UPDATE_REPO=owner/name ./full_build.sh       the GitHub repository the device looks for updates in (fixed in the build: the administrator cannot change it)
#   UPDATE_NOTES="what changed" ./full_build.sh  the line the Updates page shows for an uploaded bundle
#   VERSION=v0.10.1-dev ./full_build.sh          name the build (default below); bump it for every release

# Package import path where 'var BuildVersion' is declared
PKG="peakroute/internal/config"
VERSION="${VERSION:-v0.10.0-alpha}"
UPDATE_REPO="${UPDATE_REPO:-}"

set -e
cd "$(dirname "$0")"
[ -n "${UPDATE_REPO}" ] || echo "Note: no UPDATE_REPO set: this build cannot look for updates on GitHub (an update file can still be uploaded)." >&2

# 1. The daemon, as a static binary for the image
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-X '${PKG}.BuildVersion=${VERSION}' -X '${PKG}.DefaultUpdateRepo=${UPDATE_REPO}'" -o peakroute

# 2. The disk image and the update bundle (asks for sudo once)
PEAKROUTE_VERSION="${VERSION}" UPDATE_NOTES="${UPDATE_NOTES:-}" ./buildImage.sh

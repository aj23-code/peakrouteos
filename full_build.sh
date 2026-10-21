#!/usr/bin/env bash
# Builds the daemon and then the disk image (peakroute-os.img) and the update bundle (peakroute-os-<version>.update): see
# docs/image.md and docs/updates.md.
#
#   ./full_build.sh                              build both
#   UPDATE_REPO=owner/name ./full_build.sh       the GitHub repository the device looks for updates in (default aj23-code/peakrouteos;
#                                                fixed in the build: the administrator cannot change it)
#   UPDATE_NOTES="what changed" ./full_build.sh  the line the Updates page shows for an uploaded bundle
#   VERSION=v0.10.1 ./full_build.sh              name the build; without it the name comes from git: the tag when this commit is
#                                                tagged and clean (v0.10.1), otherwise the last tag plus the commit (v0.10.1-dev.g1a2b3c4)
#
# The name is the only version there is: the daemon, the Overview and Updates pages, the update bundle and the release file in the
# image all take it from here. To release: git tag v0.10.1 && git push origin v0.10.1 (the workflow builds it).

# Package import path where 'var BuildVersion' is declared
PKG="peakroute/internal/config"
# the name of this build: given, or from git (never a number typed into a file)
git_version() {
    local tag dirty=""
    tag="$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)"
    [ -z "$(git status --porcelain 2>/dev/null)" ] || dirty=".dirty"
    if [ "$(git describe --tags --exact-match 2>/dev/null)" = "${tag}" ] && [ -z "${dirty}" ]; then
        echo "${tag}"
    else
        echo "${tag}-dev.g$(git rev-parse --short HEAD 2>/dev/null || echo unknown)${dirty}"
    fi
}
cd "$(dirname "$0")"
VERSION="${VERSION:-$(git_version)}"
# where this software looks for its updates (GitHub releases); fixed in the build, the administrator cannot change it
UPDATE_REPO="${UPDATE_REPO:-aj23-code/peakrouteos}"

set -e
echo "Building ${VERSION}"
[ -n "${UPDATE_REPO}" ] || echo "Note: no UPDATE_REPO set: this build cannot look for updates on GitHub (an update file can still be uploaded)." >&2

# 1. The daemon, as a static binary for the image
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-X '${PKG}.BuildVersion=${VERSION}' -X '${PKG}.DefaultUpdateRepo=${UPDATE_REPO}'" -o peakroute

# 2. The disk image and the update bundle (asks for sudo once)
PEAKROUTE_VERSION="${VERSION}" UPDATE_NOTES="${UPDATE_NOTES:-}" ./buildImage.sh

#!/usr/bin/env bash
# Builds the daemon and then the disk image (peakroute-os.img): see docs/image.md.

# Package import path where 'var BuildVersion' is declared
PKG="peakroute/internal/config"
VERSION="v0.7.0-alpha"

set -e
cd "$(dirname "$0")"

# 1. The daemon, as a static binary for the image
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-X '${PKG}.BuildVersion=${VERSION}'" -o peakroute

# 2. The disk image (asks for sudo once)
./buildImage.sh

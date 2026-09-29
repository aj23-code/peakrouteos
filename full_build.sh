#!/usr/bin/env bash

# Package import path where 'var BuildVersion' is declared
PKG="peakroute/internal/config"
VERSION="v0.1.0-dev"

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-X '${PKG}.BuildVersion=${VERSION}'" -o peakroute



# 2. Run your ISO build pipeline
sudo ./buildISO.sh

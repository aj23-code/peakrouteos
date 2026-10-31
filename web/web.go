// Package web holds the web interface (pages, scripts, styles), built into every PeakRoute daemon (the router and the access point).
package web

import "embed"

//go:embed templates/* static/*
var Files embed.FS

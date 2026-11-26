package config

import (
	"fmt"
	"strings"
)

// Smart queue management: the router shapes what goes through the internet connection a little below the line's real speed, with the
// cake queue discipline, so that the queue forms in the router (where it can be kept short and fair) and not in the provider's equipment
// (where it grows to hundreds of milliseconds and ruins calls and games while something downloads). docs/sqm.md has the model.

// How the connection adds to each packet on the wire; cake counts it so that the speeds set here are the speeds of the line.
const (
	SQMEthernet = "ethernet" // fibre or an Ethernet hand-off: the usual (and the default)
	SQMDocsis   = "docsis"   // cable
	SQMVDSL     = "vdsl"     // DSL with PPPoE over PTM (VDSL2)
	SQMADSL     = "adsl"     // DSL with PPPoE over ATM
)

// SQMLinks lists the connection kinds, in the order the page offers them.
var SQMLinks = []string{SQMEthernet, SQMDocsis, SQMVDSL, SQMADSL}

const (
	// MaxSQMKbit is the fastest line that can be set: 10 Gbit/s.
	MaxSQMKbit = 10_000_000
	// DefaultSQMLink is used when none is set.
	DefaultSQMLink = SQMEthernet
)

// SQMConfig is the shaping of the internet connection. A speed of 0 leaves that direction alone.
type SQMConfig struct {
	Enabled      bool   `json:"enabled"`
	DownloadKbit int    `json:"download_kbit"`
	UploadKbit   int    `json:"upload_kbit"`
	Link         string `json:"link,omitempty"` // one of SQMLinks; empty: ethernet
	// PerHost shares the line fairly between the devices behind the router (rather than between connections only): one device with
	// many connections cannot take the line from the others.
	PerHost bool `json:"per_host"`
}

func (s SQMConfig) clone() SQMConfig { return s }

// SQMSettings returns the configuration, or the defaults when none was ever saved: off, fair between devices.
func (c NetworkConfig) SQMSettings() SQMConfig {
	if c.SQM == nil {
		return SQMConfig{PerHost: true, Link: DefaultSQMLink}
	}
	s := c.SQM.clone()
	if s.Link == "" {
		s.Link = DefaultSQMLink
	}
	return s
}

// SQMActive reports whether the connection is shaped at all.
func (c NetworkConfig) SQMActive() bool {
	s := c.SQMSettings()
	return s.Enabled && (s.DownloadKbit > 0 || s.UploadKbit > 0)
}

// ValidSQMLink reports whether s is one of the connection kinds.
func ValidSQMLink(s string) bool {
	for _, l := range SQMLinks {
		if s == l {
			return true
		}
	}
	return false
}

// validateSQM checks the shaping; errors are reported on sqm.download_kbit, sqm.upload_kbit and sqm.link.
func (c NetworkConfig) validateSQM(add func(field, format string, args ...any)) {
	if c.SQM == nil {
		return
	}
	s := *c.SQM
	if s.DownloadKbit < 0 || s.DownloadKbit > MaxSQMKbit {
		add("sqm.download_kbit", "must be between 0 and %d (Mbit/s: 0 to %d)", MaxSQMKbit, MaxSQMKbit/1000)
	}
	if s.UploadKbit < 0 || s.UploadKbit > MaxSQMKbit {
		add("sqm.upload_kbit", "must be between 0 and %d (Mbit/s: 0 to %d)", MaxSQMKbit, MaxSQMKbit/1000)
	}
	if s.Link != "" && !ValidSQMLink(s.Link) {
		add("sqm.link", "must be one of %s", strings.Join(SQMLinks, ", "))
	}
	if !s.Enabled {
		return
	}
	if s.DownloadKbit == 0 && s.UploadKbit == 0 {
		add("sqm.download_kbit", "give the download or the upload speed of your line, or turn this off")
	}
	if c.WanDevice() == "" {
		add("sqm.enabled", "there is no internet port to shape yet")
	}
}

// SQMText describes the shaping on one line, for the review screen.
func (s SQMConfig) SQMText() string {
	if !s.Enabled {
		return "off"
	}
	speed := func(k int) string {
		if k == 0 {
			return "not shaped"
		}
		return fmt.Sprintf("%g Mbit/s", float64(k)/1000)
	}
	fair := "fair between connections"
	if s.PerHost {
		fair = "fair between devices"
	}
	link := s.Link
	if link == "" {
		link = DefaultSQMLink
	}
	return fmt.Sprintf("download %s, upload %s, %s, %s", speed(s.DownloadKbit), speed(s.UploadKbit), link, fair)
}

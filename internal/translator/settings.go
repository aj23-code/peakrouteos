package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// ensureSafeSettings is the render-time twin of the validation of the router-wide settings: whatever is written to a file
// or put in a command must be exactly what the validation accepts.
func ensureSafeSettings(cfg config.NetworkConfig) error {
	if cfg.Timezone != "" && !config.IsTimezone(cfg.Timezone) {
		return fmt.Errorf("refusing to render an unknown time zone %q", cfg.Timezone)
	}
	if len(cfg.NTPServers) > config.MaxNTPServers {
		return fmt.Errorf("refusing to render %d time servers", len(cfg.NTPServers))
	}
	for _, s := range cfg.NTPServers {
		if !config.ValidServerHost(s) {
			return fmt.Errorf("refusing to render the time server %q", s)
		}
	}
	return nil
}

// renderTimesyncd writes the time servers file for systemd-timesyncd, which exists only while they are not the defaults
// (chosen servers, or the time switched off, which the action that applies the file recognises by its marker line).
func renderTimesyncd(cfg config.NetworkConfig) (string, bool) {
	if !cfg.NTPDisabled && len(cfg.NTPServers) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString("# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n")
	if cfg.NTPDisabled {
		b.WriteString("# " + ntpOffMarker + ".\n")
	}
	b.WriteString("[Time]\n")
	if len(cfg.NTPServers) > 0 {
		b.WriteString("NTP=" + strings.Join(cfg.NTPServers, " ") + "\n")
	}
	return b.String(), true
}

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

// renderTimesyncd writes the time servers file for systemd-timesyncd. It always exists: the servers are written out even while they
// are the defaults, so what the page shows is what the clock follows (without it timesyncd uses the distribution's own list, which is
// not the one the page names). With the time switched off it holds a marker line that the action applying the file recognises.
func renderTimesyncd(cfg config.NetworkConfig) string {
	var b strings.Builder
	b.WriteString("# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n")
	if cfg.NTPDisabled {
		b.WriteString("# " + ntpOffMarker + ".\n")
	}
	b.WriteString("[Time]\n")
	b.WriteString("NTP=" + strings.Join(cfg.EffectiveNTPServers(), " ") + "\n")
	return b.String()
}

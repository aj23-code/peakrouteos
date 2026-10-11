package config

import (
	"strings"
	"unicode"
)

// The router-wide settings besides the host name, the country and the local domain: the time zone, the time servers (and,
// later, syslog forwarding). docs/settings.md has the model and the reasons.

const (
	MaxNTPServers = 4
)

// DefaultNTPServers are the time servers used while none are chosen: the NTP pool project's, which any client may use.
var DefaultNTPServers = []string{"pool.ntp.org"}

// EffectiveNTPServers is the list the router uses.
func (c NetworkConfig) EffectiveNTPServers() []string {
	if len(c.NTPServers) > 0 {
		return c.NTPServers
	}
	return DefaultNTPServers
}

// ValidNTPServer accepts a usable IPv4 address or a name: lower-case DNS labels whose last label is not all digits (so that
// "999.1.1.1" is a bad address and not a strange name).
func ValidNTPServer(s string) bool {
	if IsIPv4(s) {
		return usableServer(s)
	}
	if !ValidDNSName(s) {
		return false
	}
	last := s[strings.LastIndex(s, ".")+1:]
	return strings.IndexFunc(last, unicode.IsLetter) >= 0
}

// validateSettings checks the time zone and the time servers. Errors are reported on timezone and ntp_servers.
func (c NetworkConfig) validateSettings(add func(field, format string, args ...any)) {
	if c.Timezone != "" && !IsTimezone(c.Timezone) {
		add("timezone", "%q is not a time zone the router knows", c.Timezone)
	}
	if len(c.NTPServers) > MaxNTPServers {
		add("ntp_servers", "at most %d time servers", MaxNTPServers)
	}
	seen := map[string]bool{}
	for _, s := range c.NTPServers {
		switch {
		case !ValidNTPServer(s):
			add("ntp_servers", "%q is not a time server: use a name such as pool.ntp.org or an IPv4 address", s)
		case seen[s]:
			add("ntp_servers", "%q is listed more than once", s)
		}
		seen[s] = true
	}
}

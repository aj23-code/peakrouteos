package config

import (
	"fmt"
	"strings"
	"unicode"
)

// The router-wide settings besides the host name, the country and the local domain: the time zone, the time servers (and,
// later, syslog forwarding). docs/settings.md has the model and the reasons.

const (
	MaxNTPServers     = 4
	MaxSyslogServers  = 3
	DefaultSyslogPort = 514
)

// SyslogServer is a syslog server the logs are sent to.
type SyslogServer struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"` // "udp" or "tcp"
}

// SyslogConfig is what is sent where: the servers, and which logs go to all of them.
type SyslogConfig struct {
	Servers  []SyslogServer `json:"servers,omitempty"`
	System   bool           `json:"system,omitempty"`   // the system journal
	Firewall bool           `json:"firewall,omitempty"` // the packets that rules with logging matched
}

func (s SyslogConfig) clone() SyslogConfig {
	s.Servers = append([]SyslogServer(nil), s.Servers...)
	return s
}

// SyslogSettings returns the syslog forwarding, empty when there is none.
func (c NetworkConfig) SyslogSettings() SyslogConfig {
	if c.Syslog == nil {
		return SyslogConfig{}
	}
	return c.Syslog.clone()
}

// SyslogServerText writes a server as a URL, for the review and the page: udp://logs.example.com:514.
func SyslogServerText(s SyslogServer) string {
	return fmt.Sprintf("%s://%s:%d", s.Protocol, s.Host, s.Port)
}

// SyslogText describes the forwarding on one line.
func (c NetworkConfig) SyslogText() string {
	s := c.SyslogSettings()
	if len(s.Servers) == 0 {
		return valueNone
	}
	var urls []string
	for _, srv := range s.Servers {
		urls = append(urls, SyslogServerText(srv))
	}
	return strings.Join(urls, ", ")
}

// SyslogSendsText says which logs are sent.
func (c NetworkConfig) SyslogSendsText() string {
	s := c.SyslogSettings()
	switch {
	case s.System && s.Firewall:
		return "the system log and the firewall log"
	case s.System:
		return "the system log"
	case s.Firewall:
		return "the firewall log"
	}
	return "nothing"
}

// DefaultNTPServers are the time servers used while none are chosen: the NTP pool project's, which any client may use.
var DefaultNTPServers = []string{"pool.ntp.org"}

// EffectiveNTPServers is the list the router uses.
func (c NetworkConfig) EffectiveNTPServers() []string {
	if len(c.NTPServers) > 0 {
		return c.NTPServers
	}
	return DefaultNTPServers
}

// ValidServerHost accepts a usable IPv4 address or a name (of a time server, a syslog server): lower-case DNS labels whose last
// label is not all digits (so that "999.1.1.1" is a bad address and not a strange name), and not "localhost".
func ValidServerHost(s string) bool {
	if IsIPv4(s) {
		return usableServer(s)
	}
	if !ValidDNSName(s) || s == "localhost" || strings.HasSuffix(s, ".localhost") {
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
		case !ValidServerHost(s):
			add("ntp_servers", "%q is not a time server: use a name such as pool.ntp.org or an IPv4 address", s)
		case seen[s]:
			add("ntp_servers", "%q is listed more than once", s)
		}
		seen[s] = true
	}
}

// validateSyslog checks the syslog forwarding. Errors are reported on syslog.servers[i].host|port|protocol, syslog.servers (the
// list as a whole) and syslog (what is sent).
func (c NetworkConfig) validateSyslog(add func(field, format string, args ...any)) {
	s := c.SyslogSettings()
	if len(s.Servers) > MaxSyslogServers {
		add("syslog.servers", "at most %d syslog servers", MaxSyslogServers)
	}
	seen := map[string]bool{}
	for i, srv := range s.Servers {
		field := func(name string) string { return fmt.Sprintf("syslog.servers[%d].%s", i, name) }
		if !ValidServerHost(srv.Host) {
			add(field("host"), "%q is not a syslog server: use a name such as logs.example.com or an IPv4 address (not this router itself)", srv.Host)
		}
		if srv.Port < 1 || srv.Port > 65535 {
			add(field("port"), "must be a port from 1 to 65535")
		}
		if srv.Protocol != "udp" && srv.Protocol != "tcp" {
			add(field("protocol"), "must be udp or tcp")
		}
		key := SyslogServerText(srv)
		if seen[key] {
			add("syslog.servers", "%s is listed more than once", key)
		}
		seen[key] = true
	}
	if len(s.Servers) > 0 && !s.System && !s.Firewall {
		add("syslog", "choose what to send: the system log, the firewall log or both")
	}
}

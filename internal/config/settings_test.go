package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func settingsNet(f func(*NetworkConfig)) NetworkConfig {
	c := validNet()
	f(&c)
	return c
}

func errorFieldsOf(t *testing.T, c NetworkConfig) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	if err := c.Validate(); err != nil {
		for _, e := range err.(ValidationError) {
			got[e.Field] = true
		}
	}
	return got
}

func TestTimeSettingsThatAreAccepted(t *testing.T) {
	cases := map[string]NetworkConfig{
		"nothing set":      validNet(),
		"a zone":           settingsNet(func(c *NetworkConfig) { c.Timezone = "Europe/Berlin" }),
		"UTC":              settingsNet(func(c *NetworkConfig) { c.Timezone = "UTC" }),
		"a deep zone":      settingsNet(func(c *NetworkConfig) { c.Timezone = "America/Argentina/Buenos_Aires" }),
		"a time server":    settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"time.cloudflare.com"} }),
		"an address":       settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"192.168.1.5"} }),
		"four servers":     settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"a.example", "b.example", "c.example", "d.example"} }),
		"a digit in names": settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"0.pool.ntp.org", "1.de.pool.ntp.org"} }),
		"off, with servers": settingsNet(func(c *NetworkConfig) {
			c.NTPDisabled = true
			c.NTPServers = []string{"pool.ntp.org"}
		}),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTimeSettingsThatAreRefused(t *testing.T) {
	cases := []struct {
		name  string
		c     NetworkConfig
		field string
	}{
		{"made up zone", settingsNet(func(c *NetworkConfig) { c.Timezone = "Mars/Olympus" }), "timezone"},
		{"zone in lower case", settingsNet(func(c *NetworkConfig) { c.Timezone = "europe/berlin" }), "timezone"},
		{"a path", settingsNet(func(c *NetworkConfig) { c.Timezone = "../../etc/passwd" }), "timezone"},
		{"a zone with a newline", settingsNet(func(c *NetworkConfig) { c.Timezone = "UTC\nx" }), "timezone"},
		{"a file that is not a zone", settingsNet(func(c *NetworkConfig) { c.Timezone = "zone.tab" }), "timezone"},
		{"five servers", settingsNet(func(c *NetworkConfig) {
			c.NTPServers = []string{"a.example", "b.example", "c.example", "d.example", "e.example"}
		}), "ntp_servers"},
		{"a server twice", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"a.example", "a.example"} }), "ntp_servers"},
		{"a name with a space", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"a b.example"} }), "ntp_servers"},
		{"a name with upper case", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"Pool.NTP.org"} }), "ntp_servers"},
		{"a bad address", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"999.1.1.1"} }), "ntp_servers"},
		{"this machine", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"127.0.0.1"} }), "ntp_servers"},
		{"nothing", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"0.0.0.0"} }), "ntp_servers"},
		{"an option injected", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"a.example\nFallbackNTP=evil.example"} }), "ntp_servers"},
		{"an empty entry", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{""} }), "ntp_servers"},
		{"a url", settingsNet(func(c *NetworkConfig) { c.NTPServers = []string{"http://a.example"} }), "ntp_servers"},
	}
	for _, tc := range cases {
		if !errorFieldsOf(t, tc.c)[tc.field] {
			t.Errorf("%s: no error on %s", tc.name, tc.field)
		}
	}
}

func TestTimeSettingsAreWrittenOnlyWhenUsed(t *testing.T) {
	plain, _ := json.Marshal(validNet())
	for _, key := range []string{"timezone", "ntp_disabled", "ntp_servers", "send_hostname"} {
		if strings.Contains(string(plain), key) {
			t.Errorf("%s written for a plain configuration: %s", key, plain)
		}
	}
	c := settingsNet(func(c *NetworkConfig) { c.Timezone = "UTC"; c.NTPServers = []string{"a.example"}; c.NTPDisabled = true })
	b, _ := json.Marshal(c)
	for _, want := range []string{`"timezone":"UTC"`, `"ntp_servers":["a.example"]`, `"ntp_disabled":true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
}

func TestTimeReviewLines(t *testing.T) {
	base := validNet()
	line := func(a, b NetworkConfig, field string) *Change {
		for _, c := range DiffNetwork(a, b) {
			if c.Field == field {
				c := c
				return &c
			}
		}
		return nil
	}
	with := func(f func(*NetworkConfig)) NetworkConfig { return settingsNet(f) }
	if c := line(base, with(func(c *NetworkConfig) { c.Timezone = "Europe/Berlin" }), "timezone"); c == nil || c.Old != "not set (the machine keeps its own)" || c.New != "Europe/Berlin" {
		t.Errorf("zone: %+v", c)
	}
	if c := line(base, with(func(c *NetworkConfig) { c.NTPDisabled = true }), "ntp_disabled"); c == nil || c.Old != "on" || c.New != "off" {
		t.Errorf("ntp: %+v", c)
	}
	if c := line(base, with(func(c *NetworkConfig) { c.NTPServers = []string{"a.example", "b.example"} }), "ntp_servers"); c == nil || c.Old != "the default (pool.ntp.org)" || c.New != "a.example, b.example" {
		t.Errorf("servers: %+v", c)
	}
	if got := DiffNetwork(base, base); len(got) != 0 {
		t.Errorf("no change, but %+v", got)
	}
	if got := base.EffectiveNTPServers(); len(got) != 1 || got[0] != "pool.ntp.org" {
		t.Errorf("default: %v", got)
	}
	if got := with(func(c *NetworkConfig) { c.NTPServers = []string{"x.example"} }).EffectiveNTPServers(); len(got) != 1 || got[0] != "x.example" {
		t.Errorf("chosen: %v", got)
	}
}

func TestTheTimeZoneListIsGoodAndSorted(t *testing.T) {
	if !IsTimezone("UTC") || !IsTimezone("Europe/Berlin") || IsTimezone("") || IsTimezone("Europe") || IsTimezone("europe/berlin") {
		t.Fatal("IsTimezone")
	}
	seen := map[string]bool{}
	for i, z := range Timezones {
		if seen[z.Name] {
			t.Fatalf("%s twice", z.Name)
		}
		seen[z.Name] = true
		if i > 1 && Timezones[i-1].Name > z.Name {
			t.Fatalf("not sorted: %s before %s", Timezones[i-1].Name, z.Name)
		}
		if z.Name != "UTC" && !IsCountry(z.Country) {
			t.Errorf("%s belongs to %q, which is not a country on the list", z.Name, z.Country)
		}
	}
	if len(Timezones) < 400 {
		t.Fatalf("only %d zones", len(Timezones))
	}
	if got := TimezonesOf("DE"); len(got) != 2 || got[0] != "Europe/Berlin" || got[1] != "Europe/Busingen" {
		t.Errorf("DE: %v", got)
	}
	if got := TimezonesOf("US"); len(got) < 20 {
		t.Errorf("US: %v", got)
	}
	if TimezonesOf("") != nil || TimezonesOf("ZZ") != nil {
		t.Error("an unknown country has zones")
	}
	// every name is a real entry of the time zone database of the machine the test runs on (when it has one)
	if _, err := os.Stat("/usr/share/zoneinfo/Europe/Berlin"); err != nil {
		t.Skip("no time zone database here")
	}
	for _, z := range Timezones {
		if _, err := os.Stat(filepath.Join("/usr/share/zoneinfo", z.Name)); err != nil {
			t.Errorf("%s is not in the time zone database", z.Name)
		}
	}
}

func logNet(f func(*SyslogConfig)) NetworkConfig {
	c := validNet()
	c.Syslog = &SyslogConfig{Servers: []SyslogServer{{Host: "logs.example.com", Port: 514, Protocol: "udp"}}, System: true, Firewall: true}
	if f != nil {
		f(c.Syslog)
	}
	return c
}

func TestSyslogSettingsThatAreAccepted(t *testing.T) {
	cases := map[string]NetworkConfig{
		"none":                  validNet(),
		"an empty block":        settingsNet(func(c *NetworkConfig) { c.Syslog = &SyslogConfig{} }),
		"one server":            logNet(nil),
		"tcp":                   logNet(func(s *SyslogConfig) { s.Servers[0].Protocol = "tcp"; s.Servers[0].Port = 6514 }),
		"an address":            logNet(func(s *SyslogConfig) { s.Servers[0].Host = "192.168.1.9" }),
		"the system log only":   logNet(func(s *SyslogConfig) { s.Firewall = false }),
		"the firewall log only": logNet(func(s *SyslogConfig) { s.System = false }),
		"three servers": logNet(func(s *SyslogConfig) {
			s.Servers = append(s.Servers, SyslogServer{"a.example", 514, "udp"}, SyslogServer{"a.example", 514, "tcp"})
		}),
		"nothing chosen, no server": settingsNet(func(c *NetworkConfig) { c.Syslog = &SyslogConfig{} }),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSyslogSettingsThatAreRefused(t *testing.T) {
	cases := []struct {
		name  string
		c     NetworkConfig
		field string
	}{
		{"a bad name", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "not a host" }), "syslog.servers[0].host"},
		{"upper case", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "Logs.Example.com" }), "syslog.servers[0].host"},
		{"empty", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "" }), "syslog.servers[0].host"},
		{"this router", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "127.0.0.1" }), "syslog.servers[0].host"},
		{"localhost", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "localhost" }), "syslog.servers[0].host"},
		{"a localhost name", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "logs.localhost" }), "syslog.servers[0].host"},
		{"nothing", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "0.0.0.0" }), "syslog.servers[0].host"},
		{"a url", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "udp://logs.example.com" }), "syslog.servers[0].host"},
		{"a host with a port", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "logs.example.com:514" }), "syslog.servers[0].host"},
		{"a line break", logNet(func(s *SyslogConfig) { s.Servers[0].Host = "a.example\nb" }), "syslog.servers[0].host"},
		{"port 0", logNet(func(s *SyslogConfig) { s.Servers[0].Port = 0 }), "syslog.servers[0].port"},
		{"port 65536", logNet(func(s *SyslogConfig) { s.Servers[0].Port = 65536 }), "syslog.servers[0].port"},
		{"a negative port", logNet(func(s *SyslogConfig) { s.Servers[0].Port = -1 }), "syslog.servers[0].port"},
		{"no protocol", logNet(func(s *SyslogConfig) { s.Servers[0].Protocol = "" }), "syslog.servers[0].protocol"},
		{"tls", logNet(func(s *SyslogConfig) { s.Servers[0].Protocol = "tls" }), "syslog.servers[0].protocol"},
		{"upper case protocol", logNet(func(s *SyslogConfig) { s.Servers[0].Protocol = "UDP" }), "syslog.servers[0].protocol"},
		{"the same server twice", logNet(func(s *SyslogConfig) { s.Servers = append(s.Servers, s.Servers[0]) }), "syslog.servers"},
		{"four servers", logNet(func(s *SyslogConfig) {
			s.Servers = append(s.Servers, SyslogServer{"a.example", 514, "udp"}, SyslogServer{"b.example", 514, "udp"}, SyslogServer{"c.example", 514, "udp"})
		}), "syslog.servers"},
		{"nothing to send", logNet(func(s *SyslogConfig) { s.System, s.Firewall = false, false }), "syslog"},
	}
	for _, tc := range cases {
		if !errorFieldsOf(t, tc.c)[tc.field] {
			t.Errorf("%s: no error on %s (got %v)", tc.name, tc.field, errorFieldsOf(t, tc.c))
		}
	}
}

func TestSyslogSettingsAreCopiedAndWrittenOnlyWhenUsed(t *testing.T) {
	plain, _ := json.Marshal(validNet())
	if strings.Contains(string(plain), "syslog") {
		t.Errorf("syslog written for a plain configuration: %s", plain)
	}
	c := logNet(nil)
	b, _ := json.Marshal(c)
	if !strings.Contains(string(b), `"syslog":{"servers":[{"host":"logs.example.com","port":514,"protocol":"udp"}],"system":true,"firewall":true}`) {
		t.Fatalf("%s", b)
	}
	// a copy shares nothing with the original
	cp := c.clone()
	cp.Syslog.Servers[0].Host = "other.example"
	cp.Syslog.System = false
	if c.Syslog.Servers[0].Host != "logs.example.com" || !c.Syslog.System {
		t.Fatal("the copy shares the syslog settings with the original")
	}
	if got := validNet().SyslogSettings(); len(got.Servers) != 0 || got.System {
		t.Fatalf("%+v", got)
	}
	s := c.SyslogSettings()
	s.Servers[0].Host = "changed"
	if c.Syslog.Servers[0].Host != "logs.example.com" {
		t.Fatal("SyslogSettings shares its servers")
	}
}

func TestSyslogReviewLines(t *testing.T) {
	base := validNet()
	line := func(a, b NetworkConfig, field string) *Change {
		for _, c := range DiffNetwork(a, b) {
			if c.Field == field {
				c := c
				return &c
			}
		}
		return nil
	}
	c := logNet(func(s *SyslogConfig) {
		s.Servers = append(s.Servers, SyslogServer{"10.0.0.9", 6514, "tcp"})
	})
	if l := line(base, c, "syslog_servers"); l == nil || l.Old != "(none)" || l.New != "udp://logs.example.com:514, tcp://10.0.0.9:6514" {
		t.Errorf("servers: %+v", l)
	}
	if l := line(base, c, "syslog_sends"); l == nil || l.Old != "(none)" || l.New != "the system log and the firewall log" {
		t.Errorf("sends: %+v", l)
	}
	for _, tc := range []struct {
		set  func(*SyslogConfig)
		want string
	}{
		{func(s *SyslogConfig) { s.Firewall = false }, "the system log"},
		{func(s *SyslogConfig) { s.System = false }, "the firewall log"},
	} {
		if l := line(logNet(nil), logNet(tc.set), "syslog_sends"); l == nil || l.New != tc.want {
			t.Errorf("%+v", l)
		}
	}
	// switching the servers off again shows as a removal
	if l := line(logNet(nil), base, "syslog_servers"); l == nil || l.New != "(none)" {
		t.Errorf("%+v", l)
	}
	if got := DiffNetwork(logNet(nil), logNet(nil)); len(got) != 0 {
		t.Errorf("no change, but %+v", got)
	}
	if (SyslogServer{"a.example", 1, "tcp"}).Host == "" || SyslogServerText(SyslogServer{"a.example", 1, "tcp"}) != "tcp://a.example:1" {
		t.Error("SyslogServerText")
	}
}

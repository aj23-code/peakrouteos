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

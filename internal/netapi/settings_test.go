package netapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/network"
	"peakroute/internal/syslogfwd"
)

func (e *env) settings() Settings {
	e.t.Helper()
	return decodeInto[Settings](e.t, e.get(e.api.HandleSettings))
}

func TestSettingsStartWithNoCountryAndListTheCountries(t *testing.T) {
	e := standard(t)
	s := e.settings()
	if s.Country != "" || s.Pending || !s.Editable || len(s.Countries) < 200 {
		t.Fatalf("%+v", s)
	}
}

func TestSettingTheCountryStagesIt(t *testing.T) {
	e := standard(t)
	rec := e.do(e.api.HandleSettingsCountry, http.MethodPut, `{"country":"US"}`, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	s := decodeInto[Settings](t, rec)
	if s.Country != "US" || !s.Pending {
		t.Fatalf("%+v", s)
	}
	if e.mgr.NetworkConfig().Country != "" {
		t.Fatal("the active configuration changed")
	}
	if e.draft().Country != "US" || e.settings().Country != "US" {
		t.Fatal("not staged")
	}
	// put back: no draft is left behind
	e.do(e.api.HandleSettingsCountry, http.MethodPut, `{"country":""}`, "application/json")
	if _, staged := e.mgr.StagedNetworkConfig(); staged {
		t.Fatalf("a draft is left behind: %+v", e.draft())
	}
	if e.settings().Pending {
		t.Fatal("pending with nothing staged")
	}
}

func TestACommittedCountryIsNotPending(t *testing.T) {
	e := standard(t)
	e.do(e.api.HandleSettingsCountry, http.MethodPut, `{"country":"DE"}`, "application/json")
	e.commit()
	if s := e.settings(); s.Country != "DE" || s.Pending {
		t.Fatalf("%+v", s)
	}
}

func TestACountryThatIsNotOneIsRefused(t *testing.T) {
	e := standard(t)
	for _, c := range []string{"XX", "us", "USA", "1"} {
		rec := e.do(e.api.HandleSettingsCountry, http.MethodPut, `{"country":"`+c+`"}`, "application/json")
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["country"] {
			t.Errorf("%q: %d %s", c, rec.Code, rec.Body)
		}
	}
	if _, staged := e.mgr.StagedNetworkConfig(); staged {
		t.Fatal("something was staged")
	}
}

func TestTheCountryEditorIsStrictAndRefusedWhileApplying(t *testing.T) {
	e := standard(t)
	if rec := e.do(e.api.HandleSettingsCountry, http.MethodPut, `{"country":"US","colour":"red"}`, "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: %d", rec.Code)
	}
	if rec := e.do(e.api.HandleSettingsCountry, http.MethodPost, `{}`, "application/json"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", rec.Code)
	}
	e.editable = false
	if rec := e.do(e.api.HandleSettingsCountry, http.MethodPut, `{"country":"US"}`, "application/json"); rec.Code != http.StatusConflict {
		t.Fatalf("while applying: %d", rec.Code)
	}
	if e.settings().Editable {
		t.Fatal("editable while applying")
	}
}

func TestTheHostNameIsSetOnTheSettingsPage(t *testing.T) {
	e := standard(t)
	if s := e.settings(); s.Hostname != "" || s.HostnamePending {
		t.Fatalf("%+v", s)
	}
	set := func(body string) *httptest.ResponseRecorder {
		return e.do(e.api.HandleSettingsHostname, http.MethodPut, body, "application/json")
	}
	rec := set(`{"hostname":" my-router "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if s := decodeInto[Settings](t, rec); s.Hostname != "my-router" || !s.HostnamePending {
		t.Fatalf("%+v", s)
	}
	if e.mgr.NetworkConfig().Hostname != "" {
		t.Fatal("the active configuration changed")
	}
	if !e.changed("hostname") {
		t.Fatalf("no review line: %+v", e.staged())
	}
	e.commit()
	if s := e.settings(); s.Hostname != "my-router" || s.HostnamePending {
		t.Fatalf("%+v", s)
	}
	// it belongs to the router, not to one kind of connection: it stays when the connection changes
	e.saveWAN(staticWAN)
	if e.draft().Hostname != "my-router" {
		t.Fatal("the host name went with the connection type")
	}
	// put back: no draft is left behind
	e.saveWAN(`{"mode":"dhcp"}`)
	set(`{"hostname":""}`)
	set(`{"hostname":"my-router"}`)
	if e.changed("hostname") || e.changed("wan_mode") {
		t.Fatalf("putting it back left a draft: %+v", e.staged())
	}
	for _, bad := range []string{"my router", "-router", "router-", strings.Repeat("a", 64), "a_b", "a.b.c", "a\nb"} {
		before := e.stageFileExists()
		rec := set(`{"hostname":` + jsonString(bad) + `}`)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["hostname"] {
			t.Errorf("%q: %d %s", bad, rec.Code, rec.Body)
		}
		if e.stageFileExists() != before {
			t.Errorf("%q changed the draft", bad)
		}
	}
	if rec := e.do(e.api.HandleSettingsHostname, http.MethodPost, `{}`, "application/json"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("method: %d", rec.Code)
	}
	if rec := set(`{"hostname":"x","extra":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	e.editable = false
	if rec := set(`{"hostname":"other"}`); rec.Code != http.StatusConflict {
		t.Errorf("while applying: %d", rec.Code)
	}
}

func TestTheHostNameCanAlsoBeSentToTheProvider(t *testing.T) {
	e := standard(t)
	set := func(body string) (*httptest.ResponseRecorder, Settings) {
		rec := e.do(e.api.HandleSettingsHostname, http.MethodPut, body, "application/json")
		var s Settings
		if rec.Code == http.StatusOK {
			s = decodeInto[Settings](t, rec)
		}
		return rec, s
	}
	// sending needs a name
	if rec, _ := set(`{"send_hostname":true}`); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["send_hostname"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec, _ := set(`{"hostname":"","send_hostname":true}`); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["send_hostname"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.stageFileExists() {
		t.Fatal("a refused change left a draft")
	}
	_, s := set(`{"hostname":"gw","send_hostname":true}`)
	if s.Hostname != "gw" || !s.SendHostname || !s.HostnamePending || !e.changed("send_hostname") || !e.changed("hostname") {
		t.Fatalf("%+v %+v", s, e.staged())
	}
	// a field left out stays
	if _, s := set(`{"hostname":"gw2"}`); s.Hostname != "gw2" || !s.SendHostname {
		t.Fatalf("the sending went with a rename: %+v", s)
	}
	if _, s := set(`{"send_hostname":false}`); s.Hostname != "gw2" || s.SendHostname {
		t.Fatalf("%+v", s)
	}
	// clearing the name stops the sending too, unless the request insists
	set(`{"send_hostname":true}`)
	if _, s := set(`{"hostname":""}`); s.Hostname != "" || s.SendHostname {
		t.Fatalf("%+v", s)
	}
	e.commit()
	if s := e.settings(); s.HostnamePending || s.Hostname != "" || s.SendHostname {
		t.Fatalf("%+v", s)
	}
	// only the sending changed: it is still a pending change
	set(`{"hostname":"gw"}`)
	e.commit()
	if _, s := set(`{"send_hostname":true}`); !s.HostnamePending {
		t.Fatalf("%+v", s)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestTheTimeIsSetOnTheSettingsPage(t *testing.T) {
	e := standard(t)
	s := e.settings()
	if s.Timezone != "" || !s.NTPEnabled || len(s.NTPServers) != 0 || s.NTPServers == nil || len(s.NTPDefault) != 1 || s.NTPDefault[0] != "pool.ntp.org" || s.TimePending || len(s.Timezones) < 400 {
		t.Fatalf("%+v", s)
	}
	set := func(body string) (*httptest.ResponseRecorder, Settings) {
		rec := e.do(e.api.HandleSettingsTime, http.MethodPut, body, "application/json")
		var s Settings
		if rec.Code == http.StatusOK {
			s = decodeInto[Settings](t, rec)
		}
		return rec, s
	}
	rec, s := set(`{"timezone":" Europe/Berlin ","ntp_servers":[" Time.Cloudflare.com ","", "192.168.1.5"]}`)
	if rec.Code != http.StatusOK || s.Timezone != "Europe/Berlin" || strings.Join(s.NTPServers, ",") != "time.cloudflare.com,192.168.1.5" || !s.TimePending || !s.NTPEnabled {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.mgr.NetworkConfig().Timezone != "" {
		t.Fatal("the active configuration changed")
	}
	for _, f := range []string{"timezone", "ntp_servers"} {
		if !e.changed(f) {
			t.Errorf("no review line for %s: %+v", f, e.staged())
		}
	}
	// a field left out stays
	if _, s := set(`{"ntp_enabled":false}`); s.NTPEnabled || s.Timezone != "Europe/Berlin" || len(s.NTPServers) != 2 || !e.changed("ntp_disabled") {
		t.Fatalf("%+v", s)
	}
	if _, s := set(`{"ntp_servers":[]}`); len(s.NTPServers) != 0 || s.NTPServers == nil || s.NTPEnabled {
		t.Fatalf("%+v", s)
	}
	e.commit()
	if s := e.settings(); s.TimePending || s.Timezone != "Europe/Berlin" || s.NTPEnabled {
		t.Fatalf("%+v", s)
	}
	// switching only the time servers on is a pending change of the time settings by itself
	if _, s := set(`{"ntp_enabled":true}`); !s.TimePending {
		t.Fatalf("%+v", s)
	}
	// put back: no draft is left behind
	set(`{"timezone":"","ntp_enabled":true}`)
	set(`{"timezone":"Europe/Berlin","ntp_enabled":false}`)
	if e.changed("timezone") || e.changed("ntp_disabled") || e.changed("ntp_servers") || e.stageFileExists() {
		t.Fatalf("putting it back left a draft: %+v", e.staged())
	}
	for name, body := range map[string]string{
		"a made up zone":     `{"timezone":"Mars/Olympus"}`,
		"a zone in a path":   `{"timezone":"../etc/passwd"}`,
		"a bad server":       `{"ntp_servers":["not a server"]}`,
		"an address":         `{"ntp_servers":["300.1.1.1"]}`,
		"a server twice":     `{"ntp_servers":["a.example","A.example"]}`,
		"five servers":       `{"ntp_servers":["a.example","b.example","c.example","d.example","e.example"]}`,
		"an unknown field":   `{"timezone":"UTC","x":1}`,
		"a number as a zone": `{"timezone":5}`,
	} {
		rec, _ := set(body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec, _ := set(`{"timezone":"Mars/Olympus"}`); !errorFields(t, rec)["timezone"] {
		t.Errorf("%s", rec.Body)
	}
	if rec, _ := set(`{"ntp_servers":["x y"]}`); !errorFields(t, rec)["ntp_servers"] {
		t.Errorf("%s", rec.Body)
	}
	if e.changed("timezone") || e.changed("ntp_servers") {
		t.Fatal("a refused change was staged")
	}
	if rec := e.do(e.api.HandleSettingsTime, http.MethodPost, `{}`, "application/json"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("method: %d", rec.Code)
	}
	e.editable = false
	if rec, _ := set(`{"timezone":"UTC"}`); rec.Code != http.StatusConflict {
		t.Errorf("while applying: %d", rec.Code)
	}
}

func TestTheSettingsSuggestTheZonesOfTheCountry(t *testing.T) {
	e := standard(t)
	if s := e.settings(); len(s.Suggested) != 0 {
		t.Fatalf("%v", s.Suggested)
	}
	e.do(e.api.HandleSettingsCountry, http.MethodPut, `{"country":"DE"}`, "application/json")
	if s := e.settings(); strings.Join(s.Suggested, ",") != "Europe/Berlin,Europe/Busingen" {
		t.Fatalf("%v", s.Suggested)
	}
}

func TestTheSettingsShowTheClockWhenItCanBeRead(t *testing.T) {
	e := standard(t)
	if s := e.settings(); s.Time != nil {
		t.Fatalf("%+v", s.Time)
	}
	e.api.TimeStatus = func(context.Context) (network.TimeStatus, bool) {
		return network.TimeStatus{Timezone: "Europe/Berlin", NTPEnabled: true, Synchronized: true, Server: "162.159.200.1 (time.cloudflare.com)"}, true
	}
	before := time.Now().UnixMilli()
	s := e.settings()
	if s.Time == nil || s.Time.Timezone != "Europe/Berlin" || !s.Time.NTPEnabled || !s.Time.Synchronized || s.Time.Server != "162.159.200.1 (time.cloudflare.com)" ||
		s.Time.Now < before || s.Time.Now > time.Now().UnixMilli() {
		t.Fatalf("%+v", s.Time)
	}
	e.api.TimeStatus = func(context.Context) (network.TimeStatus, bool) { return network.TimeStatus{}, false }
	if s := e.settings(); s.Time != nil {
		t.Fatalf("an unreadable clock reported as known: %+v", s.Time)
	}
}

func TestSyslogForwardingIsSetOnTheSettingsPage(t *testing.T) {
	e := standard(t)
	if s := e.settings(); len(s.Syslog.Servers) != 0 || s.Syslog.Servers == nil || s.Syslog.System || s.SyslogPending || s.SyslogStatus == nil || len(s.SyslogStatus) != 0 {
		t.Fatalf("%+v", s)
	}
	set := func(body string) (*httptest.ResponseRecorder, Settings) {
		rec := e.do(e.api.HandleSettingsSyslog, http.MethodPut, body, "application/json")
		var s Settings
		if rec.Code == http.StatusOK {
			s = decodeInto[Settings](t, rec)
		}
		return rec, s
	}
	// a port and a protocol left out are 514 and udp; the host is lower-cased and trimmed
	rec, s := set(`{"servers":[{"host":" Logs.Example.com "},{"host":"10.0.0.9","port":6514,"protocol":"TCP"}],"system":true,"firewall":false}`)
	if rec.Code != http.StatusOK || len(s.Syslog.Servers) != 2 || s.Syslog.Servers[0] != (config.SyslogServer{Host: "logs.example.com", Port: 514, Protocol: "udp"}) ||
		s.Syslog.Servers[1] != (config.SyslogServer{Host: "10.0.0.9", Port: 6514, Protocol: "tcp"}) || !s.Syslog.System || s.Syslog.Firewall || !s.SyslogPending {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if e.mgr.NetworkConfig().Syslog != nil {
		t.Fatal("the active configuration changed")
	}
	if !e.changed("syslog_servers") || !e.changed("syslog_sends") {
		t.Fatalf("no review lines: %+v", e.staged())
	}
	e.commit()
	if s := e.settings(); s.SyslogPending || len(s.Syslog.Servers) != 2 {
		t.Fatalf("%+v", s)
	}
	// the page is told how sending is going
	e.api.SyslogStatuses = func() []syslogfwd.ServerStatus {
		return []syslogfwd.ServerStatus{{Server: syslogfwd.Server{Host: "logs.example.com", Port: 514, Protocol: "udp"}, Sent: 12, Dropped: 1, LastError: "boom"}}
	}
	if st := e.settings().SyslogStatus; len(st) != 1 || st[0].Host != "logs.example.com" || st[0].Sent != 12 || st[0].Dropped != 1 || st[0].LastError != "boom" {
		t.Fatalf("%+v", st)
	}
	// no servers turns it off, and nothing of it is kept
	if rec, s := set(`{"servers":[],"system":true,"firewall":true}`); rec.Code != http.StatusOK || len(s.Syslog.Servers) != 0 || s.Syslog.System || !s.SyslogPending || e.draft().Syslog != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// putting it back: no draft is left behind
	set(`{"servers":[{"host":"logs.example.com","port":514,"protocol":"udp"},{"host":"10.0.0.9","port":6514,"protocol":"tcp"}],"system":true,"firewall":false}`)
	if e.changed("syslog_servers") || e.changed("syslog_sends") || e.stageFileExists() {
		t.Fatalf("putting it back left a draft: %+v", e.staged())
	}
	for name, body := range map[string]string{
		"a bad host":       `{"servers":[{"host":"not a host"}],"system":true}`,
		"this router":      `{"servers":[{"host":"127.0.0.1"}],"system":true}`,
		"a bad port":       `{"servers":[{"host":"a.example","port":70000}],"system":true}`,
		"a negative port":  `{"servers":[{"host":"a.example","port":-5}],"system":true}`,
		"tls":              `{"servers":[{"host":"a.example","protocol":"tls"}],"system":true}`,
		"the same twice":   `{"servers":[{"host":"a.example"},{"host":"A.example","port":514,"protocol":"udp"}],"system":true}`,
		"four servers":     `{"servers":[{"host":"a.example"},{"host":"b.example"},{"host":"c.example"},{"host":"d.example"}],"system":true}`,
		"nothing to send":  `{"servers":[{"host":"a.example"}]}`,
		"an unknown field": `{"servers":[],"system":true,"x":1}`,
	} {
		rec, _ := set(body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec, _ := set(`{"servers":[{"host":"x y"}],"system":true}`); !errorFields(t, rec)["syslog.servers[0].host"] {
		t.Errorf("%s", rec.Body)
	}
	if rec, _ := set(`{"servers":[{"host":"a.example"}]}`); !errorFields(t, rec)["syslog"] {
		t.Errorf("%s", rec.Body)
	}
	if rec := e.do(e.api.HandleSettingsSyslog, http.MethodPost, `{}`, "application/json"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("method: %d", rec.Code)
	}
	e.editable = false
	if rec, _ := set(`{"servers":[],"system":false}`); rec.Code != http.StatusConflict {
		t.Errorf("while applying: %d", rec.Code)
	}
}

func TestTheSyslogTest(t *testing.T) {
	e := standard(t)
	post := func() (int, []SyslogTestResult) {
		rec := e.do(e.api.HandleSyslogTest, http.MethodPost, ``, "")
		var out struct {
			Results []SyslogTestResult `json:"results"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.Results
	}
	if code, res := post(); code != http.StatusOK || res == nil || len(res) != 0 {
		t.Fatalf("%d %v", code, res)
	}
	e.api.SyslogTest = func(context.Context) []syslogfwd.TestResult {
		return []syslogfwd.TestResult{
			{Server: syslogfwd.Server{Host: "a.example", Port: 514, Protocol: "udp"}, OK: true, Message: "sent"},
			{Server: syslogfwd.Server{Host: "b.example", Port: 6514, Protocol: "tcp"}, Message: "could not connect: refused"},
		}
	}
	code, res := post()
	if code != http.StatusOK || len(res) != 2 || !res[0].OK || res[0].Host != "a.example" || res[1].OK || res[1].Port != 6514 || res[1].Protocol != "tcp" || res[1].Message != "could not connect: refused" {
		t.Fatalf("%d %+v", code, res)
	}
	if rec := e.do(e.api.HandleSyslogTest, http.MethodGet, ``, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}

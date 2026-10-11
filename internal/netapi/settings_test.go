package netapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

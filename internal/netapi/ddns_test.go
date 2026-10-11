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
	"peakroute/internal/ddns"
)

func (e *env) ddns() ddnsResponse {
	e.t.Helper()
	return decodeInto[ddnsResponse](e.t, e.do(e.api.HandleDDNS, http.MethodGet, "", ""))
}

func (e *env) addDDNS(body string) *httptest.ResponseRecorder {
	return e.do(e.api.HandleDDNS, http.MethodPost, body, "application/json")
}

func (e *env) ddnsEntry(method, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/x", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	e.api.HandleDDNSEntry(rec, req)
	return rec
}

const dynBody = `{"provider":"dyndns2","hostname":"Home.Example.com","server":"Members.DynDNS.org","username":" me ","password":"s3cret-pw"}`

func TestDynamicDNSStartsEmpty(t *testing.T) {
	e := standard(t)
	d := e.ddns()
	if len(d.Entries) != 0 || d.Entries == nil || !d.Editable || len(d.Servers) != 3 || d.Servers[0].Name != "DynDNS" || d.Servers[0].Host != "members.dyndns.org" || d.WANAddress != "" || d.WANPublic {
		t.Fatalf("%+v", d)
	}
}

func TestAddingANameNeverSendsThePasswordBack(t *testing.T) {
	e := standard(t)
	rec := e.addDDNS(dynBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cret") {
		t.Fatalf("the password came back: %s", rec.Body)
	}
	v := decodeInto[DDNSEntryView](t, rec)
	if v.ID != "d1" || v.Provider != "dyndns2" || v.Hostname != "home.example.com" || v.Server != "members.dyndns.org" || v.Username != "me" || v.Source != "wan" || !v.Enabled || !v.PasswordSet || !v.Pending || v.Status != nil ||
		v.Text != "dyndns2 at members.dyndns.org, address of the internet connection" {
		t.Fatalf("%+v", v)
	}
	if got := e.draft().DDNSSettings().Entries[0].Password; got != "s3cret-pw" {
		t.Fatalf("the draft holds %q", got)
	}
	if e.mgr.NetworkConfig().DDNS != nil {
		t.Fatal("the active configuration changed")
	}
	if strings.Contains(string(mustJSON(e.ddns())), "s3cret") {
		t.Fatal("the list holds the password")
	}
	// the review shows the name and no secret
	for _, c := range e.staged() {
		if strings.Contains(c.Old+c.New+c.Label, "s3cret") {
			t.Fatalf("a password in the review: %+v", c)
		}
	}
	if !e.changed("ddns") {
		t.Fatalf("no review line: %+v", e.staged())
	}
	// a second name gets the next id
	rec = e.addDDNS(`{"provider":"duckdns","hostname":"mine.duckdns.org","password":"1234abcd-12ab","source":"web"}`)
	if v := decodeInto[DDNSEntryView](t, rec); rec.Code != http.StatusCreated || v.ID != "d2" || v.Source != "web" || v.Username != "" || v.Server != "" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestANameIsCheckedWhenAdded(t *testing.T) {
	e := standard(t)
	for name, c := range map[string]struct {
		body  string
		field string
	}{
		"no host name":         {`{"provider":"dyndns2","server":"members.dyndns.org","username":"u","password":"p"}`, "hostname"},
		"no dot":               {`{"provider":"dyndns2","hostname":"home","server":"members.dyndns.org","username":"u","password":"p"}`, "hostname"},
		"no service":           {`{"hostname":"home.example.com","password":"p"}`, "provider"},
		"a made up service":    {`{"provider":"x","hostname":"home.example.com","password":"p"}`, "provider"},
		"no server":            {`{"provider":"dyndns2","hostname":"home.example.com","username":"u","password":"p"}`, "server"},
		"no user":              {`{"provider":"dyndns2","hostname":"home.example.com","server":"members.dyndns.org","password":"p"}`, "username"},
		"no password":          {`{"provider":"dyndns2","hostname":"home.example.com","server":"members.dyndns.org","username":"u"}`, "password"},
		"a bad source":         {`{"provider":"dyndns2","hostname":"home.example.com","server":"members.dyndns.org","username":"u","password":"p","source":"lan"}`, "source"},
		"duckdns elsewhere":    {`{"provider":"duckdns","hostname":"home.example.com","password":"1234abcd-12ab"}`, "hostname"},
		"cloudflare, no token": {`{"provider":"cloudflare","hostname":"home.example.com"}`, "password"},
		"custom, http":         {`{"provider":"custom","hostname":"home.example.com","url":"http://x.example/{ip}"}`, "url"},
		"custom, no {ip}":      {`{"provider":"custom","hostname":"home.example.com","url":"https://x.example/set"}`, "url"},
	} {
		rec := e.addDDNS(c.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[c.field] {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if e.stageFileExists() {
		t.Fatal("a refused name left a draft")
	}
	for name, body := range map[string]string{"an unknown field": `{"provider":"duckdns","hostname":"a.duckdns.org","password":"1234abcd","extra":1}`, "not an object": `[]`} {
		if rec := e.addDDNS(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	// the same name twice
	e.addDDNS(dynBody)
	if rec := e.addDDNS(dynBody); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["hostname"] {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.do(e.api.HandleDDNS, http.MethodPut, `{}`, "application/json"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT on the list: %d", rec.Code)
	}
	e.editable = false
	if rec := e.addDDNS(dynBody); rec.Code != http.StatusConflict {
		t.Errorf("while applying: %d", rec.Code)
	}
}

func TestChangingANameKeepsWhatWasLeftOut(t *testing.T) {
	e := standard(t)
	e.addDDNS(dynBody)
	e.commit()
	// a password left out, or empty, stays
	for _, body := range []string{`{"hostname":"new.example.com"}`, `{"hostname":"new.example.com","password":""}`} {
		rec := e.ddnsEntry(http.MethodPut, "d1", body)
		v := decodeInto[DDNSEntryView](t, rec)
		if rec.Code != http.StatusOK || v.Hostname != "new.example.com" || !v.PasswordSet || !v.Pending || e.draft().DDNSSettings().Entries[0].Password != "s3cret-pw" || strings.Contains(rec.Body.String(), "s3cret") {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
	}
	if !e.changed("ddns") || e.changed("ddns_password") {
		t.Fatalf("%+v", e.staged())
	}
	// a new password replaces it, and the review says so without saying what it is
	rec := e.ddnsEntry(http.MethodPut, "d1", `{"password":"another-pw"}`)
	if rec.Code != http.StatusOK || e.draft().DDNSSettings().Entries[0].Password != "another-pw" || !e.changed("ddns_password") {
		t.Fatalf("%d %s %+v", rec.Code, rec.Body, e.staged())
	}
	for _, c := range e.staged() {
		if strings.Contains(c.Old+c.New+c.Label, "another-pw") || strings.Contains(c.Old+c.New+c.Label, "s3cret") {
			t.Fatalf("a password in the review: %+v", c)
		}
	}
	// put back: no draft is left behind
	e.ddnsEntry(http.MethodPut, "d1", `{"hostname":"home.example.com","password":"s3cret-pw"}`)
	if e.stageFileExists() {
		t.Fatalf("putting it back left a draft: %+v", e.staged())
	}
	// switching off, and a field that is wrong
	if v := decodeInto[DDNSEntryView](t, e.ddnsEntry(http.MethodPut, "d1", `{"enabled":false}`)); v.Enabled {
		t.Fatalf("%+v", v)
	}
	if rec := e.ddnsEntry(http.MethodPut, "d1", `{"hostname":"nodots"}`); rec.Code != http.StatusBadRequest || !errorFields(t, rec)["hostname"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.ddnsEntry(http.MethodPut, "d9", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.ddnsEntry(http.MethodPost, "d1", `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.ddnsEntry(http.MethodPut, "d1", `{"hostname":"a.example.com","bogus":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
}

func TestChangingTheServiceDropsWhatBelongsToTheOldOne(t *testing.T) {
	e := standard(t)
	e.addDDNS(dynBody)
	// dyndns2 to duckdns in one step: no server, no user name, the new token
	rec := e.ddnsEntry(http.MethodPut, "d1", `{"provider":"duckdns","hostname":"mine.duckdns.org","password":"1234abcd-12ab"}`)
	got := e.draft().DDNSSettings().Entries[0]
	if rec.Code != http.StatusOK || got.Provider != "duckdns" || got.Server != "" || got.Username != "" || got.URL != "" || got.Password != "1234abcd-12ab" {
		t.Fatalf("%d %s %+v", rec.Code, rec.Body, got)
	}
	// ...to a custom address
	rec = e.ddnsEntry(http.MethodPut, "d1", `{"provider":"custom","hostname":"home.example.com","url":"https://ddns.example.net/set?h={hostname}&ip={ip}&p={password}"}`)
	got = e.draft().DDNSSettings().Entries[0]
	if rec.Code != http.StatusOK || got.Provider != "custom" || got.URL == "" || got.Server != "" {
		t.Fatalf("%d %s %+v", rec.Code, rec.Body, got)
	}
	// ...and back to dyndns2: the address is dropped
	rec = e.ddnsEntry(http.MethodPut, "d1", `{"provider":"dyndns2","server":"members.dyndns.org","username":"u"}`)
	got = e.draft().DDNSSettings().Entries[0]
	if rec.Code != http.StatusOK || got.URL != "" || got.Server != "members.dyndns.org" {
		t.Fatalf("%d %s %+v", rec.Code, rec.Body, got)
	}
}

func TestDeletingAName(t *testing.T) {
	e := standard(t)
	e.addDDNS(dynBody)
	e.addDDNS(`{"provider":"duckdns","hostname":"mine.duckdns.org","password":"1234abcd-12ab"}`)
	e.commit()
	if rec := e.ddnsEntry(http.MethodDelete, "d1", ""); rec.Code != http.StatusOK || len(decodeInto[ddnsResponse](t, rec).Entries) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := e.draft().DDNSSettings().Entries; len(got) != 1 || got[0].ID != "d2" {
		t.Fatalf("%+v", got)
	}
	if rec := e.ddnsEntry(http.MethodDelete, "d1", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
	// the last one goes: nothing of the section is kept
	e.ddnsEntry(http.MethodDelete, "d2", "")
	if e.draft().DDNS != nil {
		t.Fatalf("%+v", e.draft().DDNS)
	}
	e.addDDNS(dynBody)
	e.editable = false
	if rec := e.ddnsEntry(http.MethodDelete, "d1", ""); rec.Code != http.StatusConflict {
		t.Fatalf("while applying: %d", rec.Code)
	}
}

func TestTheListShowsHowEachNameIsGoing(t *testing.T) {
	e := standard(t)
	e.addDDNS(dynBody)
	e.addDDNS(`{"provider":"duckdns","hostname":"mine.duckdns.org","password":"1234abcd-12ab"}`)
	e.addDDNS(`{"provider":"duckdns","hostname":"off.duckdns.org","password":"1234abcd-12ab","enabled":false}`)
	e.commit()
	last := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	e.api.DDNSStatuses = func() []ddns.Status {
		return []ddns.Status{
			{ID: "d1", Hostname: "home.example.com", Address: "203.0.113.9", OK: true, Message: "Updated to 203.0.113.9", LastTry: last, LastSuccess: last},
			{ID: "d2", Hostname: "mine.duckdns.org", Attention: true, Message: "DuckDNS refused it", LastTry: last, NextTry: last.Add(time.Hour)},
			{ID: "d3", Hostname: "off.duckdns.org", Message: "should not show"},
		}
	}
	d := e.ddns()
	s1, s2, s3 := d.Entries[0].Status, d.Entries[1].Status, d.Entries[2].Status
	if s1 == nil || !s1.OK || s1.Address != "203.0.113.9" || s1.LastSuccess != last.UnixMilli() || s1.NextTry != 0 || s1.Message != "Updated to 203.0.113.9" {
		t.Fatalf("%+v", s1)
	}
	if s2 == nil || s2.OK || !s2.Attention || s2.NextTry != last.Add(time.Hour).UnixMilli() || s2.LastSuccess != 0 {
		t.Fatalf("%+v", s2)
	}
	if s3 != nil {
		t.Fatalf("a switched-off name has a status: %+v", s3)
	}
	// a name changed in the draft is not running as it is shown: no status, pending
	e.ddnsEntry(http.MethodPut, "d1", `{"username":"other"}`)
	if v := e.ddns().Entries[0]; v.Status != nil || !v.Pending {
		t.Fatalf("%+v", v)
	}
	if v := e.ddns().Entries[1]; v.Status == nil || v.Pending {
		t.Fatalf("%+v", v)
	}
}

func TestTheListSaysWhetherTheConnectionsAddressCanBeReached(t *testing.T) {
	e := standard(t)
	for addr, want := range map[string]bool{"203.0.113.9/24": true, "192.168.0.5/24": false, "100.64.3.3/10": false, "10.0.0.2/8": false} {
		e.api.Live = func(config.NetworkConfig) *Live { return &Live{Up: true, Address: addr} }
		d := e.ddns()
		if d.WANAddress != strings.Split(addr, "/")[0] || d.WANPublic != want {
			t.Errorf("%s: %+v", addr, d)
		}
	}
	e.api.Live = func(config.NetworkConfig) *Live { return &Live{Up: false} }
	if d := e.ddns(); d.WANAddress != "" || d.WANPublic {
		t.Errorf("%+v", d)
	}
	e.api.Live = func(config.NetworkConfig) *Live { return nil }
	if d := e.ddns(); d.WANAddress != "" {
		t.Errorf("%+v", d)
	}
}

func TestUpdateNow(t *testing.T) {
	e := standard(t)
	post := func(id string) (int, DDNSStatus, string) {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		e.api.HandleDDNSUpdate(rec, req)
		var st DDNSStatus
		json.Unmarshal(rec.Body.Bytes(), &st)
		return rec.Code, st, rec.Body.String()
	}
	if code, _, _ := post("d1"); code != http.StatusServiceUnavailable {
		t.Fatalf("not running: %d", code)
	}
	last := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	var asked []string
	e.api.DDNSUpdate = func(ctx context.Context, id string) (ddns.Status, bool) {
		asked = append(asked, id)
		if id != "d1" {
			return ddns.Status{}, false
		}
		return ddns.Status{ID: "d1", Address: "203.0.113.9", OK: true, Message: "Updated to 203.0.113.9", LastTry: last, LastSuccess: last}, true
	}
	code, st, body := post("d1")
	if code != http.StatusOK || !st.OK || st.Address != "203.0.113.9" || st.LastSuccess != last.UnixMilli() || strings.Join(asked, ",") != "d1" {
		t.Fatalf("%d %s", code, body)
	}
	if code, _, body := post("d9"); code != http.StatusNotFound || !strings.Contains(body, "not running yet") {
		t.Fatalf("%d %s", code, body)
	}
	if rec := e.do(e.api.HandleDDNSUpdate, http.MethodGet, "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
}

func TestAPasswordIsOnlyReportedSetWhenThereIsOne(t *testing.T) {
	e := standard(t)
	// a custom address that needs no secret has none
	rec := e.addDDNS(`{"provider":"custom","hostname":"home.example.com","url":"https://ddns.example.net/set/{hostname}/{ip}"}`)
	if v := decodeInto[DDNSEntryView](t, rec); rec.Code != http.StatusCreated || v.PasswordSet || v.URL != "https://ddns.example.net/set/{hostname}/{ip}" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !decodeInto[DDNSEntryView](t, e.addDDNS(strings.Replace(dynBody, "Home.Example.com", "Other.Example.com", 1))).PasswordSet {
		t.Fatal("a saved password is not reported")
	}
	d := e.ddns()
	if d.Entries[0].PasswordSet || !d.Entries[1].PasswordSet {
		t.Fatalf("%+v", d.Entries)
	}
}

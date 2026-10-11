package netapi

import (
	"net/http"
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

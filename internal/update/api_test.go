package update

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func apiLab(t *testing.T) (*lab, *http.ServeMux) {
	l := newLab(t, SlotA, false, SlotA)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/updates", l.s.HandleState)
	mux.HandleFunc("/api/v1/updates/check", l.s.HandleCheck)
	mux.HandleFunc("/api/v1/updates/install", l.s.HandleInstall)
	mux.HandleFunc("/api/v1/updates/upload", l.s.HandleUpload)
	mux.HandleFunc("/api/v1/updates/restart", l.s.HandleRestart)
	mux.HandleFunc("/api/v1/updates/keep", l.s.HandleKeep)
	mux.HandleFunc("/api/v1/updates/revert", l.s.HandleRevert)
	mux.HandleFunc("/api/v1/updates/settings", l.s.HandleSettings)
	return l, mux
}

func do(mux *http.ServeMux, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestTheStateIsServedAsJSON(t *testing.T) {
	_, mux := apiLab(t)
	rec := do(mux, http.MethodGet, "/api/v1/updates", nil, "")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var st StateView
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Supported || st.Running.Version != "v0.9.5-alpha" || st.Running.Slot != SlotA || st.OtherSlot != SlotB || st.Running.Layout != 1 || st.History == nil {
		t.Fatalf("state: %+v", st)
	}
	// the page needs these names
	for _, k := range []string{`"supported"`, `"running"`, `"on_trial"`, `"other_slot"`, `"settings"`, `"history":[]`, `"editable"`, `"persistent"`} {
		if !strings.Contains(rec.Body.String(), k) {
			t.Errorf("no %s in %s", k, rec.Body.String())
		}
	}
}

func TestTheWholeUpdateThroughTheAPI(t *testing.T) {
	l, mux := apiLab(t)
	rootfs := fakeRootfs(1 << 20)
	rec := do(mux, http.MethodPost, "/api/v1/updates/upload", bundleOf(t, "v0.10.0", rootfs, nil), "application/octet-stream")
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var st StateView
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st.Staged == nil || st.Staged.Version != "v0.10.0" {
		t.Fatalf("staged: %+v", st.Staged)
	}
	if rec = do(mux, http.MethodPost, "/api/v1/updates/restart", nil, ""); rec.Code != 200 {
		t.Fatalf("restart: %d %s", rec.Code, rec.Body)
	}
	l.waitReboot()
	if l.env("trial") != "b" {
		t.Fatal("no trial")
	}
	// nothing to keep from here: this run is still the old slot
	if rec = do(mux, http.MethodPost, "/api/v1/updates/keep", nil, ""); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("keep: %d %s", rec.Code, rec.Body)
	}
}

func TestTheAPIRefusesWhatItShould(t *testing.T) {
	l, mux := apiLab(t)
	// wrong methods
	for path, method := range map[string]string{"/api/v1/updates": http.MethodPost, "/api/v1/updates/upload": http.MethodGet, "/api/v1/updates/restart": http.MethodGet,
		"/api/v1/updates/settings": http.MethodPost} {
		if rec := do(mux, method, path, nil, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: %d", method, path, rec.Code)
		}
	}
	// a file that is not an update: refused, and the slot is not touched
	if rec := do(mux, http.MethodPost, "/api/v1/updates/upload", []byte("not an update"), "application/octet-stream"); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("upload of rubbish: %d %s", rec.Code, rec.Body)
	}
	if got := l.slot(SlotB); got[0] != 0xAA {
		t.Error("the slot was touched")
	}
	// a change waiting to be confirmed
	l.editable = false
	if rec := do(mux, http.MethodPost, "/api/v1/updates/upload", bundleOf(t, "v0.10.0", fakeRootfs(1<<20), nil), "application/octet-stream"); rec.Code != http.StatusConflict {
		t.Errorf("upload while a change waits: %d", rec.Code)
	}
	// install without a check first, and with a body that is not JSON
	if rec := do(mux, http.MethodPost, "/api/v1/updates/install", []byte(`{"version":"v1.0.0"}`), "application/json"); rec.Code != http.StatusConflict {
		t.Errorf("install: %d %s", rec.Code, rec.Body)
	}
	if rec := do(mux, http.MethodPost, "/api/v1/updates/install", []byte(`{"version":"v1.0.0","x":1}`), "application/json"); rec.Code != http.StatusBadRequest {
		t.Errorf("install with an unknown field: %d", rec.Code)
	}
	// settings
	if rec := do(mux, http.MethodPut, "/api/v1/updates/settings", []byte(`{"repo":"me/os","prereleases":false,"auto_check":true}`), "application/json"); rec.Code != http.StatusBadRequest {
		t.Errorf("a source chosen by the user: %d", rec.Code)
	}
	rec := do(mux, http.MethodPut, "/api/v1/updates/settings", []byte(`{"prereleases":true,"auto_check":false}`), "application/json")
	var st StateView
	json.Unmarshal(rec.Body.Bytes(), &st)
	if rec.Code != 200 || !st.Settings.Prereleases || st.Settings.AutoCheck {
		t.Errorf("settings: %d %+v", rec.Code, st.Settings)
	}
}

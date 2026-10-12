package backup

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func upload(h http.HandlerFunc, file []byte, passphrase string, omitFile bool) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if !omitFile {
		f, _ := w.CreateFormFile("file", "b.prbackup")
		f.Write(file)
	}
	w.WriteField("passphrase", passphrase)
	w.Close()
	r := httptest.NewRequest(http.MethodPost, "/x", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return e.Error
}

func TestTheBackupAPI(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	a.use()
	// making one: the file comes back, named for the router and the day
	rec := post(a.svc.HandleCreate, `{"passphrase":"`+pw+`","confirm":"`+pw+`","history":true}`)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/octet-stream" || rec.Header().Get("Cache-Control") != "no-store" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="peakroute-alpha-2026-10-11.prbackup"` {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	file := rec.Body.Bytes()
	if bytes.Contains(file, []byte("alpha")) || !bytes.HasPrefix(file, []byte(magic)) {
		t.Fatal("the file is not sealed")
	}
	for name, body := range map[string]string{
		"the passphrases differ": `{"passphrase":"` + pw + `","confirm":"another passphrase"}`,
		"too short":              `{"passphrase":"short","confirm":"short"}`,
		"none":                   `{}`,
		"an unknown field":       `{"passphrase":"` + pw + `","confirm":"` + pw + `","extra":1}`,
		"not an object":          `[]`,
	} {
		if rec := post(a.svc.HandleCreate, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if e := errorOf(t, post(a.svc.HandleCreate, `{"passphrase":"short","confirm":"short"}`)); !strings.Contains(e, "at least 10 characters") || !strings.HasPrefix(e, "The ") {
		t.Fatalf("%q", e)
	}
	if rec := httptest.NewRecorder(); true {
		a.svc.HandleCreate(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET: %d", rec.Code)
		}
	}

	// opening it on another router
	b := newRouter(t, "beta", []string{"eth0", "eth1"}, nil)
	b.use()
	rec = upload(b.svc.HandleOpen, file, pw, false)
	var rep Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil || rec.Code != 200 || rep.Token == "" || rep.Hostname != "alpha" || rep.Settings == nil || len(rep.History) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := upload(b.svc.HandleOpen, file, "wrong passphrase!", false); rec.Code != 400 || !strings.Contains(errorOf(t, rec), "passphrase is wrong") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := upload(b.svc.HandleOpen, []byte("this is not a backup, it is far too plain"), pw, false); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := upload(b.svc.HandleOpen, nil, pw, true); rec.Code != 400 || !strings.Contains(errorOf(t, rec), "Choose the backup file") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := post(b.svc.HandleOpen, `{}`); rec.Code != 400 {
		t.Fatalf("not a form: %d", rec.Code)
	}
	// the preview
	if rec := post(b.svc.HandlePreview, `{"token":"`+rep.Token+`","keep_ports":true}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"keep_ports":true`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"token":"nope"}`, `{"token":"` + rep.Token + `","x":1}`} {
		if rec := post(b.svc.HandlePreview, body); rec.Code != 404 && rec.Code != 400 {
			t.Errorf("%s: %d", body, rec.Code)
		}
	}
	// the restore
	if rec := post(b.svc.HandleRestore, `{"token":"nope","settings":true}`); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := post(b.svc.HandleRestore, `{"token":"`+rep.Token+`"}`); rec.Code != 409 || !strings.Contains(errorOf(t, rec), "Choose what to restore") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = post(b.svc.HandleRestore, `{"token":"`+rep.Token+`","settings":true,"admins":true,"certificates":true,"history":true}`)
	var res Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || rec.Code != 200 || !res.SignedOut || !res.NeedsRestart || res.Copy == "" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// the page's data, the copy, and undoing
	rec = httptest.NewRecorder()
	b.svc.HandleInfo(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	var info infoResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil || len(info.Copies) != 1 || info.Copies[0].ID != res.Copy || info.MinPassphrase != MinPassphrase || !info.Editable || info.HistoryBytes == 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	one := func(h http.HandlerFunc, method, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/x", nil)
		r.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec
	}
	if rec := one(b.svc.HandleCopyRestore, http.MethodPost, res.Copy); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := one(b.svc.HandleCopyRestore, http.MethodPost, "123"); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := one(b.svc.HandleCopyRestore, http.MethodGet, res.Copy); rec.Code != 405 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := one(b.svc.HandleCopy, http.MethodDelete, res.Copy); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := one(b.svc.HandleCopy, http.MethodDelete, res.Copy); rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := one(b.svc.HandleCopy, http.MethodPost, res.Copy); rec.Code != 405 {
		t.Fatalf("%d", rec.Code)
	}
	// only one key derivation at a time
	busy <- struct{}{}
	if rec := post(a.svc.HandleCreate, `{"passphrase":"`+pw+`","confirm":"`+pw+`"}`); rec.Code != http.StatusTooManyRequests {
		t.Errorf("%d", rec.Code)
	}
	if rec := upload(b.svc.HandleOpen, file, pw, false); rec.Code != http.StatusTooManyRequests {
		t.Errorf("%d", rec.Code)
	}
	<-busy
	if rec := post(a.svc.HandleCreate, `{"passphrase":"`+pw+`","confirm":"`+pw+`"}`); rec.Code != 200 {
		t.Errorf("after the turn was given back: %d", rec.Code)
	}
}

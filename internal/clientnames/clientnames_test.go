package clientnames

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestANicknameIsKeptUnderTheNormalisedHardwareAddress(t *testing.T) {
	s := New("")
	if err := s.Set("F8-94-C2-A4-13-A7", "  Dad's laptop "); err != nil {
		t.Fatal(err)
	}
	for _, mac := range []string{"f8:94:c2:a4:13:a7", "F8:94:C2:A4:13:A7", "f8-94-c2-a4-13-a7"} {
		if got := s.Get(mac); got != "Dad's laptop" {
			t.Errorf("%s: %q", mac, got)
		}
	}
	if s.Get("48:b0:2d:f1:fe:ca") != "" {
		t.Error("a nickname from nowhere")
	}
	// an empty name takes it away
	if err := s.Set("f8:94:c2:a4:13:a7", "  "); err != nil || s.Get("f8:94:c2:a4:13:a7") != "" {
		t.Errorf("not removed: %v %q", err, s.Get("f8:94:c2:a4:13:a7"))
	}
	var nilStore *Store
	if nilStore.Get("f8:94:c2:a4:13:a7") != "" {
		t.Error("a nil store has a name")
	}
}

func TestNicknamesAreCheckedAndKeptAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client_names.json")
	a := New(path)
	for name, c := range map[string]struct{ mac, nick string }{
		"not a MAC":       {"nope", "x"},
		"a group address": {"01:00:5e:00:00:01", "x"},
		"too long":        {"f8:94:c2:a4:13:a7", strings.Repeat("a", 41)},
		"a line break":    {"f8:94:c2:a4:13:a7", "a\nb"},
		"a control char":  {"f8:94:c2:a4:13:a7", "a\x00b"},
	} {
		if err := a.Set(c.mac, c.nick); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a refused nickname wrote the file")
	}
	// 40 characters of any script are fine
	long := strings.Repeat("é", 40)
	if err := a.Set("f8:94:c2:a4:13:a7", long); err != nil {
		t.Fatal(err)
	}
	a.Set("48:b0:2d:f1:fe:ca", "TV")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("%v %v", info, err)
	}

	b := New(path)
	b.Load()
	if b.Get("f8:94:c2:a4:13:a7") != long || b.Get("48:b0:2d:f1:fe:ca") != "TV" {
		t.Errorf("not kept: %q %q", b.Get("f8:94:c2:a4:13:a7"), b.Get("48:b0:2d:f1:fe:ca"))
	}
}

func TestThereIsALimitOnHowManyDevicesHaveOne(t *testing.T) {
	s := New("")
	for i := 0; i < MaxNames; i++ {
		mac := "02:00:" + hex2(i>>24) + ":" + hex2(i>>16) + ":" + hex2(i>>8) + ":" + hex2(i)
		if err := s.Set(mac, "n"); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	if err := s.Set("02:ff:ff:ff:ff:ff", "one too many"); err == nil {
		t.Error("the limit was not enforced")
	}
	// renaming one that has a nickname is not "one more"
	if err := s.Set("02:00:00:00:00:00", "renamed"); err != nil {
		t.Errorf("renaming at the limit: %v", err)
	}
}

func hex2(n int) string {
	const d = "0123456789abcdef"
	n &= 0xff
	return string([]byte{d[n>>4], d[n&0xf]})
}

func TestTheNameEndpoint(t *testing.T) {
	s := New("")
	call := func(method, mac, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/x", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("mac", mac)
		rec := httptest.NewRecorder()
		s.HandleName(rec, req)
		return rec
	}
	if rec := call(http.MethodPut, "F8-94-C2-A4-13-A7", `{"name":"Kitchen tablet"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Kitchen tablet") || s.Get("f8:94:c2:a4:13:a7") != "Kitchen tablet" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for name, c := range map[string]struct {
		method, mac, body string
		code              int
	}{
		"GET":            {http.MethodGet, "f8:94:c2:a4:13:a7", ``, 405},
		"a bad address":  {http.MethodPut, "nope", `{"name":"x"}`, 400},
		"too long":       {http.MethodPut, "f8:94:c2:a4:13:a7", `{"name":"` + strings.Repeat("a", 50) + `"}`, 400},
		"an unknown key": {http.MethodPut, "f8:94:c2:a4:13:a7", `{"name":"x","admin":true}`, 400},
		"not JSON":       {http.MethodPut, "f8:94:c2:a4:13:a7", `name=x`, 400},
	} {
		if rec := call(c.method, c.mac, c.body); rec.Code != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if s.Get("f8:94:c2:a4:13:a7") != "Kitchen tablet" {
		t.Error("a refused request changed the nickname")
	}
	if rec := call(http.MethodPut, "f8:94:c2:a4:13:a7", `{"name":""}`); rec.Code != 200 || s.Get("f8:94:c2:a4:13:a7") != "" {
		t.Errorf("not removed: %d %s", rec.Code, rec.Body)
	}
}

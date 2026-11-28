package filtering

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestADevicesQuestionsAreReadFromTheQueryLogAndOnlyItsOwn(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		w.Write([]byte(`{"data":[
		 {"client":"192.168.1.5","time":"2026-11-28T10:00:03.5+00:00","reason":"FilteredBlackList","status":"NOERROR","question":{"name":"Ads.Example.com.","type":"A"},"rules":[{"text":"||ads.example.com^"}]},
		 {"client":"192.168.1.50","time":"2026-11-28T10:00:02Z","reason":"NotFilteredNotFound","question":{"name":"other.example","type":"A"}},
		 {"client":"192.168.1.5","time":"2026-11-28T10:00:01Z","reason":"NotFilteredNotFound","status":"NOERROR","cached":true,"question":{"name":"example.org","type":"AAAA"}},
		 {"client":"192.168.1.5","time":"2026-11-28T10:00:00Z","reason":"NotFilteredNotFound","status":"NOERROR","question":{"name":"example.org","type":"A"}}]}`))
	}))
	defer srv.Close()
	q := &QueryLog{Upstream: strings.TrimPrefix(srv.URL, "http://")}
	rep, err := q.ForClient(context.Background(), "192.168.1.5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "search=192.168.1.5") {
		t.Errorf("%s", got)
	}
	if rep.Seen != 3 || rep.Blocked != 1 || len(rep.Queries) != 3 {
		t.Fatalf("%+v", rep)
	}
	if f := rep.Queries[0]; f.Name != "ads.example.com" || !f.Blocked || f.Rule != "||ads.example.com^" || f.Time == 0 {
		t.Errorf("%+v", f)
	}
	if rep.Top[0].Name != "example.org" || rep.Top[0].Count != 2 || len(rep.TopBlocked) != 1 || rep.TopBlocked[0].Name != "ads.example.com" {
		t.Errorf("%+v %+v", rep.Top, rep.TopBlocked)
	}
	// AdGuard Home that is not there, or answers badly, is an error the page can say
	if _, err := (&QueryLog{Upstream: "127.0.0.1:1"}).ForClient(context.Background(), "192.168.1.5"); err == nil {
		t.Error("no error without AdGuard Home")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	if _, err := (&QueryLog{Upstream: strings.TrimPrefix(bad.URL, "http://")}).ForClient(context.Background(), "192.168.1.5"); err == nil {
		t.Error("no error for a bad answer")
	}
}

package filtering

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeAGH keeps clients the way AdGuard Home's API does: by name, with the whole client as it was sent.
type fakeAGH struct {
	mu      sync.Mutex
	clients map[string]map[string]any
	calls   []string
	down    bool
}

func newFake(t *testing.T) (*fakeAGH, *httptest.Server) {
	t.Helper()
	f := &fakeAGH{clients: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/control/clients":
			list := []map[string]any{}
			for _, c := range f.clients {
				list = append(list, c)
			}
			json.NewEncoder(w).Encode(map[string]any{"clients": list})
		case "/control/clients/add":
			f.clients[body["name"].(string)] = body
		case "/control/clients/update":
			delete(f.clients, body["name"].(string))
			data := body["data"].(map[string]any)
			f.clients[data["name"].(string)] = data
		case "/control/clients/delete":
			delete(f.clients, body["name"].(string))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAGH) ids(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.clients[name]
	if !ok {
		return "(none)"
	}
	var out []string
	for _, v := range c["ids"].([]any) {
		out = append(out, v.(string))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func syncFor(srv *httptest.Server, names map[string]string, devices *[]Device) *ClientSync {
	return &ClientSync{
		Upstream:  strings.TrimPrefix(srv.URL, "http://"),
		Names:     func() map[string]string { return names },
		Devices:   func(context.Context) []Device { return *devices },
		StatePath: "",
	}
}

func TestANicknameBecomesANamedClientWithTheDevicesAddresses(t *testing.T) {
	f, srv := newFake(t)
	names := map[string]string{"aa:bb:cc:dd:ee:01": "Alec laptop", "aa:bb:cc:dd:ee:02": "Kitchen speaker", "aa:bb:cc:dd:ee:03": "Not here"}
	devices := []Device{{MAC: "AA:BB:CC:DD:EE:01", IP: "192.168.1.20"}, {MAC: "aa:bb:cc:dd:ee:02", IP: "10.20.0.5"}, {MAC: "aa:bb:cc:dd:ee:99", IP: "192.168.1.99"}}
	s := syncFor(srv, names, &devices)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.ids("Alec laptop"); got != "192.168.1.20,aa:bb:cc:dd:ee:01" {
		t.Errorf("%s", got)
	}
	if got := f.ids("Kitchen speaker"); got != "10.20.0.5,aa:bb:cc:dd:ee:02" {
		t.Errorf("%s", got)
	}
	if f.ids("Not here") != "(none)" || len(f.clients) != 2 {
		t.Errorf("a device with no address, or one with no nickname, got a client: %v", f.clients)
	}
	// nothing changed: nothing is sent
	f.calls = nil
	s.Once(context.Background())
	if len(f.calls) != 1 || f.calls[0] != "GET /control/clients" {
		t.Errorf("%v", f.calls)
	}
}

func TestAnAddressThatChangesIsUpdatedWithoutLosingWhatThePersonSetInAdGuardHome(t *testing.T) {
	f, srv := newFake(t)
	names := map[string]string{"aa:bb:cc:dd:ee:01": "Alec laptop"}
	devices := []Device{{MAC: "aa:bb:cc:dd:ee:01", IP: "192.168.1.20"}}
	s := syncFor(srv, names, &devices)
	s.Once(context.Background())
	f.mu.Lock()
	f.clients["Alec laptop"]["blocked_services"] = []any{"youtube"} // set in AdGuard Home's own interface
	f.mu.Unlock()

	devices[0].IP = "192.168.1.31"
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.ids("Alec laptop"); got != "192.168.1.31,aa:bb:cc:dd:ee:01" {
		t.Errorf("%s", got)
	}
	if bs, _ := f.clients["Alec laptop"]["blocked_services"].([]any); len(bs) != 1 {
		t.Errorf("the person's setting was lost: %v", f.clients["Alec laptop"])
	}
}

func TestTwoDevicesWithOneNicknameShareOneClient(t *testing.T) {
	f, srv := newFake(t)
	names := map[string]string{"aa:bb:cc:dd:ee:01": "Alec phone", "aa:bb:cc:dd:ee:02": "Alec phone"}
	devices := []Device{{MAC: "aa:bb:cc:dd:ee:01", IP: "192.168.1.20"}, {MAC: "aa:bb:cc:dd:ee:02", IP: "192.168.1.21"}}
	syncFor(srv, names, &devices).Once(context.Background())
	if got := f.ids("Alec phone"); got != "192.168.1.20,192.168.1.21,aa:bb:cc:dd:ee:01,aa:bb:cc:dd:ee:02" {
		t.Errorf("%s", got)
	}
}

func TestARemovedOrChangedNicknameTakesItsClientAwayButAPersonsOwnClientIsLeftAlone(t *testing.T) {
	f, srv := newFake(t)
	f.clients["My own"] = map[string]any{"name": "My own", "ids": []any{"192.168.1.7"}}
	f.clients["Alec laptop"] = map[string]any{"name": "Alec laptop", "ids": []any{"192.168.1.9"}} // theirs, with a nickname's name
	names := map[string]string{"aa:bb:cc:dd:ee:01": "Alec laptop", "aa:bb:cc:dd:ee:02": "Kitchen"}
	devices := []Device{{MAC: "aa:bb:cc:dd:ee:01", IP: "192.168.1.20"}, {MAC: "aa:bb:cc:dd:ee:02", IP: "192.168.1.21"}}
	state := filepath.Join(t.TempDir(), "agh_clients.json")
	s := syncFor(srv, names, &devices)
	s.StatePath = state
	s.Once(context.Background())
	if f.ids("Alec laptop") != "192.168.1.9" {
		t.Errorf("a client the person made was changed: %s", f.ids("Alec laptop"))
	}
	if f.ids("Kitchen") == "(none)" {
		t.Fatal("Kitchen was not made")
	}

	// the nickname is renamed: the old client goes, the new one comes; "My own" and "Alec laptop" stay
	delete(names, "aa:bb:cc:dd:ee:02")
	names["aa:bb:cc:dd:ee:02"] = "Kitchen speaker"
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ids("Kitchen") != "(none)" || f.ids("Kitchen speaker") == "(none)" || f.ids("My own") == "(none)" || f.ids("Alec laptop") != "192.168.1.9" {
		t.Errorf("%v", f.clients)
	}
	// what was made here is remembered across a restart, so it can still be taken away
	s2 := syncFor(srv, map[string]string{}, &devices)
	s2.StatePath = state
	if err := s2.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.ids("Kitchen speaker") != "(none)" || f.ids("My own") == "(none)" || f.ids("Alec laptop") != "192.168.1.9" {
		t.Errorf("%v", f.clients)
	}
}

func TestNothingHappensWhileAdGuardHomeIsNotAnswering(t *testing.T) {
	names := map[string]string{"aa:bb:cc:dd:ee:01": "Alec laptop"}
	devices := []Device{{MAC: "aa:bb:cc:dd:ee:01", IP: "192.168.1.20"}}
	s := &ClientSync{Upstream: "127.0.0.1:1", Names: func() map[string]string { return names }, Devices: func(context.Context) []Device { return devices }}
	if err := s.Once(context.Background()); err != nil {
		t.Errorf("a service that is not up is not an error: %v", err)
	}
}

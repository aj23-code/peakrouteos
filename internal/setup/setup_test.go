package setup

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"peakroute/internal/auth"
	"peakroute/internal/backup"
	"peakroute/internal/config"
)

type env struct {
	t        *testing.T
	dir      string
	mgr      *config.Manager
	svc      *Service
	sessions []string
	here     []string
	mu       sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir()}
	mgr, err := config.NewManager(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.mgr = mgr
	e.svc = &Service{Manager: mgr, Restore: &backup.Service{},
		Ports: func() ([]config.SystemInterface, error) {
			return []config.SystemInterface{
				{Name: "enp1s0", MAC: "52:54:00:00:00:01", State: "up"}, {Name: "enp2s0", MAC: "52:54:00:00:00:02", State: "down"},
				{Name: "enp3s0", MAC: "52:54:00:00:00:03", State: "down"}, {Name: "wlp4s0", MAC: "52:54:00:00:00:04", Wireless: true},
			}, nil
		},
		Here:         func(*http.Request) []string { e.mu.Lock(); defer e.mu.Unlock(); return e.here },
		DefaultRoute: func() string { return "enp1s0" },
		NewSession: func(u string, w http.ResponseWriter) string {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.sessions = append(e.sessions, u)
			return "tok"
		},
		Hostname: func() string { return "machine" }}
	return e
}

func good() Form {
	return Form{Hostname: "Home-Router", Country: "US", Timezone: "America/Chicago",
		WanPort: "enp1s0", WanMode: "dhcp",
		LanPorts: []string{"enp2s0", "enp3s0"}, LanIP: "10.5.0.1", LanSubnet: "255.255.255.0", DHCP: true, DHCPStart: "10.5.0.100", DHCPEnd: "10.5.0.200",
		Username: "alec", Password: "correct horse 9", Confirm: "correct horse 9"}
}

type reply struct {
	Status int
	Body   map[string]any
	Errors map[string]string
}

func (e *env) do(h http.HandlerFunc, method string, body any) reply {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r := httptest.NewRequest(method, "/x", rd)
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.168.1.50:4000"
	w := httptest.NewRecorder()
	e.svc.Guard(h)(w, r)
	out := reply{Status: w.Code, Errors: map[string]string{}}
	_ = json.Unmarshal(w.Body.Bytes(), &out.Body)
	if list, ok := out.Body["errors"].([]any); ok {
		for _, x := range list {
			m := x.(map[string]any)
			out.Errors[m["field"].(string)] = m["message"].(string)
		}
	}
	return out
}

func (e *env) check(through string, f Form) reply {
	e.t.Helper()
	return e.do(e.svc.HandleCheck, http.MethodPost, struct {
		Through string `json:"through"`
		Form
	}{through, f})
}

func (e *env) apply(f Form) reply { e.t.Helper(); return e.do(e.svc.HandleApply, http.MethodPost, f) }

// nothingSaved says the router is exactly as it was: not set up, no account, no draft.
func (e *env) nothingSaved() {
	e.t.Helper()
	if e.mgr.IsConfigured() || len(e.mgr.Users()) != 0 {
		e.t.Fatalf("configured %v, %d users", e.mgr.IsConfigured(), len(e.mgr.Users()))
	}
	if _, draft := e.mgr.StagedNetworkConfig(); draft {
		e.t.Fatal("a draft was left behind")
	}
	if len(e.sessions) != 0 {
		e.t.Fatal("someone was signed in")
	}
}

func TestInfoListsWiredPortsAndSuggestsAnInternetPort(t *testing.T) {
	e := newEnv(t)
	e.here = []string{"enp2s0"}
	r := e.do(e.svc.HandleInfo, http.MethodGet, nil)
	if r.Status != 200 {
		t.Fatalf("%d", r.Status)
	}
	ports := r.Body["ports"].([]any)
	if len(ports) != 3 {
		t.Fatalf("the Wi-Fi adapter is listed: %v", ports)
	}
	if r.Body["suggest_wan"] != "enp1s0" {
		t.Fatalf("%v", r.Body["suggest_wan"])
	}
	lan := r.Body["suggest_lan"].([]any)
	if len(lan) != 2 || lan[0] != "enp2s0" {
		t.Fatalf("%v", lan)
	}
	if r.Body["hostname"] != "machine" || r.Body["min_password"].(float64) < 8 {
		t.Fatalf("%v", r.Body)
	}
}

func TestSuggestionNeverTakesThePortTheBrowserIsOn(t *testing.T) {
	// the default route leaves by the port the browser is on: it is the one left alone
	wan, lan := suggest([]PortInfo{{Name: "a", Here: true, Internet: true, State: "up"}, {Name: "b", State: "up"}, {Name: "c"}})
	if wan != "b" || len(lan) != 2 || lan[0] != "a" {
		t.Fatalf("%s %v", wan, lan)
	}
	wan, _ = suggest([]PortInfo{{Name: "a", Here: true}, {Name: "b"}, {Name: "c"}})
	if wan != "c" {
		t.Fatalf("%s", wan)
	}
	wan, lan = suggest(nil)
	if wan != "" || len(lan) != 0 {
		t.Fatalf("%s %v", wan, lan)
	}
}

func TestEveryAnswerIsSavedInTheJSONFiles(t *testing.T) {
	e := newEnv(t)
	r := e.apply(good())
	if r.Status != 200 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	if r.Body["redirect"] != "/dashboard" || len(e.sessions) != 1 || e.sessions[0] != "alec" {
		t.Fatalf("%v %v", r.Body, e.sessions)
	}
	// read it all back as a router starting later would
	m, err := config.NewManager(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsConfigured() {
		t.Fatal("the flag is not saved")
	}
	users := m.Users()
	if len(users) != 1 || users[0].Role != "admin" || !auth.Authenticate(users, "alec", "correct horse 9") {
		t.Fatalf("%+v", users)
	}
	cfg, draft := m.StagedNetworkConfig()
	if !draft {
		t.Fatal("the settings are not saved as a draft")
	}
	if cfg.Hostname != "home-router" || cfg.Country != "US" || cfg.Timezone != "America/Chicago" ||
		cfg.WanInterface != "enp1s0" || cfg.WanMode != "dhcp" || len(cfg.LanInterfaces) != 2 || cfg.LanIP != "10.5.0.1" || cfg.LanSubnet != "255.255.255.0" ||
		cfg.DHCP == nil || !cfg.DHCP.Enabled || cfg.DHCP.RangeStart != "10.5.0.100" || cfg.DHCP.RangeEnd != "10.5.0.200" {
		t.Fatalf("%+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOtherWaysToConnectAndTheNetmaskAsAPrefix(t *testing.T) {
	e := newEnv(t)
	f := good()
	f.WanMode, f.WanIP, f.WanSubnet, f.WanGateway, f.WanDNS = "static", "203.0.113.10", "/24", "203.0.113.1", []string{"1.1.1.1", " 9.9.9.9 ", ""}
	f.LanSubnet = "24"
	f.DHCP = false
	if r := e.apply(f); r.Status != 200 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	cfg, _ := e.mgr.StagedNetworkConfig()
	if cfg.WanMode != "static" || cfg.WanSubnet != "255.255.255.0" || cfg.LanSubnet != "255.255.255.0" || len(cfg.WanDNS) != 2 || cfg.DHCP == nil || cfg.DHCP.Enabled {
		t.Fatalf("%+v", cfg)
	}

	e2 := newEnv(t)
	g := good()
	g.WanMode, g.PPPoEUser, g.PPPoEPass = "pppoe", "me@isp", "secret"
	if r := e2.apply(g); r.Status != 200 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	cfg, _ = e2.mgr.StagedNetworkConfig()
	if cfg.WanMode != "pppoe" || cfg.WanPPPoE == nil || cfg.WanPPPoE.Username != "me@isp" || cfg.WanPPPoE.Password != "secret" {
		t.Fatalf("%+v", cfg.WanPPPoE)
	}
	// a fixed address that is not given up leaves nothing behind from another mode
	if cfg.WanIP != "" || len(cfg.WanDNS) != 0 {
		t.Fatalf("%+v", cfg)
	}
}

func TestMaskAcceptsTheWaysPeopleWriteOne(t *testing.T) {
	for in, want := range map[string]string{"255.255.255.0": "255.255.255.0", "/24": "255.255.255.0", "24": "255.255.255.0", " /16 ": "255.255.0.0", "/8": "255.0.0.0", "/30": "255.255.255.252", "nonsense": "nonsense", "0": "0.0.0.0"} {
		if got := maskOf(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestEachStepChecksItsOwnAnswersAndTheOnesBefore(t *testing.T) {
	e := newEnv(t)
	f := good()
	f.Hostname, f.Country, f.Timezone = "bad_name!", "", ""
	f.WanPort, f.LanIP, f.Username = "", "nonsense", ""
	r := e.check("region", f)
	if r.Status != 400 || r.Errors["hostname"] == "" || r.Errors["country"] == "" || r.Errors["timezone"] == "" {
		t.Fatalf("%d %v", r.Status, r.Errors)
	}
	// the questions of later pages are not asked yet
	if r.Errors["wan_port"] != "" || r.Errors["lan_ip"] != "" || r.Errors["username"] != "" {
		t.Fatalf("%v", r.Errors)
	}
	r = e.check("internet", f)
	if r.Errors["wan_port"] == "" || r.Errors["hostname"] == "" || r.Errors["lan_ip"] != "" {
		t.Fatalf("%v", r.Errors)
	}
	r = e.check("lan", f)
	if r.Errors["lan_ip"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	r = e.check("admin", f)
	if r.Errors["username"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	// and a good form passes every step, changing nothing
	for _, s := range []string{"region", "internet", "lan", "admin"} {
		if r := e.check(s, good()); r.Status != 200 {
			t.Fatalf("%s: %d %v", s, r.Status, r.Errors)
		}
	}
	if r := e.check("nowhere", good()); r.Status != 400 {
		t.Fatalf("%d", r.Status)
	}
	e.nothingSaved()
}

func TestTheFieldsAreNamedLikeTheFormAndTheNetworkIsCheckedAsAWhole(t *testing.T) {
	e := newEnv(t)
	f := good()
	f.LanPorts = nil
	if r := e.check("lan", f); r.Errors["lan_ports"] == "" && r.Errors[""] == "" {
		t.Fatalf("%v", r.Errors)
	}
	f = good()
	f.DHCPStart, f.DHCPEnd = "192.168.9.1", "192.168.9.9" // not in the network
	r := e.check("lan", f)
	if r.Errors["dhcp_start"] == "" && r.Errors["dhcp_end"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	f = good()
	f.WanPort = "enp9s0"
	if r := e.check("internet", f); r.Errors["wan_port"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	f = good()
	f.LanPorts = []string{"enp1s0", "enp3s0"} // the internet port is also a LAN port
	if r := e.check("lan", f); r.Status != 400 {
		t.Fatalf("%d", r.Status)
	}
	f = good()
	f.Timezone = "Mars/Olympus"
	if r := e.check("region", f); r.Errors["timezone"] == "" {
		t.Fatalf("%v", r.Errors)
	}
}

func TestThePortTheBrowserIsConnectedThroughIsProtected(t *testing.T) {
	e := newEnv(t)
	e.here = []string{"enp2s0"}
	f := good()
	f.WanPort, f.LanPorts = "enp2s0", []string{"enp1s0", "enp3s0"}
	r := e.check("internet", f)
	if r.Errors["wan_port"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	f = good()
	f.LanPorts = []string{"enp3s0"}
	if r := e.check("lan", f); r.Errors["lan_ports"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	if r := e.apply(f); r.Status != 400 {
		t.Fatalf("%d", r.Status)
	}
	e.nothingSaved()
	if r := e.apply(good()); r.Status != 200 {
		t.Fatalf("%d %v", r.Status, r.Errors)
	}
}

func TestAFailedSetupLeavesTheRouterAsItWas(t *testing.T) {
	cases := map[string]func(*Form){
		"no username":      func(f *Form) { f.Username = "" },
		"short password":   func(f *Form) { f.Password, f.Confirm = "short", "short" },
		"passwords differ": func(f *Form) { f.Confirm = "different" },
		"bad network":      func(f *Form) { f.LanIP = "999.1.1.1" },
		"no country":       func(f *Form) { f.Country = "" },
		"no ports":         func(f *Form) { f.WanPort, f.LanPorts = "", nil },
	}
	for name, edit := range cases {
		e := newEnv(t)
		f := good()
		edit(&f)
		if r := e.apply(f); r.Status != 400 {
			t.Fatalf("%s: %d %v", name, r.Status, r.Body)
		}
		e.nothingSaved()
	}
}

func TestOnlyOneSetupSucceeds(t *testing.T) {
	e := newEnv(t)
	if r := e.apply(good()); r.Status != 200 {
		t.Fatalf("%d", r.Status)
	}
	// every route is closed now
	second := good()
	second.Username, second.Password, second.Confirm = "intruder", "another password 1", "another password 1"
	if r := e.apply(second); r.Status != 409 {
		t.Fatalf("%d", r.Status)
	}
	for name, h := range map[string]http.HandlerFunc{"info": e.svc.HandleInfo, "check": e.svc.HandleCheck, "restore": e.svc.HandleRestoreApply} {
		method := http.MethodPost
		if name == "info" {
			method = http.MethodGet
		}
		if r := e.do(h, method, good()); r.Status != 409 {
			t.Fatalf("%s: %d", name, r.Status)
		}
	}
	if users := e.mgr.Users(); len(users) != 1 || users[0].Username != "alec" {
		t.Fatalf("%+v", users)
	}
}

func TestTwoSetupsAtOnceMakeOneAccount(t *testing.T) {
	e := newEnv(t)
	var wg sync.WaitGroup
	results := make([]int, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := good()
			f.Username = "admin" + string(rune('a'+i))
			results[i] = e.apply(f).Status
		}(i)
	}
	wg.Wait()
	won := 0
	for _, s := range results {
		if s == 200 {
			won++
		} else if s != 409 {
			t.Fatalf("%v", results)
		}
	}
	if won != 1 || len(e.mgr.Users()) != 1 || len(e.sessions) != 1 {
		t.Fatalf("%v, %d users, %d sessions", results, len(e.mgr.Users()), len(e.sessions))
	}
}

func TestOnlyThePostedFormIsAccepted(t *testing.T) {
	e := newEnv(t)
	if r := e.do(e.svc.HandleApply, http.MethodGet, nil); r.Status != 405 {
		t.Fatalf("%d", r.Status)
	}
	if r := e.do(e.svc.HandleInfo, http.MethodPost, nil); r.Status != 405 {
		t.Fatalf("%d", r.Status)
	}
	// an unknown field is refused, not ignored
	if r := e.do(e.svc.HandleApply, http.MethodPost, map[string]any{"username": "x", "root": true}); r.Status != 400 {
		t.Fatalf("%d", r.Status)
	}
	e.nothingSaved()
}

func TestRestoreRefusesAChoiceThatCutsTheBrowserOff(t *testing.T) {
	e := newEnv(t)
	e.here = []string{"enp2s0"}
	r := e.do(e.svc.HandleRestoreApply, http.MethodPost, map[string]any{"token": "x", "wan": "enp2s0", "lan": []string{"enp1s0"}})
	if r.Status != 400 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	r = e.do(e.svc.HandleRestoreApply, http.MethodPost, map[string]any{"token": "x", "wan": "enp1s0", "lan": []string{"enp3s0"}})
	if r.Status != 400 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	// a backup that is not open is not found, and the router is still not set up
	r = e.do(e.svc.HandleRestoreApply, http.MethodPost, map[string]any{"token": "gone", "wan": "enp1s0", "lan": []string{"enp2s0"}})
	if r.Status != http.StatusNotFound {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	e.nothingSaved()
}

package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"peakroute/internal/auth"
	"peakroute/internal/backup"
	"peakroute/internal/config"
	"peakroute/internal/console"
	"peakroute/internal/wireless"
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

// ---- the console login

type fakeConsole struct {
	sets    []string // "name:hash"
	removed []string
	taken   map[string]bool
	failSet error
}

func (f *fakeConsole) Check(name string) error {
	if err := console.ValidName(name); err != nil {
		return err
	}
	if f.taken[name] {
		return errors.New("The system already has an account with that name: choose another.")
	}
	return nil
}
func (f *fakeConsole) Set(name, hash string) error {
	if f.failSet != nil {
		return f.failSet
	}
	f.sets = append(f.sets, name+":"+hash)
	return nil
}
func (f *fakeConsole) Remove(name string) error { f.removed = append(f.removed, name); return nil }

func TestTheAdministratorIsTheConsoleLoginToo(t *testing.T) {
	e := newEnv(t)
	fc := &fakeConsole{}
	e.svc.Console = fc
	if r := e.apply(good()); r.Status != 200 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	users := e.mgr.Users()
	if len(fc.sets) != 1 || fc.sets[0] != "alec:"+users[0].PasswordHash {
		t.Fatalf("%v", fc.sets)
	}
	// and it is recorded, so that it is kept in step and cannot be removed
	m, _ := config.NewManager(e.dir)
	if m.ConsoleUser() != "alec" {
		t.Fatalf("%q", m.ConsoleUser())
	}
}

func TestAConsoleNameLinuxRefusesIsRefusedAtTheAdministratorPage(t *testing.T) {
	e := newEnv(t)
	e.svc.Console = &fakeConsole{taken: map[string]bool{"daemon": true}}
	for _, name := range []string{"Alec", "a.b", "daemon", "ab"} {
		f := good()
		f.Username = name
		if r := e.check("admin", f); r.Status != 400 || r.Errors["username"] == "" {
			t.Errorf("%q: %d %v", name, r.Status, r.Errors)
		}
		if r := e.apply(f); r.Status != 400 {
			t.Errorf("%q: %d", name, r.Status)
		}
	}
	e.nothingSaved()
	// without a console the same rule of letters still holds: the account may be made one later
	e2 := newEnv(t)
	f := good()
	f.Username = "Alec"
	if r := e2.check("admin", f); r.Errors["username"] == "" {
		t.Fatal("accepted")
	}
}

func TestIfTheConsoleAccountCannotBeMadeNothingIsSetUp(t *testing.T) {
	e := newEnv(t)
	e.svc.Console = &fakeConsole{failSet: errors.New("no useradd")}
	if r := e.apply(good()); r.Status != 500 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
	e.nothingSaved()
	// the person can try again once it works
	e.svc.Console = &fakeConsole{}
	if r := e.apply(good()); r.Status != 200 {
		t.Fatalf("%d %v", r.Status, r.Body)
	}
}

func TestAfterARestoreTheConsoleLoginIsMade(t *testing.T) {
	e := newEnv(t)
	fc := &fakeConsole{}
	e.svc.Console = fc
	hash, _ := config.HashPassword("whatever pass 1")
	// a backup that knows its console account
	e.mgr.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = []config.User{{Username: "ops", PasswordHash: hash, Role: "admin"}, {Username: "Viewer", PasswordHash: hash, Role: "readonly"}}
		c.ConsoleUser = "ops"
		return nil
	})
	if w := e.svc.consoleAfterRestore(); w != "" || len(fc.sets) != 1 || fc.sets[0] != "ops:"+hash {
		t.Fatalf("%q %v", w, fc.sets)
	}
	// a backup from before there was one: the first administrator with a name that Linux accepts
	e = newEnv(t)
	fc = &fakeConsole{}
	e.svc.Console = fc
	e.mgr.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = []config.User{{Username: "Bad.Name", PasswordHash: hash, Role: "admin"}, {Username: "good", PasswordHash: hash, Role: "admin"}}
		return nil
	})
	if w := e.svc.consoleAfterRestore(); w != "" || len(fc.sets) != 1 || fc.sets[0] != "good:"+hash || e.mgr.ConsoleUser() != "good" {
		t.Fatalf("%q %v %q", w, fc.sets, e.mgr.ConsoleUser())
	}
	// none with a usable name: the person is told, the web interface works
	e = newEnv(t)
	e.svc.Console = &fakeConsole{}
	e.mgr.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = []config.User{{Username: "Bad.Name", PasswordHash: hash, Role: "admin"}}
		return nil
	})
	if w := e.svc.consoleAfterRestore(); w == "" || e.mgr.ConsoleUser() != "" {
		t.Fatalf("%q", w)
	}
	// a failing console is a warning, not a failed restore
	e = newEnv(t)
	e.svc.Console = &fakeConsole{failSet: errors.New("x")}
	e.mgr.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = []config.User{{Username: "ops", PasswordHash: hash, Role: "admin"}}
		c.ConsoleUser = "ops"
		return nil
	})
	if w := e.svc.consoleAfterRestore(); w == "" {
		t.Fatal("no warning")
	}
}

// ---- which port of a bridge the browser is plugged into

const arpTable = `IP address       HW type     Flags       HW address            Mask     Device
192.168.77.100   0x1         0x2         AA:BB:CC:00:11:22     *        brsetup
192.168.77.101   0x1         0x2         aa:bb:cc:00:11:33     *        brsetup
192.168.77.102   0x1         0x0         00:00:00:00:00:00     *        brsetup
10.0.0.5         0x1         0x2         aa:bb:cc:00:99:99     *        enp9s0
`

const fdbTable = `33:33:00:00:00:01 dev enp2s0 self permanent
aa:bb:cc:00:11:22 dev enp3s0 master brsetup
aa:bb:cc:00:11:33 dev enp2s0 master brsetup
aa:bb:cc:00:11:44 dev enp9s0 master brsetup
`

func TestTheClientsHardwareAddressComesFromTheARPTable(t *testing.T) {
	if got := parseARP(arpTable, "192.168.77.100", "brsetup"); got != "aa:bb:cc:00:11:22" {
		t.Fatalf("%q", got)
	}
	for _, c := range [][2]string{{"192.168.77.102", "brsetup"}, {"192.168.77.100", "enp9s0"}, {"1.2.3.4", "brsetup"}} {
		if got := parseARP(arpTable, c[0], c[1]); got != "" {
			t.Errorf("%v: %q", c, got)
		}
	}
	if parseARP("", "1.2.3.4", "x") != "" || parseARP("only a header", "1.2.3.4", "x") != "" {
		t.Fatal("garbage")
	}
}

func TestTheBridgeMemberIsFoundFromItsForwardingTable(t *testing.T) {
	members := []string{"enp2s0", "enp3s0", "enp4s0"}
	if got := parseFDB(fdbTable, "aa:bb:cc:00:11:22", members); got != "enp3s0" {
		t.Fatalf("%q", got)
	}
	if got := parseFDB(fdbTable, "aa:bb:cc:00:11:33", members); got != "enp2s0" {
		t.Fatalf("%q", got)
	}
	// a port that is not a member of this bridge, an address that is not there
	if parseFDB(fdbTable, "aa:bb:cc:00:11:44", members) != "" || parseFDB(fdbTable, "de:ad:be:ef:00:00", members) != "" || parseFDB("", "x", members) != "" {
		t.Fatal("found a port that is not there")
	}
}

// ---- access point mode and Wi-Fi

func goodAP() Form {
	f := good()
	f.Mode = "ap"
	f.WanPort, f.WanMode = "", ""
	f.LanPorts, f.LanIP, f.LanSubnet, f.DHCP, f.DHCPStart, f.DHCPEnd = nil, "", "", false, "", ""
	f.APPorts = []string{"enp1s0", "enp2s0", "enp3s0"}
	return f
}

func wifiReport() wireless.Report {
	return wireless.Report{IWAvailable: true, Adapters: []wireless.Adapter{
		{Name: "wlan0", CapabilitiesKnown: true, APCapable: true, MaxAP: 2, Modes: []string{"managed", "AP"}, Bands: []wireless.Band{
			{Name: config.Band24, Widths: []int{20, 40}, HT: true}, {Name: config.Band5, Widths: []int{20, 40, 80}, HT: true, VHT: true}}},
	}}
}

func (e *env) withWifi(rep wireless.Report) {
	e.svc.Wireless = func(context.Context) wireless.Report { return rep }
}

func TestSettingUpAnAccessPoint(t *testing.T) {
	e := newEnv(t)
	f := goodAP()
	f.APMode, f.APIP, f.APNetmask, f.APGateway, f.APDNS = "static", "192.168.1.2", "/24", "192.168.1.1", []string{"192.168.1.1"}
	if r := e.apply(f); r.Status != 200 {
		t.Fatalf("%d %v %v", r.Status, r.Body, r.Errors)
	}
	m, _ := config.NewManager(e.dir)
	cfg, draft := m.StagedNetworkConfig()
	if !draft || !cfg.IsAP() || strings.Join(cfg.AP.Ports, ",") != "enp1s0,enp2s0,enp3s0" || cfg.AP.IP != "192.168.1.2" || cfg.AP.Netmask != "255.255.255.0" || cfg.AP.Gateway != "192.168.1.1" {
		t.Fatalf("%+v", cfg.AP)
	}
	if cfg.Hostname != "home-router" || cfg.Country != "US" || cfg.Timezone != "America/Chicago" {
		t.Fatalf("%+v", cfg)
	}
	if !m.IsConfigured() || len(m.Users()) != 1 {
		t.Fatal("the account and the flag are not saved")
	}
	// the router's own questions are not asked of an access point
	e2 := newEnv(t)
	g := goodAP()
	g.WanPort, g.LanIP = "nonsense", "nonsense"
	if r := e2.apply(g); r.Status != 200 {
		t.Fatalf("an access point was asked router questions: %d %v", r.Status, r.Errors)
	}
}

func TestAnAccessPointIsCheckedStepByStep(t *testing.T) {
	e := newEnv(t)
	f := goodAP()
	f.APPorts = nil
	if r := e.check("ap", f); r.Status != 400 || r.Errors["ap_ports"] == "" {
		t.Fatalf("%d %v", r.Status, r.Errors)
	}
	// nothing about the router's pages
	if r := e.check("lan", f); r.Errors["ap_ports"] != "" {
		t.Fatalf("a later page's problem shown early: %v", r.Errors)
	}
	f = goodAP()
	f.APMode, f.APIP, f.APNetmask = "static", "nope", "255.255.255.0"
	if r := e.check("ap", f); r.Errors["ap_ip"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	f = goodAP()
	f.APPorts = []string{"enp9s0"}
	if r := e.check("ap", f); r.Errors["ap_ports"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	if r := e.check("ap", goodAP()); r.Status != 200 {
		t.Fatalf("%v", r.Errors)
	}
	e.nothingSaved()
}

func TestTheAccessPointKeepsThePortTheBrowserIsConnectedThrough(t *testing.T) {
	e := newEnv(t)
	e.here = []string{"enp2s0"}
	f := goodAP()
	f.APPorts = []string{"enp1s0", "enp3s0"}
	if r := e.check("ap", f); r.Errors["ap_ports"] == "" {
		t.Fatalf("%v", r.Errors)
	}
	if r := e.apply(f); r.Status != 400 {
		t.Fatalf("%d", r.Status)
	}
	e.nothingSaved()
}

func TestWifiIsOfferedWhereAnAdapterCanHostIt(t *testing.T) {
	e := newEnv(t)
	r := e.do(e.svc.HandleInfo, http.MethodGet, nil)
	if w := r.Body["wifi"].([]any); len(w) != 0 {
		t.Fatalf("Wi-Fi offered without a detector: %v", w)
	}
	e.withWifi(wifiReport())
	r = e.do(e.svc.HandleInfo, http.MethodGet, nil)
	w := r.Body["wifi"].([]any)
	if len(w) != 1 || w[0].(map[string]any)["name"] != "wlan0" {
		t.Fatalf("%v", w)
	}
	// an adapter that cannot host, or whose abilities are unknown, is not offered
	rep := wifiReport()
	rep.Adapters = append(rep.Adapters, wireless.Adapter{Name: "wlan1", CapabilitiesKnown: true, APCapable: false, Bands: []wireless.Band{{Name: config.Band24}}},
		wireless.Adapter{Name: "wlan2", CapabilitiesKnown: false})
	e.withWifi(rep)
	if w := e.do(e.svc.HandleInfo, http.MethodGet, nil).Body["wifi"].([]any); len(w) != 1 {
		t.Fatalf("%v", w)
	}
}

func TestTheWifiTheWizardMakes(t *testing.T) {
	e := newEnv(t)
	e.withWifi(wifiReport())
	f := good()
	f.WifiOn, f.WifiName, f.WifiPass = true, "Home Wi-Fi", "correct horse battery"
	if r := e.apply(f); r.Status != 200 {
		t.Fatalf("%d %v %v", r.Status, r.Body, r.Errors)
	}
	cfg, _ := e.mgr.StagedNetworkConfig()
	w := cfg.WifiSettings()
	if len(w.Radios) != 1 || w.Radios[0].Name != "wlan0" || w.Radios[0].Band != config.Band24 || w.Radios[0].Width != 20 || w.Radios[0].Channel != 0 || !w.Radios[0].HT {
		t.Fatalf("one adapter serves 2.4 GHz, automatic channel: %+v", w.Radios)
	}
	if len(w.Networks) != 1 || w.Networks[0].SSID != "Home Wi-Fi" || w.Networks[0].Network != "lan" || w.Networks[0].Security != config.SecurityWPA2WPA3 || w.Networks[0].BandSteering {
		t.Fatalf("%+v", w.Networks)
	}
}

func TestTwoAdaptersCoverBothBandsWithSteering(t *testing.T) {
	rep := wifiReport()
	rep.Adapters = append(rep.Adapters, wireless.Adapter{Name: "wlan1", CapabilitiesKnown: true, APCapable: true, MaxAP: 1, Bands: []wireless.Band{
		{Name: config.Band24, Widths: []int{20, 40}, HT: true}}})
	wf := wifiFor(rep, "Home", "correct horse battery")
	if len(wf.Radios) != 2 || wf.Radios[0].Name != "wlan0" || wf.Radios[0].Band != config.Band5 || wf.Radios[0].Width != 80 || wf.Radios[1].Name != "wlan1" || wf.Radios[1].Band != config.Band24 {
		t.Fatalf("%+v", wf.Radios)
	}
	if n := wf.Networks[0]; !n.BandSteering || len(n.Bands) != 2 {
		t.Fatalf("%+v", n)
	}
	// an adapter with only 5 GHz serves that
	only5 := wireless.Report{Adapters: []wireless.Adapter{{Name: "wlan0", CapabilitiesKnown: true, APCapable: true, Bands: []wireless.Band{{Name: config.Band5, Widths: []int{20, 40, 80}, HT: true, VHT: true}}}}}
	if wf := wifiFor(only5, "Home", "correct horse battery"); len(wf.Radios) != 1 || wf.Radios[0].Band != config.Band5 {
		t.Fatalf("%+v", wf.Radios)
	}
	if wf := wifiFor(wireless.Report{}, "Home", "correct horse battery"); len(wf.Radios) != 0 || len(wf.Networks) != 0 {
		t.Fatalf("%+v", wf)
	}
	// 5 GHz without 802.11ac gets 40 MHz at most
	n := wireless.Report{Adapters: []wireless.Adapter{{Name: "wlan0", CapabilitiesKnown: true, APCapable: true, Bands: []wireless.Band{{Name: config.Band5, Widths: []int{20, 40, 80}, HT: true}}}}}
	if wf := wifiFor(n, "Home", "correct horse battery"); wf.Radios[0].Width != 40 {
		t.Fatalf("%+v", wf.Radios)
	}
}

func TestWifiProblemsAreShownOnTheWifiPage(t *testing.T) {
	e := newEnv(t)
	e.withWifi(wifiReport())
	f := good()
	f.WifiOn, f.WifiName, f.WifiPass = true, "", "short"
	r := e.check("wifi", f)
	if r.Status != 400 || r.Errors["wifi_ssid"] == "" || r.Errors["wifi_pass"] == "" {
		t.Fatalf("%d %v", r.Status, r.Errors)
	}
	if r := e.check("lan", f); r.Errors["wifi_ssid"] != "" {
		t.Fatalf("a later page's problem shown early: %v", r.Errors)
	}
	// no adapter: a message, not a broken configuration
	e.withWifi(wireless.Report{})
	f.WifiName, f.WifiPass = "Home", "correct horse battery"
	if r := e.apply(f); r.Status != 400 || r.Errors["wifi_ssid"] == "" {
		t.Fatalf("%d %v", r.Status, r.Errors)
	}
	e.nothingSaved()
	// off means nothing is asked or made
	f.WifiOn = false
	if r := e.apply(f); r.Status != 200 {
		t.Fatalf("%d %v", r.Status, r.Errors)
	}
	if cfg, _ := e.mgr.StagedNetworkConfig(); cfg.Wifi != nil {
		t.Fatalf("%+v", cfg.Wifi)
	}
}

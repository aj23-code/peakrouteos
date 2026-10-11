package ddns

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"peakroute/internal/config"
)

// ---- a service that answers as it is told and remembers what it was asked

type seen struct {
	method, path, query string
	user, pass          string
	hasAuth             bool
	bearer, agent       string
	body                string
}

type service struct {
	ts      *httptest.Server
	mu      sync.Mutex
	reqs    []seen
	handler func(w http.ResponseWriter, r *http.Request, s seen)
}

func newService(t *testing.T, h func(w http.ResponseWriter, r *http.Request, s seen)) *service {
	t.Helper()
	sv := &service{handler: h}
	sv.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		buf := new(strings.Builder)
		var b [4096]byte
		for {
			n, err := r.Body.Read(b[:])
			buf.Write(b[:n])
			if err != nil {
				break
			}
		}
		s := seen{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, user: u, pass: p, hasAuth: ok, bearer: r.Header.Get("Authorization"), agent: r.Header.Get("User-Agent"), body: buf.String()}
		sv.mu.Lock()
		sv.reqs = append(sv.reqs, s)
		sv.mu.Unlock()
		sv.handler(w, r, s)
	}))
	t.Cleanup(sv.ts.Close)
	return sv
}

func (s *service) host() string { return strings.TrimPrefix(s.ts.URL, "https://") }
func (s *service) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}
func (s *service) last() seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}
func (s *service) all() []seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seen(nil), s.reqs...)
}

func say(text string) func(http.ResponseWriter, *http.Request, seen) {
	return func(w http.ResponseWriter, r *http.Request, s seen) { fmt.Fprint(w, text) }
}

// ---- the lab

type lab struct {
	t       *testing.T
	u       *Updater
	entries []config.DDNSEntry
	wan     netip.Addr
	wanOK   bool
	clock   time.Time
}

func newLab(t *testing.T, svc *service, entries ...config.DDNSEntry) *lab {
	t.Helper()
	l := &lab{t: t, entries: entries, wan: netip.MustParseAddr("203.0.113.9"), wanOK: true, clock: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	l.u = &Updater{
		Entries: func() []config.DDNSEntry { return l.entries },
		WAN:     func() (netip.Addr, bool) { return l.wan, l.wanOK },
		Now:     func() time.Time { return l.clock },
	}
	if svc != nil {
		l.u.Client = svc.ts.Client()
	}
	return l
}

func (l *lab) tick() { l.u.Tick(context.Background()) }

func (l *lab) status(id string) Status {
	l.t.Helper()
	for _, s := range l.u.Statuses() {
		if s.ID == id {
			return s
		}
	}
	l.t.Fatalf("no status for %s", id)
	return Status{}
}

func dyn(server string) config.DDNSEntry {
	return config.DDNSEntry{ID: "d1", Provider: config.DDNSDynDNS2, Hostname: "home.example.com", Server: server, Username: "me", Password: "s3cret-pw", Source: config.DDNSSourceWAN}
}

// ---- dyndns2

func TestDynDNS2UpdatesWhenTheAddressChangesAndNotOtherwise(t *testing.T) {
	sv := newService(t, say("good 203.0.113.9"))
	l := newLab(t, sv, dyn(sv.host()))
	if st := l.status("d1"); st.Message != "Waiting to be updated." || st.OK {
		t.Fatalf("%+v", st)
	}
	l.tick()
	r := sv.last()
	if sv.count() != 1 || r.method != "GET" || r.path != "/nic/update" || r.query != "hostname=home.example.com&myip=203.0.113.9" || !r.hasAuth || r.user != "me" || r.pass != "s3cret-pw" || r.agent != "PeakRoute-OS/1.0" {
		t.Fatalf("%d requests, %+v", sv.count(), r)
	}
	st := l.status("d1")
	if !st.OK || st.Attention || st.Address != "203.0.113.9" || st.Message != "Updated to 203.0.113.9" || !st.LastSuccess.Equal(l.clock) || !st.NextTry.IsZero() {
		t.Fatalf("%+v", st)
	}
	// nothing changed: no request, however often it looks
	for i := 0; i < 5; i++ {
		l.clock = l.clock.Add(time.Minute)
		l.tick()
	}
	if sv.count() != 1 {
		t.Fatalf("%d requests for an address that did not change", sv.count())
	}
	// the address changes
	l.wan = netip.MustParseAddr("198.51.100.7")
	l.tick()
	if sv.count() != 2 || !strings.Contains(sv.last().query, "myip=198.51.100.7") || l.status("d1").Address != "198.51.100.7" {
		t.Fatalf("%d %+v", sv.count(), sv.last())
	}
	// the service says it already had it
	sv.handler = say("nochg 198.51.100.8")
	l.wan = netip.MustParseAddr("198.51.100.8")
	l.tick()
	if st := l.status("d1"); !st.OK || st.Message != "Already 198.51.100.8" || st.Address != "198.51.100.8" {
		t.Fatalf("%+v", st)
	}
}

func TestANameIsToldAgainAfterAWeekAlthoughNothingChanged(t *testing.T) {
	sv := newService(t, say("nochg 203.0.113.9"))
	l := newLab(t, sv, dyn(sv.host()))
	l.tick()
	l.clock = l.clock.Add(7*24*time.Hour - time.Minute)
	l.tick()
	if sv.count() != 1 {
		t.Fatalf("%d", sv.count())
	}
	l.clock = l.clock.Add(time.Minute)
	l.tick()
	if sv.count() != 2 {
		t.Fatalf("not refreshed after a week: %d", sv.count())
	}
	l.u.Refresh = time.Hour
	l.clock = l.clock.Add(61 * time.Minute)
	l.tick()
	if sv.count() != 3 {
		t.Fatalf("the refresh time is not used: %d", sv.count())
	}
}

func TestAnAnswerThatNeedsSomethingChangedStopsTheEntry(t *testing.T) {
	for word, want := range map[string]string{
		"badauth":  "refused the user name or password",
		"nohost":   "no such name",
		"notfqdn":  "not a full name",
		"numhost":  "too many names",
		"abuse":    "blocked this account",
		"badagent": "refused this router's updates",
		"!donator": "paid account",
	} {
		sv := newService(t, say(word))
		l := newLab(t, sv, dyn(sv.host()))
		l.tick()
		st := l.status("d1")
		if st.OK || !st.Attention || !strings.Contains(st.Message, want) || !st.NextTry.IsZero() {
			t.Errorf("%s: %+v", word, st)
		}
		// it stays stopped: no more requests, however long it waits
		for i := 0; i < 4; i++ {
			l.clock = l.clock.Add(time.Hour)
			l.tick()
		}
		if sv.count() != 1 {
			t.Errorf("%s: %d requests after it stopped", word, sv.count())
		}
	}
}

func TestAChangedEntryStartsAfresh(t *testing.T) {
	sv := newService(t, say("badauth"))
	l := newLab(t, sv, dyn(sv.host()))
	l.tick()
	if !l.status("d1").Attention {
		t.Fatal("not stopped")
	}
	// the password is corrected: that is a different entry, and it is tried at once
	sv.handler = say("good")
	l.entries[0].Password = "right-password"
	if st := l.status("d1"); st.Attention || st.Message != "Waiting to be updated." {
		t.Fatalf("an old failure shows for a changed entry: %+v", st)
	}
	l.tick()
	if st := l.status("d1"); !st.OK || sv.count() != 2 || sv.last().pass != "right-password" {
		t.Fatalf("%+v %d", st, sv.count())
	}
	// an unchanged entry that is switched off and on again is the same entry: it is not told again
	l.entries[0].Disabled = true
	l.tick()
	l.entries[0].Disabled = false
	l.tick()
	if sv.count() != 2 {
		t.Fatalf("%d", sv.count())
	}
}

func TestATroubleThatMayPassIsTriedAgainWithALongerWaitEachTime(t *testing.T) {
	status := http.StatusOK
	body := "911"
	sv := newService(t, func(w http.ResponseWriter, r *http.Request, s seen) { w.WriteHeader(status); fmt.Fprint(w, body) })
	l := newLab(t, sv, dyn(sv.host()))
	waits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, want := range waits {
		l.tick()
		st := l.status("d1")
		if st.OK || st.Attention || st.NextTry.Sub(l.clock) != want || sv.count() != i+1 {
			t.Fatalf("attempt %d: wait %v, want %v (%d requests, %+v)", i+1, st.NextTry.Sub(l.clock), want, sv.count(), st)
		}
		// during the wait nothing is sent
		l.clock = l.clock.Add(want - time.Second)
		l.tick()
		if sv.count() != i+1 {
			t.Fatalf("a request during the wait")
		}
		l.clock = l.clock.Add(time.Second)
	}
	// it works again: the wait is forgotten
	body = "good"
	l.tick()
	if st := l.status("d1"); !st.OK || !st.NextTry.IsZero() {
		t.Fatalf("%+v", st)
	}
	// and the next trouble starts at a minute again
	l.wan = netip.MustParseAddr("198.51.100.1")
	body = "dnserr"
	l.tick()
	if st := l.status("d1"); st.NextTry.Sub(l.clock) != time.Minute {
		t.Fatalf("%+v", st)
	}
	// other troubles: the service is busy, broken, or says nothing sensible
	for name, c := range map[string]struct {
		code int
		body string
		want string
	}{
		"500":        {500, "", "not working right now (HTTP 500)"},
		"503":        {503, "", "not working right now (HTTP 503)"},
		"429":        {429, "", "too many requests"},
		"nonsense":   {200, "<html>hello</html>", "something unexpected: <html>hello</html>"},
		"nothing":    {200, "", "something unexpected"},
		"a long one": {200, strings.Repeat("x", 500), "something unexpected: " + strings.Repeat("x", 120) + "…"},
	} {
		status, body = c.code, c.body
		l2 := newLab(t, sv, dyn(sv.host()))
		l2.tick()
		st := l2.status("d1")
		if st.OK || st.Attention || !strings.Contains(st.Message, c.want) || st.NextTry.IsZero() {
			t.Errorf("%s: %+v", name, st)
		}
	}
	// the service's own words for its trouble are kept
	status = http.StatusOK // (the loop above left the last one it tried)
	for word, want := range map[string]string{"911": "problem at its end (911)", "dnserr": "problem at its end (dnserr)"} {
		body = word
		l4 := newLab(t, sv, dyn(sv.host()))
		l4.tick()
		if st := l4.status("d1"); !strings.Contains(st.Message, want) {
			t.Errorf("%s: %+v", word, st)
		}
	}
	// the service cannot be reached at all
	sv.ts.Close()
	l3 := newLab(t, sv, dyn(sv.host()))
	l3.tick()
	if st := l3.status("d1"); st.OK || st.Attention || !strings.Contains(st.Message, "Could not reach "+sv.host()) || strings.Contains(st.Message, "https://") {
		t.Fatalf("%+v", st)
	}
}

func TestSignInFailuresOfTheWebServerStopTheEntry(t *testing.T) {
	for _, code := range []int{401, 403} {
		sv := newService(t, func(w http.ResponseWriter, r *http.Request, s seen) { w.WriteHeader(code) })
		l := newLab(t, sv, dyn(sv.host()))
		l.tick()
		if st := l.status("d1"); !st.Attention || !strings.Contains(st.Message, "refused the sign-in") {
			t.Errorf("%d: %+v", code, st)
		}
	}
	sv := newService(t, func(w http.ResponseWriter, r *http.Request, s seen) { w.WriteHeader(404) })
	l := newLab(t, sv, dyn(sv.host()))
	l.tick()
	if st := l.status("d1"); !st.Attention || !strings.Contains(st.Message, "HTTP 404") {
		t.Errorf("%+v", st)
	}
}

// ---- the address

func TestOnlyAPublicAddressIsPublished(t *testing.T) {
	sv := newService(t, say("good"))
	l := newLab(t, sv, dyn(sv.host()))
	for _, private := range []string{"192.168.1.5", "10.1.2.3", "172.16.0.9", "100.64.1.1", "100.127.255.254", "169.254.1.1", "127.0.0.1", "0.0.0.0", "224.0.0.1"} {
		l.wan = netip.MustParseAddr(private)
		l.tick()
		st := l.status("d1")
		if st.OK || st.Attention || !strings.Contains(st.Message, "not a public address") || !strings.Contains(st.Message, private) {
			t.Errorf("%s: %+v", private, st)
		}
	}
	if sv.count() != 0 {
		t.Fatalf("an address nobody can reach was published")
	}
	// the connection is down
	l.wanOK = false
	l.tick()
	if st := l.status("d1"); st.OK || st.Attention || !strings.Contains(st.Message, "no address right now") {
		t.Fatalf("%+v", st)
	}
	// and it is published as soon as there is something to publish
	l.wanOK, l.wan = true, netip.MustParseAddr("203.0.113.9")
	l.tick()
	if sv.count() != 1 || !l.status("d1").OK {
		t.Fatalf("%d %+v", sv.count(), l.status("d1"))
	}
}

func TestIsPublic(t *testing.T) {
	for addr, want := range map[string]bool{
		"203.0.113.9": true, "8.8.8.8": true, "100.63.255.255": true, "100.128.0.1": true, "172.15.255.255": true, "172.32.0.1": true, "11.0.0.1": true,
		"192.168.0.1": false, "10.0.0.1": false, "172.16.0.1": false, "172.31.255.255": false, "100.64.0.0": false, "100.127.255.255": false,
		"127.0.0.1": false, "169.254.0.1": false, "0.1.2.3": false, "224.1.1.1": false, "255.255.255.255": false, "::1": false, "2001:db8::1": false,
	} {
		if got := IsPublic(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}

func TestTheAddressCanBeAskedOfAnInternetService(t *testing.T) {
	web := newService(t, say("203.0.113.50\n"))
	dd := newService(t, say("good"))
	e := dyn(dd.host())
	e.Source = config.DDNSSourceWeb
	l := newLab(t, dd, e)
	l.wan = netip.MustParseAddr("192.168.100.2") // behind another router: the connection's own address is no use
	l.u.WebIPURLs = []string{web.ts.URL}
	l.u.Client = &http.Client{Transport: web.ts.Client().Transport} // one client that trusts both servers' certificates
	pool := web.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	pool.AddCert(dd.ts.Certificate())
	l.tick()
	if dd.count() != 1 || !strings.Contains(dd.last().query, "myip=203.0.113.50") || web.count() != 1 {
		t.Fatalf("%d %d %+v", dd.count(), web.count(), dd.all())
	}
	// the answer is kept for a few minutes
	l.clock = l.clock.Add(4 * time.Minute)
	l.tick()
	if web.count() != 1 {
		t.Fatalf("asked again after four minutes")
	}
	l.clock = l.clock.Add(2 * time.Minute)
	web.handler = say("203.0.113.51")
	l.tick()
	if web.count() != 2 || dd.count() != 2 || !strings.Contains(dd.last().query, "myip=203.0.113.51") {
		t.Fatalf("%d %d", web.count(), dd.count())
	}
	// "update now" asks again, whatever was kept
	web.handler = say("203.0.113.52")
	if _, ok := l.u.UpdateNow(context.Background(), "d1"); !ok || web.count() != 3 || !strings.Contains(dd.last().query, "myip=203.0.113.52") {
		t.Fatalf("%d", web.count())
	}
}

func TestTheNextAddressServiceIsTriedWhenOneFails(t *testing.T) {
	bad := newService(t, say("not an address"))
	private := newService(t, say("10.1.1.1"))
	broken := newService(t, func(w http.ResponseWriter, r *http.Request, s seen) { w.WriteHeader(500) })
	good := newService(t, say("203.0.113.60"))
	dd := newService(t, say("good"))
	e := dyn(dd.host())
	e.Source = config.DDNSSourceWeb
	l := newLab(t, dd, e)
	l.u.WebIPURLs = []string{bad.ts.URL, private.ts.URL, broken.ts.URL, good.ts.URL}
	l.u.Client = &http.Client{Transport: good.ts.Client().Transport}
	pool := good.ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	for _, s := range []*service{bad, private, broken, dd} {
		pool.AddCert(s.ts.Certificate())
	}
	l.tick()
	if dd.count() != 1 || !strings.Contains(dd.last().query, "myip=203.0.113.60") {
		t.Fatalf("%+v", l.status("d1"))
	}
	// none of them works: nothing is published, and it says why
	l2 := newLab(t, dd, e)
	l2.u.WebIPURLs = []string{bad.ts.URL, broken.ts.URL}
	l2.u.Client = l.u.Client
	before := dd.count()
	l2.tick()
	if st := l2.status("d1"); st.OK || st.Attention || dd.count() != before || !strings.Contains(st.Message, "HTTP 500") {
		t.Fatalf("%+v", st)
	}
}

// ---- DuckDNS

func TestDuckDNS(t *testing.T) {
	sv := newService(t, say("OK"))
	e := config.DDNSEntry{ID: "d1", Provider: config.DDNSDuckDNS, Hostname: "mine.duckdns.org", Password: "1234abcd-12ab-34cd", Source: config.DDNSSourceWAN}
	l := newLab(t, sv, e)
	l.u.DuckURL = sv.ts.URL + "/update"
	l.tick()
	r := sv.last()
	v, _ := url.ParseQuery(r.query)
	if sv.count() != 1 || r.path != "/update" || v.Get("domains") != "mine" || v.Get("token") != "1234abcd-12ab-34cd" || v.Get("ip") != "203.0.113.9" || r.hasAuth {
		t.Fatalf("%+v", r)
	}
	if st := l.status("d1"); !st.OK || st.Message != "Updated to 203.0.113.9" {
		t.Fatalf("%+v", st)
	}
	sv.handler = say("KO")
	l.wan = netip.MustParseAddr("198.51.100.3")
	l.tick()
	if st := l.status("d1"); st.OK || !st.Attention || !strings.Contains(st.Message, "check the name and the token") || strings.Contains(st.Message, "1234abcd") {
		t.Fatalf("%+v", st)
	}
	sv.handler = say("???")
	l2 := newLab(t, sv, e)
	l2.u.DuckURL = sv.ts.URL + "/update"
	l2.tick()
	if st := l2.status("d1"); st.Attention || st.NextTry.IsZero() {
		t.Fatalf("%+v", st)
	}
}

// ---- Cloudflare

type cfAPI struct {
	zones   map[string]string // zone name -> id
	records []map[string]any  // the A records of the zone
	failOn  string            // "zones", "records" or "write": answer 500 there
	denied  bool
}

func (c *cfAPI) handler(w http.ResponseWriter, r *http.Request, s seen) {
	reply := func(code int, ok bool, result any, errMsg string) {
		w.WriteHeader(code)
		errs := []map[string]string{}
		if errMsg != "" {
			errs = append(errs, map[string]string{"message": errMsg})
		}
		json.NewEncoder(w).Encode(map[string]any{"success": ok, "errors": errs, "result": result})
	}
	if c.denied {
		reply(403, false, nil, "Authentication error")
		return
	}
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/zones":
		if c.failOn == "zones" {
			reply(500, false, nil, "")
			return
		}
		var out []map[string]string
		if id, ok := c.zones[q.Get("name")]; ok {
			out = append(out, map[string]string{"id": id, "name": q.Get("name")})
		}
		reply(200, true, out, "")
	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == "GET":
		if c.failOn == "records" {
			reply(500, false, nil, "")
			return
		}
		var out []map[string]any
		for _, rec := range c.records {
			if rec["name"] == q.Get("name") {
				out = append(out, rec)
			}
		}
		reply(200, true, out, "")
	case r.Method == "PUT" || r.Method == "POST":
		if c.failOn == "write" {
			reply(200, false, nil, "Record could not be saved")
			return
		}
		reply(200, true, map[string]any{}, "")
	default:
		reply(404, false, nil, "no such route")
	}
}

func cfEntry() config.DDNSEntry {
	return config.DDNSEntry{ID: "d1", Provider: config.DDNSCloudflare, Hostname: "a.b.example.com", Password: goodToken, Source: config.DDNSSourceWAN}
}

const goodToken = "abcdefghijklmnopqrstuvwxyz0123456789ABCD"

func TestCloudflareChangesTheRecordOfTheZoneItFinds(t *testing.T) {
	api := &cfAPI{zones: map[string]string{"example.com": "zone1"}, records: []map[string]any{{"id": "rec1", "name": "a.b.example.com", "content": "192.0.2.1", "ttl": 300, "proxied": true}}}
	sv := newService(t, api.handler)
	l := newLab(t, sv, cfEntry())
	l.u.CloudflareURL = sv.ts.URL + "/"
	l.tick()
	reqs := sv.all()
	// the zone is looked for with labels taken off from the left; then the record; then it is changed, keeping its settings
	var paths []string
	for _, r := range reqs {
		paths = append(paths, r.method+" "+r.path)
		if r.bearer != "Bearer "+goodToken || r.agent != "PeakRoute-OS/1.0" {
			t.Fatalf("%+v", r)
		}
	}
	want := "GET /zones,GET /zones,GET /zones,GET /zones/zone1/dns_records,PUT /zones/zone1/dns_records/rec1"
	if strings.Join(paths, ",") != want {
		t.Fatalf("%s\nwant %s", strings.Join(paths, ","), want)
	}
	if !strings.Contains(reqs[0].query, "name=a.b.example.com") || !strings.Contains(reqs[1].query, "name=b.example.com") || !strings.Contains(reqs[2].query, "name=example.com") {
		t.Fatalf("%v", reqs)
	}
	var put map[string]any
	json.Unmarshal([]byte(reqs[4].body), &put)
	if put["content"] != "203.0.113.9" || put["type"] != "A" || put["name"] != "a.b.example.com" || put["ttl"] != float64(300) || put["proxied"] != true {
		t.Fatalf("%v", put)
	}
	if st := l.status("d1"); !st.OK || st.Message != "Updated to 203.0.113.9" {
		t.Fatalf("%+v", st)
	}
	// the record already has the address
	api.records[0]["content"] = "198.51.100.4"
	l.wan = netip.MustParseAddr("198.51.100.4")
	n := sv.count()
	l.tick()
	if st := l.status("d1"); !st.OK || st.Message != "Already 198.51.100.4" {
		t.Fatalf("%+v", st)
	}
	for _, r := range sv.all()[n:] {
		if r.method != "GET" {
			t.Fatalf("a write when nothing changed: %+v", r)
		}
	}
}

func TestCloudflareKeepsTheDefaultTTLOfARecordThatHasNone(t *testing.T) {
	api := &cfAPI{zones: map[string]string{"example.com": "zone1"}, records: []map[string]any{{"id": "rec1", "name": "home.example.com", "content": "192.0.2.1"}}}
	sv := newService(t, api.handler)
	e := cfEntry()
	e.Hostname = "home.example.com"
	l := newLab(t, sv, e)
	l.u.CloudflareURL = sv.ts.URL
	l.tick()
	var put map[string]any
	json.Unmarshal([]byte(sv.last().body), &put)
	if sv.last().method != "PUT" || put["ttl"] != float64(1) || put["proxied"] != false {
		t.Fatalf("%+v", sv.last())
	}
}

func TestCloudflareAddsTheRecordWhenThereIsNone(t *testing.T) {
	api := &cfAPI{zones: map[string]string{"example.com": "zone1"}}
	sv := newService(t, api.handler)
	e := cfEntry()
	e.Hostname = "home.example.com"
	l := newLab(t, sv, e)
	l.u.CloudflareURL = sv.ts.URL
	l.tick()
	r := sv.last()
	var post map[string]any
	json.Unmarshal([]byte(r.body), &post)
	if r.method != "POST" || r.path != "/zones/zone1/dns_records" || post["content"] != "203.0.113.9" || post["proxied"] != false || post["ttl"] != float64(1) || post["name"] != "home.example.com" {
		t.Fatalf("%+v", r)
	}
	if st := l.status("d1"); !st.OK || st.Message != "Added the record, 203.0.113.9" {
		t.Fatalf("%+v", st)
	}
}

func TestCloudflareTroubles(t *testing.T) {
	cases := []struct {
		name  string
		api   *cfAPI
		stop  bool
		wants string
	}{
		{"a token that is refused", &cfAPI{denied: true}, true, "refused the sign-in: check the user name and password or token. (Authentication error)"},
		{"no zone", &cfAPI{zones: map[string]string{"other.org": "z"}}, true, "no zone for a.b.example.com"},
		{"the zones cannot be read", &cfAPI{zones: map[string]string{"example.com": "z"}, failOn: "zones"}, false, "not working right now"},
		{"the records cannot be read", &cfAPI{zones: map[string]string{"example.com": "z"}, failOn: "records"}, false, "not working right now"},
		{"a record that cannot be saved", &cfAPI{zones: map[string]string{"example.com": "z"}, failOn: "write"}, true, "Record could not be saved"},
	}
	for _, c := range cases {
		sv := newService(t, c.api.handler)
		l := newLab(t, sv, cfEntry())
		l.u.CloudflareURL = sv.ts.URL
		l.tick()
		st := l.status("d1")
		if st.OK || st.Attention != c.stop || !strings.Contains(st.Message, c.wants) || strings.Contains(st.Message, goodToken) {
			t.Errorf("%s: %+v", c.name, st)
		}
	}
	// the top-level domain alone is never asked for as a zone
	sv := newService(t, (&cfAPI{zones: map[string]string{"com": "z"}}).handler)
	l := newLab(t, sv, cfEntry())
	l.u.CloudflareURL = sv.ts.URL
	l.tick()
	for _, r := range sv.all() {
		if strings.Contains(r.query, "name=com&") || strings.HasSuffix(r.query, "name=com") {
			t.Fatalf("asked for the zone com: %+v", r)
		}
	}
}

// ---- a custom address

func TestACustomAddressIsCalledWithTheValuesEscaped(t *testing.T) {
	sv := newService(t, say("ok"))
	e := config.DDNSEntry{ID: "d1", Provider: config.DDNSCustom, Hostname: "home.example.com", Source: config.DDNSSourceWAN, Username: "me@example.com", Password: "p&ss w/rd=1",
		URL: "https://" + sv.host() + "/update?host={hostname}&u={username}&pw={password}&ip={ip}"}
	l := newLab(t, sv, e)
	l.tick()
	r := sv.last()
	v, err := url.ParseQuery(r.query)
	if err != nil || sv.count() != 1 || r.path != "/update" || v.Get("host") != "home.example.com" || v.Get("u") != "me@example.com" || v.Get("pw") != "p&ss w/rd=1" || v.Get("ip") != "203.0.113.9" || r.hasAuth {
		t.Fatalf("%+v %v", r, err)
	}
	if strings.Contains(r.query, "p&ss w/rd") {
		t.Fatalf("not escaped: %s", r.query)
	}
	if st := l.status("d1"); !st.OK || st.Message != "Updated to 203.0.113.9" {
		t.Fatalf("%+v", st)
	}
	for code, stopped := range map[int]bool{200: false, 204: false, 299: false, 400: true, 404: true, 410: true, 429: false, 500: false, 502: false} {
		sv.handler = func(w http.ResponseWriter, r *http.Request, s seen) {
			w.WriteHeader(code)
			fmt.Fprint(w, "p&ss w/rd=1 is wrong")
		}
		l2 := newLab(t, sv, e)
		l2.tick()
		st := l2.status("d1")
		if code < 300 {
			if !st.OK {
				t.Errorf("%d: %+v", code, st)
			}
			continue
		}
		if st.OK || st.Attention != stopped {
			t.Errorf("%d: %+v", code, st)
		}
	}
}

// ---- the whole

func TestTheStatusesAndTheForgetting(t *testing.T) {
	sv := newService(t, say("good"))
	a, b := dyn(sv.host()), dyn(sv.host())
	b.ID, b.Hostname = "d2", "other.example.com"
	off := dyn(sv.host())
	off.ID, off.Hostname, off.Disabled = "d3", "off.example.com", true
	l := newLab(t, sv, a, b, off)
	l.tick()
	st := l.u.Statuses()
	if len(st) != 2 || st[0].ID != "d1" || st[1].ID != "d2" || !st[0].OK || !st[1].OK || sv.count() != 2 {
		t.Fatalf("a switched-off name is not listed or updated: %+v %d", st, sv.count())
	}
	// an entry that goes away is forgotten, and a new one with the same id starts afresh
	l.entries = []config.DDNSEntry{b}
	l.tick()
	l.entries = []config.DDNSEntry{a, b}
	if s := l.status("d1"); s.Message != "Waiting to be updated." {
		t.Fatalf("%+v", s)
	}
	l.tick()
	if sv.count() != 3 {
		t.Fatalf("%d", sv.count())
	}
}

func TestUpdateNowForcesWhateverItWasWaitingFor(t *testing.T) {
	body := "badauth"
	sv := newService(t, func(w http.ResponseWriter, r *http.Request, s seen) { fmt.Fprint(w, body) })
	off := dyn(sv.host())
	off.ID, off.Disabled = "d2", true
	l := newLab(t, sv, dyn(sv.host()), off)
	l.tick()
	if !l.status("d1").Attention {
		t.Fatal("not stopped")
	}
	body = "good"
	st, ok := l.u.UpdateNow(context.Background(), "d1")
	if !ok || !st.OK || st.Attention || sv.count() != 2 {
		t.Fatalf("%v %+v %d", ok, st, sv.count())
	}
	// an unchanged address is told to the service again: that is what was asked for
	if _, ok := l.u.UpdateNow(context.Background(), "d1"); !ok || sv.count() != 3 {
		t.Fatalf("%d", sv.count())
	}
	// a wait after trouble is cut short
	body = "911"
	l.u.UpdateNow(context.Background(), "d1")
	if st := l.status("d1"); st.NextTry.IsZero() {
		t.Fatalf("%+v", st)
	}
	body = "good"
	if st, _ := l.u.UpdateNow(context.Background(), "d1"); !st.OK {
		t.Fatalf("%+v", st)
	}
	if _, ok := l.u.UpdateNow(context.Background(), "d9"); ok {
		t.Error("an entry that is not there was updated")
	}
	if _, ok := l.u.UpdateNow(context.Background(), "d2"); ok {
		t.Error("a switched-off entry was updated")
	}
}

func TestNoStatusEverHoldsASecret(t *testing.T) {
	sv := newService(t, say("badauth"))
	e := dyn(sv.host())
	e.Password = "VERY-SECRET-PASSWORD"
	e.Username = "VERY-SECRET-USER"
	l := newLab(t, sv, e)
	l.tick()
	sv.ts.Close()
	l.entries[0].Hostname = "second.example.com"
	l.tick()
	dd := config.DDNSEntry{ID: "d2", Provider: config.DDNSDuckDNS, Hostname: "mine.duckdns.org", Password: "VERY-SECRET-TOKEN", Source: "wan"}
	l.entries = append(l.entries, dd)
	l.u.DuckURL = sv.ts.URL
	l.tick()
	for _, s := range l.u.Statuses() {
		all := s.Message + s.Address + s.Hostname
		for _, secret := range []string{"VERY-SECRET-PASSWORD", "VERY-SECRET-USER", "VERY-SECRET-TOKEN"} {
			if strings.Contains(all, secret) {
				t.Fatalf("%s in %+v", secret, s)
			}
		}
	}
}

func TestRunWorksUntilItIsStopped(t *testing.T) {
	sv := newService(t, say("good"))
	l := newLab(t, sv, dyn(sv.host()))
	l.u.Every = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.u.Run(ctx); close(done) }()
	for i := 0; i < 300 && sv.count() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if sv.count() != 1 {
		t.Fatalf("%d", sv.count())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestOnlyHttpsRedirectsAreFollowed(t *testing.T) {
	check := newClient().CheckRedirect
	mk := func(raw string) *http.Request { r, _ := http.NewRequest("GET", raw, nil); return r }
	if err := check(mk("https://example.com/next"), make([]*http.Request, 1)); err != nil {
		t.Errorf("an https redirect: %v", err)
	}
	if err := check(mk("http://example.com/next"), make([]*http.Request, 1)); err == nil {
		t.Error("a redirect to plain http was followed")
	}
	if err := check(mk("https://example.com/next"), make([]*http.Request, 3)); err == nil {
		t.Error("a fourth redirect was followed")
	}
}

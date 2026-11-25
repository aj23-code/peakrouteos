package filtering

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/auth"
)

type sessions struct {
	info auth.SessionInfo
	ok   bool
}

func (s sessions) Lookup(*http.Request) (auth.SessionInfo, bool) { return s.info, s.ok }

// a stand-in for AdGuard Home that says what it was asked
func adguard(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" cookie="+r.Header.Get("Cookie")+" origin="+r.Header.Get("Origin"))
		if r.URL.Path == "/control/status" {
			io.WriteString(w, `{"version":"v1.0.0","running":true,"protection_enabled":true}`)
			return
		}
		io.WriteString(w, "AdGuard Home page")
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func proxyFor(up *httptest.Server, s sessions) *Proxy {
	return &Proxy{Sessions: s, Upstream: strings.TrimPrefix(up.URL, "http://"), MainPort: ":443"}
}

func TestSomeoneNotSignedInIsSentToTheRouterNotToAdGuardHome(t *testing.T) {
	up, seen := adguard(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://192.168.1.1:3000/", nil)
	req.Host = "192.168.1.1:3000"
	proxyFor(up, sessions{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "https://192.168.1.1:443") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	if len(*seen) != 0 {
		t.Errorf("AdGuard Home was reached without a session: %v", *seen)
	}
}

func TestASignedInAdminSeesAdGuardHomeWithoutTheRoutersCookie(t *testing.T) {
	up, seen := adguard(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://r:3000/control/status", nil)
	req.Header.Set("Cookie", "session=secret")
	proxyFor(up, sessions{info: auth.SessionInfo{Username: "a", Role: auth.RoleAdmin}, ok: true}).ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "v1.0.0") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(*seen) != 1 || strings.Contains((*seen)[0], "secret") {
		t.Errorf("the session cookie went on to AdGuard Home: %v", *seen)
	}
}

func TestAChangeNeedsAWriteAccountAndTheSamePage(t *testing.T) {
	up, seen := adguard(t)
	do := func(role, origin string) int {
		req := httptest.NewRequest(http.MethodPost, "https://r:3000/control/filtering/add_url", strings.NewReader("{}"))
		req.Host = "r:3000"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		proxyFor(up, sessions{info: auth.SessionInfo{Username: "a", Role: role}, ok: true}).ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do(auth.RoleAdmin, "https://r:3000"); got != 200 {
		t.Errorf("from its own page: %d", got)
	}
	n := len(*seen)
	for name, code := range map[string]int{
		"no origin":    do(auth.RoleAdmin, ""),
		"another site": do(auth.RoleAdmin, "https://evil.example"),
		"another port": do(auth.RoleAdmin, "https://r:443"),
		"a read-only":  do("viewer", "https://r:3000"),
	} {
		if code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(*seen) != n {
		t.Errorf("a refused change reached AdGuard Home: %v", (*seen)[n:])
	}
}

func TestWhenAdGuardHomeIsNotRunningTheAnswerSaysSo(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://r:3000/", nil)
	p := &Proxy{Sessions: sessions{info: auth.SessionInfo{Role: auth.RoleAdmin}, ok: true}, Upstream: "127.0.0.1:1"}
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "not running") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestStatusSaysWhatIsInstalledAndRunning(t *testing.T) {
	up, _ := adguard(t)
	bin := filepath.Join(t.TempDir(), "AdGuardHome")
	if st := ReadStatus(context.Background(), bin, strings.TrimPrefix(up.URL, "http://")); st.Installed || !st.Running || st.Version != "v1.0.0" || !st.Protected {
		t.Errorf("%+v", st)
	}
	os.WriteFile(bin, []byte("x"), 0755)
	if st := ReadStatus(context.Background(), bin, "127.0.0.1:1"); !st.Installed || st.Running || st.Version != "" {
		t.Errorf("%+v", st)
	}
}

func TestTheServerOffersTheProxyOverHTTPSAndStops(t *testing.T) {
	cert := selfSigned(t)
	var err error
	up, _ := adguard(t)
	s := &Server{Proxy: proxyFor(up, sessions{info: auth.SessionInfo{Role: auth.RoleAdmin}, ok: true}),
		TLS: &tls.Config{Certificates: []tls.Certificate{cert}}, Addr: "127.0.0.1:31300"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 2 * time.Second}
	var res *http.Response
	for i := 0; i < 50; i++ {
		if res, err = client.Get("https://127.0.0.1:31300/"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !strings.Contains(string(body), "AdGuard Home page") {
		t.Errorf("%s", body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("it did not stop")
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

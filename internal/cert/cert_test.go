package cert

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// lab points the package at a fresh directory for one test.
func lab(t *testing.T, names Names) *Manager {
	t.Helper()
	oldC, oldK := CertPath, KeyPath
	EnableDevPaths(t.TempDir())
	t.Cleanup(func() { CertPath, KeyPath = oldC, oldK })
	return &Manager{Wanted: func() Names { return names }}
}

var router = Names{DNS: []string{"gateway", "gateway.home.arpa"}, IPs: []net.IP{net.ParseIP("192.168.1.1"), net.ParseIP("10.20.0.1")}}

func leafOf(t *testing.T) *x509.Certificate {
	t.Helper()
	c, err := readCert(CertPath)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTheBuiltInCertificateIsMadeByTheAuthorityForTheRoutersNames(t *testing.T) {
	m := lab(t, router)
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	leaf, ca := leafOf(t), func() *x509.Certificate { c, _ := readCert(caCertPath()); return c }()
	if !ca.IsCA || ca.Subject.CommonName != caCommonName || ca.NotAfter.Before(time.Now().AddDate(9, 11, 0)) {
		t.Fatalf("%+v", ca.Subject)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	// a browser that trusts the authority accepts every name and address, and nothing else
	for _, host := range []string{"gateway", "gateway.home.arpa", "localhost", "192.168.1.1", "10.20.0.1", "127.0.0.1", "::1"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
	for _, host := range []string{"evil.example.com", "192.168.1.2"} {
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: host}); err == nil {
			t.Errorf("%s is accepted", host)
		}
	}
	if leaf.Subject.CommonName != "gateway" || leaf.IsCA || leaf.NotAfter.After(time.Now().AddDate(1, 0, 1)) || !leaf.NotBefore.Before(time.Now()) {
		t.Fatalf("%+v %v", leaf.Subject, leaf.NotAfter)
	}
	// without the authority it is not trusted (it is not vouching for itself)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "gateway"}); err == nil {
		t.Fatal("trusted without the authority")
	}
	// the key is private, the pair is served, and nothing is marked custom
	if info, _ := os.Stat(KeyPath); info.Mode().Perm() != 0600 {
		t.Fatalf("%v", info.Mode())
	}
	if info, _ := os.Stat(caKeyPath()); info.Mode().Perm() != 0600 {
		t.Fatalf("%v", info.Mode())
	}
	got, err := m.GetCertificate(nil)
	if err != nil || !bytes.Equal(got.Certificate[0], leaf.Raw) {
		t.Fatalf("%v", err)
	}
	if Custom() {
		t.Fatal("marked custom")
	}
	st := m.Status()
	if st.Mode != "builtin" || st.Web == nil || st.Authority == nil || st.SelfSigned || len(st.Missing) != 0 || st.Web.Fingerprint == "" || strings.Count(st.Web.Fingerprint, ":") != 31 || st.Web.DaysLeft < 360 {
		t.Fatalf("%+v", st)
	}
}

func TestNothingChangesWhileNothingIsWrong(t *testing.T) {
	m := lab(t, router)
	m.Ensure()
	cert, key, ca := read(t, CertPath), read(t, KeyPath), read(t, caCertPath())
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cert, read(t, CertPath)) || !bytes.Equal(key, read(t, KeyPath)) || !bytes.Equal(ca, read(t, caCertPath())) {
		t.Fatal("a certificate was made again for nothing")
	}
	// a router with fewer names does not need a new one either
	m.Wanted = func() Names { return Names{DNS: []string{"gateway"}} }
	m.Ensure()
	if !bytes.Equal(cert, read(t, CertPath)) {
		t.Fatal("made again for fewer names")
	}
}

func TestANewNameOrAddressMakesANewCertificateButNotANewAuthority(t *testing.T) {
	m := lab(t, router)
	m.Ensure()
	before, ca := read(t, CertPath), read(t, caCertPath())
	m.Wanted = func() Names {
		return Names{DNS: router.DNS, IPs: append(append([]net.IP{}, router.IPs...), net.ParseIP("172.16.5.1"))}
	}
	m.Ensure()
	if bytes.Equal(before, read(t, CertPath)) {
		t.Fatal("no new certificate for a new address")
	}
	if !bytes.Equal(ca, read(t, caCertPath())) {
		t.Fatal("a new authority: every device that trusted the old one would have to start again")
	}
	if !contains(leafOf(t).IPAddresses, "172.16.5.1") {
		t.Fatal("the address is not in")
	}
	// a hostname change
	m.Wanted = func() Names { return Names{DNS: []string{"newname"}, IPs: router.IPs} }
	m.Ensure()
	if !containsS(leafOf(t).DNSNames, "newname") || leafOf(t).Subject.CommonName != "newname" {
		t.Fatalf("%+v", leafOf(t).DNSNames)
	}
}

func contains(ips []net.IP, s string) bool {
	for _, ip := range ips {
		if ip.String() == s {
			return true
		}
	}
	return false
}
func containsS(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestExtraNamesGoIntoTheCertificate(t *testing.T) {
	m := lab(t, router)
	m.Ensure()
	got, err := SetExtraNames([]string{" VPN.Example.com ", "203.0.113.9", "vpn.example.com", ""})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	m.Ensure() // the daily check finds what is missing
	leaf := leafOf(t)
	if !containsS(leaf.DNSNames, "vpn.example.com") || !contains(leaf.IPAddresses, "203.0.113.9") {
		t.Fatalf("%v %v", leaf.DNSNames, leaf.IPAddresses)
	}
	for _, bad := range []string{"a b", "-x.example", "x..example", "exa_mple.com", ".example.com", "example.com.", strings.Repeat("a", 64) + ".com", "http://x"} {
		if _, err := SetExtraNames([]string{bad}); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	var many []string
	for i := 0; i < 11; i++ {
		many = append(many, "h"+string(rune('a'+i))+".example.com")
	}
	if _, err := SetExtraNames(many); err == nil {
		t.Error("eleven names were accepted")
	}
	if list := ExtraNames(); len(list) != 2 {
		t.Fatalf("a refused list was kept: %v", list)
	}
	if _, err := SetExtraNames(nil); err != nil || len(ExtraNames()) != 0 {
		t.Fatal("clearing")
	}
}

// selfSigned makes a certificate for tests: signed by itself, or by (parent, parentKey), with the given dates.
func makeCert(t *testing.T, cn string, org string, isCA bool, notBefore, notAfter time.Time, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) ([]byte, []byte, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: bigOne(), Subject: pkix.Name{CommonName: cn, Organization: []string{org}}, NotBefore: notBefore, NotAfter: notAfter,
		DNSNames: []string{cn}, BasicConstraintsValid: true, IsCA: isCA, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if isCA {
		tpl.KeyUsage = x509.KeyUsageCertSign
	}
	signer, signKey := tpl, key
	if parent != nil {
		signer, signKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, signer, &key.PublicKey, signKey)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := pemKey(key)
	c, _ := x509.ParseCertificate(der)
	return pemCert(der), k, c, key
}

func bigOne() *big.Int { return big.NewInt(7) }

func TestTheOldSelfSignedCertificateIsReplacedByOneFromTheAuthority(t *testing.T) {
	m := lab(t, router)
	certPEM, keyPEM, _, _ := makeCert(t, "PeakRoute OS Local CA", "PeakRoute OS", false, time.Now().Add(-time.Hour), time.Now().AddDate(1, 0, 0), nil, nil)
	os.WriteFile(CertPath, certPEM, 0644)
	os.WriteFile(KeyPath, keyPEM, 0600)
	if Custom() {
		t.Fatal("an old self-signed certificate is not custom")
	}
	if !m.Status().SelfSigned {
		t.Fatal("not noticed as self-signed")
	}
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.SelfSigned || st.Mode != "builtin" || len(st.Missing) != 0 {
		t.Fatalf("%+v", st)
	}
	ca, _ := readCert(caCertPath())
	if leafOf(t).CheckSignatureFrom(ca) != nil {
		t.Fatal("not made by the authority")
	}
}

func TestACertificateThatIsAboutToRunOutIsRenewed(t *testing.T) {
	m := lab(t, router)
	m.Ensure()
	ca, caKey, _ := ensureAuthority()
	short, key, _, _ := makeCert(t, "gateway", "PeakRoute OS", false, time.Now().Add(-time.Hour), time.Now().Add(10*24*time.Hour), ca, caKey)
	for _, n := range []string{"gateway.home.arpa", "localhost"} { // as if the router's names were all in it
		_ = n
	}
	os.WriteFile(CertPath, short, 0644)
	os.WriteFile(KeyPath, key, 0600)
	m.Wanted = func() Names { return Names{DNS: []string{"gateway"}} }
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	if left := time.Until(leafOf(t).NotAfter); left < 360*24*time.Hour {
		t.Fatalf("%v left", left)
	}
}

func TestAMismatchedPairOrDamagedFilesAreMadeAgain(t *testing.T) {
	m := lab(t, router)
	m.Ensure()
	os.WriteFile(KeyPath, []byte("garbage"), 0600)
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(CertPath, KeyPath); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(CertPath, []byte("garbage"), 0644)
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(CertPath, KeyPath); err != nil {
		t.Fatal(err)
	}
	// a damaged authority: a new one, and a certificate from it
	os.WriteFile(caKeyPath(), []byte("garbage"), 0600)
	if err := m.Ensure(); err != nil {
		t.Fatal(err)
	}
	ca, _ := readCert(caCertPath())
	if leafOf(t).CheckSignatureFrom(ca) != nil {
		t.Fatal("the certificate is not from the authority that is there")
	}
}

func TestACustomCertificateIsInstalledKeptAndOnlyReplacedOnRequest(t *testing.T) {
	m := lab(t, router)
	m.Ensure()
	builtin := read(t, CertPath)
	pub, pubKey, pubLeaf, _ := makeCert(t, "router.example.com", "Public CA", false, time.Now().Add(-time.Hour), time.Now().AddDate(0, 3, 0), nil, nil)
	if err := m.Install(pub, pubKey); err != nil {
		t.Fatal(err)
	}
	if !Custom() || m.Status().Mode != "custom" {
		t.Fatal("not custom")
	}
	got, _ := m.GetCertificate(nil)
	if !bytes.Equal(got.Certificate[0], pubLeaf.Raw) {
		t.Fatal("not served at once")
	}
	// kept: not renewed (it would qualify: it has less than the built-in's year, and lacks the router's names)
	if err := m.Ensure(); err != nil || !bytes.Equal(read(t, CertPath), pub) {
		t.Fatalf("a custom certificate was touched: %v", err)
	}
	st := m.Status()
	if len(st.Missing) == 0 || containsS(st.Missing, "localhost") {
		t.Fatalf("what it does not cover: %v", st.Missing)
	}
	// regenerate: refused without the word, done with it
	h := httptest.NewRecorder()
	plain := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{}`)).WithContext(context.Background())
	plain.Header.Set("Content-Type", "application/json")
	m.HandleRegenerate(h, plain)
	if h.Code != http.StatusConflict || !bytes.Equal(read(t, CertPath), pub) {
		t.Fatalf("%d %s", h.Code, h.Body)
	}
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"replace_custom":true}`))
	req.Header.Set("Content-Type", "application/json")
	h = httptest.NewRecorder()
	m.HandleRegenerate(h, req)
	if h.Code != http.StatusOK || Custom() || bytes.Equal(read(t, CertPath), pub) || bytes.Equal(read(t, CertPath), builtin) {
		t.Fatalf("%d %s", h.Code, h.Body)
	}
	if m.Status().Mode != "builtin" {
		t.Fatal("still custom")
	}
}

func TestAnUploadThatIsNotGoodChangesNothing(t *testing.T) {
	m := lab(t, router)
	m.Ensure()
	before, beforeKey := read(t, CertPath), read(t, KeyPath)
	good, goodKey, _, _ := makeCert(t, "a.example.com", "X", false, time.Now().Add(-time.Hour), time.Now().AddDate(0, 3, 0), nil, nil)
	_, otherKey, _, _ := makeCert(t, "b.example.com", "X", false, time.Now().Add(-time.Hour), time.Now().AddDate(0, 3, 0), nil, nil)
	expired, expiredKey, _, _ := makeCert(t, "c.example.com", "X", false, time.Now().AddDate(0, -3, 0), time.Now().Add(-time.Hour), nil, nil)
	future, futureKey, _, _ := makeCert(t, "d.example.com", "X", false, time.Now().AddDate(0, 1, 0), time.Now().AddDate(0, 3, 0), nil, nil)
	ca, caKey, _, _ := makeCert(t, "An authority", "X", true, time.Now().Add(-time.Hour), time.Now().AddDate(1, 0, 0), nil, nil)
	for name, args := range map[string][2][]byte{
		"a key that does not fit": {good, otherKey},
		"garbage":                 {[]byte("x"), []byte("y")},
		"no key":                  {good, nil},
		"expired":                 {expired, expiredKey},
		"not valid yet":           {future, futureKey},
		"an authority":            {ca, caKey},
	} {
		if err := m.Install(args[0], args[1]); err == nil {
			t.Errorf("%s was installed", name)
		}
	}
	if !bytes.Equal(before, read(t, CertPath)) || !bytes.Equal(beforeKey, read(t, KeyPath)) || Custom() {
		t.Fatal("a refused upload changed the installed pair")
	}
	// a good one with a chain after it
	chain := append(append([]byte{}, good...), pemCert(mustDER(t, ca))...)
	if err := m.Install(chain, goodKey); err != nil {
		t.Fatalf("a chain: %v", err)
	}
}

func mustDER(t *testing.T, pemBytes []byte) []byte {
	b, _ := pem.Decode(pemBytes)
	return b.Bytes
}

func TestTheAuthorityCertificateIsPublicAndStable(t *testing.T) {
	m := lab(t, router)
	b, err := m.AuthorityPEM() // made when asked for, before anything else
	if err != nil || !bytes.Contains(b, []byte("BEGIN CERTIFICATE")) || bytes.Contains(b, []byte("PRIVATE")) {
		t.Fatalf("%v %s", err, b)
	}
	m.Ensure()
	again, _ := m.AuthorityPEM()
	if !bytes.Equal(b, again) {
		t.Fatal("the authority changed")
	}
	h := httptest.NewRecorder()
	m.HandleAuthority(h, httptest.NewRequest(http.MethodGet, "/x", nil))
	if h.Code != http.StatusOK || h.Header().Get("Content-Type") != "application/x-x509-ca-cert" || !strings.Contains(h.Header().Get("Content-Disposition"), "peakroute-web-ca.crt") || !bytes.Equal(h.Body.Bytes(), b) {
		t.Fatalf("%d %v", h.Code, h.Header())
	}
	if strings.Contains(h.Body.String(), "PRIVATE") {
		t.Fatal("the key is public")
	}
}

func TestTheAPI(t *testing.T) {
	m := lab(t, router)
	m.Ensure()
	h := httptest.NewRecorder()
	m.HandleStatus(h, httptest.NewRequest(http.MethodGet, "/x", nil))
	var st Status
	if err := json.Unmarshal(h.Body.Bytes(), &st); err != nil || h.Code != 200 || st.Mode != "builtin" || st.Web == nil || h.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", h.Code, h.Body)
	}
	if strings.Contains(h.Body.String(), "PRIVATE") {
		t.Fatal("a key in the status")
	}
	for _, f := range []func(http.ResponseWriter, *http.Request){m.HandleStatus, m.HandleAuthority} {
		h = httptest.NewRecorder()
		f(h, httptest.NewRequest(http.MethodPost, "/x", nil))
		if h.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST: %d", h.Code)
		}
	}
	// names
	json := func(method, body string) *http.Request {
		r := httptest.NewRequest(method, "/x", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		return r
	}
	h = httptest.NewRecorder()
	m.HandleExtraNames(h, json(http.MethodPut, `{"names":["vpn.example.com"]}`))
	if h.Code != 200 || !containsS(leafOf(t).DNSNames, "vpn.example.com") {
		t.Fatalf("%d %s", h.Code, h.Body)
	}
	h = httptest.NewRecorder()
	m.HandleExtraNames(h, json(http.MethodPut, `{"names":["not a name"]}`))
	if h.Code != 400 {
		t.Fatalf("%d", h.Code)
	}
	for _, body := range []string{`{"nope":1}`, `[]`} {
		h = httptest.NewRecorder()
		m.HandleExtraNames(h, json(http.MethodPut, body))
		if h.Code != 400 {
			t.Errorf("%s: %d", body, h.Code)
		}
	}
	// upload
	good, goodKey, _, _ := makeCert(t, "a.example.com", "X", false, time.Now().Add(-time.Hour), time.Now().AddDate(0, 3, 0), nil, nil)
	post := func(cert, key []byte, omit string) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		if omit != "cert" {
			f, _ := w.CreateFormFile("cert", "c.crt")
			f.Write(cert)
		}
		if omit != "key" {
			f, _ := w.CreateFormFile("key", "c.key")
			f.Write(key)
		}
		w.Close()
		r := httptest.NewRequest(http.MethodPost, "/x", &buf)
		r.Header.Set("Content-Type", w.FormDataContentType())
		h := httptest.NewRecorder()
		m.HandleUpload(h, r)
		return h
	}
	if h := post(good, goodKey, "cert"); h.Code != 400 {
		t.Errorf("no cert: %d", h.Code)
	}
	if h := post(good, goodKey, "key"); h.Code != 400 {
		t.Errorf("no key: %d", h.Code)
	}
	if h := post(good, []byte("x"), ""); h.Code != 400 || Custom() {
		t.Errorf("bad key: %d", h.Code)
	}
	h = post(good, goodKey, "")
	if h.Code != 200 || !Custom() {
		t.Fatalf("%d %s", h.Code, h.Body)
	}
	h = httptest.NewRecorder()
	m.HandleUpload(h, httptest.NewRequest(http.MethodGet, "/x", nil))
	if h.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", h.Code)
	}
	// not a form at all
	h = httptest.NewRecorder()
	m.HandleUpload(h, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("x")))
	if h.Code != 400 {
		t.Errorf("%d", h.Code)
	}
}

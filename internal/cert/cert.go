// Package cert is the certificate of the router's web interface (docs/certificates.md).
//
// Two ways to have one:
//   - the built-in certificate: made by this router's own web authority (a certificate authority of its own, separate from the
//     VPN's), for the router's names and addresses, and renewed by itself. A browser warns until it trusts the authority; the page
//     offers the authority's certificate to install once on a computer or phone, and the warnings stop;
//   - a custom certificate: one the administrator uploaded (from a public authority, say). It is never replaced or renewed here.
//
// Changes apply at once: the server asks for its certificate on every new connection (GetCertificate), so nothing restarts.
package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"peakroute/internal/fsutil"
)

var (
	CertPath = "/etc/peakroute/certs/tls.crt"
	KeyPath  = "/etc/peakroute/certs/tls.key"
)

// EnableDevPaths keeps everything under dir (the development mode never touches the host's /etc).
func EnableDevPaths(dir string) {
	os.MkdirAll(dir, 0700)
	CertPath = dir + "/tls.crt"
	KeyPath = dir + "/tls.key"
}

const (
	org          = "PeakRoute OS"
	caCommonName = "PeakRoute OS Web CA"

	leafYears    = 1
	caYears      = 10
	renewBefore  = 30 * 24 * time.Hour
	maxExtraName = 10
)

func dir() string        { return filepath.Dir(CertPath) }
func caCertPath() string { return filepath.Join(dir(), "ca.crt") }
func caKeyPath() string  { return filepath.Join(dir(), "ca.key") }
func customMark() string { return filepath.Join(dir(), "custom") }
func extraPath() string  { return filepath.Join(dir(), "extra-names.json") }

// Names are what the built-in certificate must be good for: the router's names and its addresses on its own networks.
type Names struct {
	DNS []string
	IPs []net.IP
}

// Manager keeps the web interface's certificate.
type Manager struct {
	// Wanted says which names and addresses the router has right now (from its configuration). Nil: only "localhost".
	Wanted func() Names

	mu  sync.Mutex
	cur *tls.Certificate
}

func newKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }

func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("not a certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func readKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("not a key")
	}
	switch blk.Type {
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(blk.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		if e, ok := k.(*ecdsa.PrivateKey); ok {
			return e, nil
		}
	}
	return nil, errors.New("not an EC key")
}

func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func pemKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ensureAuthority returns the web authority, making it when there is none (or its files are not a pair).
func ensureAuthority() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	c, cerr := readCert(caCertPath())
	k, kerr := readKey(caKeyPath())
	if cerr == nil && kerr == nil && c.IsCA {
		if pub, ok := c.PublicKey.(*ecdsa.PublicKey); ok && pub.Equal(&k.PublicKey) && time.Now().Before(c.NotAfter) {
			return c, k, nil
		}
	}
	key, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	sn, err := serial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{Organization: []string{org}, CommonName: caCommonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(caYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kp, err := pemKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(dir(), 0755); err != nil {
		return nil, nil, err
	}
	// the key first: a certificate without its key is useless, a key without its certificate is only made again
	if err := fsutil.WriteFileAtomic(caKeyPath(), kp, 0600); err != nil {
		return nil, nil, err
	}
	if err := fsutil.WriteFileAtomic(caCertPath(), pemCert(der), 0644); err != nil {
		return nil, nil, err
	}
	c, err = x509.ParseCertificate(der)
	return c, key, err
}

func cleanNames(n Names, extra []string) (dns []string, ips []net.IP) {
	seen := map[string]bool{}
	addDNS := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s != "" && !seen[s] {
			seen[s] = true
			dns = append(dns, s)
		}
	}
	addIP := func(ip net.IP) {
		if ip == nil {
			return
		}
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		if s := ip.String(); !seen[s] {
			seen[s] = true
			ips = append(ips, ip)
		}
	}
	addDNS("localhost")
	for _, d := range n.DNS {
		addDNS(d)
	}
	for _, ip := range n.IPs {
		addIP(ip)
	}
	addIP(net.ParseIP("127.0.0.1"))
	addIP(net.ParseIP("::1"))
	for _, e := range extra {
		if ip := net.ParseIP(strings.TrimSpace(e)); ip != nil {
			addIP(ip)
		} else {
			addDNS(e)
		}
	}
	return dns, ips
}

// ValidExtraName reports whether text may be added to the certificate: a name or an address, nothing else.
func ValidExtraName(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if len(s) == 0 || len(s) > 253 || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// ExtraNames are the names and addresses the administrator added to the built-in certificate (a name from Dynamic DNS, say).
func ExtraNames() []string {
	b, err := os.ReadFile(extraPath())
	if err != nil {
		return []string{}
	}
	var list []string
	if json.Unmarshal(b, &list) != nil {
		return []string{}
	}
	return list
}

// SetExtraNames saves them (lower case, no duplicates).
func SetExtraNames(list []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, s := range list {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		if !ValidExtraName(s) {
			return nil, fmt.Errorf("%q is not a name or an address", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) > maxExtraName {
		return nil, fmt.Errorf("at most %d names", maxExtraName)
	}
	sort.Strings(out)
	b, _ := json.Marshal(out)
	if err := os.MkdirAll(dir(), 0755); err != nil {
		return nil, err
	}
	if err := fsutil.WriteFileAtomic(extraPath(), b, 0644); err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

func (m *Manager) wanted() ([]string, []net.IP) {
	var n Names
	if m.Wanted != nil {
		n = m.Wanted()
	}
	return cleanNames(n, ExtraNames())
}

// issue makes the built-in certificate for the names and writes it (and its key) in place.
func (m *Manager) issue() error {
	ca, caKey, err := ensureAuthority()
	if err != nil {
		return err
	}
	dns, ips := m.wanted()
	key, err := newKey()
	if err != nil {
		return err
	}
	sn, err := serial()
	if err != nil {
		return err
	}
	now := time.Now()
	cn := "PeakRoute OS"
	if len(dns) > 1 {
		cn = dns[1] // the first is "localhost"
	}
	notAfter := now.AddDate(leafYears, 0, 0)
	if notAfter.After(ca.NotAfter) {
		notAfter = ca.NotAfter
	}
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{Organization: []string{org}, CommonName: cn},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dns,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	kp, err := pemKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir(), 0755); err != nil {
		return err
	}
	// both files are staged first and swapped in back to back, so that a failed write cannot leave a mismatched pair
	sk, err := fsutil.StageFile(KeyPath, kp, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(sk)
	sc, err := fsutil.StageFile(CertPath, pemCert(der), 0644)
	if err != nil {
		return err
	}
	defer os.Remove(sc)
	if err := fsutil.CommitFile(sk, KeyPath); err != nil {
		return err
	}
	if err := fsutil.CommitFile(sc, CertPath); err != nil {
		return err
	}
	os.Remove(customMark())
	log.Printf("[+] Made the web interface's certificate for %s", strings.Join(append(append([]string{}, dns...), ipStrings(ips)...), ", "))
	return m.Reload()
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}

// Custom says whether the installed certificate is one the administrator uploaded.
func Custom() bool {
	if _, err := os.Stat(customMark()); err == nil {
		return true
	}
	// installed by an older version, which had no mark: not ours when it was not issued by an authority named like ours
	c, err := readCert(CertPath)
	if err != nil {
		return false
	}
	if len(c.Issuer.Organization) == 0 || c.Issuer.Organization[0] != org {
		return true
	}
	return false
}

// Ensure makes sure there is a good certificate: at start, and now and then. A custom certificate is left alone. The built-in one is
// made again when it is missing, damaged, not made by the web authority (the self-signed one of older versions), about to run out,
// or not good for a name or address the router now has.
func (m *Manager) Ensure() error {
	if _, err := os.Stat(CertPath); err == nil && Custom() {
		log.Println("[+] Custom TLS certificate in use; not renewing it.")
		return m.Reload()
	}
	leaf, err := readCert(CertPath)
	if err != nil {
		return m.issue()
	}
	ca, _, err := ensureAuthority()
	if err != nil {
		return err
	}
	if _, err := tls.LoadX509KeyPair(CertPath, KeyPath); err != nil {
		return m.issue()
	}
	if leaf.CheckSignatureFrom(ca) != nil || leaf.Issuer.String() != ca.Subject.String() {
		return m.issue()
	}
	if time.Until(leaf.NotAfter) < renewBefore {
		return m.issue()
	}
	if missing := m.missing(leaf); len(missing) > 0 {
		log.Printf("[!] The web interface's certificate lacks %s; making it again", strings.Join(missing, ", "))
		return m.issue()
	}
	return m.Reload()
}

func isLoopback(s string) bool { return s == "localhost" || s == "127.0.0.1" || s == "::1" }

// missing lists what the router has (names and addresses) that the certificate is not good for.
func (m *Manager) missing(leaf *x509.Certificate) []string {
	dns, ips := m.wanted()
	have := map[string]bool{}
	for _, d := range leaf.DNSNames {
		have[strings.ToLower(d)] = true
	}
	for _, ip := range leaf.IPAddresses {
		have[ip.String()] = true
	}
	var out []string
	for _, d := range dns {
		if !have[d] {
			out = append(out, d)
		}
	}
	for _, ip := range ips {
		if !have[ip.String()] {
			out = append(out, ip.String())
		}
	}
	return out
}

// Reload reads the installed pair into memory; new connections use it from then on.
func (m *Manager) Reload() error {
	pair, err := tls.LoadX509KeyPair(CertPath, KeyPath)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cur = &pair
	m.mu.Unlock()
	return nil
}

// GetCertificate is for tls.Config.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return nil, errors.New("no certificate")
	}
	return m.cur, nil
}

// Regenerate makes the built-in certificate now, for the names the router has (and takes the place of a custom one).
func (m *Manager) Regenerate() error { return m.issue() }

// Install puts an uploaded pair in place, if it is a good one: a certificate and the key that belongs to it, not out of date, and the
// first certificate of the file is the one for the server (a chain may follow).
func (m *Manager) Install(certPEM, keyPEM []byte) error {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return errors.New("the certificate or the private key is not valid, or they do not belong together")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return errors.New("the certificate cannot be read")
	}
	if time.Now().After(leaf.NotAfter) {
		return errors.New("the certificate has already run out")
	}
	if time.Now().Before(leaf.NotBefore.Add(-24 * time.Hour)) {
		return errors.New("the certificate is not valid yet")
	}
	if leaf.IsCA {
		return errors.New("that is the certificate of an authority, not of a server")
	}
	if err := os.MkdirAll(dir(), 0755); err != nil {
		return err
	}
	sk, err := fsutil.StageFile(KeyPath, keyPEM, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(sk)
	sc, err := fsutil.StageFile(CertPath, certPEM, 0644)
	if err != nil {
		return err
	}
	defer os.Remove(sc)
	// the mark first: a crash between the steps must leave a custom certificate marked as custom, never one that would be replaced
	if err := fsutil.WriteFileAtomic(customMark(), []byte("custom\n"), 0644); err != nil {
		return err
	}
	if err := fsutil.CommitFile(sk, KeyPath); err != nil {
		return err
	}
	if err := fsutil.CommitFile(sc, CertPath); err != nil {
		return err
	}
	log.Println("[+] Custom TLS certificate installed.")
	return m.Reload()
}

// AuthorityPEM is the web authority's certificate (public), to be installed on a computer or a phone. It is made if there is none.
func (m *Manager) AuthorityPEM() ([]byte, error) {
	if _, _, err := ensureAuthority(); err != nil {
		return nil, err
	}
	return os.ReadFile(caCertPath())
}

// ---- what the page shows -------------------------------------------------------------------------------------------

// Details describes a certificate.
type Details struct {
	Subject     string   `json:"subject"`
	Issuer      string   `json:"issuer"`
	NotBefore   int64    `json:"not_before"` // Unix milliseconds
	NotAfter    int64    `json:"not_after"`
	DaysLeft    int      `json:"days_left"`
	Names       []string `json:"names"`
	Fingerprint string   `json:"fingerprint"` // SHA-256, upper case hex with colons
}

func fingerprint(der []byte) string {
	h := sha256.Sum256(der)
	s := strings.ToUpper(hex.EncodeToString(h[:]))
	var parts []string
	for i := 0; i < len(s); i += 2 {
		parts = append(parts, s[i:i+2])
	}
	return strings.Join(parts, ":")
}

func describe(c *x509.Certificate) Details {
	names := append([]string{}, c.DNSNames...)
	names = append(names, ipStrings(c.IPAddresses)...)
	sub := c.Subject.CommonName
	if sub == "" && len(c.Subject.Organization) > 0 {
		sub = c.Subject.Organization[0]
	}
	iss := c.Issuer.CommonName
	if iss == "" && len(c.Issuer.Organization) > 0 {
		iss = c.Issuer.Organization[0]
	}
	return Details{Subject: sub, Issuer: iss, NotBefore: c.NotBefore.UnixMilli(), NotAfter: c.NotAfter.UnixMilli(),
		DaysLeft: int(time.Until(c.NotAfter).Hours() / 24), Names: names, Fingerprint: fingerprint(c.Raw)}
}

// Status is the page's data.
type Status struct {
	Mode       string   `json:"mode"` // "builtin" or "custom"
	Web        *Details `json:"web"`  // null: the certificate cannot be read
	Wanted     []string `json:"wanted"`
	Missing    []string `json:"missing"`     // names the router has that the certificate is not good for
	SelfSigned bool     `json:"self_signed"` // the certificate vouches for itself (it is not made by an authority)
	Extra      []string `json:"extra_names"`
	Authority  *Details `json:"authority"`
}

// Status says what is installed.
func (m *Manager) Status() Status {
	st := Status{Mode: "builtin", Extra: ExtraNames(), Wanted: []string{}, Missing: []string{}}
	if Custom() {
		st.Mode = "custom"
	}
	dns, ips := m.wanted()
	st.Wanted = append(append([]string{}, dns...), ipStrings(ips)...)
	if leaf, err := readCert(CertPath); err == nil {
		d := describe(leaf)
		st.Web = &d
		for _, n := range m.missing(leaf) {
			if !isLoopback(n) { // a certificate for the network's names is not wrong for lacking "localhost"
				st.Missing = append(st.Missing, n)
			}
		}
		st.SelfSigned = leaf.Subject.String() == leaf.Issuer.String()
	}
	if ca, err := readCert(caCertPath()); err == nil {
		d := describe(ca)
		st.Authority = &d
	}
	if st.Missing == nil {
		st.Missing = []string{}
	}
	return st
}

// DescribePEM describes the certificate in PEM text (the first one, when there is a chain).
func DescribePEM(b []byte) (Details, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return Details{}, errors.New("not a certificate")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return Details{}, err
	}
	return describe(c), nil
}

// ReloadAndCheck re-reads the installed files and makes sure they are good (made again when they are not): after a restore.
func (m *Manager) ReloadAndCheck() error { return m.Ensure() }

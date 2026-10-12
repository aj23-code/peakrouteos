// Package pki is the router's built-in certificate authority for OpenVPN: it makes the authority, signs server and client
// certificates, keeps the list of revoked ones and writes the files OpenVPN reads. Keys are ECDSA P-256 (small, fast, and
// understood by every current OpenVPN). Everything is PEM text, so that it can live in network.json; the private keys in it are
// secrets like every other (docs/vpn.md, section 11).
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// Lifetimes: the authority and the server outlive any device; a device's certificate is renewed by making a new file.
const (
	CAYears     = 20
	ServerYears = 10
	ClientYears = 5
)

// Info is what is shown about a certificate.
type Info struct {
	CommonName string
	Serial     string // hexadecimal
	NotBefore  time.Time
	NotAfter   time.Time
	IsCA       bool
}

func newKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }

func serial() (*big.Int, error) {
	// 127 random bits: positive, and never the same twice
	max := new(big.Int).Lsh(big.NewInt(1), 127)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

func keyPEM(k *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

func certPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// ParseCert reads one certificate from PEM text.
func ParseCert(text string) (*x509.Certificate, error) {
	b, rest := pem.Decode([]byte(strings.TrimSpace(text)))
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("not a certificate")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("more than one certificate")
	}
	return x509.ParseCertificate(b.Bytes)
}

// ParseKey reads one EC private key from PEM text (also accepts the PKCS#8 form).
func ParseKey(text string) (*ecdsa.PrivateKey, error) {
	b, rest := pem.Decode([]byte(strings.TrimSpace(text)))
	if b == nil || strings.TrimSpace(string(rest)) != "" {
		return nil, errors.New("not a private key")
	}
	switch b.Type {
	case "EC PRIVATE KEY":
		return x509.ParseECPrivateKey(b.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
		if err != nil {
			return nil, err
		}
		if e, ok := k.(*ecdsa.PrivateKey); ok {
			return e, nil
		}
	}
	return nil, errors.New("not an EC private key")
}

// Describe returns what is shown about a certificate.
func Describe(text string) (Info, error) {
	c, err := ParseCert(text)
	if err != nil {
		return Info{}, err
	}
	return Info{CommonName: c.Subject.CommonName, Serial: c.SerialNumber.Text(16), NotBefore: c.NotBefore, NotAfter: c.NotAfter, IsCA: c.IsCA}, nil
}

// NewCA makes a certificate authority named cn.
func NewCA(cn string, now time.Time) (cert, key string, err error) {
	k, err := newKey()
	if err != nil {
		return "", "", err
	}
	sn, err := serial()
	if err != nil {
		return "", "", err
	}
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"PeakRoute OS"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(CAYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		return "", "", err
	}
	kp, err := keyPEM(k)
	if err != nil {
		return "", "", err
	}
	return certPEM(der), kp, nil
}

func loadCA(caCert, caKey string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	c, err := ParseCert(caCert)
	if err != nil {
		return nil, nil, fmt.Errorf("the authority's certificate: %w", err)
	}
	if !c.IsCA {
		return nil, nil, errors.New("the authority's certificate is not one for an authority")
	}
	k, err := ParseKey(caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("the authority's key: %w", err)
	}
	if pub, ok := c.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&k.PublicKey) {
		return nil, nil, errors.New("the authority's key does not belong to its certificate")
	}
	return c, k, nil
}

// Issue signs a certificate for cn with a new key. A server certificate may be used to serve, a client one to connect, never both.
func Issue(caCert, caKey, cn string, server bool, now time.Time) (cert, key string, err error) {
	ca, ck, err := loadCA(caCert, caKey)
	if err != nil {
		return "", "", err
	}
	k, err := newKey()
	if err != nil {
		return "", "", err
	}
	sn, err := serial()
	if err != nil {
		return "", "", err
	}
	years, usage := ClientYears, x509.ExtKeyUsageClientAuth
	if server {
		years, usage = ServerYears, x509.ExtKeyUsageServerAuth
	}
	tpl := &x509.Certificate{
		SerialNumber:          sn,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"PeakRoute OS"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(years, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{usage},
		BasicConstraintsValid: true,
	}
	if notAfter := ca.NotAfter; tpl.NotAfter.After(notAfter) {
		tpl.NotAfter = notAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, ck)
	if err != nil {
		return "", "", err
	}
	kp, err := keyPEM(k)
	if err != nil {
		return "", "", err
	}
	return certPEM(der), kp, nil
}

// Verify checks that cert was signed by the authority, is in its dates, and may be used for what it is for.
func Verify(caCert, cert string, server bool, now time.Time) error {
	ca, err := ParseCert(caCert)
	if err != nil {
		return fmt.Errorf("the authority's certificate: %w", err)
	}
	c, err := ParseCert(cert)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	usage := x509.ExtKeyUsageClientAuth
	if server {
		usage = x509.ExtKeyUsageServerAuth
	}
	_, err = c.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{usage}})
	return err
}

// MatchesKey reports whether the private key belongs to the certificate.
func MatchesKey(cert, key string) bool {
	c, err := ParseCert(cert)
	if err != nil {
		return false
	}
	k, err := ParseKey(key)
	if err != nil {
		return false
	}
	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	return ok && pub.Equal(&k.PublicKey)
}

// Revoked is a certificate that must no longer be accepted.
type Revoked struct {
	Serial string // hexadecimal, as in Info
	At     time.Time
}

// NewCRL writes the list of revoked certificates, signed by the authority. number rises with every list that is made.
func NewCRL(caCert, caKey string, revoked []Revoked, number int64, now time.Time) (string, error) {
	ca, ck, err := loadCA(caCert, caKey)
	if err != nil {
		return "", err
	}
	var entries []x509.RevocationListEntry
	for _, r := range revoked {
		n, ok := new(big.Int).SetString(r.Serial, 16)
		if !ok {
			return "", fmt.Errorf("%q is not a serial number", r.Serial)
		}
		entries = append(entries, x509.RevocationListEntry{SerialNumber: n, RevocationTime: r.At})
	}
	tpl := &x509.RevocationList{
		Number:                    big.NewInt(number),
		ThisUpdate:                now.Add(-time.Hour),
		NextUpdate:                now.AddDate(CAYears, 0, 0), // OpenVPN refuses a list that has run out: this one is replaced when it changes, not on a schedule
		RevokedCertificateEntries: entries,
	}
	der, err := x509.CreateRevocationList(rand.Reader, tpl, ca, ck)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})), nil
}

// CRLSerials lists the serial numbers of a revocation list.
func CRLSerials(text string) ([]string, error) {
	b, _ := pem.Decode([]byte(strings.TrimSpace(text)))
	if b == nil || b.Type != "X509 CRL" {
		return nil, errors.New("not a revocation list")
	}
	l, err := x509.ParseRevocationList(b.Bytes)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range l.RevokedCertificateEntries {
		out = append(out, e.SerialNumber.Text(16))
	}
	return out, nil
}

// NewTLSCryptKey makes the shared key that protects OpenVPN's control channel (the "tls-crypt" key): 2048 random bits in the file
// format OpenVPN reads.
func NewTLSCryptKey() (string, error) {
	raw := make([]byte, 256)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	h := hex.EncodeToString(raw)
	var b strings.Builder
	b.WriteString("#\n# 2048 bit OpenVPN static key\n#\n-----BEGIN OpenVPN Static key V1-----\n")
	for i := 0; i < len(h); i += 32 {
		b.WriteString(h[i:i+32] + "\n")
	}
	b.WriteString("-----END OpenVPN Static key V1-----\n")
	return b.String(), nil
}

// ValidTLSCryptKey reports whether text is a key in that format.
func ValidTLSCryptKey(text string) bool {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var hexLines []string
	in, closed := false, false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		switch {
		case l == "-----BEGIN OpenVPN Static key V1-----":
			in = true
		case l == "-----END OpenVPN Static key V1-----":
			closed = in
			in = false
		case in:
			hexLines = append(hexLines, l)
		case strings.HasPrefix(l, "#") || l == "":
		default:
			return false
		}
	}
	if !closed || len(hexLines) != 16 {
		return false
	}
	for _, l := range hexLines {
		if len(l) != 32 {
			return false
		}
		if _, err := hex.DecodeString(l); err != nil {
			return false
		}
	}
	return true
}

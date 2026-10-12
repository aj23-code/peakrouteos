package pki

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func TestTheAuthorityIsAnAuthority(t *testing.T) {
	cert, key, err := NewCA("PeakRoute CA", now)
	if err != nil {
		t.Fatal(err)
	}
	info, err := Describe(cert)
	if err != nil || !info.IsCA || info.CommonName != "PeakRoute CA" || info.NotAfter.Year() != 2046 || !info.NotBefore.Before(now) {
		t.Fatalf("%+v %v", info, err)
	}
	if !MatchesKey(cert, key) {
		t.Fatal("the key is not the authority's")
	}
	if !strings.HasPrefix(key, "-----BEGIN EC PRIVATE KEY-----") {
		t.Fatalf("%q", key[:40])
	}
	// two authorities are never the same
	c2, k2, _ := NewCA("PeakRoute CA", now)
	if c2 == cert || k2 == key || MatchesKey(cert, k2) {
		t.Fatal("two authorities are alike")
	}
}

func TestCertificatesAreSignedForWhatTheyAreFor(t *testing.T) {
	ca, ck, _ := NewCA("CA", now)
	sc, sk, err := Issue(ca, ck, "router", true, now)
	if err != nil {
		t.Fatal(err)
	}
	cc, kk, err := Issue(ca, ck, "alec-phone", false, now)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchesKey(sc, sk) || !MatchesKey(cc, kk) || MatchesKey(sc, kk) || MatchesKey(cc, sk) {
		t.Fatal("keys and certificates are mixed up")
	}
	if err := Verify(ca, sc, true, now); err != nil {
		t.Fatalf("the server certificate: %v", err)
	}
	if err := Verify(ca, cc, false, now); err != nil {
		t.Fatalf("the client certificate: %v", err)
	}
	// a client cannot pass for a server and the other way round
	if Verify(ca, cc, true, now) == nil || Verify(ca, sc, false, now) == nil {
		t.Fatal("the use is not checked")
	}
	// another authority's certificates are not ours
	other, ok, _ := NewCA("CA", now)
	oc, _, _ := Issue(other, ok, "alec-phone", false, now)
	if Verify(ca, oc, false, now) == nil {
		t.Fatal("a certificate of another authority is accepted")
	}
	// dates
	if Verify(ca, cc, false, now.AddDate(ClientYears, 0, 1)) == nil {
		t.Fatal("an expired certificate is accepted")
	}
	if Verify(ca, cc, false, now.AddDate(0, 0, -1)) == nil {
		t.Fatal("a certificate that is not valid yet is accepted")
	}
	// lifetimes: a client's is shorter than a server's, neither longer than the authority's
	ci, _ := Describe(cc)
	si, _ := Describe(sc)
	if ci.CommonName != "alec-phone" || ci.NotAfter.Year() != 2031 || si.NotAfter.Year() != 2036 || ci.Serial == si.Serial || ci.Serial == "" {
		t.Fatalf("%+v %+v", ci, si)
	}
	short, sk2, _ := NewCA("Short", now.AddDate(-19, 0, 0)) // an authority with one year left
	sh, _, err := Issue(short, sk2, "x", true, now)
	if err != nil {
		t.Fatal(err)
	}
	if i, _ := Describe(sh); i.NotAfter.After(now.AddDate(1, 0, 1)) {
		t.Fatalf("a certificate outlives its authority: %v", i.NotAfter)
	}
}

func TestIssuingNeedsTheRightAuthority(t *testing.T) {
	ca, ck, _ := NewCA("CA", now)
	other, ok, _ := NewCA("CA", now)
	leaf, lk, _ := Issue(ca, ck, "leaf", false, now)
	for name, args := range map[string][2]string{
		"a key of another authority": {ca, ok},
		"not an authority":           {leaf, lk},
		"no certificate":             {"", ck},
		"no key":                     {ca, ""},
		"garbage":                    {"x", "y"},
	} {
		if _, _, err := Issue(args[0], args[1], "x", false, now); err == nil {
			t.Errorf("%s: a certificate was issued", name)
		}
	}
	_ = other
}

func TestTheRevocationList(t *testing.T) {
	ca, ck, _ := NewCA("CA", now)
	c1, _, _ := Issue(ca, ck, "one", false, now)
	c2, _, _ := Issue(ca, ck, "two", false, now)
	i1, _ := Describe(c1)
	i2, _ := Describe(c2)
	empty, err := NewCRL(ca, ck, nil, 1, now)
	if err != nil || !strings.HasPrefix(empty, "-----BEGIN X509 CRL-----") {
		t.Fatalf("%v %q", err, empty)
	}
	if s, err := CRLSerials(empty); err != nil || len(s) != 0 {
		t.Fatalf("%v %v", s, err)
	}
	list, err := NewCRL(ca, ck, []Revoked{{i1.Serial, now}, {i2.Serial, now}}, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	s, err := CRLSerials(list)
	if err != nil || len(s) != 2 || s[0] != i1.Serial || s[1] != i2.Serial {
		t.Fatalf("%v %v", s, err)
	}
	if _, err := NewCRL(ca, ck, []Revoked{{"zz", now}}, 3, now); err == nil {
		t.Fatal("a serial that is not a number")
	}
	if _, err := CRLSerials("nope"); err == nil {
		t.Fatal("not a list")
	}
	if _, err := NewCRL("x", ck, nil, 1, now); err == nil {
		t.Fatal("a list made without the authority")
	}
}

func TestTheControlChannelKey(t *testing.T) {
	k, err := NewTLSCryptKey()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidTLSCryptKey(k) || !strings.Contains(k, "-----BEGIN OpenVPN Static key V1-----\n") || strings.Count(k, "\n") != 21 {
		t.Fatalf("%q", k)
	}
	k2, _ := NewTLSCryptKey()
	if k == k2 {
		t.Fatal("two keys are alike")
	}
	for name, bad := range map[string]string{
		"empty":        "",
		"no end":       strings.Replace(k, "-----END OpenVPN Static key V1-----", "", 1),
		"a short line": strings.Replace(k, "\n", "x\n", 5),
		"not hex":      strings.Replace(k, k[strings.Index(k, "V1-----\n")+8:strings.Index(k, "V1-----\n")+10], "zz", 1),
		"extra text":   "hello\n" + k,
		"short":        "-----BEGIN OpenVPN Static key V1-----\n00112233445566778899aabbccddeeff\n-----END OpenVPN Static key V1-----\n",
	} {
		if ValidTLSCryptKey(bad) {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestParsingIsStrict(t *testing.T) {
	ca, ck, _ := NewCA("CA", now)
	if _, err := ParseCert(ca + ca); err == nil {
		t.Error("two certificates in one")
	}
	if _, err := ParseCert(ck); err == nil {
		t.Error("a key as a certificate")
	}
	if _, err := ParseKey(ca); err == nil {
		t.Error("a certificate as a key")
	}
	if _, err := ParseKey(ck + ck); err == nil {
		t.Error("two keys in one")
	}
	if MatchesKey("x", ck) || MatchesKey(ca, "x") {
		t.Error("garbage matches")
	}
}

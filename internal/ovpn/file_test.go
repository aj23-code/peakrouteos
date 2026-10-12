package ovpn

import (
	"strings"
	"testing"
	"time"

	"peakroute/internal/pki"
)

type material struct{ ca, cert, key, tc string }

func mat(t *testing.T) material {
	t.Helper()
	ca, ck, _ := pki.NewCA("CA", time.Now())
	cert, key, err := pki.Issue(ca, ck, "ra-c1", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tc, _ := pki.NewTLSCryptKey()
	return material{ca, cert, key, tc}
}

func params(m material) Params {
	return Params{Name: "Anna phone", Host: "vpn.example.com", Port: 1194, Proto: "udp", CA: m.ca, Cert: m.cert, Key: m.key, TLSCrypt: m.tc}
}

func TestAFileIsWrittenAndReadBack(t *testing.T) {
	m := mat(t)
	p := params(m)
	p.Tunnel, p.Networks, p.RouteNoPull = "10.98.1.2/29", []string{"192.168.1.0/24", "10.30.0.0/24"}, true
	text, err := Write(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# PeakRoute OS OpenVPN file: Anna phone\n", "client\n", "dev tun\n", "proto udp\n", "remote vpn.example.com 1194\n", "nobind\n", "route-nopull\n",
		"remote-cert-tls server\n", "<ca>\n-----BEGIN CERTIFICATE-----", "<cert>\n", "<key>\n-----BEGIN EC PRIVATE KEY-----", "<tls-crypt>\n-----BEGIN OpenVPN Static key V1-----", "# peakroute-tunnel 10.98.1.2/29\n",
		"# peakroute-networks 192.168.1.0/24,10.30.0.0/24\n"} {
		if !strings.Contains(text, want) {
			t.Errorf("the file lacks %q:\n%s", want, text)
		}
	}
	got, err := Read(text)
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "vpn.example.com" || got.Port != 1194 || got.Proto != "udp" || got.CA != m.ca || got.Cert != m.cert || got.Key != m.key || !pki.ValidTLSCryptKey(got.TLSCrypt) ||
		got.Tunnel != "10.98.1.2/29" || len(got.Networks) != 2 || got.Networks[1] != "10.30.0.0/24" {
		t.Fatalf("%+v", got)
	}
	// a device's file has none of the site hints and takes the router's routes
	p.Tunnel, p.Networks, p.RouteNoPull = "", nil, false
	text, _ = Write(p)
	if strings.Contains(text, "peakroute-") || strings.Contains(text, "route-nopull") {
		t.Errorf("%s", text)
	}
}

func TestADeviceWithItsOwnKeyGetsAPlaceForIt(t *testing.T) {
	m := mat(t)
	p := params(m)
	p.Key = ""
	text, err := Write(p)
	if err != nil || !strings.Contains(text, "Paste the private key") || !strings.Contains(text, "<key>\n</key>") || strings.Contains(text, "PRIVATE KEY") {
		t.Fatalf("%v\n%s", err, text)
	}
	// ... which is no file for a tunnel until the key is in
	if _, err := Read(text); err == nil {
		t.Error("a file without its key was read")
	}
}

func TestNothingUnsafeIsWrittenIntoAFile(t *testing.T) {
	m := mat(t)
	for name, edit := range map[string]func(*Params){
		"a certificate with text after": func(p *Params) { p.Cert += "</cert>\nup /bin/sh\n" },
		"no authority":                  func(p *Params) { p.CA = "" },
		"a key that is garbage":         func(p *Params) { p.Key = "x" },
		"a bad control channel key":     func(p *Params) { p.TLSCrypt = "x" },
		"a protocol that is a command":  func(p *Params) { p.Proto = "udp\nup /bin/sh" },
		"a host that is a command":      func(p *Params) { p.Host = "a.example\nup /bin/sh" },
		"no port":                       func(p *Params) { p.Port = 0 },
	} {
		p := params(m)
		edit(&p)
		if text, err := Write(p); err == nil && strings.Contains(text, "up /bin/sh") {
			t.Errorf("%s: %s", name, text)
		} else if err == nil && name != "a certificate with text after" {
			t.Errorf("%s was written", name)
		}
	}
	// names and hints cannot leave their comment line
	p := params(m)
	p.Name = "Anna\nup /bin/sh"
	p.Tunnel = "10.0.0.1/30\nup /bin/sh"
	p.Networks = []string{"10.0.0.0/24\nup /bin/sh"}
	text, _ := Write(p)
	if strings.Contains(text, "\nup /bin/sh") {
		t.Errorf("a name started a line:\n%s", text)
	}
}

func TestReadingOnlyTakesWhatItNeeds(t *testing.T) {
	m := mat(t)
	base, _ := Write(params(m))
	// other lines are ignored, not copied: scripts, plugins, anything
	evil := "script-security 2\nup /bin/sh\nplugin /tmp/x.so\n" + base + "\nroute-up /bin/sh\n"
	got, err := Read(evil)
	if err != nil || got.Host != "vpn.example.com" {
		t.Fatalf("%v %+v", err, got)
	}
	// other spellings of the protocol, the port on its own line
	text := strings.Replace(base, "proto udp\nremote vpn.example.com 1194\n", "remote vpn.example.com 443 tcp-client\n", 1)
	if got, err := Read(text); err != nil || got.Proto != "tcp" || got.Port != 443 {
		t.Fatalf("%v %+v", err, got)
	}
	text = strings.Replace(base, "proto udp\nremote vpn.example.com 1194\n", "proto tcp4-client\nremote vpn.example.com\n", 1)
	if got, err := Read(text); err != nil || got.Proto != "tcp" || got.Port != 1194 {
		t.Fatalf("%v %+v", err, got)
	}
	// a Windows file
	if _, err := Read(strings.ReplaceAll(base, "\n", "\r\n")); err != nil {
		t.Fatalf("CRLF: %v", err)
	}
	for name, bad := range map[string]string{
		"nothing":                     "",
		"no remote":                   strings.Replace(base, "remote vpn.example.com 1194\n", "", 1),
		"a bad host":                  strings.Replace(base, "remote vpn.example.com 1194", "remote bad_host!! 1194", 1),
		"a bad port":                  strings.Replace(base, "remote vpn.example.com 1194", "remote vpn.example.com 99999", 1),
		"a bad protocol":              strings.Replace(base, "proto udp", "proto icmp", 1),
		"a block that is not shut":    strings.Replace(base, "</ca>", "", 1),
		"two authorities":             base + "<ca>\n" + m.ca + "</ca>\n",
		"no key":                      strings.Replace(base, "-----BEGIN EC PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----", 1),
		"no control channel key":      strings.Replace(base, "<tls-crypt>", "<tls-auth>", 1),
		"a certificate that is a key": strings.Replace(base, "<cert>\n", "<cert>\n"+m.key+"</cert>\n<cert>\n", 1),
		"too big":                     strings.Repeat("# x\n", 40000) + base,
	} {
		if _, err := Read(bad); err == nil {
			t.Errorf("%s was read", name)
		}
	}
}

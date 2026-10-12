package wg

import (
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestKeysFromTheCurve25519Vectors(t *testing.T) {
	// RFC 7748, section 6.1: Alice's private key and the public key that goes with it (WireGuard keys are Curve25519 keys)
	priv := base64.StdEncoding.EncodeToString(mustHex(t, "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a"))
	want := base64.StdEncoding.EncodeToString(mustHex(t, "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a"))
	got, err := PublicKey(priv)
	if err != nil || got != want {
		t.Fatalf("%q %v, want %q", got, err, want)
	}
	// Bob's
	priv = base64.StdEncoding.EncodeToString(mustHex(t, "5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb"))
	want = base64.StdEncoding.EncodeToString(mustHex(t, "de9edb7d7b7dc1b4d35b61c2ece435373f8343c85b78674dadfc7e146f882b4f"))
	if got, err := PublicKey(priv); err != nil || got != want {
		t.Fatalf("%q %v, want %q", got, err, want)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := range b {
		var v byte
		for _, c := range s[2*i : 2*i+2] {
			v <<= 4
			switch {
			case c >= '0' && c <= '9':
				v |= byte(c - '0')
			default:
				v |= byte(c-'a') + 10
			}
		}
		b[i] = v
	}
	return b
}

func TestGeneratedKeysAreKeys(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		priv, pub, err := GenerateKeyPair()
		if err != nil || !ValidKey(priv) || !ValidKey(pub) || priv == pub || seen[priv] {
			t.Fatalf("%q %q %v", priv, pub, err)
		}
		seen[priv] = true
		// the bits Curve25519 fixes
		b, _ := base64.StdEncoding.DecodeString(priv)
		if b[0]&7 != 0 || b[31]&128 != 0 || b[31]&64 == 0 {
			t.Fatalf("the private key is not clamped: %x", b)
		}
		if again, _ := PublicKey(priv); again != pub {
			t.Fatal("the public key is not the key of the private one")
		}
	}
	psk, err := GeneratePresharedKey()
	if err != nil || !ValidKey(psk) {
		t.Fatalf("%q %v", psk, err)
	}
	if other, _ := GeneratePresharedKey(); other == psk {
		t.Fatal("two preshared keys are the same")
	}
}

func TestKeysAgreeWithTheWgTool(t *testing.T) {
	wg := os.Getenv("WG_BIN")
	if wg == "" {
		if p, err := exec.LookPath("wg"); err == nil {
			wg = p
		} else if _, err := os.Stat("/tmp/wgdeb/root/usr/bin/wg"); err == nil {
			wg = "/tmp/wgdeb/root/usr/bin/wg"
		} else {
			t.Skip("no wg tool here")
		}
	}
	priv, pub, _ := GenerateKeyPair()
	cmd := exec.Command(wg, "pubkey")
	cmd.Stdin = strings.NewReader(priv + "\n")
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != pub {
		t.Fatalf("wg says %q (%v), we say %q", out, err, pub)
	}
	// and a key made by wg is one we accept and derive from
	gen, err := exec.Command(wg, "genkey").Output()
	if err != nil {
		t.Fatal(err)
	}
	k := strings.TrimSpace(string(gen))
	if !ValidKey(k) {
		t.Fatalf("wg made %q", k)
	}
	cmd = exec.Command(wg, "pubkey")
	cmd.Stdin = strings.NewReader(k)
	want, _ := cmd.Output()
	if got, err := PublicKey(k); err != nil || got != strings.TrimSpace(string(want)) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestValidKey(t *testing.T) {
	good := "xaK9u1tb7qXV24tincBDPeKK62WUd4Msi2eHllo+blM="
	if !ValidKey(good) || !ValidKey(strings.Repeat("A", 43)+"=") {
		t.Fatal("a good key (the all-zero key is a key, if a useless one)")
	}
	for _, bad := range []string{"", "short", good[:43], good + "=", strings.Replace(good, "=", "A", 1), strings.Repeat("A", 44), good[:43] + "?", " " + good[1:], good + "\n", strings.Repeat("A", 42) + "B="} {
		if ValidKey(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
	// a non-canonical encoding (the unused bits of the last character are not zero) is refused
	if ValidKey(strings.Repeat("A", 42) + "B=") {
		t.Error("a non-canonical key was accepted")
	}
	if _, err := PublicKey("nope"); err == nil {
		t.Error("PublicKey of nothing")
	}
}

const sampleDump = "wgra\tPRIVATESECRET\tSERVERPUB=\t51821\toff\n" +
	"wgra\tCLIENT1PUB=\tPSKSECRET\t203.0.113.7:51000\t10.8.0.2/32\t1791729000\t1234\t5678\t25\n" +
	"wgra\tCLIENT2PUB=\t(none)\t(none)\t10.8.0.3/32\t0\t0\t0\toff\n" +
	"wgs1\tPRIVATE2\tSITEPUB=\t51830\toff\n" +
	"wgs1\tPEERPUB=\t(none)\t198.51.100.4:51830\t10.20.0.0/24,10.99.1.2/32\t1791729100\t99\t100\t25\n"

func TestParseDump(t *testing.T) {
	ifs := ParseDump(sampleDump)
	if len(ifs) != 2 || ifs[0].Name != "wgra" || ifs[0].PublicKey != "SERVERPUB=" || ifs[0].ListenPort != 51821 || len(ifs[0].Peers) != 2 || ifs[1].Name != "wgs1" || len(ifs[1].Peers) != 1 {
		t.Fatalf("%+v", ifs)
	}
	p := ifs[0].Peers[0]
	if p.PublicKey != "CLIENT1PUB=" || p.Endpoint != "203.0.113.7:51000" || len(p.AllowedIPs) != 1 || p.AllowedIPs[0] != "10.8.0.2/32" || !p.LatestHandshake.Equal(time.Unix(1791729000, 0)) ||
		p.Received != 1234 || p.Sent != 5678 || p.Keepalive != 25 {
		t.Fatalf("%+v", p)
	}
	q := ifs[0].Peers[1]
	if q.Endpoint != "" || !q.LatestHandshake.IsZero() || q.Received != 0 || q.Keepalive != 0 {
		t.Fatalf("%+v", q)
	}
	if got := ifs[1].Peers[0].AllowedIPs; len(got) != 2 || got[0] != "10.20.0.0/24" || got[1] != "10.99.1.2/32" {
		t.Fatalf("%v", got)
	}
	// no secret is kept anywhere
	if strings.Contains(strings.Join([]string{ifs[0].PublicKey, p.PublicKey, p.Endpoint}, " "), "SECRET") {
		t.Fatal("a secret was kept")
	}
	for _, none := range []string{"", "\n", "garbage", "a\tb\tc"} {
		if got := ParseDump(none); len(got) != 0 {
			t.Errorf("%q gave %+v", none, got)
		}
	}
	// a peer line without its interface is ignored
	if got := ParseDump("wgx\tp=\t(none)\t(none)\t(none)\t0\t0\t0\toff\n"); len(got) != 0 {
		t.Errorf("%+v", got)
	}
}

func TestAClientFile(t *testing.T) {
	got := ClientConfig(ClientParams{Name: "Anna's phone\nPostUp = reboot", PrivateKey: "PRIV=", Address: "10.8.0.2/32", DNS: "10.8.0.1", ServerPublicKey: "SERVERPUB=", PresharedKey: "PSK=",
		AllowedIPs: []string{"192.168.1.0/24", "10.20.0.0/24"}, Endpoint: "vpn.example.com:51821", Keepalive: 25})
	want := "# PeakRoute OS remote access: Anna's phone?PostUp = reboot\n[Interface]\nPrivateKey = PRIV=\nAddress = 10.8.0.2/32\nDNS = 10.8.0.1\n\n[Peer]\nPublicKey = SERVERPUB=\nPresharedKey = PSK=\nAllowedIPs = 192.168.1.0/24, 10.20.0.0/24\nEndpoint = vpn.example.com:51821\nPersistentKeepalive = 25\n"
	if got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
	// no DNS, no preshared key, no keepalive: those lines are left out
	got = ClientConfig(ClientParams{Name: "x", PrivateKey: "P=", Address: "10.8.0.3/32", ServerPublicKey: "S=", AllowedIPs: []string{"0.0.0.0/0"}, Endpoint: "1.2.3.4:5"})
	for _, absent := range []string{"DNS", "PresharedKey", "PersistentKeepalive"} {
		if strings.Contains(got, absent) {
			t.Errorf("%s is in %q", absent, got)
		}
	}
	if !strings.Contains(got, "AllowedIPs = 0.0.0.0/0\n") {
		t.Errorf("%q", got)
	}
}

package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

// the key the tests sign their releases with
var testPub, testPriv = func() (ed25519.PublicKey, ed25519.PrivateKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return pub, priv
}()

func TestASignatureNamesTheFileAndItsChecksum(t *testing.T) {
	keys := []ed25519.PublicKey{testPub}
	sig := Sign(testPriv, "peakroute-os-amd64-generic-v1.0.0.update", sum([]byte("a")))
	if err := VerifySignature(keys, "peakroute-os-amd64-generic-v1.0.0.update", sum([]byte("a")), sig); err != nil {
		t.Fatal(err)
	}
	// another file's checksum, another file's name (a signed file passed off as another version, product or board), another key
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for name, c := range map[string]struct {
		keys []ed25519.PublicKey
		file string
		sum  string
		sig  string
	}{
		"another checksum": {keys, "peakroute-os-amd64-generic-v1.0.0.update", sum([]byte("b")), sig},
		"another name":     {keys, "peakroute-os-amd64-generic-v1.0.1.update", sum([]byte("a")), sig},
		"another key":      {[]ed25519.PublicKey{other}, "peakroute-os-amd64-generic-v1.0.0.update", sum([]byte("a")), sig},
		"no keys":          {nil, "peakroute-os-amd64-generic-v1.0.0.update", sum([]byte("a")), sig},
		"not base64":       {keys, "peakroute-os-amd64-generic-v1.0.0.update", sum([]byte("a")), "not a signature!"},
		"too short":        {keys, "peakroute-os-amd64-generic-v1.0.0.update", sum([]byte("a")), "AAAA"},
		"empty":            {keys, "peakroute-os-amd64-generic-v1.0.0.update", sum([]byte("a")), ""},
	} {
		if err := VerifySignature(c.keys, c.file, c.sum, c.sig); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAKeyPairIsWrittenAndReadBack(t *testing.T) {
	pub, priv, err := GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := ParsePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := ParsePrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySignature([]ed25519.PublicKey{pk}, "f", sum([]byte("x")), Sign(sk, "f", sum([]byte("x")))); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "x", pub + pub} {
		if _, err := ParsePrivateKey(bad); err == nil {
			t.Errorf("private key %q accepted", bad)
		}
	}
	if _, err := ParsePublicKey(priv + priv); err == nil {
		t.Error("a long string passed for a public key")
	}
}

func TestTheBuiltInReleaseKeyIsAValidKey(t *testing.T) {
	if len(ReleaseKeys()) == 0 {
		t.Fatal("no release key is built in: every download would be refused (make one with: go run ./cmd/peakroute-sign keygen)")
	}
	if len(ReleaseKeys()) != len(releaseKeys) {
		t.Fatal("a built-in release key cannot be read")
	}
}

func TestAReleaseThatIsNotSignedIsNotOffered(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = `[{"tag_name":"v0.10.0","draft":false,"prerelease":false,"assets":[{"name":"peakroute-os-v0.10.0.update","browser_download_url":"@@/dl/v0.10.0.update"},{"name":"peakroute-os-v0.10.0.update.sha256","browser_download_url":"@@/dl/v0.10.0.update.sha256"}]}]`
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	if _, err := l.s.FindRelease(context.Background(), "owner/repo", false, "v0.9.5-alpha"); err == nil || !strings.Contains(err.Error(), "signed") {
		t.Fatalf("an unsigned release was offered: %v", err)
	}
}

func TestADownloadSignedByAnotherKeyIsNeverMadeStartable(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = releasesJSON
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	// the bundle, its checksum and its signature were all replaced by someone with access to the release but not to the key
	_, attacker, _ := ed25519.GenerateKey(rand.Reader)
	evil := bundleOf(t, "v0.10.0", fakeRootfs(1<<20), nil)
	publish(g, "v0.10.0", evil)
	g.files["/dl/v0.10.0.update.sig"] = []byte(Sign(attacker, "peakroute-os-v0.10.0.update", sum(evil)))
	l.s.CheckNow(context.Background())
	if err := l.s.Install("v0.10.0"); err != nil {
		t.Fatal(err)
	}
	j := waitJob(t, l)
	if j.Phase != "failed" || !strings.Contains(j.Error, "signature") {
		t.Fatalf("job: %+v", j)
	}
	if l.called("tune2fs") != 0 {
		t.Fatal("the slot was named although the signature was wrong")
	}
}

func TestADownloadWhoseSignatureFileIsMissingFailsBeforeAnythingIsWritten(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = releasesJSON
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	publish(g, "v0.10.0", bundleOf(t, "v0.10.0", fakeRootfs(1<<20), nil))
	delete(g.files, "/dl/v0.10.0.update.sig")
	l.s.CheckNow(context.Background())
	if err := l.s.Install("v0.10.0"); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, l); j.Phase != "failed" || !strings.Contains(j.Error, "signature") {
		t.Fatalf("job: %+v", j)
	}
	if g.hits["/dl/v0.10.0.update"] != 0 {
		t.Fatal("the update was downloaded without a signature to check it against")
	}
}

func TestAMachineWithoutAReleaseKeyInstallsNothingFromGitHub(t *testing.T) {
	g := newFakeGitHub(t)
	g.releases = releasesJSON
	l := newLab(t, SlotA, false, SlotA)
	g.point(l)
	l.s.TrustedKeys = []ed25519.PublicKey{} // not nil: none (nil means the built-in ones)
	publish(g, "v0.10.0", bundleOf(t, "v0.10.0", fakeRootfs(1<<20), nil))
	l.s.CheckNow(context.Background())
	if err := l.s.Install("v0.10.0"); err != nil {
		t.Fatal(err)
	}
	if j := waitJob(t, l); j.Phase != "failed" || !strings.Contains(j.Error, "release key") {
		t.Fatalf("job: %+v", j)
	}
	if g.hits["/dl/v0.10.0.update"] != 0 {
		t.Fatal("downloaded without any key to check it against")
	}
}

package image

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The part of the initramfs script that carries out a factory reset runs here on a directory standing in for the data partition. The
// function is taken out of the real script, so the test cannot drift from what the image has.

func factoryResetFunction(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("overlay/etc/initramfs-tools/scripts/local-bottom/peakroute-state")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "factory_reset_if_asked() {")
	if i < 0 {
		t.Fatal("the script has no factory_reset_if_asked")
	}
	j := strings.Index(s[i:], "\n}\n")
	if j < 0 {
		t.Fatal("the function does not end")
	}
	return s[i : i+j+3]
}

func runFactoryReset(t *testing.T, data string) string {
	t.Helper()
	need(t, "sh", "rm")
	script := "say() { echo \"$*\"; }\nsync() { :; }\n" + factoryResetFunction(t) + "\nfactory_reset_if_asked \"$1\"\n"
	out, err := exec.Command("sh", "-c", script, "sh", data).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return string(out)
}

func fill(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestAFactoryResetErasesTheLayersAndTheRequestAndNothingElse(t *testing.T) {
	data := t.TempDir()
	fill(t,
		filepath.Join(data, "overlay/etc/upper/peakroute/network.json"),
		filepath.Join(data, "overlay/var/upper/lib/misc/dnsmasq.leases"),
		filepath.Join(data, "overlay/home/work/x"),
		filepath.Join(data, "factory-reset"),
		filepath.Join(data, "updates/staged.update"), // not part of the router's settings
	)
	out := runFactoryReset(t, data)
	if !strings.Contains(out, "factory reset") {
		t.Errorf("it said nothing about erasing: %q", out)
	}
	if exists(filepath.Join(data, "overlay")) || exists(filepath.Join(data, "factory-reset")) {
		t.Errorf("the layers or the request are still there")
	}
	if !exists(filepath.Join(data, "updates/staged.update")) {
		t.Errorf("something outside the layers was erased")
	}
}

func TestWithoutARequestNothingIsErased(t *testing.T) {
	data := t.TempDir()
	keep := filepath.Join(data, "overlay/etc/upper/peakroute/network.json")
	fill(t, keep)
	if out := runFactoryReset(t, data); out != "" {
		t.Errorf("said something: %q", out)
	}
	if !exists(keep) {
		t.Errorf("settings were erased without a request")
	}
}

func TestAFactoryResetThatFindsNothingStillClearsTheRequest(t *testing.T) {
	data := t.TempDir()
	fill(t, filepath.Join(data, "factory-reset"))
	runFactoryReset(t, data)
	if exists(filepath.Join(data, "factory-reset")) {
		t.Errorf("the request stays, and would erase the next settings too")
	}
}

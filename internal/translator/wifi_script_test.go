package translator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runHostapdScript runs the script the wifi service uses, with fake adapters, a fake hostapd that records
// how it was called, and a fake rfkill.
func runHostapdScript(t *testing.T, confs map[string]string, present []string) (out string, calls string, err error) {
	t.Helper()
	root := t.TempDir()
	confDir, sysNet := filepath.Join(root, "conf"), filepath.Join(root, "sys")
	os.MkdirAll(confDir, 0755)
	os.MkdirAll(sysNet, 0755)
	for name, text := range confs {
		os.WriteFile(filepath.Join(confDir, name), []byte(text), 0600)
	}
	for _, p := range present {
		os.MkdirAll(filepath.Join(sysNet, p), 0755)
	}
	log := filepath.Join(root, "calls.log")
	fake := func(name string) string {
		path := filepath.Join(root, name)
		os.WriteFile(path, []byte("#!/bin/sh\necho \""+name+" $*\" >> "+log+"\n"), 0755)
		return path
	}
	cmd := exec.Command("sh", "../scripts/hostapd-run.sh")
	cmd.Env = append(os.Environ(),
		"PEAKROUTE_HOSTAPD_DIR="+confDir, "PEAKROUTE_SYS_NET="+sysNet, "PEAKROUTE_HOSTAPD="+fake("hostapd"),
		"PEAKROUTE_RFKILL="+fake("rfkill"), "PEAKROUTE_HOSTAPD_PID="+filepath.Join(root, "pid"))
	b, err := cmd.CombinedOutput()
	logged, _ := os.ReadFile(log)
	// the directory names differ per run: make the log comparable
	text := strings.ReplaceAll(string(logged), confDir, "CONF")
	return string(b), text, err
}

func TestTheWifiScriptStartsOneHostapdForEveryPresentAdapter(t *testing.T) {
	out, calls, err := runHostapdScript(t, map[string]string{
		"wlan0.conf": "# x\ninterface=wlan0\nssid=a\n",
		"wlan1.conf": "interface=wlan1\nssid=a\n",
	}, []string{"wlan0", "wlan1", "eth0"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(calls), "\n")
	if len(lines) != 2 || lines[0] != "rfkill unblock wifi" {
		t.Fatalf("calls: %q", calls)
	}
	hostapd := lines[1]
	if !strings.HasPrefix(hostapd, "hostapd -B -P ") || !strings.HasSuffix(hostapd, " CONF/wlan0.conf CONF/wlan1.conf") || strings.Count(calls, "hostapd") != 1 {
		t.Fatalf("one process, detaching, with every file: %q", hostapd)
	}
}

func TestTheWifiScriptLeavesOutAnAdapterThatIsNotThere(t *testing.T) {
	out, calls, err := runHostapdScript(t, map[string]string{
		"wlan0.conf": "interface=wlan0\n",
		"wlan9.conf": "interface=wlan9\n", // unplugged
	}, []string{"wlan0"})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(calls, " CONF/wlan0.conf") || strings.Contains(calls, "wlan9") {
		t.Fatalf("%q", calls)
	}
	if !strings.Contains(out, "wlan9") {
		t.Errorf("it should say what it left out: %q", out)
	}
}

func TestTheWifiScriptFailsWhenNoAdapterIsPresent(t *testing.T) {
	out, calls, err := runHostapdScript(t, map[string]string{"wlan0.conf": "interface=wlan0\n"}, []string{"eth0"})
	if err == nil {
		t.Fatal("it must fail, or a commit would report success for an access point that never started")
	}
	if strings.Contains(calls, "hostapd") || !strings.Contains(out, "none of the configured Wi-Fi adapters") {
		t.Fatalf("%q %q", calls, out)
	}
	// nothing configured at all
	if _, _, err := runHostapdScript(t, nil, []string{"wlan0"}); err == nil {
		t.Fatal("no configuration is also a failure of the script (the service's condition keeps it from running then)")
	}
}

func TestTheWifiScriptIgnoresFilesThatAreNotConfigurations(t *testing.T) {
	_, calls, err := runHostapdScript(t, map[string]string{"wlan0.conf": "interface=wlan0\n", "notes.txt": "interface=wlan1\n", "wlan1.conf.bak": "interface=wlan1\n"}, []string{"wlan0", "wlan1"})
	if err != nil || strings.Contains(calls, "wlan1") {
		t.Fatalf("%v %q", err, calls)
	}
}

func TestTheWifiScriptWorksWithoutRfkill(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "sys", "wlan0"), 0755)
	os.MkdirAll(filepath.Join(root, "conf"), 0755)
	os.WriteFile(filepath.Join(root, "conf", "wlan0.conf"), []byte("interface=wlan0\n"), 0600)
	log := filepath.Join(root, "calls.log")
	hostapd := filepath.Join(root, "hostapd")
	os.WriteFile(hostapd, []byte("#!/bin/sh\necho \"$*\" >> "+log+"\n"), 0755)
	cmd := exec.Command("sh", "../scripts/hostapd-run.sh")
	cmd.Env = append(os.Environ(), "PEAKROUTE_HOSTAPD_DIR="+filepath.Join(root, "conf"), "PEAKROUTE_SYS_NET="+filepath.Join(root, "sys"),
		"PEAKROUTE_HOSTAPD="+hostapd, "PEAKROUTE_RFKILL=definitely-not-installed")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b, _ := os.ReadFile(log); !strings.Contains(string(b), "-B") {
		t.Fatalf("hostapd was not started: %q", b)
	}
}

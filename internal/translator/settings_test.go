package translator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func TestTheTimeZoneIsWrittenWhenSet(t *testing.T) {
	a, ok := file(t, wanCfg(func(c *config.NetworkConfig) { c.Timezone = "Europe/Berlin" }), PathTimezone)
	if !ok || a.Remove || string(a.Content) != "Europe/Berlin\n" || a.Service != SvcTimezone || a.Mode != 0644 {
		t.Fatalf("%+v %v", a, ok)
	}
	// not set: the machine keeps its own, so the file is neither written nor removed
	if _, ok := file(t, wanCfg(func(c *config.NetworkConfig) {}), PathTimezone); ok {
		t.Fatal("a configuration without a time zone touches /etc/timezone")
	}
}

func TestTheTimeServersFile(t *testing.T) {
	cases := []struct {
		name string
		set  func(*config.NetworkConfig)
		want string // "" : the file is removed
	}{
		{"defaults", func(c *config.NetworkConfig) {}, ""},
		{"chosen servers", func(c *config.NetworkConfig) { c.NTPServers = []string{"time.cloudflare.com", "192.168.1.5"} },
			"# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n[Time]\nNTP=time.cloudflare.com 192.168.1.5\n"},
		{"off", func(c *config.NetworkConfig) { c.NTPDisabled = true },
			"# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n# NTP is switched off.\n[Time]\n"},
		{"off, servers kept", func(c *config.NetworkConfig) { c.NTPDisabled = true; c.NTPServers = []string{"a.example"} },
			"# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n# NTP is switched off.\n[Time]\nNTP=a.example\n"},
	}
	for _, tc := range cases {
		a, ok := file(t, wanCfg(tc.set), PathTimesyncd)
		if !ok || a.Service != SvcNTP {
			t.Fatalf("%s: %+v %v", tc.name, a, ok)
		}
		if tc.want == "" {
			if !a.Remove {
				t.Errorf("%s: not removed: %q", tc.name, a.Content)
			}
			continue
		}
		if a.Remove || string(a.Content) != tc.want || a.Mode != 0644 {
			t.Errorf("%s: %q", tc.name, a.Content)
		}
	}
}

func TestTimeValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	saved := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = saved }()
	evil := map[string]func(*config.NetworkConfig){
		"a zone with a newline":   func(c *config.NetworkConfig) { c.Timezone = "UTC\nx" },
		"a zone that is a path":   func(c *config.NetworkConfig) { c.Timezone = "../../etc/passwd" },
		"an unknown zone":         func(c *config.NetworkConfig) { c.Timezone = "Mars/Olympus" },
		"a server with an option": func(c *config.NetworkConfig) { c.NTPServers = []string{"a.example\nFallbackNTP=evil.example"} },
		"a server with a space":   func(c *config.NetworkConfig) { c.NTPServers = []string{"a.example evil.example"} },
		"a loopback server":       func(c *config.NetworkConfig) { c.NTPServers = []string{"127.0.0.1"} },
		"too many servers": func(c *config.NetworkConfig) {
			c.NTPServers = []string{"a.example", "b.example", "c.example", "d.example", "e.example"}
		},
	}
	for name, f := range evil {
		if _, err := Render(wanCfg(f), "ROUTER"); err == nil {
			t.Errorf("%s was rendered", name)
		}
	}
}

func TestTimeChangesRestartOnlyWhatTheyNeed(t *testing.T) {
	fs := RootFS(t.TempDir())
	ap := &Applier{FS: fs, Run: &LogRunner{}}
	apply := func(f func(*config.NetworkConfig)) Plan {
		d := mustRender(t, wanCfg(f))
		plan, err := PlanChanges(fs, d)
		if err != nil {
			t.Fatal(err)
		}
		if res := ap.Apply(context.Background(), d, plan); !res.OK {
			t.Fatalf("%+v", res)
		}
		return plan
	}
	apply(func(c *config.NetworkConfig) {})
	if p := apply(func(c *config.NetworkConfig) { c.Timezone = "Europe/Berlin" }); actionSummary(p) != "apply timezone" {
		t.Fatalf("%s", actionSummary(p))
	}
	if p := apply(func(c *config.NetworkConfig) { c.Timezone = "Europe/Berlin"; c.NTPServers = []string{"a.example"} }); actionSummary(p) != "restart ntp" {
		t.Fatalf("%s", actionSummary(p))
	}
	// back to the defaults removes the file and still re-runs the action (the time may have been switched off before)
	p := apply(func(c *config.NetworkConfig) { c.Timezone = "Europe/Berlin" })
	if actionSummary(p) != "restart ntp" || len(p.Files) != 1 || p.Files[0].Kind != Remove {
		t.Fatalf("%+v", p)
	}
	if p := apply(func(c *config.NetworkConfig) { c.Timezone = "Europe/Berlin" }); !p.Empty() {
		t.Fatalf("nothing changed, but %+v", p)
	}
	// the order of a commit that changes it all: the machine's own settings first, the time servers after the network is up
	p = apply(func(c *config.NetworkConfig) {
		c.Hostname, c.Timezone, c.NTPDisabled = "gw", "UTC", true
		c.Domain = "lan"
		c.WanMTU = 1400
	})
	got := actionSummary(p)
	for _, pair := range [][2]string{{"hostname", "timezone"}, {"timezone", "networking"}, {"dnsmasq", "ntp"}} {
		i, j := strings.Index(got, pair[0]), strings.Index(got, pair[1])
		if i < 0 || j < 0 || i > j {
			t.Errorf("%s should come before %s in %s", pair[0], pair[1], got)
		}
	}
}

// The two commands are shell scripts: run them for real, against a sandbox, with the system paths swapped for temporary ones.
func sandboxed(t *testing.T, svc string) (script string, dir string) {
	t.Helper()
	dir = t.TempDir()
	cmd := serviceCommands[svc]
	if cmd.name != "sh" || len(cmd.args) != 2 || cmd.args[0] != "-c" {
		t.Fatalf("%s is not a sh -c command: %+v", svc, cmd)
	}
	script = strings.NewReplacer(
		PathTimezone, filepath.Join(dir, "timezone"),
		"/etc/localtime", filepath.Join(dir, "localtime"),
		"/usr/share/zoneinfo", filepath.Join(dir, "zoneinfo"),
		PathTimesyncd, filepath.Join(dir, "peakroute.conf"),
	).Replace(cmd.args[1])
	if strings.Contains(script, "/etc/") {
		t.Fatalf("a system path is left in the script: %s", script)
	}
	return script, dir
}

func TestTheTimeZoneCommandPointsLocaltimeAtTheZone(t *testing.T) {
	script, dir := sandboxed(t, SvcTimezone)
	os.MkdirAll(filepath.Join(dir, "zoneinfo", "Europe"), 0755)
	os.WriteFile(filepath.Join(dir, "zoneinfo", "Europe", "Berlin"), []byte("tz"), 0644)
	run := func() (string, error) {
		out, err := exec.Command("sh", "-c", script).CombinedOutput()
		return string(out), err
	}
	os.WriteFile(filepath.Join(dir, "timezone"), []byte("Europe/Berlin\n"), 0644)
	if out, err := run(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if target, err := os.Readlink(filepath.Join(dir, "localtime")); err != nil || target != filepath.Join(dir, "zoneinfo", "Europe", "Berlin") {
		t.Fatalf("%q %v", target, err)
	}
	// a zone this machine does not have fails the commit and leaves localtime as it was
	os.WriteFile(filepath.Join(dir, "timezone"), []byte("Europe/Atlantis\n"), 0644)
	out, err := run()
	if err == nil || !strings.Contains(out, "unknown time zone Europe/Atlantis") {
		t.Fatalf("%v %s", err, out)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "localtime")); !strings.HasSuffix(target, "Europe/Berlin") {
		t.Fatalf("localtime changed to %q", target)
	}
}

func TestTheTimeServersCommandStartsOrStopsTimesyncd(t *testing.T) {
	script, dir := sandboxed(t, SvcNTP)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0755)
	record := filepath.Join(dir, "calls")
	stub := "#!/bin/sh\necho \"$@\" >> " + record + "\n[ \"$FAIL\" = \"$2\" ] && exit 1\nexit 0\n"
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte(stub), 0755)
	run := func(fail string) (string, error) {
		os.Remove(record)
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAIL=" + fail}
		out, err := cmd.CombinedOutput()
		calls, _ := os.ReadFile(record)
		return strings.TrimSpace(string(calls)), func() error { _ = out; return err }()
	}
	conf := filepath.Join(dir, "peakroute.conf")
	// no file (the defaults): on, and restarted to pick the change up
	if calls, err := run(""); err != nil || calls != "enable systemd-timesyncd\nrestart systemd-timesyncd" {
		t.Fatalf("%q %v", calls, err)
	}
	// servers chosen
	data, _ := renderTimesyncd(wanCfg(func(c *config.NetworkConfig) { c.NTPServers = []string{"a.example"} }))
	os.WriteFile(conf, []byte(data), 0644)
	if calls, err := run(""); err != nil || calls != "enable systemd-timesyncd\nrestart systemd-timesyncd" {
		t.Fatalf("%q %v", calls, err)
	}
	// switched off
	data, _ = renderTimesyncd(wanCfg(func(c *config.NetworkConfig) { c.NTPDisabled = true }))
	os.WriteFile(conf, []byte(data), 0644)
	if calls, err := run(""); err != nil || calls != "disable --now systemd-timesyncd" {
		t.Fatalf("%q %v", calls, err)
	}
	// a service that cannot be enabled is not restarted, and the commit hears about it
	os.Remove(conf)
	if calls, err := run("systemd-timesyncd"); err == nil || calls != "enable systemd-timesyncd" {
		t.Fatalf("%q %v", calls, err)
	}
}

package translator

import (
	"context"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func TestTheHostNameIsWrittenToTheMachine(t *testing.T) {
	cfg := wanCfg(func(c *config.NetworkConfig) { c.Hostname = "my-router" })
	a, ok := file(t, cfg, PathHostname)
	if !ok || a.Remove || string(a.Content) != "my-router\n" || a.Service != SvcHostname || a.Mode != 0644 {
		t.Fatalf("%+v %v", a, ok)
	}
	// with no name set, the file is not touched at all (neither written nor removed)
	if _, ok := file(t, wanCfg(func(c *config.NetworkConfig) {}), PathHostname); ok {
		t.Fatal("a configuration without a host name touches /etc/hostname")
	}
}

func TestOnlyAskedForTheHostNameGoesToTheProvider(t *testing.T) {
	named := func(f func(*config.NetworkConfig)) config.NetworkConfig {
		return wanCfg(func(c *config.NetworkConfig) { c.Hostname = "gw"; f(c) })
	}
	cases := map[string]struct {
		cfg  config.NetworkConfig
		want string
	}{
		"named, not sent": {named(func(c *config.NetworkConfig) {}), "auto eth0\\niface eth0 inet dhcp\\n\\n"},
		"named and sent":  {named(func(c *config.NetworkConfig) { c.SendHostname = true }), "auto eth0\\niface eth0 inet dhcp\\n    hostname gw\\n\\n"},
		"static, sent": {named(func(c *config.NetworkConfig) {
			c.SendHostname = true
			c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway = "static", "203.0.113.10", "255.255.255.0", "203.0.113.1"
		}), "auto eth0\\niface eth0 inet static\\n    address 203.0.113.10\\n    netmask 255.255.255.0\\n    gateway 203.0.113.1\\n\\n"},
	}
	for name, c := range cases {
		want := strings.ReplaceAll(c.want, "\\n", "\n")
		if got := wanStanzas(t, c.cfg); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

func TestChangingTheHostNameJustSaysSo(t *testing.T) {
	dir := t.TempDir()
	fs := RootFS(dir)
	old := wanCfg(func(c *config.NetworkConfig) { c.Hostname = "old-name" })
	ap := &Applier{FS: fs, Run: &LogRunner{}}
	apply := func(cfg config.NetworkConfig) Plan {
		d := mustRender(t, cfg)
		plan, err := PlanChanges(fs, d)
		if err != nil {
			t.Fatal(err)
		}
		if res := ap.Apply(context.Background(), d, plan); !res.OK {
			t.Fatalf("%+v", res)
		}
		return plan
	}
	apply(old)
	plan := apply(wanCfg(func(c *config.NetworkConfig) { c.Hostname = "new-name" }))
	// only the name: no network or DHCP restart for it
	if actionSummary(plan) != "apply hostname" || len(plan.Files) != 1 || plan.Files[0].Path != PathHostname {
		t.Fatalf("%+v", plan)
	}
	got, _ := fs.ReadFile(PathHostname)
	if string(got) != "new-name\n" {
		t.Fatalf("%q", got)
	}
	// the machine is told with hostname -F
	run := &LogRunner{}
	ap = &Applier{FS: fs, Run: run}
	d := mustRender(t, wanCfg(func(c *config.NetworkConfig) { c.Hostname = "third" }))
	p, _ := PlanChanges(fs, d)
	ap.Apply(context.Background(), d, p)
	if cmds := strings.Join(run.Commands(), "\n"); !strings.Contains(cmds, "hostname -F "+PathHostname) {
		t.Fatalf("commands: %s", cmds)
	}
	// the sending to the provider is a networking matter
	plan = apply(wanCfg(func(c *config.NetworkConfig) { c.Hostname = "third"; c.SendHostname = true }))
	if !strings.HasPrefix(actionSummary(plan), "restart networking") || strings.Contains(actionSummary(plan), "hostname") {
		t.Fatalf("%+v", plan)
	}
}

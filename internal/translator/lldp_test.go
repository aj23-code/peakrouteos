package translator

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func TestLLDPIsOffAndLeavesNoFileUntilItIsSwitchedOn(t *testing.T) {
	cfg := routerCfg()
	a, ok := file(t, cfg, PathLLDP)
	if !ok || !a.Remove || a.Service != SvcLLDP {
		t.Fatalf("off: %+v", a)
	}
	// nothing on the machine either: an empty system is told nothing about it
	plan, _ := PlanChanges(RootFS(t.TempDir()), mustRender(t, cfg))
	for _, act := range plan.Actions {
		if act.Service == SvcLLDP {
			t.Errorf("an empty system was told to act on LLDP: %+v", act)
		}
	}
}

func TestLLDPListensOnTheLANPortsOnlyAndNeverTheInternetPort(t *testing.T) {
	cfg := routerCfg()
	cfg.LLDP = true
	cfg.LanInterfaces = []string{"eth1", "eth2"}
	conf := content(t, cfg, PathLLDP)
	for _, want := range []string{"configure system interface pattern eth1,eth2\n", "configure lldp portidsubtype ifname\n", "configure system description \"PeakRoute OS\"\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("missing %q in\n%s", want, conf)
		}
	}
	if strings.Contains(conf, cfg.WanInterface) {
		t.Errorf("the internet port %q is in\n%s", cfg.WanInterface, conf)
	}
	// switching it on is one action, run after the firewall and mDNS; switching it off removes the file and runs it again
	plan, _ := PlanChanges(RootFS(t.TempDir()), mustRender(t, cfg))
	n := 0
	for _, act := range plan.Actions {
		if act.Service == SvcLLDP {
			n++
			if act.Verb != "restart" {
				t.Errorf("%+v", act)
			}
		}
	}
	if n != 1 {
		t.Errorf("%d actions for LLDP: %+v", n, plan.Actions)
	}
	// a port name that could do harm is refused
	bad := cfg
	bad.LanInterfaces = []string{"eth1;reboot"}
	if _, err := Render(bad, "ROUTER"); err == nil {
		t.Error("an unsafe port name was rendered into lldpd's file")
	}
}

func TestTheLLDPCommandStartsItWithTheFileAndStopsItWithout(t *testing.T) {
	script := strings.Join(lldpCommand.args, " ")
	for _, want := range []string{PathLLDP, "systemctl enable lldpd.service", "systemctl restart lldpd.service", "systemctl disable --now lldpd.service", "lldpd is not installed"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in %s", want, script)
		}
	}
}

func TestLLDPIsInTheReviewOfChanges(t *testing.T) {
	a, b := routerCfg(), routerCfg()
	b.LLDP = true
	changes := config.DiffNetwork(a, b)
	if len(changes) != 1 || changes[0].Field != "lldp" || changes[0].New != "on (LAN ports)" || changes[0].Old != "off" {
		t.Fatalf("%+v", changes)
	}
}

// lldpd refuses to start without its _lldpd user, which a router updated from an image without lldpd does not have (its /etc overlay keeps
// the old passwd and group files): the command makes them first, and only when they are missing.
func TestTheLLDPCommandMakesTheUserLldpdNeedsBeforeStartingIt(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	conf := filepath.Join(dir, "peakroute.conf")
	os.WriteFile(conf, []byte("x"), 0600)
	fake := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(have string) []string {
		os.Remove(logFile)
		fake("getent", `case "$1 $2" in "group _lldpd"|"passwd _lldpd") case "`+have+`" in *"$1"*) exit 0;; esac;; esac; exit 2`)
		for _, n := range []string{"groupadd", "useradd", "systemctl", "lldpd"} {
			fake(n, `echo "`+n+` $*" >> `+logFile)
		}
		script := strings.ReplaceAll(lldpCommand.args[1], PathLLDP, conf)
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		b, _ := os.ReadFile(logFile)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	got := run("") // neither the group nor the user
	if len(got) != 4 || !strings.HasPrefix(got[0], "groupadd --system _lldpd") || !strings.HasPrefix(got[1], "useradd --system --gid _lldpd") ||
		!strings.Contains(got[1], "--home-dir /run/lldpd") || got[2] != "systemctl enable lldpd.service" || got[3] != "systemctl restart lldpd.service" {
		t.Errorf("%q", got)
	}
	if got = run("group passwd"); len(got) != 2 || got[0] != "systemctl enable lldpd.service" { // both there: nothing is made
		t.Errorf("%q", got)
	}
	if got = run("group"); len(got) != 3 || !strings.HasPrefix(got[0], "useradd") { // only the group
		t.Errorf("%q", got)
	}
}

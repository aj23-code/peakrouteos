package pipeline

import (
	"strings"
	"testing"

	"peakroute/internal/config"
	"peakroute/internal/translator"
)

// watch records when the hooks run and what network.json says at that moment.
func (e *env) watch() *[]string {
	var events []string
	e.svc.before = func() { events = append(events, "before:"+e.mgr.NetworkConfig().LanIP) }
	e.svc.settled = func() { events = append(events, "settled:"+e.mgr.NetworkConfig().LanIP) }
	return &events
}

func TestTheHooksWrapACommitAndItsRevert(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	ev := e.watch()
	e.stage(lanMoved)
	e.commitAndWait()
	// network.json already has the new address while it is applied and when it is settled
	if got := strings.Join(*ev, " "); got != "before:10.0.0.1 settled:10.0.0.1" {
		t.Fatalf("%s", got)
	}
	*ev = nil
	e.fire() // not confirmed: reverted
	if got := strings.Join(*ev, " "); got != "before:10.0.0.1 settled:192.168.1.1" {
		t.Fatalf("the settled hook ran before network.json was put back: %s", got)
	}
}

func TestTheHooksSeeAFailedCommitAfterNetworkJSONIsBack(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	ev := e.watch()
	e.run.fail["dnsmasq --test"] = "dnsmasq: bad dhcp-range"
	e.stage(lanMoved)
	e.commitAndWait()
	if got := strings.Join(*ev, " "); got != "before:10.0.0.1 settled:192.168.1.1" {
		t.Fatalf("%s", got)
	}
}

func TestACommitThatChangesNothingStillSettles(t *testing.T) {
	e := newEnv(t)
	cfg := baseCfg()
	cfg.DHCP = nil // unmanaged: the system is left alone
	e.baseline(cfg)
	ev := e.watch()
	e.stage(func(c *config.NetworkConfig) {
		c.DHCP = &config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"}
	})
	// the system already has the file the new settings would write
	d, _ := translator.Render(e.stagedConfig(), "ROUTER")
	for _, a := range d.Artifacts {
		if a.Path == translator.PathDnsmasqDHCP && !a.Remove {
			if err := e.root.WriteFile(a.Path, a.Content, a.Mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := e.svc.Commit(); err != nil {
		t.Fatal(err)
	}
	e.svc.Wait()
	if len(e.run.log()) != 0 {
		t.Fatalf("the change needed work: %v", e.run.log())
	}
	if got := strings.Join(*ev, " "); got != "settled:192.168.1.1" {
		t.Fatalf("%s", got)
	}
}

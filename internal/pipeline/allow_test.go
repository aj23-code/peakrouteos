package pipeline

import (
	"errors"
	"testing"

	"peakroute/internal/config"
)

// The access point build (cmd/peakroute-ap) refuses the settings of a router before anything is saved or touched: a router's
// draft (a restored backup, say) stays a draft and the system is left as it was.
func TestAnAccessPointBuildRefusesARouterBeforeTouchingAnything(t *testing.T) {
	e := newEnv(t)
	e.baseline(baseCfg())
	e.svc.allow = config.RequireAP
	e.stage(lanMoved)

	err := e.svc.Commit()
	var ve config.ValidationError
	if !errors.As(err, &ve) || len(ve) != 1 || ve[0].Field != "mode" {
		t.Fatalf("Commit = %v, want the mode refused", err)
	}
	if got := e.mgr.NetworkConfig().LanIP; got != "192.168.1.1" {
		t.Fatalf("the refused settings were committed: LAN %s", got)
	}
	if _, draft := e.mgr.StagedNetworkConfig(); !draft {
		t.Fatal("the draft was lost")
	}
	if len(e.run.commands) != 0 {
		t.Fatalf("commands ran: %v", e.run.commands)
	}
	if e.svc.Status().State != StateIdle {
		t.Fatalf("state %s", e.svc.Status().State)
	}
}

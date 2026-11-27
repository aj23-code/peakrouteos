package failover

import (
	"context"
	"strings"
	"testing"
)

type scripted struct {
	calls []string
	wg    string
}

func (s *scripted) Run(ctx context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	s.calls = append(s.calls, line)
	if line == "wg show all endpoints" {
		return s.wg, nil
	}
	return "", nil
}

func TestTheServicesAreToldWhenTheTrafficMoves(t *testing.T) {
	s := &scripted{wg: "wgra\tKEY1=\t203.0.113.9:51820\nwgs1\tKEY2=\t(none)\nwgs2\tKEY3=\t[2001:db8::1]:51820\nwgs2\tKEY4=\t198.51.100.4:51820\n"}
	MoveServices(context.Background(), s)
	got := strings.Join(s.calls, "\n")
	for _, want := range []string{
		"wg set wgra peer KEY1= endpoint 203.0.113.9:51820",
		"wg set wgs2 peer KEY3= endpoint [2001:db8::1]:51820",
		"wg set wgs2 peer KEY4= endpoint 198.51.100.4:51820",
		"systemctl try-restart openvpn-client@*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "KEY2=") {
		t.Errorf("a peer with no endpoint was touched:\n%s", got)
	}
	// no WireGuard here: nothing to do for it, and the rest still runs
	MoveServices(context.Background(), &scripted{})
}

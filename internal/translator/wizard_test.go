package translator

import (
	"testing"

	"peakroute/internal/config"
)

// Typical answers for a new router, as the first-run page collects them: the settings most tests start from.

type wizardAnswers struct {
	wan, lanIP, netmask, dhcpStart, dhcpEnd string
	lans                                    []string
}

func (w wizardAnswers) config() config.NetworkConfig {
	return config.NetworkConfig{
		WanInterface:  w.wan,
		WanMode:       "dhcp",
		LanInterfaces: w.lans,
		LanIP:         w.lanIP,
		LanSubnet:     w.netmask,
		DHCP:          &config.DHCPConfig{Enabled: true, RangeStart: w.dhcpStart, RangeEnd: w.dhcpEnd, LeaseTime: "12h"},
	}
}

var wizardScenarios = map[string]wizardAnswers{
	"single LAN port, defaults": {
		wan: "eth0", lans: []string{"eth1"}, lanIP: "192.168.1.1", netmask: "255.255.255.0",
		dhcpStart: "192.168.1.100", dhcpEnd: "192.168.1.200",
	},
	"two LAN ports are bridged": {
		wan: "enp1s0", lans: []string{"enp2s0", "enp3s0"}, lanIP: "10.10.0.1", netmask: "255.255.0.0",
		dhcpStart: "10.10.1.10", dhcpEnd: "10.10.1.250",
	},
	"three LAN ports": {
		wan: "eth0", lans: []string{"eth1", "eth2", "eth3"}, lanIP: "172.16.5.1", netmask: "255.255.255.0",
		dhcpStart: "172.16.5.50", dhcpEnd: "172.16.5.99",
	},
}

func artifactContent(t *testing.T, d Desired, path string) string {
	t.Helper()
	for _, a := range d.Artifacts {
		if a.Path == path && !a.Remove {
			return string(a.Content)
		}
	}
	t.Fatalf("no rendered file for %s", path)
	return ""
}

// A router set up from these answers renders a working configuration, and rendering is repeatable: a second render of the same settings
// changes nothing (the plan against what the first one wrote is empty).
func TestEveryScenarioRendersAndSettlesAfterOneCommit(t *testing.T) {
	for name, w := range wizardScenarios {
		d, err := Render(w.config(), "ROUTER")
		if err != nil {
			t.Fatalf("%s: Render: %v", name, err)
		}
		if got := artifactContent(t, d, PathSysctl); got != "net.ipv4.ip_forward=1\n" {
			t.Errorf("%s: sysctl = %q", name, got)
		}
		root := RootFS(t.TempDir())
		install(t, string(root), d)
		plan, err := PlanChanges(root, mustRender(t, w.config()))
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Empty() {
			t.Errorf("%s: rendering twice is not stable: %+v", name, plan)
		}
	}
}

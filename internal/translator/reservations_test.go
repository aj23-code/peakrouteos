package translator

import (
	"strings"
	"testing"

	"peakroute/internal/config"
)

func reservedCfg() config.NetworkConfig {
	c := routerCfg()
	c.Reservations = []config.Reservation{
		{MAC: "f8:94:c2:a4:13:a7", IP: "192.168.1.50", Name: "printer"},
		{MAC: "48:b0:2d:f1:fe:ca", IP: "192.168.1.60"},
	}
	return c
}

func TestReservedAddressesGoIntoTheDhcpFile(t *testing.T) {
	got := content(t, reservedCfg(), PathDnsmasqDHCP)
	for _, want := range []string{"dhcp-host=f8:94:c2:a4:13:a7,192.168.1.50,printer\n", "dhcp-host=48:b0:2d:f1:fe:ca,192.168.1.60\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	// with none the file is what it always was
	if plain := content(t, routerCfg(), PathDnsmasqDHCP); strings.Contains(plain, "dhcp-host") {
		t.Errorf("a reservation from nowhere:\n%s", plain)
	}
}

func TestReservedAddressesOnSeveralNetworks(t *testing.T) {
	iot := config.Network{ID: "net2", Name: "IoT", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0", Isolated: true,
		DHCP: &config.DHCPConfig{Enabled: true, RangeStart: "10.20.0.100", RangeEnd: "10.20.0.200", LeaseTime: "12h"}}
	cfg := netsCfg(iot)
	cfg.Reservations = []config.Reservation{{MAC: "48:b0:2d:f1:fe:ca", IP: "10.20.0.7", Name: "tv"}}
	if got := content(t, cfg, PathDnsmasqDHCP); !strings.Contains(got, "dhcp-host=48:b0:2d:f1:fe:ca,10.20.0.7,tv\n") {
		t.Errorf("\n%s", got)
	}
}

func TestChangingOnlyReservationsRestartsOnlyTheDhcpServer(t *testing.T) {
	before := routerCfg()
	root := RootFS(t.TempDir())
	ap := &Applier{FS: root, Run: newRunner()}
	d, p := planFor(t, root, before)
	if res := ap.Apply(t.Context(), d, p); !res.OK {
		t.Fatalf("%+v", res)
	}
	_, plan := planFor(t, root, reservedCfg())
	if got := actionSummary(plan); got != "restart dnsmasq" {
		t.Errorf("actions: %q", got)
	}
}

func TestAnUnsafeReservationIsNeverRendered(t *testing.T) {
	for name, r := range map[string]config.Reservation{
		"a name with a newline": {MAC: "f8:94:c2:a4:13:a7", IP: "192.168.1.50", Name: "x\nbind-interfaces"},
		"a name with a dot":     {MAC: "f8:94:c2:a4:13:a7", IP: "192.168.1.50", Name: "a.b"},
		"a bad address":         {MAC: "f8:94:c2:a4:13:a7", IP: "192.168.1.50,evil"},
		"an upper case MAC":     {MAC: "F8:94:C2:A4:13:A7", IP: "192.168.1.50"},
	} {
		if err := ensureSafeReservations([]config.Reservation{r}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

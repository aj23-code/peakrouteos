package image

import (
	"os"
	"strings"
	"testing"
)

// AdGuard Home lists the machine's addresses over a netlink socket. The unit's address-family list once left it out, and the service
// died at every start with "address family not supported by protocol".
func TestTheAdGuardHomeServiceMayUseTheNetworkSocketsItNeeds(t *testing.T) {
	b, err := os.ReadFile("overlay/etc/systemd/system/peakroute-adguard.service")
	if err != nil {
		t.Fatal(err)
	}
	var families string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "RestrictAddressFamilies="); ok {
			families = v
		}
	}
	for _, want := range []string{"AF_INET", "AF_INET6", "AF_UNIX", "AF_NETLINK"} {
		if !strings.Contains(families, want) {
			t.Errorf("RestrictAddressFamilies=%q lacks %s", families, want)
		}
	}
	if strings.Contains(families, "AF_PACKET") {
		t.Errorf("a packet socket is more than it needs: %q", families)
	}
}

package translator

import (
	"fmt"
	"strings"
	"testing"
)

// A network other than the default one may only ask the router for the few things a network needs from it (names, addresses, ping, and
// what its settings switch on): never its web interface, its console or anything else it listens on. Only the default network is
// trusted with the whole router. This pins that, with every option of the network switched on.
func TestAnotherNetworkCannotReachTheRoutersWebInterfaceOrAnythingButWhatItIsGiven(t *testing.T) {
	guest := guestNet()
	guest.MDNS, guest.NTP, guest.Filter = true, true, true
	cfg := netsCfg(guest, iotNet())
	nft := content(t, cfg, PathNftables)

	start := strings.Index(nft, "chain input")
	if start < 0 {
		t.Fatalf("no input chain in\n%s", nft)
	}
	input := nft[start:]
	input = input[:strings.Index(input, "chain forward")]
	if !strings.Contains(input, "policy drop") {
		t.Fatal("the router's input chain does not drop by default")
	}

	given := []string{
		`udp dport { 53, 67 } accept`,
		`tcp dport 53 accept`,
		`icmp type echo-request accept`,
		`udp dport 5353 accept`,
		`udp dport 123 accept`,
		fmt.Sprintf(`udp dport %d accept`, DNSFilterPort),
		fmt.Sprintf(`tcp dport %d accept`, DNSFilterPort),
	}
	seen := 0
	for _, line := range strings.Split(input, "\n") {
		line = strings.TrimSpace(line)
		// the guest and IoT networks' interfaces (the default network's own line is the one that lets everything in)
		if !strings.HasPrefix(line, `iifname "`) || strings.Contains(line, `iifname "eth1"`) || strings.Contains(line, `iifname "lo"`) ||
			strings.Contains(line, `iifname "br0"`) {
			continue
		}
		seen++
		ok := false
		for _, g := range given {
			if strings.HasSuffix(line, g) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("a network is given more of the router than DNS, DHCP, ping and what it switched on: %s", line)
		}
	}
	if seen == 0 {
		t.Fatalf("found no line for the other networks in\n%s", input)
	}
}

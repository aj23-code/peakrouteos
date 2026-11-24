package translator

import (
	"strings"
	"testing"

	"peakroute/internal/config"
)

func TestNoOneIsToldToAskTheRouterForTheTimeUntilANetworkServesIt(t *testing.T) {
	for _, cfg := range []config.NetworkConfig{routerCfg(), netsCfg(guestNet())} {
		if strings.Contains(content(t, cfg, PathDnsmasqDHCP), "ntp-server") {
			t.Error("DHCP advertises a time server nobody switched on")
		}
		if strings.Contains(content(t, cfg, PathNftables), "dport 123") {
			t.Error("the firewall lets time questions in")
		}
	}
}

func TestADefaultNetworkThatServesTheTimeSaysSoInDHCP(t *testing.T) {
	cfg := routerCfg()
	cfg.LanNTP = true
	if got := content(t, cfg, PathDnsmasqDHCP); !strings.Contains(got, "dhcp-option=option:ntp-server,192.168.1.1\n") {
		t.Errorf("%s", got)
	}
	// with several networks the option is tagged, so another network's clients never get this one's router
	guest := guestNet()
	guest.NTP = true
	cfg = netsCfg(guest)
	cfg.LanNTP = true
	got := content(t, cfg, PathDnsmasqDHCP)
	for _, want := range []string{"dhcp-option=tag:lan,option:ntp-server,192.168.1.1\n", "dhcp-option=tag:guest,option:ntp-server,10.20.0.1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	only := netsCfg(guestNet(), iotNet())
	only.Networks[1].NTP = true
	got = content(t, only, PathDnsmasqDHCP)
	if strings.Contains(got, "tag:guest,option:ntp-server") || strings.Contains(got, "tag:lan,option:ntp-server") {
		t.Errorf("a network that does not serve the time is told it does:\n%s", got)
	}
}

func TestTimeQuestionsReachTheRouterOnlyFromNetworksThatServeIt(t *testing.T) {
	guest := guestNet()
	guest.NTP = true
	nft := content(t, netsCfg(guest, iotNet()), PathNftables)
	input := nft[strings.Index(nft, "chain input"):strings.Index(nft, "chain forward")]
	if !strings.Contains(input, `iifname "br20" udp dport 123 accept`) {
		t.Errorf("missing in:\n%s", input)
	}
	if strings.Contains(input, `"br30" udp dport 123`) {
		t.Errorf("another network got it:\n%s", input)
	}
}

func TestServingTheTimeWithTheClockLeftUnsetIsSaidInTheReview(t *testing.T) {
	cfg := routerCfg()
	cfg.LanNTP = true
	cfg.NTPDisabled = true
	var found bool
	for _, n := range mustRender(t, cfg).Notes {
		found = found || strings.Contains(n, "serves the time")
	}
	if !found {
		t.Error("no note")
	}
	cfg.NTPDisabled = false
	for _, n := range mustRender(t, cfg).Notes {
		if strings.Contains(n, "serves the time") {
			t.Errorf("a note for nothing: %s", n)
		}
	}
}

func TestTheAddressesTheRouterAnswersOn(t *testing.T) {
	cfg := netsCfg(guestNet(), iotNet())
	if got := cfg.NTPAddrs(); len(got) != 0 {
		t.Errorf("%v", got)
	}
	cfg.LanNTP = true
	cfg.Networks[1].NTP = true
	if got := strings.Join(cfg.NTPAddrs(), ","); got != "192.168.1.1,10.30.0.1" {
		t.Errorf("%s", got)
	}
}

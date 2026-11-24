package translator

import (
	"strings"
	"testing"

	"peakroute/internal/config"
)

func TestMDNSIsOffAndLeavesNoFileUntilANetworkAsksForIt(t *testing.T) {
	for _, cfg := range []config.NetworkConfig{routerCfg(), netsCfg(guestNet(), iotNet())} {
		a, ok := file(t, cfg, PathAvahi)
		if !ok || !a.Remove || a.Service != SvcMDNS {
			t.Errorf("no network shares mDNS, yet %+v", a)
		}
		if strings.Contains(content(t, cfg, PathNftables), "5353") {
			t.Error("the firewall mentions mDNS while it is off")
		}
	}
	// nothing is on the machine either: nothing to plan
	root := RootFS(t.TempDir())
	plan, _ := PlanChanges(root, mustRender(t, netsCfg(guestNet())))
	for _, a := range plan.Actions {
		if a.Service == SvcMDNS {
			t.Errorf("an empty system was told to act on mDNS: %+v", a)
		}
	}
}

func TestAvahiReflectsBetweenTheNetworksThatShareMDNSOnly(t *testing.T) {
	guest, iot := guestNet(), iotNet()
	guest.MDNS = true
	cfg := netsCfg(guest, iot)
	cfg.LanMDNS = true // the default network lives on br0 (two ports)
	conf := content(t, cfg, PathAvahi)
	for _, want := range []string{"allow-interfaces=br0,br20\n", "enable-reflector=yes\n", "enable-wide-area=no\n", "publish-workstation=no\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("missing %q in\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "br30") || strings.Contains(conf, "eth0") {
		t.Errorf("a network that does not share mDNS (or the internet port) is listed:\n%s", conf)
	}

	// one port: the default network is the port itself
	one := routerCfg()
	one.LanMDNS = true
	if got := content(t, one, PathAvahi); !strings.Contains(got, "allow-interfaces=eth1\n") {
		t.Errorf("%s", got)
	}
}

func TestMDNSReachesTheRouterOnlyFromTheNetworksThatShareIt(t *testing.T) {
	guest := guestNet()
	guest.MDNS = true
	nft := content(t, netsCfg(guest, iotNet()), PathNftables)
	input := nft[strings.Index(nft, "chain input"):strings.Index(nft, "chain forward")]
	if !strings.Contains(input, `iifname "br20" udp dport 5353 accept`) {
		t.Errorf("missing in:\n%s", input)
	}
	if strings.Contains(input, `"br30" udp dport 5353`) {
		t.Errorf("another network got mDNS:\n%s", input)
	}
	// it is not a forwarding rule: the router itself repeats the packets, so isolation is untouched
	fwd := nft[strings.Index(nft, "chain forward"):strings.Index(nft, "chain output")]
	if strings.Contains(fwd, "5353") {
		t.Errorf("mDNS is forwarded:\n%s", fwd)
	}
}

func TestSwitchingMDNSOnStartsAvahiAndOffStopsIt(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, netsCfg(guestNet())))

	on := guestNet()
	on.MDNS = true
	plan, _ := PlanChanges(root, mustRender(t, netsCfg(on)))
	if got := actionSummary(plan); got != "reload nftables, restart mdns" {
		t.Fatalf("switching on: %q (networking must not restart)", got)
	}
	install(t, string(root), mustRender(t, netsCfg(on)))

	off := on
	off.MDNS = false
	plan, _ = PlanChanges(root, mustRender(t, netsCfg(off)))
	if got := actionSummary(plan); got != "reload nftables, restart mdns" {
		t.Fatalf("switching off: %q", got)
	}
	// the one action reads the file: with it avahi runs, without it avahi is stopped
	cmd := serviceCommands[SvcMDNS].String()
	for _, want := range []string{"[ -f " + PathAvahi + " ]", "systemctl enable avahi-daemon.socket avahi-daemon.service", "systemctl disable --now avahi-daemon.service"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in %s", want, cmd)
		}
	}
}

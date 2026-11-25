package translator

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func TestNothingFiltersUntilANetworkAsksFor(t *testing.T) {
	for _, cfg := range []config.NetworkConfig{routerCfg(), netsCfg(guestNet())} {
		a, ok := file(t, cfg, PathFiltering)
		if !ok || !a.Remove || a.Service != SvcFiltering {
			t.Errorf("nothing filters, yet %+v", a)
		}
		nft := content(t, cfg, PathNftables)
		if strings.Contains(nft, "redirect") || strings.Contains(nft, "dns_filter") || strings.Contains(nft, "5335") {
			t.Errorf("the firewall mentions filtering:\n%s", nft)
		}
		if strings.Contains(content(t, cfg, PathDnsmasqDHCP), "interface=lo") {
			t.Error("dnsmasq listens on the loopback for nobody")
		}
	}
}

func TestANetworkThatFiltersHasItsDNSRedirectedAndOthersDoNot(t *testing.T) {
	guest := guestNet()
	guest.Filter = true
	cfg := netsCfg(guest, iotNet())
	cfg.LanFilter = true
	nft := content(t, cfg, PathNftables)
	chain := nft[strings.Index(nft, "chain dns_filter"):]
	chain = chain[:strings.Index(chain, "\n    }\n")]
	for _, want := range []string{
		`iifname "br0" udp dport 53 redirect to :5335`, `iifname "br0" tcp dport 53 redirect to :5335`,
		`iifname "br20" udp dport 53 redirect to :5335`, `iifname "br20" tcp dport 53 redirect to :5335`,
		"type nat hook prerouting priority dstnat",
	} {
		if !strings.Contains(chain, want) {
			t.Errorf("missing %q in:\n%s", want, chain)
		}
	}
	if strings.Contains(chain, "br30") {
		t.Errorf("a network that does not filter is redirected:\n%s", chain)
	}
	// the questions arrive at the router's own port after the redirect: only the filtering networks may send them there
	input := nft[strings.Index(nft, "chain input"):strings.Index(nft, "chain forward")]
	for _, want := range []string{`iifname "br20" udp dport 5335 accept`, `iifname "br20" tcp dport 5335 accept`} {
		if !strings.Contains(input, want) {
			t.Errorf("missing %q in:\n%s", want, input)
		}
	}
	if strings.Contains(input, `"br30" udp dport 5335`) {
		t.Errorf("another network may reach AdGuard Home:\n%s", input)
	}
	// dnsmasq answers AdGuard Home over the loopback
	if got := content(t, cfg, PathDnsmasqDHCP); !strings.Contains(got, "interface=lo\n") {
		t.Errorf("%s", got)
	}
	one := routerCfg()
	one.LanFilter = true
	if got := content(t, one, PathDnsmasqDHCP); !strings.Contains(got, "interface=lo\n") {
		t.Errorf("%s", got)
	}
	if got := content(t, one, PathNftables); !strings.Contains(got, `iifname "eth1" udp dport 53 redirect to :5335`) {
		t.Errorf("a single-port router redirects the port itself:\n%s", got)
	}
	m := content(t, cfg, PathFiltering)
	if !strings.Contains(m, "Filtering: br0 br20") {
		t.Errorf("%s", m)
	}
}

func TestSwitchingFilteringOnReloadsTheFirewallAndStartsAdGuardHomeWithoutRestartingNetworking(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, netsCfg(guestNet())))

	on := guestNet()
	on.Filter = true
	plan, _ := PlanChanges(root, mustRender(t, netsCfg(on)))
	if got := actionSummary(plan); got != "reload nftables, restart dnsmasq, apply filtering" {
		t.Fatalf("on: %q", got)
	}
	install(t, string(root), mustRender(t, netsCfg(on)))
	off := on
	off.Filter = false
	plan, _ = PlanChanges(root, mustRender(t, netsCfg(off)))
	if got := actionSummary(plan); got != "reload nftables, restart dnsmasq, apply filtering" {
		t.Fatalf("off: %q", got)
	}
	cmd := serviceCommands[SvcFiltering].String()
	for _, want := range []string{"[ -f " + PathFiltering + " ]", "systemctl enable peakroute-adguard", "systemctl start peakroute-adguard", "systemctl disable --now peakroute-adguard", "not in this system image"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in %s", want, cmd)
		}
	}
}

func TestANetworkCannotFilterAndBeToldToUseOtherDNSServers(t *testing.T) {
	cfg := netsCfg(guestNet())
	cfg.Networks[0].DNSServers = []string{"1.1.1.1"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("servers alone are fine: %v", err)
	}
	cfg.Networks[0].Filter = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "networks[0].dns_servers") || !strings.Contains(err.Error(), "filters ads") {
		t.Errorf("%v", err)
	}
	cfg.Networks[0].DNSServers = nil
	if err := cfg.Validate(); err != nil {
		t.Errorf("%v", err)
	}
	cfg.LanDNSServers = []string{"9.9.9.9"}
	cfg.LanFilter = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "lan_dns_servers") {
		t.Errorf("%v", err)
	}
}

// The rules load into a real kernel (a throw-away network namespace), and the redirect is in the nat table.
func TestTheFilteringRulesLoadIntoTheKernel(t *testing.T) {
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("no nft")
	}
	if out, err := exec.Command("unshare", "-rn", "true").CombinedOutput(); err != nil {
		t.Skipf("no user and network namespaces here: %v %s", err, out)
	}
	guest := guestNet()
	guest.Filter = true
	cfg := netsCfg(guest)
	cfg.LanFilter = true
	conf := t.TempDir() + "/nft.conf"
	if err := os.WriteFile(conf, []byte(content(t, cfg, PathNftables)), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("unshare", "-rn", "sh", "-c", "nft -f "+conf+" && nft list chain ip nat dns_filter").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "redirect to :5335") || strings.Count(string(out), "redirect") != 4 {
		t.Errorf("%s", out)
	}
}

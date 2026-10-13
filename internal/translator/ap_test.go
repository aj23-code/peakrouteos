package translator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

// apCfg is an access point: two cable ports in one bridge, its address from DHCP.
func apCfg(edit func(*config.NetworkConfig)) config.NetworkConfig {
	c := config.NetworkConfig{Hostname: "ap-hall", Country: "US", AP: &config.APConfig{Enabled: true, Ports: []string{"eth0", "eth1"}}}
	if edit != nil {
		edit(&c)
	}
	return c
}

func TestAnAccessPointIsOneBridgeWithAnAddressFromDHCP(t *testing.T) {
	got := text(t, apCfg(nil), PathInterfaces)
	want := `auto lo
iface lo inet loopback

auto br0
iface br0 inet dhcp
    hostname ap-hall
    bridge_ports eth0 eth1
    post-up ip addr add 192.168.77.1/24 dev br0 label br0:setup || true
`
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if text(t, apCfg(nil), PathSysctl) != "net.ipv4.ip_forward=0\n" {
		t.Fatal("an access point forwards")
	}
}

func TestAnAccessPointWithAFixedAddress(t *testing.T) {
	c := apCfg(func(c *config.NetworkConfig) {
		c.AP.Mode, c.AP.IP, c.AP.Netmask, c.AP.Gateway, c.AP.DNS = config.APStatic, "192.168.1.2", "255.255.255.0", "192.168.1.1", []string{"192.168.1.1", "1.1.1.1"}
	})
	got := text(t, c, PathInterfaces)
	for _, w := range []string{"iface br0 inet static\n", "    address 192.168.1.2\n", "    netmask 255.255.255.0\n", "    gateway 192.168.1.1\n", "    dns-nameservers 192.168.1.1 1.1.1.1\n", "    bridge_ports eth0 eth1\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "hostname") {
		t.Fatal("a DHCP option in a fixed address")
	}
}

func TestAnAccessPointRoutesNothingAndServesNothing(t *testing.T) {
	d := mustRender(t, apCfg(nil))
	nft := text(t, apCfg(nil), PathNftables)
	for _, bad := range []string{"masquerade", "nat", "dnat", "iifname \"br0\" accept"} {
		if strings.Contains(nft, bad) {
			t.Errorf("the firewall has %q:\n%s", bad, nft)
		}
	}
	for _, want := range []string{"policy drop;", "tcp dport 443 accept", "icmp type echo-request accept", "udp sport 67 udp dport 68 accept"} {
		if !strings.Contains(nft, want) {
			t.Errorf("the firewall lacks %q", want)
		}
	}
	// everything only a router has is taken away
	for _, p := range []string{PathDnsmasqDHCP, PathDnsmasqDNS, PathDnsmasqLocal, PathDnsmasqVPN, PathRoutes, PathPPPPeer} {
		if a, ok := hasArtifact(d, p); !ok || !a.Remove {
			t.Errorf("%s is not removed: %+v", p, a)
		}
	}
	dirs := map[string]bool{}
	for _, m := range d.ManagedDirs {
		dirs[m.Path] = true
	}
	for _, p := range []string{PathHostapdDir, PathWpaDir, PathWireGuardDir, PathOVServerDir} {
		if !dirs[p] {
			t.Errorf("%s is not managed", p)
		}
	}
}

func TestEverythingAnAccessPointServesJoinsTheOneBridge(t *testing.T) {
	// Wi-Fi networks join br0 whichever local network they named (a router's leftover), and a link joins it too
	c := apCfg(func(c *config.NetworkConfig) {
		c.Networks = []config.Network{guestNet()}
		n := ssid("wifi1", "Home", config.Band24)
		g := ssid("wifi2", "Guest", config.Band24)
		g.Network = "guest"
		c.Wifi = &config.WifiConfig{Radios: []config.WifiRadio{r24()}, Networks: []config.WifiNetwork{n, g},
			Stations: []config.WifiStation{{Name: "wlan1", SSID: "RoofLink", Security: config.SecurityWPA3, Passphrase: "correct horse battery", Role: config.StationBridge, Network: "lan"}}}
	})
	h := hostapd(t, c, "wlan0")
	if strings.Count(h, "bridge=br0\n") != 2 || strings.Contains(h, "bridge=br20") {
		t.Fatalf("%s", h)
	}
	got := text(t, c, PathInterfaces)
	if !strings.Contains(got, "iface wlan1 inet manual") || !strings.Contains(got, "bridge_ports eth0 eth1 wlan1\n") {
		t.Fatalf("%s", got)
	}
	wpaFile(t, c, "wlan1")
}

func TestAWirelessExtenderNeedsNoCable(t *testing.T) {
	c := apCfg(func(c *config.NetworkConfig) {
		c.AP.Ports = nil
		c.Wifi = &config.WifiConfig{Radios: []config.WifiRadio{r24()}, Networks: []config.WifiNetwork{ssid("wifi1", "Home", config.Band24)},
			Stations: []config.WifiStation{{Name: "wlan1", SSID: "Main", Security: config.SecurityWPA2WPA3, Passphrase: "correct horse battery", Role: config.StationBridge, Network: "lan"}}}
	})
	if got := text(t, c, PathInterfaces); !strings.Contains(got, "bridge_ports wlan1\n") {
		t.Fatalf("%s", got)
	}
	// but not with nothing in the bridge at all
	c.Wifi.Stations = nil
	if _, err := Render(c, "AP"); err == nil {
		t.Fatal("an access point with nothing in its bridge was rendered")
	}
}

func TestAnInternetUplinkMeansNothingToAnAccessPoint(t *testing.T) {
	c := apCfg(func(c *config.NetworkConfig) {
		c.WanInterface = "wlan0"
		c.Wifi = &config.WifiConfig{Stations: []config.WifiStation{{Name: "wlan0", SSID: "Cafe", Security: config.SecurityWPA2, Passphrase: "correct horse battery", Role: config.StationUplink}}}
	})
	if a, ok := hasArtifact(mustRender(t, c), wpaPath("wlan0")); !ok || !a.Remove {
		t.Fatalf("an uplink was written for an access point: %+v", a)
	}
	if strings.Contains(text(t, c, PathInterfaces), "wpa-conf") {
		t.Fatal("wpa-conf in an access point")
	}
}

func TestAnAccessPointValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	orig := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = orig }()
	for name, edit := range map[string]func(*config.NetworkConfig){
		"a port that is a command": func(c *config.NetworkConfig) { c.AP.Ports = []string{"eth0; reboot"} },
		"an address with a newline": func(c *config.NetworkConfig) {
			c.AP.Mode, c.AP.IP, c.AP.Netmask = config.APStatic, "10.0.0.2\npost-up reboot", "255.255.255.0"
		},
		"a name server that is a command": func(c *config.NetworkConfig) {
			c.AP.Mode, c.AP.IP, c.AP.Netmask, c.AP.DNS = config.APStatic, "10.0.0.2", "255.255.255.0", []string{"1.1.1.1; reboot"}
		},
	} {
		if _, err := Render(apCfg(edit), "AP"); err == nil {
			t.Errorf("%s was rendered", name)
		}
	}
}

func TestSwitchingBetweenRouterAndAccessPointChangesTheRightThings(t *testing.T) {
	root := RootFS(t.TempDir())
	router := routerCfg()
	install(t, string(root), mustRender(t, router))
	plan, err := PlanChanges(root, mustRender(t, apCfg(nil)))
	if err != nil {
		t.Fatal(err)
	}
	got := actionSummary(plan)
	for _, want := range []string{"apply sysctl", "restart networking", "reload nftables", "restart dnsmasq"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	// and the same again is nothing
	install(t, string(root), mustRender(t, apCfg(nil)))
	for _, a := range mustRender(t, apCfg(nil)).Artifacts {
		if a.Remove {
			os.Remove(filepath.Join(string(root), a.Path))
		}
	}
	if p, _ := PlanChanges(root, mustRender(t, apCfg(nil))); !p.Empty() {
		t.Fatalf("%+v", p)
	}
	// back to a router
	back, _ := PlanChanges(root, mustRender(t, router))
	if !strings.Contains(actionSummary(back), "restart networking") {
		t.Fatalf("%s", actionSummary(back))
	}
}

func TestTheRealNftablesAcceptsTheAccessPointRuleset(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/nft"); err != nil {
		t.Skip("nft is not installed")
	}
	path := filepath.Join(t.TempDir(), "nft.conf")
	os.WriteFile(path, []byte(text(t, apCfg(nil), PathNftables)), 0644)
	out, err := runInNamespace(t, "nft", "-c", "-f", path)
	if err != nil {
		if strings.Contains(out, "Operation not permitted") && !strings.Contains(out, "Error:") {
			t.Skipf("nft cannot check rules here: %s", out)
		}
		t.Fatalf("%v\n%s", err, out)
	}
}

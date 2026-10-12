package translator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
)

func r24() config.WifiRadio {
	return config.WifiRadio{Name: "wlan0", Band: config.Band24, Channel: 6, Width: 20, HT: true}
}
func r5() config.WifiRadio {
	return config.WifiRadio{Name: "wlan1", Band: config.Band5, Channel: 36, Width: 80, HT: true, VHT: true}
}

func ssid(id, name string, bands ...string) config.WifiNetwork {
	return config.WifiNetwork{ID: id, SSID: name, Network: "lan", Security: config.SecurityWPA2, Passphrase: "correct horse battery", Bands: bands}
}

func wifiCfg(radios []config.WifiRadio, nets ...config.WifiNetwork) config.NetworkConfig {
	c := routerCfg()
	c.Country = "US"
	c.Wifi = &config.WifiConfig{Radios: radios, Networks: nets}
	return c
}

func hostapd(t *testing.T, cfg config.NetworkConfig, adapter string) string {
	t.Helper()
	a, ok := hasArtifact(mustRender(t, cfg), hostapdPath(adapter))
	if !ok || a.Remove {
		t.Fatalf("no hostapd file for %s: %+v", adapter, a)
	}
	if a.Mode != 0600 || a.Service != SvcWifi {
		t.Fatalf("%+v: the file holds a passphrase and must be 0600", a)
	}
	return string(a.Content)
}

func TestHostapdFileFor24GHz(t *testing.T) {
	got := hostapd(t, wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24)), "wlan0")
	want := `# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.
interface=wlan0
driver=nl80211
ctrl_interface=/run/hostapd
ctrl_interface_group=0
country_code=US
ieee80211d=1
hw_mode=g
channel=6
beacon_int=100
dtim_period=2
wmm_enabled=1
ieee80211n=1

ssid=Home
bridge=br0
auth_algs=1
wpa=2
wpa_key_mgmt=WPA-PSK
wpa_passphrase=correct horse battery
rsn_pairwise=CCMP
rrm_neighbor_report=1
bss_transition=1
`
	if got != want {
		t.Errorf("hostapd:\n%s\nwant:\n%s", got, want)
	}
}

func TestHostapdFileFor5GHzAt80MHz(t *testing.T) {
	got := hostapd(t, wifiCfg([]config.WifiRadio{r5()}, ssid("wifi1", "Home", config.Band5)), "wlan1")
	for _, want := range []string{"hw_mode=a\n", "channel=36\n", "ieee80211h=1\n", "ieee80211n=1\n", "ht_capab=[HT40+]\n", "ieee80211ac=1\n", "vht_oper_chwidth=1\n", "vht_oper_centr_freq_seg0_idx=42\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ieee80211ax") {
		t.Error("an adapter without Wi-Fi 6 was told to use it")
	}
}

func TestChannelWidthSettings(t *testing.T) {
	at := func(radio config.WifiRadio) string {
		return hostapd(t, wifiCfg([]config.WifiRadio{radio}, ssid("wifi1", "Home", radio.Band)), radio.Name)
	}
	// 160 MHz: the block's centre, width code 2
	r := r5()
	r.Width, r.Channel = 160, 52
	if got := at(r); !strings.Contains(got, "vht_oper_chwidth=2\n") || !strings.Contains(got, "vht_oper_centr_freq_seg0_idx=50\n") || !strings.Contains(got, "ht_capab=[HT40+]\n") {
		t.Errorf("160 MHz:\n%s", got)
	}
	// 40 MHz on the upper channel of a pair extends downwards, and has no VHT centre
	r = r5()
	r.Width, r.Channel = 40, 40
	if got := at(r); !strings.Contains(got, "ht_capab=[HT40-]\n") || !strings.Contains(got, "vht_oper_chwidth=0\n") || strings.Contains(got, "centr_freq") {
		t.Errorf("40 MHz:\n%s", got)
	}
	// 20 MHz: no HT40 at all
	r = r5()
	r.Width = 20
	if got := at(r); strings.Contains(got, "ht_capab") || !strings.Contains(got, "vht_oper_chwidth=0\n") {
		t.Errorf("20 MHz:\n%s", got)
	}
	// 2.4 GHz 40 MHz: the direction follows the channel, and VHT is never written for 2.4 GHz
	r = r24()
	r.Width, r.Channel, r.VHT = 40, 11, true
	if got := at(r); !strings.Contains(got, "ht_capab=[HT40-]\n") || strings.Contains(got, "ieee80211ac") {
		t.Errorf("2.4 GHz 40 MHz:\n%s", got)
	}
	// automatic channel: no direction and no centre can be known; DFS channels are left out on 5 GHz
	r = r5()
	r.Channel = 0
	got := at(r)
	if !strings.Contains(got, "channel=0\n") || !strings.Contains(got, "acs_exclude_dfs=1\n") || strings.Contains(got, "ht_capab") || strings.Contains(got, "centr_freq") {
		t.Errorf("automatic:\n%s", got)
	}
	r = r24()
	r.Channel = 0
	if got := at(r); strings.Contains(got, "acs_exclude_dfs") {
		t.Errorf("2.4 GHz has no DFS to exclude:\n%s", got)
	}
	// no 802.11n on an adapter that lacks it
	r = r24()
	r.HT = false
	if got := at(r); strings.Contains(got, "ieee80211n") {
		t.Errorf("%s", got)
	}
}

func TestWifi6Settings(t *testing.T) {
	r := r5()
	r.HE = true
	r.Width, r.Channel = 80, 149
	got := hostapd(t, wifiCfg([]config.WifiRadio{r}, ssid("wifi1", "Home", config.Band5)), "wlan1")
	for _, want := range []string{"ieee80211ax=1\n", "he_oper_chwidth=1\n", "he_oper_centr_freq_seg0_idx=155\n", "vht_oper_centr_freq_seg0_idx=155\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// on 2.4 GHz there is no width code, only the flag
	r = r24()
	r.HE = true
	got = hostapd(t, wifiCfg([]config.WifiRadio{r}, ssid("wifi1", "Home", config.Band24)), "wlan0")
	if !strings.Contains(got, "ieee80211ax=1\n") || strings.Contains(got, "he_oper") {
		t.Errorf("%s", got)
	}
	// HE without VHT still writes the HE width (a Wi-Fi 6-only radio)
	r = r5()
	r.VHT, r.HE = false, true
	got = hostapd(t, wifiCfg([]config.WifiRadio{r}, ssid("wifi1", "Home", config.Band5)), "wlan1")
	if strings.Contains(got, "ieee80211ac") || !strings.Contains(got, "he_oper_chwidth=1\n") {
		t.Errorf("%s", got)
	}
}

func TestSecurityModes(t *testing.T) {
	with := func(sec string) string {
		n := ssid("wifi1", "Home", config.Band24)
		n.Security = sec
		return hostapd(t, wifiCfg([]config.WifiRadio{r24()}, n), "wlan0")
	}
	wpa2 := with(config.SecurityWPA2)
	if !strings.Contains(wpa2, "wpa_key_mgmt=WPA-PSK\n") || strings.Contains(wpa2, "SAE") || strings.Contains(wpa2, "sae_") || strings.Contains(wpa2, "ieee80211w") {
		t.Errorf("WPA2:\n%s", wpa2)
	}
	wpa3 := with(config.SecurityWPA3)
	for _, want := range []string{"wpa_key_mgmt=SAE\n", "sae_password=correct horse battery\n", "ieee80211w=2\n", "sae_require_mfp=1\n", "sae_pwe=2\n"} {
		if !strings.Contains(wpa3, want) {
			t.Errorf("WPA3 missing %q:\n%s", want, wpa3)
		}
	}
	if strings.Contains(wpa3, "wpa_passphrase") || strings.Contains(wpa3, "WPA-PSK") {
		t.Errorf("WPA3 only must not allow WPA2:\n%s", wpa3)
	}
	mixed := with(config.SecurityWPA2WPA3)
	for _, want := range []string{"wpa_key_mgmt=WPA-PSK SAE\n", "wpa_passphrase=correct horse battery\n", "ieee80211w=1\n", "sae_require_mfp=1\n"} {
		if !strings.Contains(mixed, want) {
			t.Errorf("WPA2/WPA3 missing %q:\n%s", want, mixed)
		}
	}
	if strings.Contains(mixed, "ieee80211w=2") {
		t.Errorf("mixed mode must keep management frame protection optional:\n%s", mixed)
	}
}

func TestHiddenAndIsolatedNetworks(t *testing.T) {
	n := ssid("wifi1", "Home", config.Band24)
	if got := hostapd(t, wifiCfg([]config.WifiRadio{r24()}, n), "wlan0"); strings.Contains(got, "ignore_broadcast_ssid") || strings.Contains(got, "ap_isolate") {
		t.Errorf("%s", got)
	}
	n.Hidden, n.ClientIsolation = true, true
	got := hostapd(t, wifiCfg([]config.WifiRadio{r24()}, n), "wlan0")
	if !strings.Contains(got, "ignore_broadcast_ssid=1\n") || !strings.Contains(got, "ap_isolate=1\n") {
		t.Errorf("%s", got)
	}
}

func TestSeveralNetworksOnOneAdapter(t *testing.T) {
	cfg := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24), ssid("wifi2", "Guest", config.Band24), ssid("wifi3", "IoT", config.Band24))
	got := hostapd(t, cfg, "wlan0")
	home, guest, iot := strings.Index(got, "ssid=Home\n"), strings.Index(got, "ssid=Guest\n"), strings.Index(got, "ssid=IoT\n")
	b1, b2 := strings.Index(got, "bss=wlan0_1\n"), strings.Index(got, "bss=wlan0_2\n")
	if !(home >= 0 && home < b1 && b1 < guest && guest < b2 && b2 < iot) {
		t.Fatalf("the first network is the adapter's own, the others follow as bss= sections:\n%s", got)
	}
	if strings.Count(got, "\ninterface=") != 1 || strings.Count(got, "country_code=") != 1 || strings.Count(got, "hw_mode") != 1 {
		t.Errorf("the radio settings belong in the file once:\n%s", got)
	}
}

func TestNetworksJoinTheirBridges(t *testing.T) {
	c := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24), func() config.WifiNetwork {
		n := ssid("wifi2", "Guest", config.Band24)
		n.Network = "guest"
		return n
	}())
	c.Networks = []config.Network{guestNet()}
	got := hostapd(t, c, "wlan0")
	if !strings.Contains(got, "ssid=Home\nbridge=br0\n") || !strings.Contains(got, "ssid=Guest\nbridge=br20\n") {
		t.Errorf("%s", got)
	}
}

func TestADefaultNetworkWithWifiNeedsABridgeEvenWithOnePort(t *testing.T) {
	plain := routerCfg()
	if LanTarget(plain) != "eth1" {
		t.Fatalf("the starting point: %q", LanTarget(plain))
	}
	c := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24))
	if LanTarget(c) != "br0" {
		t.Fatalf("LanTarget = %q", LanTarget(c))
	}
	ifaces := content(t, c, PathInterfaces)
	if !strings.Contains(ifaces, "auto br0\niface br0 inet static\n    address 192.168.1.1\n    netmask 255.255.255.0\n    bridge_ports eth1\n") {
		t.Errorf("%s", ifaces)
	}
	// the firewall and DHCP files follow the interface
	if !strings.Contains(content(t, c, PathNftables), `iifname "br0" accept`) || !strings.Contains(content(t, c, PathDnsmasqDHCP), "interface=br0\n") {
		t.Error("the firewall and DHCP server must follow the default network onto br0")
	}
	// a Wi-Fi network that is switched off, or has no adapter, changes nothing
	off := ssid("wifi1", "Home", config.Band24)
	off.Disabled = true
	if LanTarget(wifiCfg([]config.WifiRadio{r24()}, off)) != "eth1" {
		t.Error("a switched-off network created a bridge")
	}
	if LanTarget(wifiCfg(nil, ssid("wifi1", "Home", config.Band24))) != "eth1" {
		t.Error("a network with no adapter created a bridge")
	}
	// a Wi-Fi network on an added network does not touch the default network
	g := wifiCfg([]config.WifiRadio{r24()}, func() config.WifiNetwork { n := ssid("wifi1", "Guest", config.Band24); n.Network = "guest"; return n }())
	g.Networks = []config.Network{guestNet()}
	if LanTarget(g) != "eth1" {
		t.Errorf("LanTarget = %q", LanTarget(g))
	}
}

func TestBandSteering(t *testing.T) {
	both := func(steer bool) config.NetworkConfig {
		n := ssid("wifi1", "Home", config.Band24, config.Band5)
		n.BandSteering = steer
		return wifiCfg([]config.WifiRadio{r24(), r5()}, n)
	}
	g24, g5 := hostapd(t, both(true), "wlan0"), hostapd(t, both(true), "wlan1")
	for _, want := range []string{"no_probe_resp_if_seen_on=wlan1\n", "no_auth_if_seen_on=wlan1\n"} {
		if !strings.Contains(g24, want) {
			t.Errorf("the 2.4 GHz side is missing %q:\n%s", want, g24)
		}
	}
	if strings.Contains(g5, "seen_on") {
		t.Errorf("the 5 GHz side must accept everyone:\n%s", g5)
	}
	// both sides help devices roam
	for _, g := range []string{g24, g5} {
		if !strings.Contains(g, "bss_transition=1\n") || !strings.Contains(g, "rrm_neighbor_report=1\n") {
			t.Errorf("%s", g)
		}
	}
	// the same name and security on both sides, or steering cannot work
	if !strings.Contains(g24, "ssid=Home\n") || !strings.Contains(g5, "ssid=Home\n") {
		t.Error("the network must carry the same name on both bands")
	}

	// off: no steering lines
	if g := hostapd(t, both(false), "wlan0"); strings.Contains(g, "seen_on") {
		t.Errorf("%s", g)
	}
}

func TestBandSteeringPointsAtTheRightTwinWhenAdaptersServeSeveralNetworks(t *testing.T) {
	a := ssid("wifi1", "Home", config.Band24) // only 2.4 GHz
	b := ssid("wifi2", "Both", config.Band24, config.Band5)
	b.BandSteering = true
	cfg := wifiCfg([]config.WifiRadio{r24(), r5()}, a, b)
	// "Both" is the second network on the 2.4 GHz adapter and the first on the 5 GHz one
	g24 := hostapd(t, cfg, "wlan0")
	if !strings.Contains(g24, "bss=wlan0_1\nssid=Both\n") || !strings.Contains(g24, "no_probe_resp_if_seen_on=wlan1\n") {
		t.Errorf("%s", g24)
	}
	// and now it is the second network on the 5 GHz adapter
	c := ssid("wifi3", "Fast", config.Band5)
	cfg = wifiCfg([]config.WifiRadio{r24(), r5()}, a, c, b)
	g24 = hostapd(t, cfg, "wlan0")
	if !strings.Contains(g24, "no_probe_resp_if_seen_on=wlan1_1\n") || !strings.Contains(g24, "no_auth_if_seen_on=wlan1_1\n") {
		t.Errorf("the twin is the second network on the 5 GHz adapter:\n%s", g24)
	}
	if g5 := hostapd(t, cfg, "wlan1"); !strings.Contains(g5, "bss=wlan1_1\nssid=Both\n") {
		t.Errorf("%s", g5)
	}
}

func TestOnlyWhatIsSwitchedOnIsRendered(t *testing.T) {
	off := r5()
	off.Disabled = true
	cfg := wifiCfg([]config.WifiRadio{r24(), off}, ssid("wifi1", "Home", config.Band24), func() config.WifiNetwork { n := ssid("wifi2", "Old", config.Band24); n.Disabled = true; return n }())
	d := mustRender(t, cfg)
	a24, _ := hasArtifact(d, hostapdPath("wlan0"))
	if strings.Contains(string(a24.Content), "ssid=Old") || !strings.Contains(string(a24.Content), "ssid=Home") {
		t.Errorf("%s", a24.Content)
	}
	if a, ok := hasArtifact(d, hostapdPath("wlan1")); !ok || !a.Remove {
		t.Errorf("a switched-off adapter's file must be removed: %+v", a)
	}
	// an adapter that is on but has nothing to serve
	idle := wifiCfg([]config.WifiRadio{r24()})
	if a, ok := hasArtifact(mustRender(t, idle), hostapdPath("wlan0")); !ok || !a.Remove {
		t.Errorf("%+v", a)
	}
	// the directory is managed whether or not there is a Wi-Fi section, so that removing the section
	// removes the files; on a box that has none the plan stays empty
	for _, c := range []config.NetworkConfig{routerCfg(), cfg} {
		if d := mustRender(t, c); len(d.ManagedDirs) != 6 || d.ManagedDirs[0].Path != PathHostapdDir || d.ManagedDirs[1].Path != PathWpaDir || d.ManagedDirs[2].Path != PathWireGuardDir || d.ManagedDirs[3].Path != PathOVServerDir || d.ManagedDirs[5].Path != PathOVCCDDir {
			t.Errorf("%+v", d.ManagedDirs)
		}
	}
}

func TestWifiValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	orig := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = orig }()

	evil := []func(*config.NetworkConfig){
		func(c *config.NetworkConfig) { c.Wifi.Networks[0].SSID = "Home\nwpa=0" },
		func(c *config.NetworkConfig) { c.Wifi.Networks[0].Passphrase = "password1\nauth_algs=0" },
		func(c *config.NetworkConfig) { c.Wifi.Networks[0].Passphrase = "pass\rword12" },
		func(c *config.NetworkConfig) { c.Wifi.Networks[0].SSID = "" },
		func(c *config.NetworkConfig) { c.Wifi.Radios[0].Name = "wlan0\nbss=evil" },
		func(c *config.NetworkConfig) { c.Country = "US\nieee80211d=0" },
		func(c *config.NetworkConfig) { c.Country = "us" },
		func(c *config.NetworkConfig) { c.Country = "USA" },
		func(c *config.NetworkConfig) { c.Wifi.Networks[0].Network = "ghost" },
	}
	for i, mutate := range evil {
		cfg := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24))
		mutate(&cfg)
		if d, err := Render(cfg, "ROUTER"); err == nil || len(d.Artifacts) != 0 {
			t.Errorf("case %d: an unsafe value reached the files (err=%v)", i, err)
		}
	}
	if _, err := Render(wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24)), "ROUTER"); err != nil {
		t.Errorf("a safe configuration was refused: %v", err)
	}
	// a switched-off network is not rendered, so it is not inspected
	off := ssid("wifi1", "Bad\nname", config.Band24)
	off.Disabled = true
	if _, err := Render(wifiCfg([]config.WifiRadio{r24()}, off), "ROUTER"); err != nil {
		t.Errorf("%v", err)
	}
}

func TestThePassphraseIsOnlyEverInTheHostapdFile(t *testing.T) {
	cfg := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24))
	d := mustRender(t, cfg)
	for _, a := range d.Artifacts {
		has := strings.Contains(string(a.Content), "correct horse battery")
		if has != strings.HasPrefix(a.Path, PathHostapdDir) {
			t.Errorf("%s: contains the passphrase = %v", a.Path, has)
		}
	}
	root := RootFS(t.TempDir())
	plan, _ := PlanChanges(root, d)
	for _, n := range plan.Notes {
		if strings.Contains(n, "correct horse battery") {
			t.Errorf("the plan's notes show the passphrase: %q", n)
		}
	}
}

// ---- plan -------------------------------------------------------------------------------------------

func TestAddingWifiRestartsOnlyWifiOnceTheBridgeExists(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := wifiCfg([]config.WifiRadio{r24()}, func() config.WifiNetwork { n := ssid("wifi1", "Guest", config.Band24); n.Network = "guest"; return n }())
	cfg.Networks = []config.Network{guestNet()}
	base := cfg
	base.Wifi = nil
	install(t, string(root), mustRender(t, base))

	plan, err := PlanChanges(root, mustRender(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if got := actionSummary(plan); got != "restart wifi" {
		t.Fatalf("%q: the network's bridge is already there, so only the access point starts", got)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != hostapdPath("wlan0") || plan.Files[0].Kind != Create {
		t.Fatalf("%+v", plan.Files)
	}
}

func TestWifiOnTheDefaultNetworkAlsoRebuildsTheNetwork(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24))
	base := cfg
	base.Wifi = nil
	install(t, string(root), mustRender(t, base))
	plan, _ := PlanChanges(root, mustRender(t, cfg))
	// the default network moves from the port to br0: networking restarts, the firewall and DHCP files change, Wi-Fi starts
	if got := actionSummary(plan); got != "restart networking, restart wifi, reload nftables, restart dnsmasq" {
		t.Fatalf("%q", got)
	}
}

func TestWifiRestartsWhenNetworkingDoes(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := wifiCfg([]config.WifiRadio{r24()}, func() config.WifiNetwork { n := ssid("wifi1", "Guest", config.Band24); n.Network = "guest"; return n }())
	cfg.Networks = []config.Network{guestNet()}
	install(t, string(root), mustRender(t, cfg))

	moved := cfg
	moved.Networks = []config.Network{func() config.Network { n := guestNet(); n.IP = "10.20.0.2"; return n }()}
	plan, _ := PlanChanges(root, mustRender(t, moved))
	got := actionSummary(plan)
	if !strings.Contains(got, "restart networking, restart wifi") {
		t.Fatalf("%q", got)
	}
	var reason string
	for _, a := range plan.Actions {
		if a.Service == SvcWifi {
			reason = a.Reason
		}
	}
	if !strings.Contains(reason, "networking is restarting") {
		t.Errorf("reason %q", reason)
	}

	// without Wi-Fi nothing is added
	root2 := RootFS(t.TempDir())
	plain := netsCfg(guestNet())
	install(t, string(root2), mustRender(t, plain))
	m2 := plain
	m2.Networks = []config.Network{func() config.Network { n := guestNet(); n.IP = "10.20.0.2"; return n }()}
	p2, _ := PlanChanges(root2, mustRender(t, m2))
	if strings.Contains(actionSummary(p2), "wifi") {
		t.Fatalf("%q", actionSummary(p2))
	}
}

func TestAChangedPassphraseRestartsWifiAndNothingElse(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24))
	install(t, string(root), mustRender(t, cfg))
	changed := cfg
	changed.Wifi = &config.WifiConfig{Radios: cfg.Wifi.Radios, Networks: []config.WifiNetwork{func() config.WifiNetwork {
		n := ssid("wifi1", "Home", config.Band24)
		n.Passphrase = "a different one!"
		return n
	}()}}
	plan, _ := PlanChanges(root, mustRender(t, changed))
	if got := actionSummary(plan); got != "restart wifi" {
		t.Fatalf("%q", got)
	}
	// the same again: nothing
	if plan, _ := PlanChanges(root, mustRender(t, cfg)); !plan.Empty() {
		t.Fatalf("%+v", plan)
	}
}

func TestAnAdapterTakenOutOfTheSetupStopsBeingServed(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := wifiCfg([]config.WifiRadio{r24(), r5()}, ssid("wifi1", "Home", config.Band24, config.Band5))
	install(t, string(root), mustRender(t, cfg))
	// a file for an adapter that is no longer in the configuration at all, and one that is not ours
	os.WriteFile(filepath.Join(string(root), PathHostapdDir, "wlan7.conf"), []byte("interface=wlan7\n"), 0600)
	os.WriteFile(filepath.Join(string(root), PathHostapdDir, "notes.txt"), []byte("mine"), 0600)

	plan, _ := PlanChanges(root, mustRender(t, cfg))
	if len(plan.Files) != 1 || plan.Files[0].Path != PathHostapdDir+"/wlan7.conf" || plan.Files[0].Kind != Remove || actionSummary(plan) != "restart wifi" {
		t.Fatalf("a stale file must be found and removed, and a file that is not a .conf left alone: %+v %q", plan.Files, actionSummary(plan))
	}

	ap, _, run := newApplier(t)
	ap.FS = root
	d := mustRender(t, cfg)
	res := ap.Apply(context.Background(), d, plan)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	if exists(root, PathHostapdDir+"/wlan7.conf") || !exists(root, PathHostapdDir+"/notes.txt") || !exists(root, PathHostapdDir+"/wlan0.conf") {
		t.Fatal("the wrong files were removed")
	}
	if !strings.Contains(strings.Join(run.log(), "\n"), "systemctl restart peakroute-wifi") {
		t.Fatalf("%v", run.log())
	}
}

func TestRemovingAllWifiRemovesTheFilesAndStopsTheService(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24))
	install(t, string(root), mustRender(t, cfg))
	gone := cfg
	gone.Wifi = nil
	d := mustRender(t, gone)
	plan, _ := PlanChanges(root, d)
	if got := actionSummary(plan); !strings.Contains(got, "restart wifi") {
		t.Fatalf("%q: with the Wi-Fi section gone the files must still go: %+v", got, plan.Files)
	}
	foundRemoval := false
	for _, f := range plan.Files {
		if f.Path == hostapdPath("wlan0") && f.Kind == Remove {
			foundRemoval = true
		}
	}
	if !foundRemoval {
		t.Fatalf("%+v", plan.Files)
	}
}

func TestAppliedHostapdFilesAreNotReadableByOthers(t *testing.T) {
	ap, root, _ := newApplier(t)
	d, plan := planFor(t, root, wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24)))
	if res := ap.Apply(context.Background(), d, plan); !res.OK {
		t.Fatalf("%+v", res)
	}
	info, err := os.Stat(filepath.Join(string(root), hostapdPath("wlan0")))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("%v %v", info, err)
	}
}

// ---- the real hostapd, when it is installed --------------------------------------------------------------

// hostapdComplains runs the real hostapd on a configuration and returns what it says about the
// configuration itself (it reads the whole file before it tries to open the adapter, which then fails here).
func hostapdComplains(t *testing.T, bin string, content []byte) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "test.conf")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "-dd", file).CombinedOutput()
	var complaints []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "Line ") || strings.Contains(l, "errors found in configuration file") {
			complaints = append(complaints, l)
		}
	}
	return strings.Join(complaints, "\n")
}

// hostapd reads a configuration file completely before it tries to open the adapter, so running it on a
// file and looking at what it complains about checks every setting the translator writes, without any radio.
func TestTheRealHostapdUnderstandsEverySettingWeWrite(t *testing.T) {
	bin, err := exec.LookPath("hostapd")
	if err != nil {
		t.Skip("hostapd is not installed here")
	}
	// a control: a file with a setting that does not exist must be complained about, or this test proves nothing
	if got := hostapdComplains(t, bin, []byte("interface=wlan0\ndriver=nl80211\nbogus_setting=1\nssid=x\n")); !strings.Contains(got, "bogus_setting") {
		t.Fatalf("hostapd did not flag a bogus setting, so its output cannot be trusted here: %q", got)
	}

	he5 := r5()
	he5.HE = true
	he5.Channel, he5.Width = 149, 80
	both := ssid("wifi1", "Home", config.Band24, config.Band5)
	both.BandSteering, both.Security, both.Hidden, both.ClientIsolation = true, config.SecurityWPA2WPA3, true, true
	wpa3 := ssid("wifi2", "Strict", config.Band24)
	wpa3.Security = config.SecurityWPA3
	r40 := r24()
	r40.Width, r40.Channel = 40, 11
	r40.HE = true
	auto := r5()
	auto.Name = "wlan2"
	auto.Channel = 0
	cases := map[string]config.NetworkConfig{
		"2.4 GHz, three networks, steering and every option": wifiCfg([]config.WifiRadio{r40, he5}, both, wpa3, ssid("wifi3", "Third", config.Band24)),
		"a link for another PeakRoute box (WDS)":             wifiCfg([]config.WifiRadio{r5()}, func() config.WifiNetwork { n := ssid("link", "RoofLink", config.Band5); n.WDS = true; return n }()),
		"5 GHz at 160 MHz":                                   wifiCfg([]config.WifiRadio{func() config.WifiRadio { r := r5(); r.Width, r.Channel = 160, 100; return r }()}, ssid("wifi1", "Home", config.Band5)),
		"automatic channel":                                  wifiCfg([]config.WifiRadio{auto}, ssid("wifi1", "Home", config.Band5)),
	}
	checked := 0
	for name, cfg := range cases {
		for _, a := range mustRender(t, cfg).Artifacts {
			if a.Remove || !strings.HasPrefix(a.Path, PathHostapdDir) {
				continue
			}
			checked++
			if got := hostapdComplains(t, bin, a.Content); got != "" {
				t.Errorf("%s (%s): hostapd objects to the file: %s\n--- file:\n%s", name, filepath.Base(a.Path), got, a.Content)
			}
		}
	}
	if checked < 5 {
		t.Fatalf("only %d files were checked", checked)
	}
}

func TestAWifiNetworkCountsAsCarryingItsLocalNetwork(t *testing.T) {
	// a local network that no port carries, but a Wi-Fi network joins, needs no "nothing can connect" note
	g := func() config.WifiNetwork { n := ssid("wifi1", "Guest", config.Band24); n.Network = "guest"; return n }()
	cfg := wifiCfg([]config.WifiRadio{r24()}, g)
	cfg.Networks = []config.Network{guestNet()}
	if d := mustRender(t, cfg); len(d.Notes) != 0 {
		t.Fatalf("%v", d.Notes)
	}
	cfg.Wifi.Networks[0].Disabled = true // switched off: the note is back
	if d := mustRender(t, cfg); len(d.Notes) != 1 || !strings.Contains(d.Notes[0], "Guest") {
		t.Fatalf("%v", d.Notes)
	}
	// every port moved off the default network, but a Wi-Fi network is on it
	c := wifiCfg([]config.WifiRadio{r24()}, ssid("wifi1", "Home", config.Band24))
	c.Networks = []config.Network{guestNet()}
	c.PortNetworks = map[string]config.PortNetworks{"eth1": {Native: "guest"}}
	for _, n := range mustRender(t, c).Notes {
		if strings.Contains(n, "default network") {
			t.Errorf("%q", n)
		}
	}
}

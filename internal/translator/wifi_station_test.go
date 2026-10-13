package translator

import (
	"context"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
)

func stationCfg(role string, st config.WifiStation) config.NetworkConfig {
	c := routerCfg()
	c.Country = "US"
	st.Role = role
	c.Wifi = &config.WifiConfig{Stations: []config.WifiStation{st}}
	return c
}

func uplinkStation() config.WifiStation {
	return config.WifiStation{Name: "wlan0", SSID: "Cafe Guest", Security: config.SecurityWPA2, Passphrase: "correct horse battery"}
}

func wpaFile(t *testing.T, cfg config.NetworkConfig, adapter string) string {
	t.Helper()
	a, ok := hasArtifact(mustRender(t, cfg), wpaPath(adapter))
	if !ok || a.Remove {
		t.Fatalf("no wpa_supplicant file for %s: %+v", adapter, a)
	}
	if a.Mode != 0600 || a.Service != SvcNetworking {
		t.Fatalf("%+v: the file holds a password and a change must restart networking", a)
	}
	return string(a.Content)
}

func TestAWifiUplinkIsTheInternetPortWithAWpaFile(t *testing.T) {
	cfg := stationCfg(config.StationUplink, uplinkStation())
	cfg.WanInterface = "wlan0"
	ifaces := mustRenderFile(t, cfg, PathInterfaces)
	if !strings.Contains(ifaces, "auto wlan0\niface wlan0 inet dhcp\n") || !strings.Contains(ifaces, "    wpa-conf /etc/peakroute/wpa/wlan0.conf\n") {
		t.Fatalf("%s", ifaces)
	}
	if strings.Contains(ifaces, "inet manual") || strings.Contains(ifaces, "4addr") {
		t.Fatalf("an uplink has no stanza of its own:\n%s", ifaces)
	}
	want := `# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.
ctrl_interface=/run/wpa_supplicant
update_config=0
country=US
network={
    ssid=` + hex.EncodeToString([]byte("Cafe Guest")) + `
    proto=RSN
    pairwise=CCMP
    group=CCMP
    key_mgmt=WPA-PSK
    psk="correct horse battery"
}
`
	if got := wpaFile(t, cfg, "wlan0"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// a fixed address works over Wi-Fi too
	cfg.WanMode, cfg.WanIP, cfg.WanSubnet, cfg.WanGateway, cfg.WanDNS = "static", "192.168.50.20", "255.255.255.0", "192.168.50.1", []string{"1.1.1.1"}
	if s := mustRenderFile(t, cfg, PathInterfaces); !strings.Contains(s, "iface wlan0 inet static\n") || !strings.Contains(s, "wpa-conf /etc/peakroute/wpa/wlan0.conf") {
		t.Fatalf("%s", s)
	}
}

func mustRenderFile(t *testing.T, cfg config.NetworkConfig, path string) string {
	t.Helper()
	a, ok := hasArtifact(mustRender(t, cfg), path)
	if !ok {
		t.Fatalf("no %s", path)
	}
	return string(a.Content)
}

func TestSecurityHiddenNetworksAndTheNameInHex(t *testing.T) {
	cfg := stationCfg(config.StationUplink, uplinkStation())
	cfg.WanInterface = "wlan0"
	st := &cfg.Wifi.Stations[0]
	st.Security, st.Hidden, st.SSID = config.SecurityWPA3, true, `Odd "name" \ é`
	got := wpaFile(t, cfg, "wlan0")
	for _, w := range []string{"key_mgmt=SAE", "sae_password=\"correct horse battery\"", "ieee80211w=2", "scan_ssid=1", "ssid=" + hex.EncodeToString([]byte(`Odd "name" \ é`))} {
		if !strings.Contains(got, w+"\n") {
			t.Errorf("lacks %q:\n%s", w, got)
		}
	}
	if strings.Contains(got, "psk=") || strings.Contains(got, "Odd") {
		t.Errorf("a plain name or a WPA2 key in a WPA3 file:\n%s", got)
	}
	st.Security, st.Hidden = config.SecurityWPA2WPA3, false
	got = wpaFile(t, cfg, "wlan0")
	if !strings.Contains(got, "key_mgmt=WPA-PSK SAE\n") || !strings.Contains(got, "ieee80211w=1\n") || strings.Contains(got, "scan_ssid") {
		t.Fatalf("%s", got)
	}
}

func TestAnUplinkThatIsNotTheInternetPortOrIsOffIsNotUsed(t *testing.T) {
	cfg := stationCfg(config.StationUplink, uplinkStation()) // the internet port is still eth0
	if a, ok := hasArtifact(mustRender(t, cfg), wpaPath("wlan0")); !ok || !a.Remove {
		t.Fatalf("an uplink that is not the internet port must write nothing: %+v", a)
	}
	if strings.Contains(mustRenderFile(t, cfg, PathInterfaces), "wpa-conf") {
		t.Fatal("wpa-conf for an adapter that is not the internet port")
	}
	cfg.WanInterface = "wlan0"
	cfg.Wifi.Stations[0].Disabled = true
	if a, ok := hasArtifact(mustRender(t, cfg), wpaPath("wlan0")); !ok || !a.Remove {
		t.Fatalf("%+v", a)
	}
}

func TestALinkIntoTheLocalNetworkJoinsItsBridgeIn4AddressMode(t *testing.T) {
	cfg := stationCfg(config.StationBridge, config.WifiStation{Name: "wlan1", SSID: "RoofLink", Security: config.SecurityWPA3, Passphrase: "correct horse battery", Network: "lan"})
	ifaces := mustRenderFile(t, cfg, PathInterfaces)
	for _, w := range []string{
		"auto wlan1\niface wlan1 inet manual\n    pre-up iw dev $IFACE set 4addr on\n    wpa-conf /etc/peakroute/wpa/wlan1.conf\n",
		"auto br0\niface br0 inet static\n",
		"    bridge_ports eth1 wlan1\n",
	} {
		if !strings.Contains(ifaces, w) {
			t.Errorf("lacks %q:\n%s", w, ifaces)
		}
	}
	// with one cable port the default network would not have a bridge: the link needs one
	if LanTarget(cfg) != "br0" {
		t.Fatal("a link joined a network with no bridge")
	}
	wpaFile(t, cfg, "wlan1")

	// a second local network: the link joins its bridge, among the ports that carry it
	cfg = stationCfg(config.StationBridge, config.WifiStation{Name: "wlan1", SSID: "RoofLink", Security: config.SecurityWPA2, Passphrase: "correct horse battery", Network: "guest"})
	cfg.Networks = []config.Network{guestNet()}
	ifaces = mustRenderFile(t, cfg, PathInterfaces)
	if !strings.Contains(ifaces, "iface br20 inet static") || !strings.Contains(ifaces, "bridge_ports wlan1\n") || !strings.Contains(ifaces, "iface wlan1 inet manual") {
		t.Fatalf("%s", ifaces)
	}
	for _, n := range mustRender(t, cfg).Notes {
		if strings.Contains(n, "Guest") {
			t.Errorf("a network the link carries is called uncarried: %q", n)
		}
	}
	// the link is not the WAN, so the firewall's idea of the internet is unchanged
	if !strings.Contains(mustRenderFile(t, cfg, PathNftables), `"eth0"`) {
		t.Fatal("the internet port changed")
	}
}

func TestTheAccessPointSideOfALinkAcceptsFourAddressStations(t *testing.T) {
	n := ssid("link", "RoofLink", config.Band5)
	cfg := wifiCfg([]config.WifiRadio{r5()}, n)
	if strings.Contains(hostapd(t, cfg, "wlan1"), "wds_sta") {
		t.Fatal("WDS is on without being asked")
	}
	cfg.Wifi.Networks[0].WDS = true
	got := hostapd(t, cfg, "wlan1")
	if !strings.Contains(got, "bridge=br0\n") || !strings.Contains(got, "wds_sta=1\n") {
		t.Fatalf("%s", got)
	}
}

func TestStationValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	orig := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = orig }()
	for name, edit := range map[string]func(*config.WifiStation){
		"a name with a line break":          func(s *config.WifiStation) { s.SSID = "a\nb" },
		"a password with a newline":         func(s *config.WifiStation) { s.Passphrase = "correct horse\nnetwork={" },
		"an adapter name that is a command": func(s *config.WifiStation) { s.Name = "wlan0; reboot" },
	} {
		cfg := stationCfg(config.StationBridge, uplinkStation())
		cfg.Wifi.Stations[0].Network = "lan"
		edit(&cfg.Wifi.Stations[0])
		if _, err := Render(cfg, "ROUTER"); err == nil {
			t.Errorf("%s was rendered", name)
		}
	}
	cfg := stationCfg(config.StationBridge, uplinkStation())
	cfg.Wifi.Stations[0].Network = "nowhere"
	if _, err := Render(cfg, "ROUTER"); err == nil {
		t.Error("a link into a network that does not exist was rendered")
	}
}

func TestChangingAStationPasswordRestartsNetworkingOnly(t *testing.T) {
	cfg := stationCfg(config.StationUplink, uplinkStation())
	cfg.WanInterface = "wlan0"
	changed := cfg
	changed.Wifi = &config.WifiConfig{Stations: []config.WifiStation{cfg.Wifi.Stations[0]}}
	changed.Wifi.Stations[0].Passphrase = "another passphrase 1"
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, cfg))
	p, _ := PlanChanges(root, mustRender(t, changed))
	// the file is read again by restarting networking, which also takes the DHCP server's address away
	if got := actionSummary(p); got != "restart networking, restart dnsmasq" {
		t.Fatalf("%s", got)
	}
	if p2, _ := PlanChanges(root, mustRender(t, cfg)); !p2.Empty() {
		t.Fatalf("%+v", p2)
	}
}

// wpa_supplicant reads the whole file before it looks for the adapter, so what it says about the file shows whether it understands it.
func wpaComplains(t *testing.T, bin string, content []byte) string {
	t.Helper()
	dir := t.TempDir()
	file := dir + "/wpa.conf"
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bin, "-c", file, "-i", "nonexistent0", "-D", "nl80211").CombinedOutput()
	var complaints []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "Line ") {
			complaints = append(complaints, l)
		}
	}
	return strings.Join(complaints, "\n")
}

func TestTheRealWpaSupplicantUnderstandsEverySettingWeWrite(t *testing.T) {
	bin, err := exec.LookPath("wpa_supplicant")
	if err != nil {
		t.Skip("wpa_supplicant is not installed here")
	}
	if got := wpaComplains(t, bin, []byte("network={\n    ssid=6162\n    key_mgmt=BOGUS\n}\n")); !strings.Contains(got, "key_mgmt") {
		t.Fatalf("wpa_supplicant did not flag a bogus setting, so its output cannot be trusted here: %q", got)
	}
	checked := 0
	for _, sec := range []string{config.SecurityWPA2, config.SecurityWPA3, config.SecurityWPA2WPA3} {
		for _, hidden := range []bool{false, true} {
			cfg := stationCfg(config.StationUplink, uplinkStation())
			cfg.WanInterface = "wlan0"
			cfg.Wifi.Stations[0].Security, cfg.Wifi.Stations[0].Hidden = sec, hidden
			cfg.Wifi.Stations[0].SSID = `Odd "name" é`
			cfg.Wifi.Stations[0].Passphrase = `pa"ss word\1 and more`
			checked++
			if got := wpaComplains(t, bin, []byte(wpaFile(t, cfg, "wlan0"))); got != "" {
				t.Errorf("%s hidden=%v: wpa_supplicant objects: %s", sec, hidden, got)
			}
		}
	}
	if checked != 6 {
		t.Fatal(checked)
	}
}

func TestTheKernelsRegulatoryCountryIsTheRouters(t *testing.T) {
	cfg := routerCfg()
	cfg.Country = "DE"
	a, ok := hasArtifact(mustRender(t, cfg), PathRegdom)
	if !ok || a.Remove || a.Service != SvcRegdom || string(a.Content) != "options cfg80211 ieee80211_regdom=DE\n" || a.Mode != 0644 {
		t.Fatalf("%+v", a)
	}
	// no country: the file goes
	cfg.Country = ""
	if a, ok := hasArtifact(mustRender(t, cfg), PathRegdom); !ok || !a.Remove {
		t.Fatalf("%+v", a)
	}
	// an access point has one too
	ap := apCfg(nil)
	if a, ok := hasArtifact(mustRender(t, ap), PathRegdom); !ok || a.Remove || !strings.Contains(string(a.Content), "=US") {
		t.Fatalf("%+v", a)
	}
}

func TestChangingTheCountryAloneTellsTheKernelAndNothingElse(t *testing.T) {
	root := RootFS(t.TempDir())
	cfg := routerCfg()
	install(t, string(root), mustRender(t, cfg))
	cfg.Country = "US"
	plan, _ := PlanChanges(root, mustRender(t, cfg))
	if got := actionSummary(plan); got != "apply regdomain" {
		t.Fatalf("%q", got)
	}
	install(t, string(root), mustRender(t, cfg))
	if p, _ := PlanChanges(root, mustRender(t, cfg)); !p.Empty() {
		t.Fatalf("%+v", p)
	}
	// and it comes before Wi-Fi starts, which needs it
	order := map[string]int{}
	for i, s := range serviceOrder {
		order[s] = i
	}
	if order[SvcRegdom] >= order[SvcWifi] || order[SvcRegdom] <= order[SvcNetworking] {
		t.Fatalf("%v", serviceOrder)
	}
}

func TestTheCountryCommandUsesTheFileAndNeedsNoWifiTools(t *testing.T) {
	c := serviceCommands[SvcRegdom]
	script := c.args[1]
	for _, want := range []string{"command -v iw", "ieee80211_regdom=", PathRegdom, `iw reg set "${cc:-00}"`} {
		if !strings.Contains(script, want) {
			t.Errorf("the command lacks %q: %s", want, script)
		}
	}
	// run for real with a fake iw: the country of the file, or the world setting without one
	dir := t.TempDir()
	rec := filepath.Join(dir, "calls")
	os.WriteFile(filepath.Join(dir, "iw"), []byte("#!/bin/sh\necho \"$@\" >> "+rec+"\n"), 0755)
	file := filepath.Join(dir, "regdom.conf")
	run := func() string {
		os.Remove(rec)
		cmd := exec.Command("sh", "-c", strings.ReplaceAll(script, PathRegdom, file))
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		b, _ := os.ReadFile(rec)
		return strings.TrimSpace(string(b))
	}
	os.WriteFile(file, []byte("options cfg80211 ieee80211_regdom=JP\n"), 0644)
	if got := run(); got != "reg set JP" {
		t.Fatalf("%q", got)
	}
	os.Remove(file)
	if got := run(); got != "reg set 00" {
		t.Fatalf("%q", got)
	}
	// without iw there is nothing to do and nothing fails
	os.Remove(filepath.Join(dir, "iw"))
	cmd := exec.Command("sh", "-c", strings.ReplaceAll(script, PathRegdom, file))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if _, err := exec.LookPath("iw"); err == nil {
		t.Skip("a real iw is on this machine's PATH directories")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

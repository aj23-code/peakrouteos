package backup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"peakroute/internal/clientnames"
	"peakroute/internal/config"
)

// What a backup carries, end to end, for everything a person sets up on the router itself: the networks and their subnets, the DHCP
// servers, the reserved addresses, the routes, the ports, the nicknames of the devices, and the usage and last-seen history of the
// devices. A restore must bring all of it back, onto the same ports or onto a router whose own are kept.

func audited(t *testing.T, name string, ports []string, edit func(*config.NetworkConfig)) (*router, *clientnames.Store) {
	t.Helper()
	r := newRouter(t, name, ports, edit)
	names := clientnames.New(filepath.Join(r.dir, "client_names.json"))
	r.svc.Names = names
	return r, names
}

func TestEverythingASetupHasComesBackInARestore(t *testing.T) {
	dhcp := func(a, b string) *config.DHCPConfig {
		return &config.DHCPConfig{Enabled: true, RangeStart: a, RangeEnd: b, LeaseTime: "12h"}
	}
	a, aNames := audited(t, "alpha", []string{"eth0", "eth1", "eth2"}, func(c *config.NetworkConfig) {
		c.LanInterfaces = []string{"eth1", "eth2"}
		c.Networks = []config.Network{
			{ID: "net2", Name: "IoT", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0", Isolated: true, DHCP: dhcp("10.20.0.100", "10.20.0.200")},
			{ID: "net3", Name: "Guest", VLAN: 30, IP: "172.16.4.1", Netmask: "255.255.252.0", Internet: true, Isolated: true, DHCP: dhcp("172.16.4.50", "172.16.5.200")},
		}
		c.PortNetworks = map[string]config.PortNetworks{"eth2": {Native: "net2"}}
		c.Reservations = []config.Reservation{
			{MAC: "f8:94:c2:a4:13:a7", IP: "192.168.1.50", Name: "printer"},
			{MAC: "48:b0:2d:f1:fe:ca", IP: "10.20.0.7", Name: "tv"},
			{MAC: "02:aa:bb:cc:dd:03", IP: "172.16.5.20"},
		}
		c.Routes = []config.Route{{Name: "lab", Destination: "10.99.0.0/16", Gateway: "192.168.1.2", Metric: 50}}
	})
	for mac, nick := range map[string]string{"f8:94:c2:a4:13:a7": "Dad's printer", "48:b0:2d:f1:fe:ca": "Living room TV", "02:aa:bb:cc:dd:03": "Visitor phone"} {
		if err := aNames.Set(mac, nick); err != nil {
			t.Fatal(err)
		}
	}
	usage := []byte(`{"totals":{"f8:94:c2:a4:13:a7":123456},"seen":{"up4|192.168.1.50":99}}`)
	seen := []byte(`{"f8:94:c2:a4:13:a7":{"last_seen":1790000000}}`)
	os.WriteFile(filepath.Join(a.dir, "client_usage.json"), usage, 0600)
	os.WriteFile(filepath.Join(a.dir, "client_presence.json"), seen, 0600)
	data := backupOf(t, a, true)
	committed := a.mgr.NetworkConfig()

	t.Run("onto the same ports", func(t *testing.T) {
		b, bNames := audited(t, "beta", []string{"eth0", "eth1", "eth2"}, nil)
		bNames.Set("02:00:00:00:00:99", "an old nickname that goes")
		os.WriteFile(filepath.Join(b.dir, "client_usage.json"), []byte(`{"totals":{"02:00:00:00:00:99":1}}`), 0600)
		rep, err := b.svc.Inspect(data, pw)
		if err != nil {
			t.Fatal(err)
		}
		if !rep.Settings.Valid || rep.Settings.Nicknames != 3 {
			t.Fatalf("the report: %+v", rep.Settings)
		}
		res, err := b.svc.Restore(rep.Token, Options{Settings: true, History: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range res.Done {
			if len(d) > 6 && d[len(d)-6:] == "failed" || bytes.Contains([]byte(d), []byte("failed")) {
				t.Fatalf("a part failed: %v", res.Done)
			}
		}
		staged, ok := b.mgr.StagedNetworkConfig()
		if !ok {
			t.Fatal("no draft")
		}
		// the networks, the subnets, the DHCP servers, the reservations, the routes and the ports: the draft is alpha's setup (the name of
		// the router and what the draft carried are the settings of the backup too)
		want := committed
		want.Interfaces, staged.Interfaces = nil, nil
		if !reflect.DeepEqual(staged.Networks, want.Networks) || !reflect.DeepEqual(staged.Reservations, want.Reservations) ||
			!reflect.DeepEqual(staged.Routes, want.Routes) || !reflect.DeepEqual(staged.PortNetworks, want.PortNetworks) ||
			!reflect.DeepEqual(staged.LanInterfaces, want.LanInterfaces) || staged.WanInterface != want.WanInterface ||
			staged.LanIP != want.LanIP || staged.LanSubnet != want.LanSubnet || !reflect.DeepEqual(staged.DHCP, want.DHCP) {
			t.Fatalf("the draft is not alpha's setup:\n got %+v\nwant %+v", staged, want)
		}
		if err := staged.Validate(); err != nil {
			t.Fatalf("what was restored is not valid: %v", err)
		}
		// the nicknames count at once, and replace the ones the router had
		if got := bNames.All(); !reflect.DeepEqual(got, aNames.All()) || bNames.Get("02:00:00:00:00:99") != "" {
			t.Fatalf("nicknames: %v", got)
		}
		onDisk, _ := os.ReadFile(filepath.Join(b.dir, "client_names.json"))
		var m map[string]string
		if json.Unmarshal(onDisk, &m) != nil || len(m) != 3 {
			t.Fatalf("the nicknames were not saved: %s", onDisk)
		}
		// the history of the devices comes at the next start, before the daemon reads it
		if !res.NeedsRestart {
			t.Error("the history waits for a restart and the page must say so")
		}
		if done := ApplyPending(b.dir); len(done) == 0 {
			t.Fatal("nothing was staged")
		}
		if got, _ := os.ReadFile(filepath.Join(b.dir, "client_usage.json")); !bytes.Equal(got, usage) {
			t.Errorf("usage: %s", got)
		}
		if got, _ := os.ReadFile(filepath.Join(b.dir, "client_presence.json")); !bytes.Equal(got, seen) {
			t.Errorf("last seen: %s", got)
		}
	})

	t.Run("onto a router with other ports, keeping its own", func(t *testing.T) {
		g, gNames := audited(t, "gamma", []string{"enp1s0", "enp2s0", "enp3s0"}, nil)
		rep, err := g.svc.Inspect(data, pw)
		if err != nil {
			t.Fatal(err)
		}
		res, err := g.svc.Restore(rep.Token, Options{Settings: true, KeepPorts: true})
		if err != nil {
			t.Fatal(err)
		}
		_ = res
		staged, _ := g.mgr.StagedNetworkConfig()
		// the setup comes across (the subnets, the DHCP servers, the reservations, the routes) and the ports are this router's own
		if !reflect.DeepEqual(staged.Networks, committed.Networks) || !reflect.DeepEqual(staged.Reservations, committed.Reservations) ||
			!reflect.DeepEqual(staged.Routes, committed.Routes) || staged.LanIP != committed.LanIP {
			t.Errorf("the setup did not come across: %+v", staged)
		}
		if staged.WanInterface != "enp1s0" || len(staged.LanInterfaces) != 1 || staged.LanInterfaces[0] != "enp2s0" {
			t.Errorf("the router's own ports were not kept: wan %q lan %v", staged.WanInterface, staged.LanInterfaces)
		}
		if err := staged.Validate(); err != nil {
			t.Errorf("not valid on this router: %v", err)
		}
		if !reflect.DeepEqual(gNames.All(), aNames.All()) {
			t.Errorf("nicknames: %v", gNames.All())
		}
	})

	t.Run("a backup from a version without nicknames leaves the router's own", func(t *testing.T) {
		old := &Bundle{Files: map[string][]byte{}}
		plain, err := Open(data, pw)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(plain)
		if err != nil {
			t.Fatal(err)
		}
		for p, body := range decoded.Files {
			if p != FileNames {
				old.Files[p] = body
			}
		}
		old.Manifest = decoded.Manifest
		o, oNames := audited(t, "omega", []string{"eth0", "eth1", "eth2"}, nil)
		oNames.Set("02:00:00:00:00:77", "kept")
		encoded, err := old.Encode()
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := Seal(encoded, pw)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := o.svc.Inspect(sealed, pw)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Settings.Nicknames != 0 {
			t.Errorf("nicknames in a backup that has none: %d", rep.Settings.Nicknames)
		}
		if _, err := o.svc.Restore(rep.Token, Options{Settings: true}); err != nil {
			t.Fatal(err)
		}
		if oNames.Get("02:00:00:00:00:77") != "kept" {
			t.Error("a backup without nicknames removed the router's own")
		}
	})
}

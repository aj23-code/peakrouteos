package backup

import (
	"bytes"
	"database/sql"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/cert"
	"peakroute/internal/config"
)

type sessions struct{ revoked int }

func (s *sessions) RevokeAll() int { s.revoked++; return 1 }

// router is a router in a directory: its settings, administrators, certificates and a database, and a service over them.
type router struct {
	t        *testing.T
	dir      string
	certDir  string
	mgr      *config.Manager
	certs    *cert.Manager
	svc      *Service
	sessions *sessions
	editable bool
	ports    []string
}

func hash(t *testing.T, pw string) string {
	h, err := config.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// newRouter makes one. The certificate package keeps its place in a variable, so use() points it at this router.
func newRouter(t *testing.T, name string, ports []string, edit func(*config.NetworkConfig)) *router {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	r := &router{t: t, dir: dir, certDir: filepath.Join(dir, "certs"), sessions: &sessions{}, editable: true, ports: ports}
	os.MkdirAll(r.certDir, 0700)
	mgr, err := config.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.mgr = mgr
	if err := mgr.UpdateAuth(func(a *config.AuthConfig) error {
		a.Users = []config.User{{Username: "admin", PasswordHash: hash(t, "admin-password-"+name), Role: "admin"}, {Username: "viewer", PasswordHash: hash(t, "viewer-password-"+name), Role: "readonly"}}
		a.SessionTimeoutMinutes = 30
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.WanInterface, c.WanMode = ports[0], "dhcp"
		c.LanInterfaces = []string{ports[1]}
		c.LanIP, c.LanSubnet = "192.168.1.1", "255.255.255.0"
		c.Hostname = name
		c.DHCP = &config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"}
		if edit != nil {
			edit(c)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
	r.use()
	r.certs = &cert.Manager{Wanted: func() cert.Names { return cert.Names{DNS: []string{name}, IPs: []net.IP{net.ParseIP("192.168.1.1")}} }}
	if err := r.certs.Ensure(); err != nil {
		t.Fatal(err)
	}
	// a history database
	db, err := sql.Open("sqlite", filepath.Join(dir, "wan_health.db")+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE samples (n TEXT); INSERT INTO samples VALUES ('" + name + "')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r.svc = &Service{Env: Env{ConfigDir: dir, CertDir: r.certDir, Manager: mgr, Certs: r.certs, Sessions: r.sessions,
		Ports: func() ([]config.SystemInterface, error) {
			var out []config.SystemInterface
			for _, p := range r.ports {
				out = append(out, config.SystemInterface{Name: p})
			}
			return out, nil
		},
		Editable: func() bool { return r.editable }, Hostname: func() string { return name }, Software: "test",
		Now: func() time.Time { return time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC) }}}
	return r
}

func fingerprintOf(t *testing.T, pemBytes []byte) string {
	d, err := cert.DescribePEM(pemBytes)
	if err != nil {
		t.Fatal(err)
	}
	return d.Fingerprint
}

func (r *router) use() {
	cert.EnableDevPaths(r.certDir)
}

func (r *router) file(name string) []byte {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

const pw = "a long passphrase"

func backupOf(t *testing.T, r *router, history bool) []byte {
	t.Helper()
	r.use()
	data, name, err := r.svc.Create(pw, history)
	if err != nil {
		t.Fatal(err)
	}
	if name != "peakroute-"+r.mgr.NetworkConfig().Hostname+"-2026-10-11.prbackup" {
		t.Fatalf("%s", name)
	}
	return data
}

func TestABackupIsEncryptedAndShowsWhatIsInIt(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	data := backupOf(t, a, false)
	for _, secret := range []string{"alpha", "admin", "BEGIN", "wan_interface", "$2a$"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("%q is visible in the file", secret)
		}
	}
	rep, err := a.svc.Inspect(data, pw)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Hostname != "alpha" || rep.Software != "test" || rep.Created == 0 || rep.Token == "" || strings.Join(rep.Sections, ",") != "settings,admins,certificates" || len(rep.History) != 0 {
		t.Fatalf("%+v", rep)
	}
	if rep.Settings == nil || !rep.Settings.Valid || len(rep.Settings.Changes) != 0 || len(rep.Settings.MissingPorts) != 0 || rep.Settings.DraftWaits {
		t.Fatalf("%+v", rep.Settings)
	}
	if rep.Admins == nil || !rep.Admins.Valid || len(rep.Admins.Accounts) != 2 || rep.Admins.Current != 2 || rep.Admins.Accounts[0].Name != "admin" {
		t.Fatalf("%+v", rep.Admins)
	}
	if rep.Certificates == nil || !rep.Certificates.Valid || rep.Certificates.Web == nil || rep.Certificates.Authority == nil || rep.Certificates.Custom {
		t.Fatalf("%+v", rep.Certificates)
	}
	// with history: the databases are listed
	rep, _ = a.svc.Inspect(backupOf(t, a, true), pw)
	if len(rep.History) != 1 || rep.History[0].Name != "wan_health.db" || rep.History[0].Size == 0 || !strings.Contains(strings.Join(rep.Sections, ","), "history") {
		t.Fatalf("%+v", rep.History)
	}
	// a wrong passphrase, a bad passphrase for a new backup, something that is not a backup
	if _, err := a.svc.Inspect(data, "not the passphrase"); err == nil || !strings.Contains(err.Error(), "wrong") {
		t.Fatalf("%v", err)
	}
	if _, _, err := a.svc.Create("short", false); err == nil {
		t.Fatal("a short passphrase")
	}
	if _, err := a.svc.Inspect([]byte("not a backup file at all, not even close to one"), pw); err == nil {
		t.Fatal("a file that is no backup")
	}
}

func TestARestoreOntoAnotherRouterBringsEverythingAcross(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, func(c *config.NetworkConfig) {
		c.Networks = []config.Network{{ID: "net2", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0", Internet: true}}
		c.Domain = "home.arpa"
	})
	data := backupOf(t, a, true)
	wantCA, wantCAKey := a.file("certs/ca.crt"), a.file("certs/ca.key")
	wantAuth := a.file("auth.json")
	wantNet := a.file("network.json")

	b := newRouter(t, "beta", []string{"eth0", "eth1"}, nil)
	b.use()
	// a draft is waiting on the other router: restoring the settings replaces it
	b.mgr.StageNetwork(func(c *config.NetworkConfig) error { c.Domain = "draft.example"; return nil })
	rep, err := b.svc.Inspect(data, pw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Settings.Changes) == 0 || !rep.Settings.DraftWaits {
		t.Fatalf("%+v", rep.Settings)
	}
	res, err := b.svc.Restore(rep.Token, Options{Settings: true, Admins: true, Certificates: true, History: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Done, ",") != "certificates,admins,settings,history" || !res.SignedOut || !res.NeedsRestart || !res.DraftReplaced || res.Copy == "" || b.sessions.revoked != 1 {
		t.Fatalf("%+v", res)
	}
	// the settings are the draft, not yet committed; the committed ones are still beta's own
	if b.mgr.NetworkConfig().Hostname != "beta" {
		t.Fatalf("the settings were committed at once: %q", b.mgr.NetworkConfig().Hostname)
	}
	staged, ok := b.mgr.StagedNetworkConfig()
	if !ok || staged.Hostname != "alpha" || staged.Domain != "home.arpa" || len(staged.Networks) != 1 {
		t.Fatalf("%+v %v", staged, ok)
	}
	// the administrators and the certificates are alpha's, byte for byte
	if !bytes.Equal(b.file("auth.json"), wantAuth) || !bytes.Equal(b.file("certs/ca.crt"), wantCA) || !bytes.Equal(b.file("certs/ca.key"), wantCAKey) {
		t.Fatal("what was restored is not what was backed up")
	}
	// the web authority is alpha's, so that the devices that trust it still do; the certificate it signed was for alpha's name, so beta
	// has one made again for its own, by that authority
	authority, _ := os.ReadFile(filepath.Join(b.certDir, "ca.crt"))
	if st := b.certs.Status(); st.Web == nil || st.Authority == nil || st.Authority.Fingerprint != fingerprintOf(t, wantCA) || len(st.Missing) != 0 || st.Mode != "builtin" {
		t.Fatalf("%+v %s", st, authority)
	}
	if users := b.mgr.Users(); len(users) != 2 || users[0].Username != "admin" || b.mgr.SessionTimeout() != 30*time.Minute {
		t.Fatalf("%+v", users)
	}
	got, _ := b.certs.GetCertificate(nil)
	if served, err := os.ReadFile(filepath.Join(b.certDir, "tls.crt")); err != nil || got == nil || !bytes.Contains(served, []byte("BEGIN CERTIFICATE")) {
		t.Fatal("the certificate is not served")
	}
	// committing the draft makes it the settings
	if err := b.mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.file("network.json"), wantNet) {
		t.Fatalf("the committed settings are not the backed up ones:\n%s\n%s", b.file("network.json"), wantNet)
	}
	// the history waits for the next start, and is swapped in then
	if _, err := os.Stat(filepath.Join(b.dir, "restore-pending", "wan_health.db")); err != nil {
		t.Fatal("the history is not staged")
	}
	if done := ApplyPending(b.dir); len(done) != 1 || done[0] != "wan_health.db" {
		t.Fatalf("%v", done)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "restore-pending")); err == nil {
		t.Fatal("the staging directory stays")
	}
	db, _ := sql.Open("sqlite", filepath.Join(b.dir, "wan_health.db"))
	defer db.Close()
	var who string
	if err := db.QueryRow("SELECT n FROM samples").Scan(&who); err != nil || who != "alpha" {
		t.Fatalf("%q %v", who, err)
	}
	// the open backup was used up
	if _, err := b.svc.Restore(rep.Token, Options{Settings: true}); err == nil {
		t.Fatal("a backup that was used up opened again")
	}
}

func TestOnlyTheChosenSectionsAreRestored(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	data := backupOf(t, a, true)
	b := newRouter(t, "beta", []string{"eth0", "eth1"}, nil)
	b.use()
	authBefore, certBefore := b.file("auth.json"), b.file("certs/tls.crt")
	rep, _ := b.svc.Inspect(data, pw)
	res, err := b.svc.Restore(rep.Token, Options{Settings: true})
	if err != nil || strings.Join(res.Done, ",") != "settings" || res.SignedOut || res.NeedsRestart {
		t.Fatalf("%v %+v", err, res)
	}
	if !bytes.Equal(authBefore, b.file("auth.json")) || !bytes.Equal(certBefore, b.file("certs/tls.crt")) || b.sessions.revoked != 0 {
		t.Fatal("something that was not chosen changed")
	}
	if _, err := os.Stat(filepath.Join(b.dir, "restore-pending")); err == nil {
		t.Fatal("history staged without being chosen")
	}
	// nothing chosen, something the backup lacks
	rep, _ = b.svc.Inspect(data, pw)
	if _, err := b.svc.Restore(rep.Token, Options{}); err == nil || !strings.Contains(err.Error(), "choose") {
		t.Fatalf("%v", err)
	}
	noHist, _ := b.svc.Inspect(backupOf(t, a, false), pw)
	if _, err := b.svc.Restore(noHist.Token, Options{History: true}); err == nil || !strings.Contains(err.Error(), "does not have") {
		t.Fatalf("%v", err)
	}
}

func TestOnADifferentRouterItsOwnPortsCanBeKept(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, func(c *config.NetworkConfig) {
		c.Networks = []config.Network{{ID: "net2", Name: "Guest", VLAN: 20, IP: "10.20.0.1", Netmask: "255.255.255.0"}}
		c.PortNetworks = map[string]config.PortNetworks{"eth1": {Tagged: []string{"net2"}}}
	})
	data := backupOf(t, a, false)
	b := newRouter(t, "beta", []string{"enp1s0", "enp2s0", "enp3s0"}, nil)
	b.use()
	rep, err := b.svc.Inspect(data, pw)
	if err != nil {
		t.Fatal(err)
	}
	// the page is told the ports differ, and the report is for keeping the router's own
	if len(rep.Settings.MissingPorts) != 2 || rep.Settings.MissingPorts[0] != "eth0" || !rep.Settings.KeepPorts || !rep.Settings.Valid {
		t.Fatalf("%+v", rep.Settings)
	}
	keep, _ := b.svc.Preview(rep.Token, true)
	asIs, _ := b.svc.Preview(rep.Token, false)
	if !keep.Settings.KeepPorts || asIs.Settings.KeepPorts || len(asIs.Settings.MissingPorts) != 2 {
		t.Fatalf("%+v %+v", keep.Settings, asIs.Settings)
	}
	if _, err := b.svc.Restore(rep.Token, Options{Settings: true, KeepPorts: true}); err != nil {
		t.Fatal(err)
	}
	st, _ := b.mgr.StagedNetworkConfig()
	if st.WanInterface != "enp1s0" || len(st.LanInterfaces) != 1 || st.LanInterfaces[0] != "enp2s0" || st.Hostname != "alpha" || len(st.Networks) != 1 {
		t.Fatalf("%+v", st)
	}
	// alpha's eth1 carried the Guest network as a VLAN: beta has no eth1, and its own ports carried nothing extra
	if len(st.PortNetworks) != 0 {
		t.Fatalf("the ports of the backup came along: %+v", st.PortNetworks)
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("%v", err)
	}
	// where a port of the router carried a network that the backup lacks, the reference is dropped
	c := newRouter(t, "gamma", []string{"eth0", "eth1"}, func(c *config.NetworkConfig) {
		c.Networks = []config.Network{{ID: "iot", Name: "IoT", VLAN: 30, IP: "10.30.0.1", Netmask: "255.255.255.0"}}
		c.PortNetworks = map[string]config.PortNetworks{"eth1": {Native: "iot", Tagged: []string{"iot"}}}
	})
	c.use()
	rep, _ = c.svc.Inspect(data, pw)
	if _, err := c.svc.Restore(rep.Token, Options{Settings: true, KeepPorts: true}); err != nil {
		t.Fatal(err)
	}
	st, _ = c.mgr.StagedNetworkConfig()
	if pn, ok := st.PortNetworks["eth1"]; ok {
		t.Fatalf("a port keeps a network that is gone: %+v", pn)
	}
	if err := st.Validate(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestWifiAdaptersThatAreNotThereAreLeftOut(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, func(c *config.NetworkConfig) {
		c.Wifi = &config.WifiConfig{Radios: []config.WifiRadio{{Name: "wlp2s0", Band: config.Band24, Width: 20}, {Name: "wlan9", Band: config.Band5, Width: 20}}}
	})
	data := backupOf(t, a, false)
	b := newRouter(t, "beta", []string{"eth0", "eth1"}, nil)
	b.use()
	b.svc.Radios = func() []string { return []string{"wlp2s0"} }
	rep, _ := b.svc.Inspect(data, pw)
	if _, err := b.svc.Restore(rep.Token, Options{Settings: true, KeepPorts: true}); err != nil {
		t.Fatal(err)
	}
	st, _ := b.mgr.StagedNetworkConfig()
	if st.Wifi == nil || len(st.Wifi.Radios) != 1 || st.Wifi.Radios[0].Name != "wlp2s0" {
		t.Fatalf("%+v", st.Wifi)
	}
}

func TestAnythingThatCannotBeRestoredStopsBeforeAnythingChanges(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	good := backupOf(t, a, false)
	b := newRouter(t, "beta", []string{"eth0", "eth1"}, nil)
	b.use()
	authBefore, certBefore, netBefore := b.file("auth.json"), b.file("certs/tls.crt"), b.file("network.json")

	forge := func(edit func(*Bundle)) []byte {
		plain, _ := Open(good, pw)
		bundle, _ := Decode(plain)
		edit(bundle)
		enc, err := bundle.Encode()
		if err != nil {
			t.Fatal(err)
		}
		sealed, _ := Seal(enc, pw)
		return sealed
	}
	bad := map[string]func(*Bundle){
		"settings that are not valid": func(x *Bundle) { x.Files[FileNetwork] = []byte(`{"wan_interface":"eth0","wan_mode":"carrier-pigeon"}`) },
		"settings that are not json":  func(x *Bundle) { x.Files[FileNetwork] = []byte(`{`) },
		"no administrator": func(x *Bundle) {
			x.Files[FileAuth] = []byte(`{"users":[{"username":"viewer","password_hash":"$2a$10$abc","role":"readonly"}]}`)
		},
		"an account with no password": func(x *Bundle) {
			x.Files[FileAuth] = []byte(`{"users":[{"username":"admin","password_hash":"plain","role":"admin"}]}`)
		},
		"an account with a bad name": func(x *Bundle) {
			x.Files[FileAuth] = []byte(`{"users":[{"username":"a b","password_hash":"$2a$10$abc","role":"admin"}]}`)
		},
		"the same account twice": func(x *Bundle) {
			x.Files[FileAuth] = []byte(`{"users":[{"username":"admin","password_hash":"$2a$10$abc","role":"admin"},{"username":"ADMIN","password_hash":"$2a$10$abc","role":"admin"}]}`)
		},
		"a role that is none": func(x *Bundle) {
			x.Files[FileAuth] = []byte(`{"users":[{"username":"admin","password_hash":"$2a$10$abc","role":"god"}]}`)
		},
		"a key that does not fit":    func(x *Bundle) { x.Files["certificates/tls.key"] = x.Files["certificates/ca.key"] },
		"no key for the certificate": func(x *Bundle) { delete(x.Files, "certificates/tls.key") },
	}
	for name, edit := range bad {
		rep, err := b.svc.Inspect(forge(edit), pw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		valid := (rep.Settings == nil || rep.Settings.Valid) && (rep.Admins == nil || rep.Admins.Valid) && (rep.Certificates == nil || rep.Certificates.Valid)
		if valid {
			t.Errorf("%s: the report says all is well", name)
		}
		_, err = b.svc.Restore(rep.Token, Options{Settings: true, Admins: true, Certificates: true})
		if err == nil {
			t.Errorf("%s: it was restored", name)
		}
		if !bytes.Equal(authBefore, b.file("auth.json")) || !bytes.Equal(certBefore, b.file("certs/tls.crt")) || !bytes.Equal(netBefore, b.file("network.json")) {
			t.Fatalf("%s: something changed", name)
		}
		if _, ok := b.mgr.StagedNetworkConfig(); ok {
			t.Fatalf("%s: a draft appeared", name)
		}
	}
	if list, _ := b.svc.Copies(); len(list) != 0 {
		t.Fatalf("a copy was kept for restores that did not happen: %v", list)
	}
	// a change that is waiting for confirmation: no restore
	b.editable = false
	rep, _ := b.svc.Inspect(good, pw)
	if _, err := b.svc.Restore(rep.Token, Options{Settings: true}); err == nil || !strings.Contains(err.Error(), "waiting for confirmation") {
		t.Fatalf("%v", err)
	}
	// an open backup that has run out
	b.editable = true
	rep, _ = b.svc.Inspect(good, pw)
	b.svc.Now = func() time.Time { return time.Date(2026, 10, 11, 12, 30, 0, 0, time.UTC) }
	if _, err := b.svc.Restore(rep.Token, Options{Settings: true}); err == nil || !strings.Contains(err.Error(), "no longer open") {
		t.Fatalf("%v", err)
	}
	if _, err := b.svc.Preview("nope", false); err == nil {
		t.Fatal("a token that never was")
	}
}

func TestARestoreCanBeUndoneFromTheCopyTheRouterKept(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	data := backupOf(t, a, false)
	b := newRouter(t, "beta", []string{"eth0", "eth1"}, func(c *config.NetworkConfig) { c.Domain = "beta.example" })
	b.use()
	was := struct{ auth, cert, key, ca, net []byte }{b.file("auth.json"), b.file("certs/tls.crt"), b.file("certs/tls.key"), b.file("certs/ca.crt"), b.file("network.json")}
	rep, _ := b.svc.Inspect(data, pw)
	res, err := b.svc.Restore(rep.Token, Options{Settings: true, Admins: true, Certificates: true})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(was.auth, b.file("auth.json")) || bytes.Equal(a.file("certs/ca.crt"), was.ca) == false && !bytes.Equal(b.file("certs/ca.crt"), a.file("certs/ca.crt")) {
		t.Fatal("nothing was restored")
	}
	copies, err := b.svc.Copies()
	if err != nil || len(copies) != 1 || copies[0].ID != res.Copy || copies[0].Hostname != "beta" || strings.Join(copies[0].Sections, ",") != "settings,admins,certificates" || copies[0].Size == 0 {
		t.Fatalf("%v %+v", err, copies)
	}
	// the copy holds beta's own state and is not downloadable by any route here: it is a file in a directory of its own
	if info, err := os.Stat(filepath.Join(b.dir, "backups")); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("%v", err)
	}
	undo, err := b.svc.RestoreCopy(res.Copy)
	if err != nil || undo.Copy == "" || undo.Copy == res.Copy {
		t.Fatalf("%v %+v", err, undo)
	}
	if !bytes.Equal(was.auth, b.file("auth.json")) || !bytes.Equal(was.cert, b.file("certs/tls.crt")) || !bytes.Equal(was.key, b.file("certs/tls.key")) || !bytes.Equal(was.net, b.file("network.json")) {
		t.Fatal("the undo did not bring back what was there")
	}
	if _, ok := b.mgr.StagedNetworkConfig(); ok {
		t.Fatal("a draft is left that is no change at all")
	}
	// the undo is undoable too, and only the newest three are kept
	for i := 0; i < 4; i++ {
		b.svc.Now = func() time.Time { return time.Date(2026, 10, 11, 13, i, 0, 0, time.UTC) }
		newest, _ := b.svc.Copies()
		if _, err := b.svc.RestoreCopy(newest[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	if list, _ := b.svc.Copies(); len(list) != 3 || list[0].ID < list[1].ID {
		t.Fatalf("%v", list)
	}
	// deleting, and what is not a copy
	list, _ := b.svc.Copies()
	if err := b.svc.DeleteCopy(list[2].ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../../etc/passwd", "12a", "999999999999999", "123456789012345678901"} {
		if err := b.svc.DeleteCopy(id); err == nil {
			t.Errorf("%q was deleted", id)
		}
		if _, err := b.svc.RestoreCopy(id); err == nil {
			t.Errorf("%q was restored", id)
		}
	}
	os.WriteFile(filepath.Join(b.dir, "backups", "999.tgz"), []byte("not a backup"), 0600)
	if l, _ := b.svc.Copies(); len(l) != 2 {
		t.Fatalf("a damaged file is listed: %v", l)
	}
}

func TestACustomCertificateComesBackAsCustom(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	a.use()
	// a custom certificate on alpha: another authority's, with its marker
	other := newRouter(t, "other", []string{"eth0", "eth1"}, nil)
	custom, key := other.file("certs/tls.crt"), other.file("certs/tls.key")
	a.use()
	if err := a.certs.Install(custom, key); err != nil {
		t.Fatal(err)
	}
	data := backupOf(t, a, false)
	b := newRouter(t, "beta", []string{"eth0", "eth1"}, nil)
	b.use()
	rep, _ := b.svc.Inspect(data, pw)
	if !rep.Certificates.Custom {
		t.Fatalf("%+v", rep.Certificates)
	}
	if _, err := b.svc.Restore(rep.Token, Options{Certificates: true}); err != nil {
		t.Fatal(err)
	}
	if !cert.Custom() || !bytes.Equal(b.file("certs/tls.crt"), custom) || b.certs.Status().Mode != "custom" {
		t.Fatal("the custom certificate did not come back as custom")
	}
	// and the other way: a built-in certificate over a custom one takes the marker away
	c := newRouter(t, "gamma", []string{"eth0", "eth1"}, nil)
	builtinBackup := backupOf(t, c, false)
	b.use()
	rep, _ = b.svc.Inspect(builtinBackup, pw)
	if _, err := b.svc.Restore(rep.Token, Options{Certificates: true}); err != nil {
		t.Fatal(err)
	}
	if cert.Custom() || b.certs.Status().Mode != "builtin" {
		t.Fatal("the marker of the custom certificate stays")
	}
}

func TestTheInfoForThePage(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	copies, _ := a.svc.Copies()
	if copies == nil || len(copies) != 0 {
		t.Fatalf("%v", copies)
	}
	if ApplyPending(t.TempDir()) != nil {
		t.Fatal("something was applied that was not there")
	}
}

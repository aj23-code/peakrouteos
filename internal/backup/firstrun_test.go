package backup

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"peakroute/internal/cert"
	"peakroute/internal/config"
)

// blank is a router that has just been installed: nothing set up, only its certificate.
func blank(t *testing.T, name string, ports []string) *router {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	r := &router{t: t, dir: dir, certDir: filepath.Join(dir, "certs"), sessions: &sessions{}, editable: true, ports: ports}
	os.MkdirAll(r.certDir, 0700)
	mgr, err := config.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	r.mgr = mgr
	r.use()
	r.certs = &cert.Manager{Wanted: func() cert.Names { return cert.Names{DNS: []string{name}, IPs: []net.IP{net.ParseIP("192.168.1.1")}} }}
	if err := r.certs.Ensure(); err != nil {
		t.Fatal(err)
	}
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

func TestARestoreSetsUpAnUnconfiguredRouter(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	data := backupOf(t, a, false)

	b := blank(t, "fresh", []string{"enp1s0", "enp2s0", "enp3s0"})
	if b.mgr.IsConfigured() {
		t.Fatal("a new router starts set up")
	}
	rep, err := b.svc.Inspect(data, pw)
	if err != nil {
		t.Fatal(err)
	}
	// the ports of the backup are not here: they must be assigned
	if len(rep.Settings.MissingPorts) != 2 {
		t.Fatalf("%+v", rep.Settings)
	}
	if _, err := b.svc.RestoreFirstRun(rep.Token, Options{Settings: true}); err == nil {
		t.Fatal("restored with ports that do not exist")
	}
	if b.mgr.IsConfigured() {
		t.Fatal("a failed restore left the router set up")
	}
	// a choice that is not valid is refused, one that is is reported
	for _, bad := range []PortChoice{{}, {Wan: "enp1s0"}, {Wan: "enp1s0", Lan: []string{"enp1s0"}}, {Wan: "enp9s0", Lan: []string{"enp2s0"}}} {
		if r, err := b.svc.PreviewChoice(rep.Token, bad); err != nil || r.Settings.Valid {
			t.Fatalf("%+v: %v %+v", bad, err, r)
		}
	}
	good := PortChoice{Wan: "enp1s0", Lan: []string{"enp2s0", "enp3s0"}}
	if r, err := b.svc.PreviewChoice(rep.Token, good); err != nil || !r.Settings.Valid {
		t.Fatalf("%v %+v", err, r)
	}
	// the choice is applied; the administrators and certificates come, the history only when asked
	if _, err := b.svc.RestoreFirstRun(rep.Token, Options{Settings: true, Certificates: true, Ports: &good}); err != nil {
		t.Fatal(err)
	}
	if !b.mgr.IsConfigured() {
		t.Fatal("the router is not marked as set up")
	}
	if len(b.mgr.Users()) != 2 {
		t.Fatalf("%d administrators", len(b.mgr.Users()))
	}
	st, draft := b.mgr.StagedNetworkConfig()
	if !draft || st.Hostname != "alpha" || st.WanInterface != "enp1s0" || len(st.LanInterfaces) != 2 {
		t.Fatalf("%v %+v", draft, st)
	}
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	if string(b.file("configured")) == "" {
		t.Fatal("the flag is not on disk")
	}
	if got := fingerprintOf(t, b.file("certs/ca.crt")); got != fingerprintOf(t, a.file("certs/ca.crt")) {
		t.Fatalf("the authority is not the backup's")
	}
	// it needs no more: a second restore with the same token is gone
	if _, err := b.svc.RestoreFirstRun(rep.Token, Options{Settings: true}); err == nil {
		t.Fatal("the token worked twice")
	}
	// and a fresh manager reading the same directory sees the router as set up (all of it is in files)
	m2, err := config.NewManager(b.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !m2.IsConfigured() || len(m2.Users()) != 2 {
		t.Fatal("not saved")
	}
	if _, draft := m2.StagedNetworkConfig(); !draft {
		t.Fatal("the draft is not saved")
	}
}

func TestAFirstRunRestoreAlwaysBringsTheAdministrators(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	b := blank(t, "fresh", []string{"eth0", "eth1"})
	rep, err := b.svc.Inspect(backupOf(t, a, false), pw)
	if err != nil {
		t.Fatal(err)
	}
	// the ports are the same here, so nothing needs choosing; the administrators come although only the settings were asked for
	// (there would be no one to sign in)
	if _, err := b.svc.RestoreFirstRun(rep.Token, Options{Settings: true}); err != nil {
		t.Fatal(err)
	}
	if len(b.mgr.Users()) != 2 || !b.mgr.IsConfigured() {
		t.Fatalf("%d administrators, configured %v", len(b.mgr.Users()), b.mgr.IsConfigured())
	}
}

func TestTheConsoleLoginIsTheOneOfTheMachine(t *testing.T) {
	a := newRouter(t, "alpha", []string{"eth0", "eth1"}, nil)
	a.mgr.UpdateAuth(func(c *config.AuthConfig) error { c.ConsoleUser = "admin"; return nil })
	data := backupOf(t, a, false)

	// a router that has none takes the backup's (the setup then makes the Linux account)
	b := blank(t, "fresh", []string{"eth0", "eth1"})
	rep, _ := b.svc.Inspect(data, pw)
	if _, err := b.svc.RestoreFirstRun(rep.Token, Options{Settings: true}); err != nil {
		t.Fatal(err)
	}
	if b.mgr.ConsoleUser() != "admin" {
		t.Fatalf("%q", b.mgr.ConsoleUser())
	}

	// a router that has one keeps it: its Linux account is the one that exists
	c := newRouter(t, "gamma", []string{"eth0", "eth1"}, nil)
	c.mgr.UpdateAuth(func(cfg *config.AuthConfig) error { cfg.ConsoleUser = "mine"; return nil })
	c.use()
	rep, _ = c.svc.Inspect(data, pw)
	if _, err := c.svc.Restore(rep.Token, Options{Admins: true}); err != nil {
		t.Fatal(err)
	}
	if c.mgr.ConsoleUser() != "mine" {
		t.Fatalf("%q", c.mgr.ConsoleUser())
	}
}

package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"peakroute/internal/config"
)

type recorder struct {
	mu           sync.Mutex
	cmds         []string
	failOn       string // a command line that starts with this fails
	bridgeExists bool
}

func (r *recorder) Run(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, line)
	if strings.HasPrefix(line, "ip link show") {
		if r.bridgeExists {
			return "", nil
		}
		return "", errors.New("does not exist")
	}
	if r.failOn != "" && strings.HasPrefix(line, r.failOn) {
		return "boom", errors.New("exit 1")
	}
	if strings.HasPrefix(line, "ip link add "+Bridge) {
		r.bridgeExists = true
	}
	if strings.HasPrefix(line, "ip link del "+Bridge) {
		r.bridgeExists = false
	}
	return "", nil
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.cmds
	r.cmds = nil
	return out
}

func newService(t *testing.T) (*Service, *recorder, *config.Manager) {
	t.Helper()
	mgr, err := config.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	s := &Service{Run: rec, Manager: mgr, Dir: filepath.Join(t.TempDir(), "setup"),
		Ports: func() ([]config.SystemInterface, error) {
			return []config.SystemInterface{{Name: "enp1s0"}, {Name: "enp2s0"}, {Name: "wlp3s0", Wireless: true}, {Name: "enp4s0"}}, nil
		}}
	return s, rec, mgr
}

// commitNetwork gives the router a local network, as the first commit does.
func commitNetwork(t *testing.T, mgr *config.Manager) {
	t.Helper()
	if err := mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.WanInterface, c.WanMode = "enp1s0", "dhcp"
		c.LanInterfaces = []string{"enp2s0"}
		c.LanIP, c.LanSubnet = "10.1.0.1", "255.255.255.0"
		c.DHCP = &config.DHCPConfig{Enabled: true, RangeStart: "10.1.0.100", RangeEnd: "10.1.0.200", LeaseTime: "12h"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.CommitNetwork(); err != nil {
		t.Fatal(err)
	}
}

func TestTheAddressIsNotTheOneInternetProvidersUse(t *testing.T) {
	if Address == "192.168.1.1" || Address == "192.168.0.1" || Address == "10.0.0.1" || !strings.HasPrefix(Address, "192.168.77.") {
		t.Fatal(Address)
	}
	if !strings.HasPrefix(Bridge, "br") {
		t.Fatal("the port lists would offer the bridge")
	}
}

func TestTheSetupNetworkIsNeededUntilThereIsALocalNetwork(t *testing.T) {
	_, _, mgr := newService(t)
	if !Needed(mgr.NetworkConfig()) {
		t.Fatal("a new router has no local network")
	}
	// the draft is not the network: only what is committed counts
	if err := mgr.StageNetwork(func(c *config.NetworkConfig) error {
		c.WanInterface, c.LanInterfaces = "enp1s0", []string{"enp2s0"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !Needed(mgr.NetworkConfig()) {
		t.Fatal("a draft ended the setup network")
	}
	mgr.DiscardNetwork()
	commitNetwork(t, mgr)
	if Needed(mgr.NetworkConfig()) {
		t.Fatal("still needed with a local network")
	}
}

func TestStartingJoinsTheWiredPortsAndServesAddresses(t *testing.T) {
	s, rec, _ := newService(t)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rec.take(), "\n")
	want := strings.Join([]string{
		"ip link show brsetup",
		"ip link add brsetup type bridge",
		"ip addr flush dev enp1s0", "ip link set enp1s0 master brsetup", "ip link set enp1s0 up",
		"ip addr flush dev enp2s0", "ip link set enp2s0 master brsetup", "ip link set enp2s0 up",
		"ip addr flush dev enp4s0", "ip link set enp4s0 master brsetup", "ip link set enp4s0 up",
		"ip addr add 192.168.77.1/24 dev brsetup",
		"ip link set brsetup up",
		"dnsmasq --conf-file=" + s.conf(),
	}, "\n")
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "wlp3s0") {
		t.Fatal("a Wi-Fi adapter was joined")
	}
	if !s.Running() {
		t.Fatal("not running")
	}
	conf, _ := os.ReadFile(s.conf())
	for _, line := range []string{"interface=brsetup", "bind-interfaces", "port=0", "dhcp-range=192.168.77.100,192.168.77.199,255.255.255.0,1h", "dhcp-option=3", "dhcp-option=6"} {
		if !strings.Contains(string(conf), "\n"+line+"\n") {
			t.Errorf("the DHCP file lacks %q:\n%s", line, conf)
		}
	}
	// no DNS server: it would clash with the router's own, and the address is typed
	if strings.Contains(string(conf), "dhcp-option=3,") || strings.Contains(string(conf), "dhcp-option=6,") {
		t.Fatal("a router or DNS server is offered")
	}
}

func TestAFailureWhileStartingLeavesNothingBehind(t *testing.T) {
	s, rec, _ := newService(t)
	rec.failOn = "dnsmasq"
	if err := s.Start(); err == nil {
		t.Fatal("no error")
	}
	if s.Running() || rec.bridgeExists {
		t.Fatalf("running %v, bridge %v", s.Running(), rec.bridgeExists)
	}
	rec = &recorder{}
	s.Run = rec
	s.Ports = func() ([]config.SystemInterface, error) {
		return []config.SystemInterface{{Name: "wlp3s0", Wireless: true}}, nil
	}
	if err := s.Start(); err == nil || s.Running() {
		t.Fatalf("started without a wired port: %v", err)
	}
	for _, c := range rec.take() {
		if strings.HasPrefix(c, "ip link add") {
			t.Fatalf("made a bridge with nothing to join: %s", c)
		}
	}
}

func TestStoppingLetsGoOfThePorts(t *testing.T) {
	s, rec, _ := newService(t)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.pid(), []byte("123\n"), 0644) // the DHCP server writes it
	rec.take()
	s.Stop()
	got := rec.take()
	if len(got) != 2 || got[0] != "pkill --pidfile "+s.pid() || got[1] != "ip link del brsetup" {
		t.Fatalf("%v", got)
	}
	if s.Running() {
		t.Fatal("still running")
	}
	// stopping again, with nothing there, runs nothing that changes anything
	s.Stop()
	for _, c := range rec.take() {
		if !strings.HasPrefix(c, "ip link show") {
			t.Fatalf("%s", c)
		}
	}
}

func TestARestartedDaemonRemovesWhatTheLastOneLeft(t *testing.T) {
	s, rec, _ := newService(t)
	rec.bridgeExists = true // left by the daemon that ran before
	s.Stop()
	if got := rec.take(); len(got) != 2 || got[1] != "ip link del brsetup" {
		t.Fatalf("%v", got)
	}
	rec.bridgeExists = true
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	got := rec.take()
	del, add := -1, -1
	for i, c := range got {
		if c == "ip link del brsetup" {
			del = i
		}
		if strings.HasPrefix(c, "ip link add") {
			add = i
		}
	}
	if del < 0 || add < 0 || del > add {
		t.Fatalf("%v", got)
	}
}

func TestReconcileFollowsTheCommittedSettings(t *testing.T) {
	s, rec, mgr := newService(t)
	s.Reconcile()
	if !s.Running() {
		t.Fatal("not up for a router without a local network")
	}
	rec.take()
	s.Reconcile() // nothing to do
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("restarted: %v", got)
	}
	commitNetwork(t, mgr)
	s.Reconcile()
	if s.Running() || rec.bridgeExists {
		t.Fatal("still up with a local network")
	}
	// the change is reverted: the router has no local network again
	if err := mgr.RevertNetwork(config.NetworkConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	s.Reconcile()
	if !s.Running() || !rec.bridgeExists {
		t.Fatal("did not come back")
	}
}

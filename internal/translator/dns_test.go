package translator

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"peakroute/internal/config"
)

func dnsCfg(f func(*config.NetworkConfig)) config.NetworkConfig {
	c := fwCfg(nil)
	c.Firewall = nil
	c.DHCP = &config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"}
	for i := range c.Networks {
		c.Networks[i].DHCP = &config.DHCPConfig{Enabled: true, RangeStart: strings.Replace(c.Networks[i].IP, ".1", ".100", 1), RangeEnd: strings.Replace(c.Networks[i].IP, ".1", ".200", 1), LeaseTime: "1h"}
	}
	if f != nil {
		f(&c)
	}
	return c
}

func plainDNSCfg(f func(*config.NetworkConfig)) config.NetworkConfig {
	c := wanCfg(func(c *config.NetworkConfig) {})
	c.DHCP = &config.DHCPConfig{Enabled: true, RangeStart: "192.168.1.100", RangeEnd: "192.168.1.200", LeaseTime: "12h"}
	if f != nil {
		f(&c)
	}
	return c
}

func TestNothingChangesWithoutDNSSettings(t *testing.T) {
	for name, cfg := range map[string]config.NetworkConfig{"one network": plainDNSCfg(nil), "several": dnsCfg(nil)} {
		d := mustRender(t, cfg)
		a, ok := hasArtifact(d, PathDnsmasqLocal)
		if !ok || !a.Remove || a.Service != SvcDnsmasq {
			t.Errorf("%s: the DNS file is not removed: %+v", name, a)
		}
		dhcp := text(t, cfg, PathDnsmasqDHCP)
		if !strings.Contains(dhcp, "option:dns-server,192.168.1.1") && !strings.Contains(dhcp, "option:dns-server,10.20.0.1") {
			t.Errorf("%s: the router is not the DNS server:\n%s", name, dhcp)
		}
	}
	// an empty section changes nothing either
	c := plainDNSCfg(func(c *config.NetworkConfig) { c.DNS = &config.DNSConfig{} })
	if a, _ := hasArtifact(mustRender(t, c), PathDnsmasqLocal); !a.Remove {
		t.Errorf("an empty DNS section writes a file")
	}
	// artifacts stay grouped by service
	var order []string
	for _, a := range mustRender(t, dnsCfg(func(c *config.NetworkConfig) { c.Domain = "lan" })).Artifacts {
		if len(order) == 0 || order[len(order)-1] != a.Service {
			order = append(order, a.Service)
		}
	}
	if got := strings.Join(order, ","); got != "sysctl,networking,routes,nftables,dnsmasq" {
		t.Errorf("%s", got)
	}
}

func TestEachNetworkIsToldItsOwnDNSServers(t *testing.T) {
	one := text(t, plainDNSCfg(func(c *config.NetworkConfig) { c.LanDNSServers = []string{"192.168.1.5", "192.168.1.6"} }), PathDnsmasqDHCP)
	if !strings.Contains(one, "dhcp-option=option:dns-server,192.168.1.5,192.168.1.6\n") || strings.Contains(one, "dns-server,192.168.1.1\n") {
		t.Errorf("one network:\n%s", one)
	}
	if !strings.Contains(one, "dhcp-option=option:router,192.168.1.1\n") {
		t.Errorf("the router is still the gateway:\n%s", one)
	}

	many := text(t, dnsCfg(func(c *config.NetworkConfig) {
		c.LanDNSServers = []string{"192.168.1.5"}
		c.Networks[0].DNSServers = []string{"1.1.1.1", "9.9.9.9"} // Guest
		// IoT keeps the router
	}), PathDnsmasqDHCP)
	for _, want := range []string{
		"dhcp-option=tag:lan,option:router,192.168.1.1\n",
		"dhcp-option=tag:lan,option:dns-server,192.168.1.5\n",
		"dhcp-option=tag:guest,option:router,10.20.0.1\n",
		"dhcp-option=tag:guest,option:dns-server,1.1.1.1,9.9.9.9\n",
		"dhcp-option=tag:iot,option:router,10.30.0.1\n",
		"dhcp-option=tag:iot,option:dns-server,10.30.0.1\n",
	} {
		if !strings.Contains(many, want) {
			t.Errorf("missing %q in\n%s", want, many)
		}
	}
}

func TestTheLocalDNSFile(t *testing.T) {
	cfg := dnsCfg(func(c *config.NetworkConfig) {
		c.Domain = "home.example.com"
		c.DNS = &config.DNSConfig{
			Hosts: []config.DNSHost{
				{ID: "h1", Name: "nas", Address: "192.168.1.50"},
				{ID: "h2", Name: "www.example.com", Address: "192.168.1.50"},
				{ID: "h3", Name: "off", Address: "192.168.1.9", Disabled: true},
			},
			Forwards: []config.DNSForward{
				{ID: "f1", Domain: "corp.example.com", Servers: []string{"10.0.0.5", "10.0.0.6"}},
				{ID: "f2", Domain: "dead.example", Servers: []string{"10.0.0.7"}, Disabled: true},
			},
		}
	})
	a, ok := hasArtifact(mustRender(t, cfg), PathDnsmasqLocal)
	if !ok || a.Remove || a.Mode != 0644 || a.Service != SvcDnsmasq {
		t.Fatalf("%+v", a)
	}
	want := `# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.
domain=home.example.com
local=/home.example.com/
expand-hosts
host-record=nas,nas.home.example.com,192.168.1.50
host-record=www.example.com,192.168.1.50
server=/corp.example.com/10.0.0.5
server=/corp.example.com/10.0.0.6
`
	if string(a.Content) != want {
		t.Errorf("\n%s\nwant\n%s", a.Content, want)
	}
}

func TestTheLocalDNSFileOnlyHoldsWhatIsSet(t *testing.T) {
	cases := map[string]struct {
		cfg  config.NetworkConfig
		want string // "" means no file
	}{
		"only a domain": {plainDNSCfg(func(c *config.NetworkConfig) { c.Domain = "lan" }),
			"domain=lan\nlocal=/lan/\nexpand-hosts\n"},
		"names without a domain": {plainDNSCfg(func(c *config.NetworkConfig) {
			c.DNS = &config.DNSConfig{Hosts: []config.DNSHost{{ID: "h1", Name: "nas", Address: "192.168.1.50"}}}
		}), "host-record=nas,192.168.1.50\n"},
		"only a forward": {plainDNSCfg(func(c *config.NetworkConfig) {
			c.DNS = &config.DNSConfig{Forwards: []config.DNSForward{{ID: "f1", Domain: "a.example", Servers: []string{"10.0.0.5"}}}}
		}), "server=/a.example/10.0.0.5\n"},
		"everything switched off": {plainDNSCfg(func(c *config.NetworkConfig) {
			c.DNS = &config.DNSConfig{
				Hosts:    []config.DNSHost{{ID: "h1", Name: "nas", Address: "192.168.1.50", Disabled: true}},
				Forwards: []config.DNSForward{{ID: "f1", Domain: "a.example", Servers: []string{"10.0.0.5"}, Disabled: true}},
			}
		}), ""},
	}
	for name, c := range cases {
		a, ok := hasArtifact(mustRender(t, c.cfg), PathDnsmasqLocal)
		if !ok {
			t.Fatalf("%s: no artifact", name)
		}
		if c.want == "" {
			if !a.Remove {
				t.Errorf("%s: %q", name, a.Content)
			}
			continue
		}
		if a.Remove || !strings.HasSuffix(string(a.Content), c.want) || strings.Count(string(a.Content), "\n") != strings.Count(c.want, "\n")+1 {
			t.Errorf("%s:\n%s", name, a.Content)
		}
	}
}

func TestDNSValuesAreRefusedAtRenderTimeIfValidationIsBypassed(t *testing.T) {
	saved := validateConfig
	validateConfig = func(config.NetworkConfig) error { return nil }
	defer func() { validateConfig = saved }()

	host := func(name, addr string) config.NetworkConfig {
		return plainDNSCfg(func(c *config.NetworkConfig) {
			c.DNS = &config.DNSConfig{Hosts: []config.DNSHost{{ID: "h1", Name: name, Address: addr}}}
		})
	}
	fwd := func(domain string, servers ...string) config.NetworkConfig {
		return plainDNSCfg(func(c *config.NetworkConfig) {
			c.DNS = &config.DNSConfig{Forwards: []config.DNSForward{{ID: "f1", Domain: domain, Servers: servers}}}
		})
	}
	evil := map[string]config.NetworkConfig{
		"domain injecting":       plainDNSCfg(func(c *config.NetworkConfig) { c.Domain = "lan\nserver=6.6.6.6" }),
		"domain local":           plainDNSCfg(func(c *config.NetworkConfig) { c.Domain = "local" }),
		"domain in capitals":     plainDNSCfg(func(c *config.NetworkConfig) { c.Domain = "LAN" }),
		"host name injecting":    host("nas,6.6.6.6\naddress=/x/1.1.1.1", "192.168.1.5"),
		"host name with a slash": host("a/b", "192.168.1.5"),
		"host address injecting": host("nas", "192.168.1.5\nserver=6.6.6.6"),
		"host address a name":    host("nas", "evil.example"),
		"forward domain":         fwd("a.example/6.6.6.6", "10.0.0.5"),
		"forward server":         fwd("a.example", "10.0.0.5\naddress=/#/6.6.6.6"),
		"forward server a name":  fwd("a.example", "dns.corp"),
		"lan server":             plainDNSCfg(func(c *config.NetworkConfig) { c.LanDNSServers = []string{"1.1.1.1\ndhcp-option=3,6.6.6.6"} }),
		"network server":         dnsCfg(func(c *config.NetworkConfig) { c.Networks[0].DNSServers = []string{"1.1.1.1;6.6.6.6"} }),
	}
	for name, cfg := range evil {
		if _, err := Render(cfg, "ROUTER"); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestThePlanRestartsOnlyTheDHCPAndDNSServer(t *testing.T) {
	root := RootFS(t.TempDir())
	install(t, string(root), mustRender(t, plainDNSCfg(nil)))
	changed := plainDNSCfg(func(c *config.NetworkConfig) {
		c.Domain = "lan"
		c.DNS = &config.DNSConfig{Hosts: []config.DNSHost{{ID: "h1", Name: "nas", Address: "192.168.1.50"}}}
	})
	plan, err := PlanChanges(root, mustRender(t, changed))
	if err != nil {
		t.Fatal(err)
	}
	if got := actionSummary(plan); got != "restart dnsmasq" {
		t.Errorf("%s", got)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != PathDnsmasqLocal || plan.Files[0].Kind != Create {
		t.Errorf("%+v", plan.Files)
	}
	// and a change of what a network is told only touches the DHCP file
	install(t, string(root), mustRender(t, changed))
	servers := changed
	servers.LanDNSServers = []string{"192.168.1.5"}
	plan, err = PlanChanges(root, mustRender(t, servers))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != PathDnsmasqDHCP || actionSummary(plan) != "restart dnsmasq" {
		t.Errorf("%+v %s", plan.Files, actionSummary(plan))
	}
}

// ---- the real dnsmasq ------------------------------------------------------------------------------------------

func TestTheRealDnsmasqAcceptsTheDNSFiles(t *testing.T) {
	bin, err := exec.LookPath("dnsmasq")
	if err != nil {
		t.Skip("dnsmasq is not installed")
	}
	cfg := dnsCfg(func(c *config.NetworkConfig) {
		c.Domain = "lan"
		c.LanDNSServers = []string{"192.168.1.5"}
		c.Networks[0].DNSServers = []string{"1.1.1.1", "9.9.9.9"}
		c.DNS = &config.DNSConfig{
			Hosts:    []config.DNSHost{{ID: "h1", Name: "nas", Address: "192.168.1.50"}, {ID: "h2", Name: "home.example.com", Address: "192.168.1.50"}},
			Forwards: []config.DNSForward{{ID: "f1", Domain: "corp.example.com", Servers: []string{"10.0.0.5", "10.0.0.6"}}},
		}
	})
	dir := t.TempDir()
	for _, p := range []string{PathDnsmasqDHCP, PathDnsmasqLocal} {
		os.WriteFile(filepath.Join(dir, filepath.Base(p)), []byte(text(t, cfg, p)), 0644)
	}
	main := filepath.Join(dir, "main.conf")
	os.WriteFile(main, []byte("no-resolv\nno-hosts\nconf-dir="+dir+",*.conf\n"), 0644)
	out, err := exec.Command(bin, "--test", "--conf-file="+main).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "syntax check OK") {
		t.Fatalf("%v\n%s", err, out)
	}
	// the control: dnsmasq does complain about a line it does not know
	os.WriteFile(filepath.Join(dir, "bad.conf"), []byte("this-is-not-an-option=1\n"), 0644)
	if out, err := exec.Command(bin, "--test", "--conf-file="+main).CombinedOutput(); err == nil {
		t.Skipf("dnsmasq accepts anything here, so the check above proves nothing:\n%s", out)
	}
}

const envDNS = "PEAKROUTE_TEST_DNS_ROUTER"

// The real dnsmasq answers real queries according to the generated file. Three dnsmasq processes run in one throw-away
// namespace: the router's (10.9.0.1) with the generated local file, a server for a forwarded domain (10.9.0.2) and a general
// upstream (10.9.0.3) that answers every name with an address of its own, so that what the router did with a question can be
// read from the answer.
func TestTheRealDnsmasqAnswersAsTheFileSays(t *testing.T) {
	bin, err := exec.LookPath("dnsmasq")
	if err != nil {
		t.Skip("dnsmasq is not installed")
	}
	if os.Getenv(envDNS) != "1" {
		if _, err := exec.LookPath("unshare"); err != nil {
			t.Skip("no unshare")
		}
		exe, _ := os.Executable()
		cmd := exec.Command("unshare", "-rn", exe, "-test.run=^TestTheRealDnsmasqAnswersAsTheFileSays$", "-test.v")
		cmd.Env = append(os.Environ(), envDNS+"=1")
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), "--- SKIP") && !strings.Contains(string(out), "--- FAIL") {
			t.Skipf("%s", out)
		}
		if err != nil || strings.Contains(string(out), "--- FAIL") {
			t.Fatalf("%v\n%s", err, out)
		}
		return
	}

	run := func(name string, args ...string) {
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Skipf("cannot build the lab here: %s %v: %v %s", name, args, err, out)
		}
	}
	run("ip", "link", "set", "lo", "up")
	run("ip", "link", "add", "d0", "type", "dummy")
	for _, a := range []string{"10.9.0.1", "10.9.0.2", "10.9.0.3"} {
		run("ip", "addr", "add", a+"/24", "dev", "d0")
	}
	run("ip", "link", "set", "d0", "up")

	dir := t.TempDir()
	start := func(name, listen string, lines ...string) {
		conf := filepath.Join(dir, name+".conf")
		os.WriteFile(conf, []byte(strings.Join(append([]string{"no-resolv", "no-hosts", "no-daemon", "user=root", "bind-interfaces", "listen-address=" + listen, "pid-file=" + filepath.Join(dir, name+".pid")}, lines...), "\n")+"\n"), 0644)
		cmd := exec.Command(bin, "--conf-file="+conf)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Skipf("dnsmasq does not start here: %v", err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		time.Sleep(300 * time.Millisecond)
		if cmd.ProcessState != nil {
			t.Skipf("dnsmasq %s stopped: %s", name, stderr.String())
		}
	}
	// the server for the forwarded domain, and a general upstream that claims every name
	start("forward", "10.9.0.2", "address=/corp.example.com/1.1.1.1")
	start("upstream", "10.9.0.3", "address=/#/2.2.2.2")

	cfg := dnsCfg(func(c *config.NetworkConfig) {
		c.Domain = "lan"
		c.DNS = &config.DNSConfig{
			Hosts: []config.DNSHost{
				{ID: "h1", Name: "nas", Address: "192.168.1.50"},
				{ID: "h2", Name: "home.example.com", Address: "192.168.1.50"},
				{ID: "h3", Name: "off", Address: "192.168.1.9", Disabled: true},
			},
			Forwards: []config.DNSForward{{ID: "f1", Domain: "corp.example.com", Servers: []string{"10.9.0.2"}}},
		}
	})
	local := filepath.Join(dir, "peakroute-dns.conf")
	os.WriteFile(local, []byte(text(t, cfg, PathDnsmasqLocal)), 0644)
	start("router", "10.9.0.1", "conf-file="+local, "server=10.9.0.3")

	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "udp", "10.9.0.1:53")
	}}
	ask := func(name string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		addrs, err := resolver.LookupHost(ctx, name)
		if err != nil {
			if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
				return "not found"
			}
			return "error: " + err.Error()
		}
		return strings.Join(addrs, ",")
	}
	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: %s, want %s", what, got, want)
		}
	}
	check("a local name, in full", ask("nas.lan"), "192.168.1.50")
	check("a local name, short (the device's search domain adds the rest; asked plain it is answered too)", ask("nas"), "192.168.1.50")
	check("a local name that is a full name", ask("home.example.com"), "192.168.1.50")
	check("a switched-off name is not answered locally and, being inside the domain, not asked upstream", ask("off.lan"), "not found")
	check("an unknown name inside the domain stays inside", ask("ghost.lan"), "not found")
	check("a forwarded domain goes to its own server, not to the upstream", ask("intranet.corp.example.com"), "1.1.1.1")
	check("a name that is only forwarded below the domain", ask("a.b.corp.example.com"), "1.1.1.1")
	check("any other name goes upstream", ask("www.public.example"), "2.2.2.2")
	check("a name a local entry shadows is not sent upstream", ask("home.example.com"), "192.168.1.50")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	names, err := resolver.LookupAddr(ctx, "192.168.1.50")
	if err != nil || len(names) == 0 || !strings.HasPrefix(names[0], "nas") && !strings.HasPrefix(names[0], "home.example.com") {
		t.Errorf("a reverse lookup: %v %v", names, err)
	}
	_ = fmt.Sprint
}

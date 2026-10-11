package translator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

// install writes the files a render would produce into a fake root, as a box already in sync
func install(t *testing.T, root string, d Desired) {
	t.Helper()
	for _, a := range d.Artifacts {
		if a.Remove {
			continue
		}
		full := filepath.Join(root, a.Path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, a.Content, a.Mode); err != nil {
			t.Fatal(err)
		}
	}
}

func mustRender(t *testing.T, cfg config.NetworkConfig) Desired {
	t.Helper()
	d, err := Render(cfg, "ROUTER")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func actionSummary(p Plan) string {
	parts := make([]string, len(p.Actions))
	for i, a := range p.Actions {
		parts[i] = a.Verb + " " + a.Service
	}
	return strings.Join(parts, ", ")
}

func TestAnEmptySystemNeedsEverythingCreatedInOrder(t *testing.T) {
	plan, err := PlanChanges(RootFS(t.TempDir()), mustRender(t, routerCfg()))
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range plan.Files {
		if f.Kind != Create {
			t.Errorf("%s: kind = %s, want create", f.Path, f.Kind)
		}
	}
	if len(plan.Files) != 4 { // the upstream DNS file is "remove" and absent: nothing to do
		t.Errorf("%d files to write, want 4: %+v", len(plan.Files), plan.Files)
	}
	if got := actionSummary(plan); got != "apply sysctl, restart networking, reload nftables, restart dnsmasq" {
		t.Errorf("actions = %q", got)
	}
}

func TestAnUnchangedConfigurationNeedsNothing(t *testing.T) {
	root := t.TempDir()
	d := mustRender(t, routerCfg())
	install(t, root, d)

	plan, err := PlanChanges(RootFS(root), d)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Errorf("nothing changed but the plan is %+v", plan)
	}
}

func TestOnlyTheServicesWhoseFilesChangedAreTouched(t *testing.T) {
	cases := []struct {
		name    string
		change  func(*config.NetworkConfig)
		actions string
		files   []string
	}{
		{
			"DHCP lease time only",
			func(c *config.NetworkConfig) { c.DHCP.LeaseTime = "1d" },
			"restart dnsmasq",
			[]string{PathDnsmasqDHCP},
		},
		{
			"DHCP range only",
			func(c *config.NetworkConfig) { c.DHCP.RangeEnd = "192.168.1.150" },
			"restart dnsmasq",
			[]string{PathDnsmasqDHCP},
		},
		{
			"LAN address moves: networking, firewall and dhcp all change",
			func(c *config.NetworkConfig) {
				c.LanIP = "10.0.0.1"
				c.LanSubnet = "255.255.255.0"
				c.DHCP.RangeStart, c.DHCP.RangeEnd = "10.0.0.100", "10.0.0.200"
			},
			"restart networking, restart dnsmasq",
			[]string{PathInterfaces, PathDnsmasqDHCP},
		},
		{
			"WAN interface renamed: networking and firewall change, dhcp cascades",
			func(c *config.NetworkConfig) { c.WanInterface = "eth5" },
			"restart networking, reload nftables, restart dnsmasq",
			[]string{PathInterfaces, PathNftables},
		},
		{
			"second LAN port added: everything that names the LAN changes",
			func(c *config.NetworkConfig) { c.LanInterfaces = []string{"eth1", "eth2"} },
			"restart networking, reload nftables, restart dnsmasq",
			[]string{PathInterfaces, PathNftables, PathDnsmasqDHCP},
		},
	}

	for _, c := range cases {
		root := t.TempDir()
		install(t, root, mustRender(t, routerCfg()))

		next := routerCfg()
		c.change(&next)
		plan, err := PlanChanges(RootFS(root), mustRender(t, next))
		if err != nil {
			t.Fatal(err)
		}

		if got := actionSummary(plan); got != c.actions {
			t.Errorf("%s: actions = %q, want %q", c.name, got, c.actions)
		}
		var paths []string
		for _, f := range plan.Files {
			paths = append(paths, f.Path)
		}
		if strings.Join(paths, ",") != strings.Join(c.files, ",") {
			t.Errorf("%s: files = %v, want %v", c.name, paths, c.files)
		}
	}
}

func TestRestartingNetworkingAlsoRestartsDnsmasqWithAReason(t *testing.T) {
	root := t.TempDir()
	install(t, root, mustRender(t, routerCfg()))

	next := routerCfg()
	next.WanInterface = "eth5" // interfaces file changes, the dnsmasq file does not
	plan, err := PlanChanges(RootFS(root), mustRender(t, next))
	if err != nil {
		t.Fatal(err)
	}

	last := plan.Actions[len(plan.Actions)-1]
	if last.Service != SvcDnsmasq || !strings.Contains(last.Reason, "networking is restarting") {
		t.Errorf("last action = %+v, want a dnsmasq restart explained by the networking restart", last)
	}
	for _, f := range plan.Files {
		if f.Path == PathDnsmasqDHCP {
			t.Error("the dnsmasq file did not change and must not be listed as a file change")
		}
	}
}

func TestNoDnsmasqCascadeWhenDhcpIsNotManaged(t *testing.T) {
	root := t.TempDir()
	cfg := routerCfg()
	cfg.DHCP = nil
	install(t, root, mustRender(t, cfg))

	next := cfg
	next.WanInterface = "eth5"
	plan, err := PlanChanges(RootFS(root), mustRender(t, next))
	if err != nil {
		t.Fatal(err)
	}
	if got := actionSummary(plan); got != "restart networking, reload nftables" {
		t.Errorf("actions = %q: dnsmasq is not ours to restart when DHCP is unmanaged", got)
	}
	if len(plan.Notes) == 0 {
		t.Error("the plan should carry the note that DHCP is unmanaged")
	}
}

func TestDisablingDhcpRemovesTheFileAndRestartsDnsmasq(t *testing.T) {
	root := t.TempDir()
	install(t, root, mustRender(t, routerCfg()))

	off := routerCfg()
	off.DHCP = &config.DHCPConfig{Enabled: false}
	plan, err := PlanChanges(RootFS(root), mustRender(t, off))
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Files) != 1 || plan.Files[0].Kind != Remove || plan.Files[0].Path != PathDnsmasqDHCP {
		t.Errorf("files = %+v, want only the dhcp file removed", plan.Files)
	}
	if got := actionSummary(plan); got != "restart dnsmasq" {
		t.Errorf("actions = %q", got)
	}

	// once it is gone, asking again changes nothing
	if err := os.Remove(filepath.Join(root, PathDnsmasqDHCP)); err != nil {
		t.Fatal(err)
	}
	plan, err = PlanChanges(RootFS(root), mustRender(t, off))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Errorf("a removal of a file that is already gone should be a no-op, got %+v", plan)
	}
}

func TestSwitchingToStaticWanWithDnsCreatesTheUpstreamFile(t *testing.T) {
	root := t.TempDir()
	install(t, root, mustRender(t, routerCfg()))

	next := routerCfg()
	next.WanMode = "static"
	next.WanIP, next.WanSubnet, next.WanGateway = "203.0.113.10", "255.255.255.0", "203.0.113.1"
	next.WanDNS = []string{"1.1.1.1"}
	plan, err := PlanChanges(RootFS(root), mustRender(t, next))
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]ChangeKind{}
	for _, f := range plan.Files {
		kinds[f.Path] = f.Kind
	}
	if kinds[PathInterfaces] != Update || kinds[PathDnsmasqDNS] != Create {
		t.Errorf("file changes = %v", kinds)
	}
	if got := actionSummary(plan); got != "restart networking, restart dnsmasq" {
		t.Errorf("actions = %q", got)
	}
}

// Permissions are applied when a file is written; they don't make a plan non-empty
func TestOnlyContentsAreComparedNotPermissions(t *testing.T) {
	root := t.TempDir()
	d := mustRender(t, routerCfg())
	install(t, root, d)
	if err := os.Chmod(filepath.Join(root, PathNftables), 0600); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanChanges(RootFS(root), d)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Empty() {
		t.Errorf("a permission difference alone produced a plan: %+v", plan)
	}
}

func TestPlanReportsUnreadableFilesInsteadOfGuessing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read everything")
	}
	root := t.TempDir()
	d := mustRender(t, routerCfg())
	install(t, root, d)
	if err := os.Chmod(filepath.Join(root, PathInterfaces), 0000); err != nil {
		t.Fatal(err)
	}

	if _, err := PlanChanges(RootFS(root), d); err == nil {
		t.Error("an unreadable file must be reported, not treated as missing")
	}
}

func TestPlanDoesNotMutateTheDesiredNotes(t *testing.T) {
	cfg := routerCfg()
	cfg.DHCP = nil
	d := mustRender(t, cfg)
	before := len(d.Notes)

	plan, err := PlanChanges(RootFS(t.TempDir()), d)
	if err != nil {
		t.Fatal(err)
	}
	plan.Notes = append(plan.Notes, "extra")
	if len(d.Notes) != before {
		t.Error("the plan shares its notes slice with the rendered output")
	}
}

func TestAPlanWithNothingToDoSerialisesEmptyListsNotNull(t *testing.T) {
	root := RootFS(t.TempDir())
	plan, err := PlanChanges(root, Desired{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(plan)
	if !strings.Contains(string(b), `"files":[]`) || !strings.Contains(string(b), `"actions":[]`) {
		t.Fatalf("%s", b)
	}
}

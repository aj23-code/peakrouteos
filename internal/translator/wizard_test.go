package translator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"peakroute/internal/config"
)

// The first-boot wizard (internal/scripts/first_boot.sh) writes the system files on a new box.
// The translator must render exactly the same bytes for the same settings, otherwise the first
// commit on a wizard-configured box would rewrite files and restart services for no reason.
// These tests read the heredocs straight out of the script so the two cannot drift apart.

func wizardScript(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "scripts", "first_boot.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// heredoc returns the body of the first `cat << EOF > <target>` block, as the shell would write it.
func heredoc(t *testing.T, script, target string) string {
	t.Helper()
	header := "cat << EOF > " + target
	i := strings.Index(script, header)
	if i < 0 {
		t.Fatalf("wizard script no longer contains %q", header)
	}
	rest := script[i+len(header):]
	rest = rest[strings.Index(rest, "\n")+1:]
	end := strings.Index(rest, "\nEOF\n")
	if end < 0 {
		t.Fatalf("unterminated heredoc for %s", target)
	}
	return rest[:end+1]
}

type wizardAnswers struct {
	wan, lanIP, netmask, dhcpStart, dhcpEnd string
	lans                                    []string
}

// expand fills in a heredoc the way the shell does, using the wizard's own variable logic.
func (w wizardAnswers) expand(t *testing.T, body string) string {
	t.Helper()

	lanTarget, extra := w.lans[0], ""
	if len(w.lans) > 1 {
		lanTarget = "br0"
		extra = "    bridge_ports " + strings.Join(w.lans, " ")
	}
	vars := map[string]string{
		"WAN_IFACE":           w.wan,
		"LAN_TARGET":          lanTarget,
		"LAN_IP":              w.lanIP,
		"NETMASK":             w.netmask,
		"EXTRA_BRIDGE_STANZA": extra,
		"DHCP_START":          w.dhcpStart,
		"DHCP_END":            w.dhcpEnd,
		"NFT_LAN_TARGET":      `"` + lanTarget + `"`,
	}
	return os.Expand(body, func(name string) string {
		v, ok := vars[name]
		if !ok {
			t.Fatalf("wizard heredoc uses $%s, which this test does not model; update the translator and this test", name)
		}
		return v
	})
}

func (w wizardAnswers) config() config.NetworkConfig {
	return config.NetworkConfig{
		WanInterface:  w.wan,
		WanMode:       "dhcp",
		LanInterfaces: w.lans,
		LanIP:         w.lanIP,
		LanSubnet:     w.netmask,
		DHCP:          &config.DHCPConfig{Enabled: true, RangeStart: w.dhcpStart, RangeEnd: w.dhcpEnd, LeaseTime: "12h"},
	}
}

var wizardScenarios = map[string]wizardAnswers{
	"single LAN port, defaults": {
		wan: "eth0", lans: []string{"eth1"}, lanIP: "192.168.1.1", netmask: "255.255.255.0",
		dhcpStart: "192.168.1.100", dhcpEnd: "192.168.1.200",
	},
	"two LAN ports are bridged": {
		wan: "enp1s0", lans: []string{"enp2s0", "enp3s0"}, lanIP: "10.10.0.1", netmask: "255.255.0.0",
		dhcpStart: "10.10.1.10", dhcpEnd: "10.10.1.250",
	},
	"three LAN ports": {
		wan: "eth0", lans: []string{"eth1", "eth2", "eth3"}, lanIP: "172.16.5.1", netmask: "255.255.255.0",
		dhcpStart: "172.16.5.50", dhcpEnd: "172.16.5.99",
	},
}

func artifactContent(t *testing.T, d Desired, path string) string {
	t.Helper()
	for _, a := range d.Artifacts {
		if a.Path == path && !a.Remove {
			return string(a.Content)
		}
	}
	t.Fatalf("no rendered file for %s", path)
	return ""
}

func TestRenderMatchesTheFirstBootWizardByteForByte(t *testing.T) {
	script := wizardScript(t)
	files := map[string]string{
		PathInterfaces:  heredoc(t, script, "/etc/network/interfaces"),
		PathDnsmasqDHCP: heredoc(t, script, "/etc/dnsmasq.d/peakroute-dhcp.conf"),
		PathNftables:    heredoc(t, script, "/etc/nftables.conf"),
	}

	for name, w := range wizardScenarios {
		d, err := Render(w.config(), "router")
		if err != nil {
			t.Fatalf("%s: Render: %v", name, err)
		}
		for path, body := range files {
			want := w.expand(t, body)
			if got := artifactContent(t, d, path); got != want {
				t.Errorf("%s: %s differs from what the wizard writes\n--- wizard ---\n%s\n--- translator ---\n%s", name, path, want, got)
			}
		}
		if got := artifactContent(t, d, PathSysctl); got != "net.ipv4.ip_forward=1\n" {
			t.Errorf("%s: sysctl = %q", name, got)
		}
	}
}

// The wizard's sysctl line is an echo, not a heredoc: pin it to the script too
func TestSysctlLineMatchesTheWizard(t *testing.T) {
	if !strings.Contains(wizardScript(t), `echo "net.ipv4.ip_forward=1" > /etc/sysctl.d/99-peakroute-forwarding.conf`) {
		t.Error("the wizard no longer writes the forwarding sysctl the way the translator assumes")
	}
}

// What a wizard-configured box needs on its first commit is nothing at all
func TestAWizardConfiguredBoxNeedsNoChangesOnFirstCommit(t *testing.T) {
	script := wizardScript(t)

	for name, w := range wizardScenarios {
		root := t.TempDir()
		write := func(path, content string) {
			full := filepath.Join(root, path)
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}
		}
		write(PathInterfaces, w.expand(t, heredoc(t, script, "/etc/network/interfaces")))
		write(PathDnsmasqDHCP, w.expand(t, heredoc(t, script, "/etc/dnsmasq.d/peakroute-dhcp.conf")))
		write(PathNftables, w.expand(t, heredoc(t, script, "/etc/nftables.conf")))
		write(PathSysctl, "net.ipv4.ip_forward=1\n")

		d, err := Render(w.config(), "ROUTER")
		if err != nil {
			t.Fatal(err)
		}
		plan, err := PlanChanges(RootFS(root), d)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Empty() {
			t.Errorf("%s: a box the wizard just configured would be changed on its first commit: %+v", name, plan)
		}
	}
}

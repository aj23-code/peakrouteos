// Package translator turns the committed network configuration (network.json) into the system
// configuration files that implement it, and works out which services must be restarted when
// those files change. This file renders; plan.go compares with what is on disk.
package translator

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"peakroute/internal/config"
)

// Files the translator owns on the appliance.
const (
	PathSysctl      = "/etc/sysctl.d/99-peakroute-forwarding.conf"
	PathInterfaces  = "/etc/network/interfaces"
	PathNftables    = "/etc/nftables.conf"
	PathDnsmasqDHCP = "/etc/dnsmasq.d/peakroute-dhcp.conf"
	PathDnsmasqDNS  = "/etc/dnsmasq.d/peakroute-upstream.conf"
)

// Services that pick up those files, in the order changes must be applied.
const (
	SvcSysctl     = "sysctl"
	SvcNetworking = "networking"
	SvcNftables   = "nftables"
	SvcDnsmasq    = "dnsmasq"
)

// ErrUnsupportedMode is returned for operation modes the translator cannot render yet.
var ErrUnsupportedMode = errors.New("operation mode not supported by the translator yet")

// Artifact is one file the system should contain (or, with Remove, should not).
type Artifact struct {
	Path    string
	Content []byte
	Mode    os.FileMode
	Remove  bool
	Service string // the service that must pick up a change to this file
}

// Desired is everything the translator wants on disk for a configuration.
type Desired struct {
	Artifacts []Artifact
	Notes     []string // things the user should know about, shown with the plan
}

// LanTarget is the interface the LAN lives on: the single LAN port itself, or the br0 bridge
// when several ports are combined. This mirrors what the first-boot wizard does.
func LanTarget(cfg config.NetworkConfig) string {
	if len(cfg.LanInterfaces) > 1 {
		return "br0"
	}
	return cfg.LanInterfaces[0]
}

var safeToken = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ensureSafe is a last line of defence: every value placed into a config file must be a plain
// token. Validate already guarantees this; checking again here means a future validation bug
// cannot become config injection into root-owned files.
func ensureSafe(tokens ...string) error {
	for _, t := range tokens {
		if !safeToken.MatchString(t) {
			return fmt.Errorf("refusing to render unsafe value %q", t)
		}
	}
	return nil
}

// validateConfig is a variable only so tests can switch it off and prove the render-time
// safety check holds up on its own.
var validateConfig = func(c config.NetworkConfig) error { return c.Validate() }

// Render produces the files for a router-mode configuration.
func Render(cfg config.NetworkConfig, mode string) (Desired, error) {
	if m := strings.ToUpper(strings.TrimSpace(mode)); m != "" && m != "ROUTER" {
		return Desired{}, fmt.Errorf("%w: %q", ErrUnsupportedMode, mode)
	}
	if err := validateConfig(cfg); err != nil {
		return Desired{}, fmt.Errorf("invalid network configuration: %w", err)
	}

	tokens := append([]string{cfg.WanInterface, cfg.LanIP, cfg.LanSubnet}, cfg.LanInterfaces...)
	if strings.EqualFold(cfg.WanMode, "static") {
		tokens = append(tokens, cfg.WanIP, cfg.WanSubnet)
		if cfg.WanGateway != "" {
			tokens = append(tokens, cfg.WanGateway)
		}
		tokens = append(tokens, cfg.WanDNS...)
	}
	if d := cfg.DHCP; d != nil && d.Enabled {
		tokens = append(tokens, d.RangeStart, d.RangeEnd, d.LeaseTime)
	}
	if err := ensureSafe(tokens...); err != nil {
		return Desired{}, err
	}

	lan := LanTarget(cfg)
	var d Desired

	d.Artifacts = append(d.Artifacts,
		Artifact{Path: PathSysctl, Content: []byte("net.ipv4.ip_forward=1\n"), Mode: 0644, Service: SvcSysctl},
		Artifact{Path: PathInterfaces, Content: []byte(renderInterfaces(cfg, lan)), Mode: 0644, Service: SvcNetworking},
		// Debian ships nftables.conf as an executable script (it has a shebang)
		Artifact{Path: PathNftables, Content: []byte(renderNftables(cfg, lan)), Mode: 0755, Service: SvcNftables},
	)

	// DHCP server: a nil section means older installs configured it outside the JSON, so leave
	// the existing dnsmasq file alone rather than guess
	switch dhcp := cfg.DHCP; {
	case dhcp == nil:
		d.Notes = append(d.Notes, "DHCP is not managed by network.json yet, so "+PathDnsmasqDHCP+" is left as it is.")
	case dhcp.Enabled:
		d.Artifacts = append(d.Artifacts, Artifact{
			Path: PathDnsmasqDHCP, Content: []byte(renderDnsmasqDHCP(cfg, lan)), Mode: 0644, Service: SvcDnsmasq,
		})
	default:
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathDnsmasqDHCP, Remove: true, Service: SvcDnsmasq})
	}

	// Upstream DNS servers for LAN clients when the WAN has a fixed address and its own DNS
	if strings.EqualFold(cfg.WanMode, "static") && len(cfg.WanDNS) > 0 {
		d.Artifacts = append(d.Artifacts, Artifact{
			Path: PathDnsmasqDNS, Content: []byte(renderDnsmasqUpstream(cfg)), Mode: 0644, Service: SvcDnsmasq,
		})
	} else {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathDnsmasqDNS, Remove: true, Service: SvcDnsmasq})
	}

	return d, nil
}

func renderInterfaces(cfg config.NetworkConfig, lan string) string {
	var b strings.Builder

	b.WriteString("auto lo\niface lo inet loopback\n\n")

	fmt.Fprintf(&b, "auto %s\n", cfg.WanInterface)
	if strings.EqualFold(cfg.WanMode, "static") {
		fmt.Fprintf(&b, "iface %s inet static\n", cfg.WanInterface)
		fmt.Fprintf(&b, "    address %s\n", cfg.WanIP)
		fmt.Fprintf(&b, "    netmask %s\n", cfg.WanSubnet)
		if cfg.WanGateway != "" {
			fmt.Fprintf(&b, "    gateway %s\n", cfg.WanGateway)
		}
		if len(cfg.WanDNS) > 0 {
			fmt.Fprintf(&b, "    dns-nameservers %s\n", strings.Join(cfg.WanDNS, " "))
		}
	} else {
		fmt.Fprintf(&b, "iface %s inet dhcp\n", cfg.WanInterface)
	}
	b.WriteString("\n")

	fmt.Fprintf(&b, "auto %s\n", lan)
	fmt.Fprintf(&b, "iface %s inet static\n", lan)
	fmt.Fprintf(&b, "    address %s\n", cfg.LanIP)
	fmt.Fprintf(&b, "    netmask %s\n", cfg.LanSubnet)
	if len(cfg.LanInterfaces) > 1 {
		fmt.Fprintf(&b, "    bridge_ports %s\n", strings.Join(cfg.LanInterfaces, " "))
	} else {
		// The wizard leaves an empty line here for a single LAN port; keeping it means a box the
		// wizard configured shows no change on its first commit
		b.WriteString("\n")
	}

	return b.String()
}

func renderDnsmasqDHCP(cfg config.NetworkConfig, lan string) string {
	d := cfg.DHCP
	var b strings.Builder
	fmt.Fprintf(&b, "interface=%s\n", lan)
	fmt.Fprintf(&b, "dhcp-range=%s,%s,%s,%s\n", d.RangeStart, d.RangeEnd, cfg.LanSubnet, d.LeaseTime)
	fmt.Fprintf(&b, "dhcp-option=option:router,%s\n", cfg.LanIP)
	fmt.Fprintf(&b, "dhcp-option=option:dns-server,%s\n", cfg.LanIP)
	return b.String()
}

func renderDnsmasqUpstream(cfg config.NetworkConfig) string {
	var b strings.Builder
	b.WriteString("no-resolv\n")
	for _, s := range cfg.WanDNS {
		fmt.Fprintf(&b, "server=%s\n", s)
	}
	return b.String()
}

func renderNftables(cfg config.NetworkConfig, lan string) string {
	return fmt.Sprintf(`#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;
        iifname "lo" accept
        ct state established,related accept
        ct state invalid drop
        iifname "%[1]s" accept
    }
    chain forward {
        type filter hook forward priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
        iifname "%[1]s" oifname "%[2]s" accept
    }
    chain output {
        type filter hook output priority filter; policy accept;
    }
}

table ip nat {
    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "%[2]s" masquerade
    }
}
`, lan, cfg.WanInterface)
}

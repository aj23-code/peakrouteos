// Package translator turns the committed network configuration (network.json) into the system
// configuration files that implement it, and works out which services must be restarted when
// those files change. This file renders; plan.go compares with what is on disk.
package translator

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"

	"peakroute/internal/config"
	"peakroute/internal/network"
)

// Files the translator owns on the appliance.
const (
	PathSysctl      = "/etc/sysctl.d/99-peakroute-forwarding.conf"
	PathInterfaces  = "/etc/network/interfaces"
	PathNftables    = "/etc/nftables.conf"
	PathDnsmasqDHCP = "/etc/dnsmasq.d/peakroute-dhcp.conf"
	PathDnsmasqDNS  = "/etc/dnsmasq.d/peakroute-upstream.conf"
	PathPPPPeer     = "/etc/ppp/peers/peakroute-wan" // the PPPoE login, when the connection is PPPoE

	// Static routes. ifupdown runs every script in if-up.d whenever an interface comes up, so
	// the routes come back after a reboot or a networking restart without touching /etc/network/interfaces.
	PathRoutes = "/etc/network/if-up.d/peakroute-routes"

	// The machine's name, written while the configuration has a host name (an empty one leaves the file as it is).
	PathHostname = "/etc/hostname"

	// The time zone (Debian's own file for it; the action also points /etc/localtime at the zone) and the time servers.
	PathTimezone  = "/etc/timezone"
	PathTimesyncd = "/etc/systemd/timesyncd.conf.d/peakroute.conf"
	ntpOffMarker  = "NTP is switched off"
)

// RouteProto is the routing protocol number the translator tags its routes with, so it can
// remove exactly its own routes and never anything the kernel or a DHCP client added.
const RouteProto = "77"

// Services that pick up those files, in the order changes must be applied.
const (
	SvcHostname   = "hostname"
	SvcTimezone   = "timezone"
	SvcNTP        = "ntp"
	SvcRegdom     = "regdomain"
	SvcSysctl     = "sysctl"
	SvcNetworking = "networking"
	SvcRoutes     = "routes"
	SvcWifi       = "wifi"
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
	// ManagedDirs are directories whose *.conf files all belong to the translator: a file there that no
	// artifact accounts for is removed (an adapter taken out of the Wi-Fi setup, say).
	ManagedDirs []ManagedDir
	Notes       []string // things the user should know about, shown with the plan
}

// ManagedDir is a directory the translator owns the *.conf files of (or, with Prefix, the ones that start with it; with Any, every
// file).
type ManagedDir struct {
	Path    string
	Service string // the service that must pick up a removal
	Prefix  string
	Any     bool
}

// LanTarget is the interface the default network lives on: the single LAN port that carries it, or
// the br0 bridge when several ports are combined.
func LanTarget(cfg config.NetworkConfig) string {
	// a Wi-Fi network on the default network needs a bridge to join, whatever the number of ports
	if ports := defaultPorts(cfg); len(ports) == 1 && !wifiOnDefaultNetwork(cfg) {
		return ports[0]
	}
	return "br0"
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
	// the configuration says what the device is; a mode that is asked for must agree with it ("" asks for whatever it is)
	if m := strings.ToUpper(strings.TrimSpace(mode)); m != "" && m != cfg.OperationMode() {
		return Desired{}, fmt.Errorf("%w: %q", ErrUnsupportedMode, mode)
	}
	if err := validateConfig(cfg); err != nil {
		return Desired{}, fmt.Errorf("invalid network configuration: %w", err)
	}
	// the configuration says what the device is: a router, or an access point
	if cfg.IsAP() {
		return renderAccessPoint(cfg)
	}

	tokens := append([]string{cfg.WanInterface, cfg.LanIP, cfg.LanSubnet}, cfg.LanInterfaces...)
	if err := ensureSafeWan(cfg); err != nil {
		return Desired{}, err
	}
	if err := ensureSafeReservations(cfg.Reservations); err != nil {
		return Desired{}, err
	}
	if err := ensureSafeFirewall(cfg); err != nil {
		return Desired{}, err
	}
	if err := ensureSafeDNS(cfg); err != nil {
		return Desired{}, err
	}
	if err := ensureSafeSettings(cfg); err != nil {
		return Desired{}, err
	}
	if err := ensureSafeVPN(cfg); err != nil {
		return Desired{}, err
	}
	if err := ensureSafeOVPN(cfg); err != nil {
		return Desired{}, err
	}
	tokens = append(tokens, cfg.WanLink())
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
	for _, n := range cfg.Networks {
		tokens = append(tokens, n.ID, n.IP, n.Netmask)
		if d := n.DHCP; d != nil && d.Enabled {
			tokens = append(tokens, d.RangeStart, d.RangeEnd, d.LeaseTime)
		}
	}
	for port, pn := range cfg.PortNetworks {
		tokens = append(tokens, port, pn.NativeID())
		tokens = append(tokens, pn.Tagged...)
	}
	if err := ensureSafe(tokens...); err != nil {
		return Desired{}, err
	}

	lan := LanTarget(cfg)
	var d Desired

	// Static routes: a script that is only present while there is something to apply
	routesArtifact := Artifact{Path: PathRoutes, Remove: true, Service: SvcRoutes}
	if script, any := renderRoutes(cfg.Routes); any {
		if err := ensureSafeRoutes(cfg.Routes); err != nil {
			return Desired{}, err
		}
		routesArtifact = Artifact{Path: PathRoutes, Content: []byte(script), Mode: 0755, Service: SvcRoutes}
	}

	// The PPPoE login: only present while the connection is PPPoE (it holds the password)
	pppArtifact := Artifact{Path: PathPPPPeer, Remove: true, Service: SvcNetworking}
	if cfg.WanKind() == config.WanPPPoE {
		pppArtifact = Artifact{Path: PathPPPPeer, Content: []byte(renderPPPPeer(cfg)), Mode: 0600, Service: SvcNetworking}
	}

	// the kernel's regulatory country
	regdom, err := regdomArtifact(cfg)
	if err != nil {
		return Desired{}, err
	}

	d.Artifacts = append(d.Artifacts,
		Artifact{Path: PathSysctl, Content: []byte("net.ipv4.ip_forward=1\n"), Mode: 0644, Service: SvcSysctl},
		Artifact{Path: PathInterfaces, Content: []byte(renderInterfaces(cfg, lan)), Mode: 0644, Service: SvcNetworking},
		pppArtifact,
		routesArtifact,
		regdom,
		// Debian ships nftables.conf as an executable script (it has a shebang)
		Artifact{Path: PathNftables, Content: []byte(renderNftables(cfg, lan)), Mode: 0755, Service: SvcNftables},
	)

	// DHCP server: a nil section means older installs configured it outside the JSON, so leave
	// the existing dnsmasq file alone rather than guess
	switch dhcp := cfg.DHCP; {
	case dhcp == nil:
		d.Notes = append(d.Notes, "DHCP is not managed by network.json yet, so "+PathDnsmasqDHCP+" is left as it is.")
	case dhcp.Enabled || anyNetworkDHCP(cfg):
		d.Artifacts = append(d.Artifacts, Artifact{
			Path: PathDnsmasqDHCP, Content: []byte(renderDnsmasqDHCP(cfg, lan)), Mode: 0644, Service: SvcDnsmasq,
		})
	default:
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathDnsmasqDHCP, Remove: true, Service: SvcDnsmasq})
	}

	// The router's own DNS answers: the local domain, local names and forwarded domains. Only present while there is something to say.
	if content, ok := renderDnsmasqLocal(cfg); ok {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathDnsmasqLocal, Content: []byte(content), Mode: 0644, Service: SvcDnsmasq})
	} else {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathDnsmasqLocal, Remove: true, Service: SvcDnsmasq})
	}

	d.Notes = append(d.Notes, networkNotes(cfg)...)

	// Wi-Fi access points
	wifi, err := wifiArtifacts(cfg)
	if err != nil {
		return Desired{}, err
	}
	d.Artifacts = append(d.Artifacts, wifi...)
	// always managed, so that taking the Wi-Fi section out of the configuration clears the files too
	d.ManagedDirs = append(d.ManagedDirs, ManagedDir{Path: PathHostapdDir, Service: SvcWifi})
	// Wi-Fi client adapters (the internet connection, or a link into a local network)
	stations, err := stationArtifacts(cfg)
	if err != nil {
		return Desired{}, err
	}
	d.Artifacts = append(d.Artifacts, stations...)
	d.ManagedDirs = append(d.ManagedDirs, ManagedDir{Path: PathWpaDir, Service: SvcNetworking})

	// Upstream DNS servers for LAN clients: the ones set with a static address, or the ones that replace
	// the provider's with DHCP or PPPoE
	if len(cfg.WanDNS) > 0 {
		d.Artifacts = append(d.Artifacts, Artifact{
			Path: PathDnsmasqDNS, Content: []byte(renderDnsmasqUpstream(cfg)), Mode: 0644, Service: SvcDnsmasq,
		})
	} else {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathDnsmasqDNS, Remove: true, Service: SvcDnsmasq})
	}

	// The router's host name
	if cfg.Hostname != "" {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathHostname, Content: []byte(cfg.Hostname + "\n"), Mode: 0644, Service: SvcHostname})
	}

	// The VPN: the tunnels' files (the directory belongs to the translator, so a tunnel that is taken out loses its file), the script
	// that applies them, and the DNS server listening on the remote access interface
	if content, ok := renderDnsmasqVPN(cfg); ok {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathDnsmasqVPN, Content: []byte(content), Mode: 0644, Service: SvcDnsmasq})
	} else {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathDnsmasqVPN, Remove: true, Service: SvcDnsmasq})
	}
	d.Artifacts = append(d.Artifacts, vpnArtifacts(cfg)...)
	d.ManagedDirs = append(d.ManagedDirs, ManagedDir{Path: PathWireGuardDir, Service: SvcWireGuard})
	ov, err := ovArtifacts(cfg)
	if err != nil {
		return Desired{}, err
	}
	d.Artifacts = append(d.Artifacts, ov...)
	d.ManagedDirs = append(d.ManagedDirs,
		ManagedDir{Path: PathOVServerDir, Prefix: ovPrefix, Service: SvcOpenVPN},
		ManagedDir{Path: PathOVClientDir, Prefix: ovPrefix, Service: SvcOpenVPN},
		ManagedDir{Path: PathOVCCDDir, Any: true, Service: SvcOpenVPN}) // the device files have no suffix

	// The time zone, and the time servers (a file only while they are not the defaults)
	if cfg.Timezone != "" {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathTimezone, Content: []byte(cfg.Timezone + "\n"), Mode: 0644, Service: SvcTimezone})
	}
	if content, ok := renderTimesyncd(cfg); ok {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathTimesyncd, Content: []byte(content), Mode: 0644, Service: SvcNTP})
	} else {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathTimesyncd, Remove: true, Service: SvcNTP})
	}

	return d, nil
}

var (
	routeDestPattern = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}/[0-9]{1,2}$`)
	routeAddrPattern = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}$`)
)

// ensureSafeRoutes is the render-time twin of ensureSafe for routes, whose destinations contain
// a slash.
// ensureSafeReservations: the lines of dnsmasq's file are a hardware address, an IPv4 address and a host name, nothing else.
func ensureSafeReservations(rs []config.Reservation) error {
	for _, r := range rs {
		if m, ok := config.NormalizeMAC(r.MAC); !ok || m != r.MAC {
			return fmt.Errorf("refusing to render an unsafe reserved hardware address %q", r.MAC)
		}
		if a, err := netip.ParseAddr(r.IP); err != nil || !a.Is4() || a.String() != r.IP {
			return fmt.Errorf("refusing to render an unsafe reserved address %q", r.IP)
		}
		if r.Name != "" {
			if err := ensureSafe(r.Name); err != nil || strings.Contains(r.Name, ".") || strings.Contains(r.Name, "_") {
				return fmt.Errorf("refusing to render an unsafe reserved name %q", r.Name)
			}
		}
	}
	return nil
}

func ensureSafeRoutes(routes []config.Route) error {
	for _, r := range routes {
		if r.Disabled {
			continue
		}
		if !routeDestPattern.MatchString(r.Destination) || !routeAddrPattern.MatchString(r.Gateway) || r.Metric < 0 || r.Metric > 9999 {
			return fmt.Errorf("refusing to render unsafe route %q via %q", r.Destination, r.Gateway)
		}
	}
	return nil
}

// renderRoutes writes the script that applies the enabled routes. It is run by ifupdown whenever
// an interface comes up (it then never fails, so it cannot hold an interface back) and by the
// commit step with "strict", where a route the kernel refuses must fail the commit.
func renderRoutes(routes []config.Route) (string, bool) {
	var lines []string
	for _, r := range routes {
		if r.Disabled {
			continue
		}
		line := fmt.Sprintf("ip route replace %s via %s", r.Destination, r.Gateway)
		if r.Metric > 0 {
			line += fmt.Sprintf(" metric %d", r.Metric)
		}
		lines = append(lines, line+" proto "+RouteProto+" || rc=1")
	}
	if len(lines) == 0 {
		return "", false
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# Static routes managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.\n")
	b.WriteString("# ifupdown runs it whenever an interface comes up; the commit step runs it with \"strict\".\n")
	b.WriteString("PATH=/usr/sbin:/sbin:/usr/bin:/bin\n")
	b.WriteString("rc=0\n")
	fmt.Fprintf(&b, "ip route flush proto %s 2>/dev/null\n", RouteProto)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString("[ \"$1\" = strict ] && exit \"$rc\"\n")
	b.WriteString("exit 0\n")
	return b.String(), true
}

func renderInterfaces(cfg config.NetworkConfig, lan string) string {
	if cfg.Bridged() {
		return renderInterfacesNetworks(cfg, lan)
	}
	var b strings.Builder

	b.WriteString("auto lo\niface lo inet loopback\n\n")

	writeWanStanza(&b, cfg)
	writeStationStanzas(&b, cfg)
	fmt.Fprintf(&b, "auto %s\n", lan)
	fmt.Fprintf(&b, "iface %s inet static\n", lan)
	fmt.Fprintf(&b, "    address %s\n", cfg.LanIP)
	fmt.Fprintf(&b, "    netmask %s\n", cfg.LanSubnet)
	if lan == "br0" {
		fmt.Fprintf(&b, "    bridge_ports %s\n", strings.Join(defaultPorts(cfg), " "))
	} else {
		// The wizard leaves an empty line here for a single LAN port; keeping it means a box the
		// wizard configured shows no change on its first commit
		b.WriteString("\n")
	}

	return b.String()
}

func renderDnsmasqDHCP(cfg config.NetworkConfig, lan string) string {
	if cfg.Bridged() {
		return renderDnsmasqDHCPNetworks(cfg, lan)
	}
	d := cfg.DHCP
	var b strings.Builder
	fmt.Fprintf(&b, "interface=%s\n", lan)
	fmt.Fprintf(&b, "dhcp-range=%s,%s,%s,%s\n", d.RangeStart, d.RangeEnd, cfg.LanSubnet, d.LeaseTime)
	fmt.Fprintf(&b, "dhcp-option=option:router,%s\n", cfg.LanIP)
	fmt.Fprintf(&b, "dhcp-option=option:dns-server,%s\n", dnsServerOption(cfg.LanDNSServers, cfg.LanIP))
	writeReservations(&b, cfg)
	return b.String()
}

// writeReservations adds a line for each reserved address: the device with that hardware address always gets that address (and its name
// in the router's DNS). The values were checked when the configuration was validated; they are plain tokens, and rechecked here.
func writeReservations(b *strings.Builder, cfg config.NetworkConfig) {
	for _, r := range cfg.Reservations {
		line := "dhcp-host=" + r.MAC + "," + r.IP
		if r.Name != "" {
			line += "," + r.Name
		}
		b.WriteString(line + "\n")
	}
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
	inputExtra, forwardExtra := nftablesNetworkRules(cfg, lan)
	fwParts := renderFirewall(cfg)
	return fmt.Sprintf(`#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;
        iifname "lo" accept
        ct state established,related accept
        ct state invalid drop
%[6]s        iifname "%[1]s" accept
%[3]s    }
    chain forward {
        type filter hook forward priority filter; policy drop;
        ct state established,related accept
        ct state invalid drop
%[5]s%[7]s        iifname "%[1]s" oifname "%[2]s" accept
%[4]s    }
    chain output {
        type filter hook output priority filter; policy accept;
    }
}

table ip nat {
%[8]s    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        oifname "%[2]s" masquerade
    }
}
%[9]s`, lan, cfg.WanDevice(), inputExtra, forwardExtra, mssClamp(cfg), fwParts.input, fwParts.forward, fwParts.preroute, renderAccounting(cfg))
}

// UsageTable is the nftables table the per-device usage is counted in (the daemon reads its sets, network/usage.go).
const UsageTable = network.UsageTable

// AccountingRuleset is the usage table on its own, ready for `nft -f`: whatever there is of it is replaced. The daemon loads it when the
// table is missing (a firewall file written by an older version, or a ruleset flushed by something else), so counting does not wait for a
// commit to rewrite /etc/nftables.conf.
func AccountingRuleset(cfg config.NetworkConfig) string {
	return "add table inet " + UsageTable + "\ndelete table inet " + UsageTable + "\n" + renderAccounting(cfg)
}

// renderAccounting counts, in the kernel, how much each device uses the internet connection: one set of addresses for each direction
// and family, each element with its own counter, filled by the traffic that is routed between a local network and the internet
// connection. It runs after the firewall (priority 10: a packet the rules drop is not counted), before the masquerade (a device still
// has its own address), and costs nothing per device until it sends something. The elements expire after ten idle minutes, which
// bounds the sets; the daemon adds up the counters (and copes with them starting again from zero, which every reload of this file
// does, as the whole ruleset is flushed first).
func renderAccounting(cfg config.NetworkConfig) string {
	wan := cfg.WanDevice()
	set := func(name, typ string) string {
		return fmt.Sprintf("    set %s { type %s; flags dynamic,timeout; timeout 10m; size 8192; counter; }\n", name, typ)
	}
	return fmt.Sprintf(`
table inet %[1]s {
%[2]s%[3]s%[4]s%[5]s    chain forward {
        type filter hook forward priority 10; policy accept;
        iifname != "%[6]s" oifname "%[6]s" add @up4 { ip saddr }
        iifname "%[6]s" oifname != "%[6]s" add @down4 { ip daddr }
        iifname != "%[6]s" oifname "%[6]s" add @up6 { ip6 saddr }
        iifname "%[6]s" oifname != "%[6]s" add @down6 { ip6 daddr }
    }
}
`, UsageTable, set("up4", "ipv4_addr"), set("down4", "ipv4_addr"), set("up6", "ipv6_addr"), set("down6", "ipv6_addr"), wan)
}

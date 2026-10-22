package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// Access point mode: a bridge, not a router (docs/access-point.md). br0 holds the cable ports and the Wi-Fi networks (and links); the
// device's own address is on it, from the main router's DHCP server or fixed, and the setup address is kept beside it so that the device
// can always be reached by whoever set it up. Nothing is routed, translated or served: no NAT, DHCP, DNS server, extra networks, VPN or
// routes, and the router's files are removed so that going back to a router writes them again. The firewall only protects the device
// itself: its web interface, ping and the replies to what it asked for.

// apBridge is the interface the access point's network is on.
const apBridge = "br0"

// apVLANBridge is the bridge of a tagged network on an access point: it holds the cable ports' VLAN interfaces and the Wi-Fi networks
// on that VLAN, and no address (the access point is only a wire for them).
func apVLANBridge(vlan int) string { return fmt.Sprintf("br%d", vlan) }

// ensureSafeAP is the render-time twin of the AP validation.
func ensureSafeAP(cfg config.NetworkConfig) error {
	ap := cfg.APSettings()
	if err := ensureSafe(cfg.APBridgeMembers()...); err != nil {
		return err
	}
	if ap.Mode == config.APStatic {
		tokens := []string{ap.IP, ap.Netmask}
		if ap.Gateway != "" {
			tokens = append(tokens, ap.Gateway)
		}
		tokens = append(tokens, ap.DNS...)
		if err := ensureSafe(tokens...); err != nil {
			return err
		}
	}
	for _, v := range cfg.APVLANs() {
		if v < config.MinAPVLAN || v > config.MaxVLAN {
			return fmt.Errorf("refusing to render an unsafe VLAN %d", v)
		}
	}
	if cfg.Hostname != "" {
		return ensureSafe(cfg.Hostname)
	}
	return nil
}

func renderInterfacesAP(cfg config.NetworkConfig) string {
	ap := cfg.APSettings()
	var b strings.Builder
	b.WriteString("auto lo\niface lo inet loopback\n\n")
	writeStationStanzas(&b, cfg)

	members := cfg.APBridgeMembers()
	if ap.Mode == config.APStatic {
		fmt.Fprintf(&b, "auto %s\niface %s inet static\n", apBridge, apBridge)
		fmt.Fprintf(&b, "    address %s\n    netmask %s\n", ap.IP, ap.Netmask)
		if ap.Gateway != "" {
			fmt.Fprintf(&b, "    gateway %s\n", ap.Gateway)
		}
		if len(ap.DNS) > 0 {
			fmt.Fprintf(&b, "    dns-nameservers %s\n", strings.Join(ap.DNS, " "))
		}
	} else {
		// The address comes from the main router, and its client runs in the background (dhcpClientHooks): with the link or the main router
		// not answering yet the bridge still comes up, with the recovery address below, instead of the whole change failing.
		fmt.Fprintf(&b, "auto %s\niface %s inet manual\n", apBridge, apBridge)
	}
	fmt.Fprintf(&b, "    bridge_ports %s\n", portList(members))
	if ap.Mode != config.APStatic {
		// the main router lists the device under this name, which is how it is found
		for _, l := range dhcpClientHooks(cfg.Hostname) {
			fmt.Fprintf(&b, "    %s\n", l)
		}
	}
	fmt.Fprintf(&b, "    post-up ip addr add %s dev %s label %s:setup || true\n", config.APRecoveryAddress, apBridge, apBridge)

	// A tagged network for each VLAN a Wi-Fi network uses: the cable ports carry it tagged (a trunk, VLAN interface eth0.20), and its own
	// bridge, without an address, joins them with the Wi-Fi networks on it.
	ports := ap.Ports
	for _, v := range cfg.APVLANs() {
		var members []string
		for _, p := range ports {
			name := fmt.Sprintf("%s.%d", p, v)
			fmt.Fprintf(&b, "\nauto %s\niface %s inet manual\n    vlan-raw-device %s\n", name, name, p)
			members = append(members, name)
		}
		fmt.Fprintf(&b, "\nauto %s\niface %s inet manual\n    bridge_ports %s\n", apVLANBridge(v), apVLANBridge(v), portList(members))
	}
	return b.String()
}

func renderNftablesAP() string {
	return `#!/usr/sbin/nft -f
flush ruleset

table inet filter {
    chain input {
        type filter hook input priority filter; policy drop;
        iifname "lo" accept
        ct state established,related accept
        ct state invalid drop
        icmp type echo-request accept
        icmpv6 type { echo-request, nd-neighbor-solicit, nd-neighbor-advert, nd-router-advert } accept
        udp sport 67 udp dport 68 accept
        tcp dport 443 accept
    }
    chain forward {
        type filter hook forward priority filter; policy drop;
    }
    chain output {
        type filter hook output priority filter; policy accept;
    }
}
`
}

// renderAccessPoint produces the files for an access point.
func renderAccessPoint(cfg config.NetworkConfig) (Desired, error) {
	if err := ensureSafeAP(cfg); err != nil {
		return Desired{}, err
	}
	if err := ensureSafeSettings(cfg); err != nil {
		return Desired{}, err
	}
	regdom, err := regdomArtifact(cfg)
	if err != nil {
		return Desired{}, err
	}
	var d Desired
	d.Artifacts = append(d.Artifacts,
		Artifact{Path: PathSysctl, Content: []byte("net.ipv4.ip_forward=0\n"), Mode: 0644, Service: SvcSysctl},
		Artifact{Path: PathInterfaces, Content: []byte(renderInterfacesAP(cfg)), Mode: 0644, Service: SvcNetworking},
		regdom,
		Artifact{Path: PathNftables, Content: []byte(renderNftablesAP()), Mode: 0755, Service: SvcNftables},
	)
	// what only a router has is taken away
	for _, p := range []string{PathPPPPeer} {
		d.Artifacts = append(d.Artifacts, Artifact{Path: p, Remove: true, Service: SvcNetworking})
	}
	d.Artifacts = append(d.Artifacts, Artifact{Path: PathRoutes, Remove: true, Service: SvcRoutes})
	for _, p := range []string{PathDnsmasqDHCP, PathDnsmasqDNS, PathDnsmasqLocal, PathDnsmasqVPN} {
		d.Artifacts = append(d.Artifacts, Artifact{Path: p, Remove: true, Service: SvcDnsmasq})
	}
	// no tunnels: the same renderers with nothing configured remove the files they wrote
	bare := cfg
	bare.VPN = nil
	d.Artifacts = append(d.Artifacts, vpnArtifacts(bare)...)
	ov, err := ovArtifacts(bare)
	if err != nil {
		return Desired{}, err
	}
	d.Artifacts = append(d.Artifacts, ov...)
	d.ManagedDirs = append(d.ManagedDirs,
		ManagedDir{Path: PathWireGuardDir, Service: SvcWireGuard},
		ManagedDir{Path: PathOVServerDir, Prefix: ovPrefix, Service: SvcOpenVPN},
		ManagedDir{Path: PathOVClientDir, Prefix: ovPrefix, Service: SvcOpenVPN},
		ManagedDir{Path: PathOVCCDDir, Any: true, Service: SvcOpenVPN})

	// Wi-Fi: the networks all join the one bridge
	wifi, err := wifiArtifacts(cfg)
	if err != nil {
		return Desired{}, err
	}
	d.Artifacts = append(d.Artifacts, wifi...)
	d.ManagedDirs = append(d.ManagedDirs, ManagedDir{Path: PathHostapdDir, Service: SvcWifi})
	stations, err := stationArtifacts(cfg)
	if err != nil {
		return Desired{}, err
	}
	d.Artifacts = append(d.Artifacts, stations...)
	d.ManagedDirs = append(d.ManagedDirs, ManagedDir{Path: PathWpaDir, Service: SvcNetworking})

	if cfg.Hostname != "" {
		d.Artifacts = append(d.Artifacts, Artifact{Path: PathHostname, Content: []byte(cfg.Hostname + "\n"), Mode: 0644, Service: SvcHostname})
	}
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

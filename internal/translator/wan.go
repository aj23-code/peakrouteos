package translator

import (
	"fmt"
	"strings"

	"peakroute/internal/config"
)

// How the internet connection is built (standard Debian packages: ifupdown, vlan, udhcpc, ppp):
//
//   - Plain DHCP or static: the port's own stanza, exactly as it has always been. A cloned MAC address
//     (hwaddress), an MTU other than the usual one and the host name sent to the provider (udhcpc's
//     "hostname") are added to it only when set.
//   - With a VLAN: a VLAN interface (eth0.835) holds the connection; the vlan package brings the port up
//     under it. The cloned MAC address goes on the VLAN interface, which is what the provider sees.
//   - PPPoE: a link stanza that only brings the port (or its VLAN interface) up, and a "ppp" stanza that
//     starts pppd with the provider file /etc/ppp/peers/peakroute-wan. The session's interface is ppp0.
//
// The firewall, the masquerade rule and the health checks all use WanDevice: ppp0 for PPPoE, else the link.

// pppProvider is the name of the PPP provider file, which ifupdown's ppp method hands to pppd.
const pppProvider = "peakroute-wan"

// ensureSafeWan is the render-time twin of the WAN validation: everything written to the interfaces
// file and the PPP options file must be plain.
func ensureSafeWan(cfg config.NetworkConfig) error {
	if cfg.WanVlan < 0 || cfg.WanVlan > 4094 {
		return fmt.Errorf("refusing to render an unsafe WAN VLAN %d", cfg.WanVlan)
	}
	if cfg.WanMTU < 0 || cfg.WanMTU > config.MaxWanMTU {
		return fmt.Errorf("refusing to render an unsafe WAN MTU %d", cfg.WanMTU)
	}
	if cfg.WanMAC != "" {
		if m, ok := config.NormalizeMAC(cfg.WanMAC); !ok || m != cfg.WanMAC {
			return fmt.Errorf("refusing to render an unsafe WAN MAC address %q", cfg.WanMAC)
		}
	}
	if cfg.Hostname != "" {
		if err := ensureSafe(cfg.Hostname); err != nil {
			return err
		}
	}
	if p := cfg.WanPPPoE; p != nil && cfg.WanKind() == config.WanPPPoE {
		for _, v := range []string{p.Username, p.Password, p.Service} {
			if !config.SafeCredential(v) {
				return fmt.Errorf("refusing to render an unsafe PPPoE login")
			}
		}
	}
	return nil
}

// writeWanStanza writes the internet connection's part of /etc/network/interfaces.
func writeWanStanza(b *strings.Builder, cfg config.NetworkConfig) {
	link := cfg.WanLink()
	vlan := cfg.WanVlan > 0
	pppoe := cfg.WanKind() == config.WanPPPoE

	// the options that go on the interface that holds the address (or, with PPPoE, on the link)
	var opts []string
	if cfg.WanMAC != "" {
		opts = append(opts, "hwaddress ether "+cfg.WanMAC)
	}
	static := strings.EqualFold(cfg.WanMode, "static")
	if mtu := cfg.WanMTU; !pppoe && mtu > 0 && mtu != cfg.UsualWanMTU() {
		// a VLAN interface cannot be larger than the port under it
		if vlan && mtu > config.DefaultWanMTU {
			opts = append(opts, fmt.Sprintf("pre-up ip link set dev %s mtu %d", cfg.WanInterface, mtu))
		}
		if static {
			opts = append(opts, fmt.Sprintf("mtu %d", mtu))
		} else {
			// ifupdown's DHCP method has no mtu option: it would be ignored
			opts = append(opts, fmt.Sprintf("post-up ip link set dev $IFACE mtu %d", mtu))
		}
	}

	stanza := func(method string, extra ...string) {
		fmt.Fprintf(b, "auto %s\niface %s inet %s\n", link, link, method)
		if vlan {
			fmt.Fprintf(b, "    vlan-raw-device %s\n", cfg.WanInterface)
		}
		for _, l := range extra {
			fmt.Fprintf(b, "    %s\n", l)
		}
		for _, l := range opts {
			fmt.Fprintf(b, "    %s\n", l)
		}
	}

	switch {
	case pppoe:
		// the link only has to be up for the session; pppd does the rest
		stanza("manual") // ifupdown brings a manual interface up by itself
		b.WriteString("\n")
		fmt.Fprintf(b, "auto %s\niface %s inet ppp\n    provider %s\n", pppProvider, pppProvider, pppProvider)
	case static:
		var extra []string
		extra = append(extra, "address "+cfg.WanIP, "netmask "+cfg.WanSubnet)
		if cfg.WanGateway != "" {
			extra = append(extra, "gateway "+cfg.WanGateway)
		}
		if len(cfg.WanDNS) > 0 {
			extra = append(extra, "dns-nameservers "+strings.Join(cfg.WanDNS, " "))
		}
		stanza("static", extra...)
	default:
		var extra []string
		if cfg.Hostname != "" && cfg.SendHostname {
			extra = append(extra, "hostname "+cfg.Hostname)
		}
		stanza("dhcp", extra...)
	}
	b.WriteString("\n")
}

// renderPPPPeer writes the PPPoE login for pppd. The file holds the password, so it is written 0600.
func renderPPPPeer(cfg config.NetworkConfig) string {
	p := cfg.WanPPPoE
	mtu := cfg.EffectiveWanMTU()
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("# Managed by PeakRoute OS. Do not edit: this file is rewritten on every commit.")
	line("plugin rp-pppoe.so")
	line("nic-%s", cfg.WanLink())
	if p.Service != "" {
		line("rp_pppoe_service \"%s\"", p.Service)
	}
	line("user \"%s\"", p.Username)
	line("password \"%s\"", p.Password)
	line("hide-password")
	line("noauth")
	line("unit 0")
	line("mtu %d", mtu)
	line("mru %d", mtu)
	line("noipdefault")
	line("defaultroute")
	line("usepeerdns")
	// keep the session up: dial again whenever it drops, waiting a little between tries
	line("persist")
	line("maxfail 0")
	line("holdoff 10")
	line("lcp-echo-interval 20")
	line("lcp-echo-failure 3")
	line("noaccomp")
	line("nodeflate")
	line("nobsdcomp")
	line("novj")
	line("novjccomp")
	line("nopcomp")
	return b.String()
}

// mssClamp returns the firewall lines that keep TCP connections working when the internet connection's
// MTU is smaller than the LAN's (PPPoE, or a lower MTU set by hand): without them, a page can stall because
// a full-size packet does not fit and the "too big" message does not always make it back. Both directions
// are clamped: our devices' SYNs going out and the servers' SYN-ACKs coming in.
func mssClamp(cfg config.NetworkConfig) string {
	mtu := cfg.EffectiveWanMTU()
	if mtu >= config.DefaultWanMTU {
		return ""
	}
	mss := mtu - 40
	dev := cfg.WanDevice()
	return fmt.Sprintf("        oifname \"%[1]s\" tcp flags syn tcp option maxseg size > %[2]d tcp option maxseg size set %[2]d\n"+
		"        iifname \"%[1]s\" tcp flags syn tcp option maxseg size > %[2]d tcp option maxseg size set %[2]d\n", dev, mss)
}

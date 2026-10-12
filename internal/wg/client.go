package wg

import (
	"fmt"
	"strings"
)

// ClientParams is what a remote access client's file says.
type ClientParams struct {
	Name            string
	PrivateKey      string
	Address         string // the client's address in the remote access network: 10.8.0.2/32
	DNS             string // the router's address in that network, or "" when the client keeps its own DNS
	ServerPublicKey string
	PresharedKey    string // "" when there is none
	AllowedIPs      []string
	Endpoint        string // host:port
	Keepalive       int
}

// ClientConfig writes the file a client imports (the WireGuard apps and wg-quick read it).
func ClientConfig(p ClientParams) string {
	var b strings.Builder
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, p.Name)
	fmt.Fprintf(&b, "# PeakRoute OS remote access: %s\n", name)
	b.WriteString("[Interface]\n")
	fmt.Fprintf(&b, "PrivateKey = %s\n", p.PrivateKey)
	fmt.Fprintf(&b, "Address = %s\n", p.Address)
	if p.DNS != "" {
		fmt.Fprintf(&b, "DNS = %s\n", p.DNS)
	}
	b.WriteString("\n[Peer]\n")
	fmt.Fprintf(&b, "PublicKey = %s\n", p.ServerPublicKey)
	if p.PresharedKey != "" {
		fmt.Fprintf(&b, "PresharedKey = %s\n", p.PresharedKey)
	}
	fmt.Fprintf(&b, "AllowedIPs = %s\n", strings.Join(p.AllowedIPs, ", "))
	fmt.Fprintf(&b, "Endpoint = %s\n", p.Endpoint)
	if p.Keepalive > 0 {
		fmt.Fprintf(&b, "PersistentKeepalive = %d\n", p.Keepalive)
	}
	return b.String()
}

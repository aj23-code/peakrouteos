// Package ovpn writes and reads the .ovpn files: the file a device imports to connect to the remote access server, and the file one
// router gives another to make a site tunnel. The router never copies anything from a file it reads except the certificates, the key,
// the control channel key and the address to connect to; its own configuration for the tunnel is written from those (docs/vpn.md).
package ovpn

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"peakroute/internal/config"
	"peakroute/internal/pki"
)

// Params is what goes into a file.
type Params struct {
	Name     string // shown in a comment
	Host     string
	Port     int
	Proto    string // udp or tcp
	CA       string
	Cert     string
	Key      string // "" when the device's key is not known here: the file then says where to put it
	TLSCrypt string
	// Tunnel and Networks are hints for another PeakRoute router that imports the file: the address that router will have in the
	// tunnel, and the networks behind this router. Empty for a device.
	Tunnel   string
	Networks []string
	// RouteNoPull makes the client refuse the routes and DNS the server pushes (a site tunnel: the networks are listed by hand)
	RouteNoPull bool
}

const keyPlaceholder = "# Paste the private key of this device here, between the two lines below (or add a key = ... line).\n"

func comment(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// Write is the text of a file.
func Write(p Params) (string, error) {
	ca, err := pki.Normalize(p.CA, "CERTIFICATE")
	if err != nil {
		return "", fmt.Errorf("the authority's certificate: %v", err)
	}
	cert, err := pki.Normalize(p.Cert, "CERTIFICATE")
	if err != nil {
		return "", fmt.Errorf("the certificate: %v", err)
	}
	tc, err := pki.NormalizeTLSCrypt(p.TLSCrypt)
	if err != nil {
		return "", fmt.Errorf("the control channel key: %v", err)
	}
	if p.Proto != "udp" && p.Proto != "tcp" {
		return "", errors.New("the protocol is neither udp nor tcp")
	}
	if !config.ValidServerHost(p.Host) || p.Port < 1 || p.Port > 65535 {
		return "", errors.New("the address to connect to is not one")
	}
	var b strings.Builder
	b.WriteString("# PeakRoute OS OpenVPN file")
	if p.Name != "" {
		b.WriteString(": " + comment(p.Name))
	}
	b.WriteString("\n")
	if p.Tunnel != "" {
		b.WriteString("# peakroute-tunnel " + comment(p.Tunnel) + "\n")
	}
	if len(p.Networks) > 0 {
		b.WriteString("# peakroute-networks " + comment(strings.Join(p.Networks, ",")) + "\n")
	}
	fmt.Fprintf(&b, "client\ndev tun\nproto %s\nremote %s %d\nnobind\npersist-key\npersist-tun\nresolv-retry infinite\n", p.Proto, p.Host, p.Port)
	if p.RouteNoPull {
		b.WriteString("route-nopull\n")
	}
	b.WriteString("remote-cert-tls server\ndata-ciphers AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305\ntls-version-min 1.2\nverb 3\n")
	for _, blk := range []struct{ tag, text string }{{"ca", ca}, {"cert", cert}} {
		fmt.Fprintf(&b, "<%s>\n%s</%s>\n", blk.tag, blk.text, blk.tag)
	}
	if p.Key == "" {
		b.WriteString(keyPlaceholder)
		b.WriteString("<key>\n</key>\n")
	} else {
		key, err := pki.Normalize(p.Key, "EC PRIVATE KEY", "PRIVATE KEY")
		if err != nil {
			return "", fmt.Errorf("the key: %v", err)
		}
		fmt.Fprintf(&b, "<key>\n%s</key>\n", key)
	}
	fmt.Fprintf(&b, "<tls-crypt>\n%s</tls-crypt>\n", tc)
	return b.String(), nil
}

// Parsed is what is taken from a file.
type Parsed struct {
	Host     string
	Port     int
	Proto    string
	CA       string
	Cert     string
	Key      string
	TLSCrypt string
	Tunnel   string   // from the hint line, when there is one
	Networks []string // from the hint line, when there is one
}

// Read takes what a tunnel needs from the text of a file. It is strict about what it takes (one block of each kind, certificates that
// really are certificates) and ignores every other line.
func Read(text string) (Parsed, error) {
	if len(text) > 64<<10 {
		return Parsed{}, errors.New("the file is too big to be an OpenVPN file")
	}
	var p Parsed
	blocks := map[string]string{}
	var open string
	var buf strings.Builder
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if open != "" {
			if line == "</"+open+">" {
				if _, dup := blocks[open]; dup {
					return Parsed{}, fmt.Errorf("the file has two <%s> blocks", open)
				}
				blocks[open] = buf.String()
				open = ""
				buf.Reset()
				continue
			}
			buf.WriteString(raw + "\n")
			continue
		}
		switch {
		case line == "<ca>" || line == "<cert>" || line == "<key>" || line == "<tls-crypt>":
			open = strings.Trim(line, "<>")
		case strings.HasPrefix(line, "# peakroute-tunnel "):
			p.Tunnel = strings.TrimSpace(strings.TrimPrefix(line, "# peakroute-tunnel "))
		case strings.HasPrefix(line, "# peakroute-networks "):
			for _, n := range strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "# peakroute-networks ")), ",") {
				if n = strings.TrimSpace(n); n != "" {
					p.Networks = append(p.Networks, n)
				}
			}
		case strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || line == "":
		default:
			f := strings.Fields(line)
			switch f[0] {
			case "remote":
				if p.Host != "" {
					continue // the first one; this router does not rotate through several
				}
				if len(f) < 2 {
					return Parsed{}, errors.New("a remote line has no address")
				}
				p.Host = f[1]
				p.Port = 1194
				if len(f) >= 3 {
					n, err := strconv.Atoi(f[2])
					if err != nil || n < 1 || n > 65535 {
						return Parsed{}, fmt.Errorf("the port %q is not a port", f[2])
					}
					p.Port = n
				}
				if len(f) >= 4 && p.Proto == "" {
					p.Proto = protoOf(f[3])
				}
			case "proto":
				if len(f) >= 2 {
					p.Proto = protoOf(f[1])
				}
			}
		}
	}
	if open != "" {
		return Parsed{}, fmt.Errorf("the <%s> block is not closed", open)
	}
	if p.Host == "" {
		return Parsed{}, errors.New("the file has no remote line (the address to connect to)")
	}
	if !config.ValidServerHost(p.Host) {
		return Parsed{}, fmt.Errorf("%q is not a name or an address", p.Host)
	}
	if p.Proto == "" {
		p.Proto = "udp"
	}
	if p.Proto != "udp" && p.Proto != "tcp" {
		return Parsed{}, errors.New("the protocol is neither udp nor tcp")
	}
	var err error
	if p.CA, err = pki.Normalize(blocks["ca"], "CERTIFICATE"); err != nil {
		return Parsed{}, fmt.Errorf("the <ca> block: %v", err)
	}
	if p.Cert, err = pki.Normalize(blocks["cert"], "CERTIFICATE"); err != nil {
		return Parsed{}, fmt.Errorf("the <cert> block: %v", err)
	}
	if p.Key, err = pki.Normalize(blocks["key"], "EC PRIVATE KEY", "PRIVATE KEY"); err != nil {
		return Parsed{}, fmt.Errorf("the <key> block: %v (only keys of the kind this router makes, EC, are understood)", err)
	}
	if p.TLSCrypt, err = pki.NormalizeTLSCrypt(blocks["tls-crypt"]); err != nil {
		return Parsed{}, fmt.Errorf("the <tls-crypt> block: %v (a file with tls-auth or without a control channel key is not understood)", err)
	}
	return p, nil
}

func protoOf(s string) string {
	switch strings.ToLower(s) {
	case "udp", "udp4", "udp6":
		return "udp"
	case "tcp", "tcp4", "tcp6", "tcp-client", "tcp4-client", "tcp6-client":
		return "tcp"
	}
	return s
}

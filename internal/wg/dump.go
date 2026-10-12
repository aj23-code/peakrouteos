package wg

import (
	"strconv"
	"strings"
	"time"
)

// Peer is how one peer of a running tunnel is doing.
type Peer struct {
	PublicKey       string
	Endpoint        string    // where it was last heard from ("" when never)
	AllowedIPs      []string  // as the tunnel has them
	LatestHandshake time.Time // zero: never
	Received        uint64    // bytes
	Sent            uint64
	Keepalive       int // seconds, 0: off
}

// Interface is a running tunnel.
type Interface struct {
	Name       string
	PublicKey  string
	ListenPort int
	Peers      []Peer
}

// ParseDump reads the output of `wg show all dump`: tab separated lines, the first line of an interface describing it (name,
// private key, public key, listen port, fwmark) and each peer line after it (name, public key, preshared key, endpoint, allowed
// ips, latest handshake as Unix seconds, bytes received, bytes sent, persistent keepalive). The private and preshared keys are
// never kept.
func ParseDump(out string) []Interface {
	var res []Interface
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch len(f) {
		case 5: // interface: name, private key, public key, listen port, fwmark
			port, _ := strconv.Atoi(f[3])
			res = append(res, Interface{Name: f[0], PublicKey: f[2], ListenPort: port})
		case 9: // peer: interface, public key, preshared key, endpoint, allowed ips, handshake, rx, tx, keepalive
			if len(res) == 0 || res[len(res)-1].Name != f[0] {
				continue
			}
			p := Peer{PublicKey: f[1]}
			if f[3] != "(none)" {
				p.Endpoint = f[3]
			}
			if f[4] != "(none)" && f[4] != "" {
				p.AllowedIPs = strings.Split(f[4], ",")
			}
			if sec, err := strconv.ParseInt(f[5], 10, 64); err == nil && sec > 0 {
				p.LatestHandshake = time.Unix(sec, 0)
			}
			p.Received, _ = strconv.ParseUint(f[6], 10, 64)
			p.Sent, _ = strconv.ParseUint(f[7], 10, 64)
			if f[8] != "off" {
				p.Keepalive, _ = strconv.Atoi(f[8])
			}
			res[len(res)-1].Peers = append(res[len(res)-1].Peers, p)
		}
	}
	return res
}

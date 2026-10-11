package config

import (
	"fmt"
	"net/netip"
	"strings"
)

// The router's DNS: the local domain, local names, domain forwarding, and which servers each network is told to use.
// docs/dns.md has the model and the reasons.

const (
	MaxDNSHosts    = 200
	MaxDNSForwards = 50
	MaxDNSServers  = 3 // per network, and per forwarded domain
	maxDNSName     = 253
	maxDNSLabel    = 63
)

// DNSHost is a name the router answers itself with an address.
type DNSHost struct {
	ID       string `json:"id"` // h1, h2...
	Name     string `json:"name"`
	Address  string `json:"address"`
	Disabled bool   `json:"disabled,omitempty"`
}

// DNSForward sends the questions about a domain (and everything under it) to other servers.
type DNSForward struct {
	ID       string   `json:"id"` // f1, f2...
	Domain   string   `json:"domain"`
	Servers  []string `json:"servers"`
	Disabled bool     `json:"disabled,omitempty"`
}

// DNSConfig is what the DNS page edits besides the per-network choice and the domain.
type DNSConfig struct {
	Hosts    []DNSHost    `json:"hosts,omitempty"`
	Forwards []DNSForward `json:"forwards,omitempty"`
}

func (d DNSConfig) clone() DNSConfig {
	d.Hosts = append([]DNSHost(nil), d.Hosts...)
	if len(d.Forwards) > 0 {
		fw := make([]DNSForward, len(d.Forwards))
		for i, f := range d.Forwards {
			f.Servers = append([]string(nil), f.Servers...)
			fw[i] = f
		}
		d.Forwards = fw
	} else {
		d.Forwards = nil
	}
	return d
}

// DNSSettings returns the local names and forwards, empty when there are none.
func (c NetworkConfig) DNSSettings() DNSConfig {
	if c.DNS == nil {
		return DNSConfig{}
	}
	return *c.DNS
}

// FindHost returns a local name by id.
func (d DNSConfig) FindHost(id string) (DNSHost, bool) {
	for _, h := range d.Hosts {
		if h.ID == id {
			return h, true
		}
	}
	return DNSHost{}, false
}

// FindForward returns a forwarded domain by id.
func (d DNSConfig) FindForward(id string) (DNSForward, bool) {
	for _, f := range d.Forwards {
		if f.ID == id {
			return f, true
		}
	}
	return DNSForward{}, false
}

// NetworkDNSServers returns the servers a network is told to use instead of the router (empty: the router).
func (c NetworkConfig) NetworkDNSServers(id string) []string {
	if id == DefaultNetworkID {
		return c.LanDNSServers
	}
	if n, ok := c.FindNetwork(id); ok {
		return n.DNSServers
	}
	return nil
}

// ValidDNSName reports whether s is a host or domain name: dot separated labels of lower-case letters, digits and
// hyphens (none starting or ending with one), each up to 63 characters, at most 253 in all. Names are kept in lower case.
func ValidDNSName(s string) bool {
	if s == "" || len(s) > maxDNSName {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" || len(label) > maxDNSLabel || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return false
			}
		}
	}
	return true
}

// ValidDomain is ValidDNSName without the names that must not be used as a local domain.
func ValidDomain(s string) bool {
	switch s {
	case "local", "localhost", "invalid", "test", "example":
		return false // reserved for other uses (mDNS, the machine itself, documentation)
	}
	return ValidDNSName(s)
}

// IsIPv4 reports whether s is a plain IPv4 address.
func IsIPv4(s string) bool {
	_, ok := parseIPv4(s)
	return ok
}

// usableServer reports whether an address can be a DNS server: an IPv4 address that is neither unspecified, loopback,
// multicast nor the broadcast address.
func usableServer(s string) bool {
	a, ok := parseIPv4(s)
	return ok && !a.IsUnspecified() && !a.IsLoopback() && !a.IsMulticast() && a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

func validateServers(servers []string, field string, add func(field, format string, args ...any)) {
	if len(servers) > MaxDNSServers {
		add(field, "at most %d servers", MaxDNSServers)
	}
	seen := map[string]bool{}
	for _, s := range servers {
		switch {
		case !usableServer(s):
			add(field, "%q is not a usable IPv4 address for a DNS server", s)
		case seen[s]:
			add(field, "%q is listed more than once", s)
		}
		seen[s] = true
	}
}

// validateDNS checks the domain, the local names, the forwards and the servers each network advertises. Errors are
// reported on domain, dns.hosts[i].field, dns.forwards[i].field, dns.hosts / dns.forwards for the lists as a whole,
// lan_dns_servers and networks[i].dns_servers.
func (c NetworkConfig) validateDNS(add func(field, format string, args ...any)) {
	if c.Domain != "" && !ValidDomain(c.Domain) {
		add("domain", "must be a domain name in lower case, such as lan or home.example.com (not local, localhost, invalid, test or example)")
	}
	validateServers(c.LanDNSServers, "lan_dns_servers", add)
	for i, n := range c.Networks {
		validateServers(n.DNSServers, fmt.Sprintf("networks[%d].dns_servers", i), add)
	}

	if c.DNS == nil {
		return
	}
	d := *c.DNS

	if len(d.Hosts) > MaxDNSHosts {
		add("dns.hosts", "at most %d local names are supported", MaxDNSHosts)
	}
	ids := map[string]bool{}
	names := map[string]string{}
	for i, h := range d.Hosts {
		field := func(name string) string { return fmt.Sprintf("dns.hosts[%d].%s", i, name) }
		switch {
		case !ruleIDPattern.MatchString(h.ID):
			add(field("id"), "must be a short lower-case id such as h1")
		case ids[h.ID]:
			add(field("id"), "another local name already has this id")
		}
		ids[h.ID] = true
		switch {
		case !ValidDNSName(h.Name):
			add(field("name"), "must be a host name or a full name in lower case, such as nas or home.example.com")
		case !h.Disabled && names[h.Name] != "":
			add(field("name"), "is already listed (%s)", names[h.Name])
		}
		if !h.Disabled {
			names[h.Name] = h.ID
		}
		a, ok := parseIPv4(h.Address)
		switch {
		case !ok:
			add(field("address"), "must be an IPv4 address such as 192.168.1.50")
		case a.IsUnspecified() || a.IsLoopback() || a.IsMulticast():
			add(field("address"), "must be the address of a device, not %s", h.Address)
		}
	}

	if len(d.Forwards) > MaxDNSForwards {
		add("dns.forwards", "at most %d forwarded domains are supported", MaxDNSForwards)
	}
	fids := map[string]bool{}
	domains := map[string]string{}
	for i, f := range d.Forwards {
		field := func(name string) string { return fmt.Sprintf("dns.forwards[%d].%s", i, name) }
		switch {
		case !ruleIDPattern.MatchString(f.ID):
			add(field("id"), "must be a short lower-case id such as f1")
		case fids[f.ID]:
			add(field("id"), "another forwarded domain already has this id")
		}
		fids[f.ID] = true
		switch {
		case !ValidDNSName(f.Domain):
			add(field("domain"), "must be a domain name in lower case, such as corp.example.com")
		case c.Domain != "" && (f.Domain == c.Domain || strings.HasSuffix(f.Domain, "."+c.Domain)):
			add(field("domain"), "is inside the local domain (%s), which the router answers itself", c.Domain)
		case !f.Disabled && domains[f.Domain] != "":
			add(field("domain"), "is already forwarded (%s)", domains[f.Domain])
		}
		if !f.Disabled {
			domains[f.Domain] = f.ID
		}
		if len(f.Servers) == 0 {
			add(field("servers"), "enter at least one server")
		}
		validateServers(f.Servers, field("servers"), add)
	}
}

// ---- words ----------------------------------------------------------------------------------------------------

// HostText describes a local name in a sentence.
func HostText(h DNSHost) string {
	t := fmt.Sprintf("%s is %s", h.Name, h.Address)
	if h.Disabled {
		t += ", switched off"
	}
	return t
}

// ForwardText describes a forwarded domain in a sentence.
func DNSForwardText(f DNSForward) string {
	t := fmt.Sprintf("%s is asked of %s", f.Domain, strings.Join(f.Servers, ", "))
	if f.Disabled {
		t += ", switched off"
	}
	return t
}

func serversText(servers []string) string {
	if len(servers) == 0 {
		return "this router"
	}
	return strings.Join(servers, ", ")
}

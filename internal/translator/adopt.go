package translator

import (
	"bufio"
	"bytes"
	"os"
	"strings"

	"peakroute/internal/config"
)

// ParseDnsmasqDHCP reads the DHCP settings back out of a dnsmasq file written by this project
// (or an older setup). It accepts exactly one plain "dhcp-range=start,end,netmask,lease"
// line; anything else (tags, several ranges, IPv6, odd fields) is reported as not adoptable
// rather than guessed at.
func ParseDnsmasqDHCP(content []byte) (config.DHCPConfig, bool) {
	var found *config.DHCPConfig

	sc := bufio.NewScanner(bytes.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		value, ok := strings.CutPrefix(line, "dhcp-range=")
		if !ok {
			continue
		}
		if found != nil {
			return config.DHCPConfig{}, false // more than one range
		}
		fields := strings.Split(value, ",")
		if len(fields) != 4 {
			return config.DHCPConfig{}, false
		}
		found = &config.DHCPConfig{Enabled: true, RangeStart: fields[0], RangeEnd: fields[1], LeaseTime: fields[3]}
	}
	if found == nil {
		return config.DHCPConfig{}, false
	}
	return *found, true
}

// AdoptExisting fills in what network.json leaves blank, for router mode, and reports whether it
// changed anything:
//
//   - a blank LAN address or netmask becomes the router default (192.168.1.1/24)
//   - a missing DHCP section is imported from an existing dnsmasq file (so a box set up by the
//     wizard keeps its range), or defaults if there is no file, or stays unmanaged when the file
//     cannot be understood
//
// Existing values are never overwritten, so a second call changes nothing.
func AdoptExisting(cfg *config.NetworkConfig, fsys FileReader, mode string) bool {
	if m := strings.ToUpper(strings.TrimSpace(mode)); m != "" && m != "ROUTER" {
		return false
	}

	changed := false

	if strings.TrimSpace(cfg.LanIP) == "" {
		cfg.LanIP = config.DefaultLanIP
		changed = true
	}
	if strings.TrimSpace(cfg.LanSubnet) == "" {
		cfg.LanSubnet = config.DefaultLanNetmask
		changed = true
	}

	if cfg.DHCP == nil {
		content, err := fsys.ReadFile(PathDnsmasqDHCP)
		switch {
		case err == nil:
			// An existing file wins over defaults, but only if it is understood and fits this LAN
			if d, ok := ParseDnsmasqDHCP(content); ok &&
				config.DHCPRangeFitsLan(cfg.LanIP, cfg.LanSubnet, d.RangeStart, d.RangeEnd) == nil {
				if _, leaseOK := config.ParseLeaseTime(d.LeaseTime); leaseOK {
					cfg.DHCP = &d
					changed = true
				}
			}
		case os.IsNotExist(err):
			if d := config.DefaultDHCP(cfg.LanIP, cfg.LanSubnet); d != nil {
				cfg.DHCP = d
				changed = true
			}
		}
		// any other read error: leave DHCP unmanaged rather than overwrite what we cannot see
	}

	return changed
}

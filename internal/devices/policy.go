package devices

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/filtering"
	"peakroute/internal/fwlog"
)

// What the router's firewall and DNS filtering say about one device (docs/device-pages.md, sections 5 and 6).

// FirewallLog is where the packets of rules with logging on are kept (internal/fwlog).
type FirewallLog interface {
	Query(fwlog.Query) ([]fwlog.Event, error)
}

// networkOf says which local network an address is on: its id (the default network is "lan"), its name, and whether its DNS is filtered.
func networkOf(cfg config.NetworkConfig, a netip.Addr) (id, name string, filtered, ok bool) {
	in := func(ip, mask string) bool {
		base, err := netip.ParseAddr(ip)
		m := net.IPMask(net.ParseIP(mask).To4())
		if err != nil || m == nil {
			return false
		}
		ones, bits := m.Size()
		return bits != 0 && netip.PrefixFrom(base, ones).Masked().Contains(a)
	}
	if in(cfg.LanIP, cfg.LanSubnet) {
		return config.DefaultNetworkID, "LAN", cfg.LanFilter, true
	}
	for _, n := range cfg.Networks {
		if in(n.IP, n.Netmask) {
			return n.ID, n.Name, n.Filter, true
		}
	}
	return "", "", false, false
}

// RuleHit is a firewall rule that applies to a device.
type RuleHit struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Action   string `json:"action"`
	Text     string `json:"text"`
	Disabled bool   `json:"disabled"`
	Log      bool   `json:"log"`
	Role     string `json:"role"`  // "source": the device's own traffic; "destination": traffic to the device
	Scope    string `json:"scope"` // "device": the rule names its address; "network": it is for everything on the device's network
}

// ForwardHit is a port forward that ends at a device.
type ForwardHit struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Text     string `json:"text"`
	Disabled bool   `json:"disabled"`
}

// LogHit is a logged packet to or from a device.
type LogHit struct {
	ID        int64  `json:"id"`
	Time      int64  `json:"time"`
	Last      int64  `json:"last"`
	Count     int    `json:"count"`
	Rule      string `json:"rule"`
	Name      string `json:"name"`
	Action    string `json:"action,omitempty"`
	Direction string `json:"direction"` // "from" the device or "to" it
	Peer      string `json:"peer"`
	Port      int    `json:"port"` // the destination port
	Proto     string `json:"proto"`
}

// FirewallReport is the firewall's say about a device.
type FirewallReport struct {
	IP          string       `json:"ip"`
	Network     string       `json:"network,omitempty"`
	NetworkName string       `json:"network_name,omitempty"`
	Rules       []RuleHit    `json:"rules"`
	Forwards    []ForwardHit `json:"forwards"`
	LogKnown    bool         `json:"log_known"`
	LogDays     int          `json:"log_days"`
	Log         []LogHit     `json:"log"`
}

const (
	policyLogDays  = 7
	policyLogShown = 50
)

func contains(spec string, a netip.Addr) bool {
	p, ok := config.ParseAddrRange(spec)
	return ok && p.Contains(a)
}

// FirewallFor finds the rules and port forwards that apply to the device with this address. A rule that names the device's address (or a
// range holding it) is listed before the rules that are for the whole network.
func FirewallFor(cfg config.NetworkConfig, ip string) FirewallReport {
	rep := FirewallReport{IP: ip, Rules: []RuleHit{}, Forwards: []ForwardHit{}, Log: []LogHit{}, LogDays: policyLogDays}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return rep
	}
	id, name, _, ok := networkOf(cfg, a)
	rep.Network, rep.NetworkName = id, name
	fw := cfg.FirewallSettings()
	var wide []RuleHit
	for _, r := range fw.Rules {
		hit := RuleHit{ID: r.ID, Name: r.Name, Action: r.Action, Text: cfg.RuleText(r), Disabled: r.Disabled, Log: r.Log}
		switch {
		case ok && r.From == id && r.Source != "" && contains(r.Source, a):
			hit.Role, hit.Scope = "source", "device"
		case r.Destination != "" && contains(r.Destination, a) && (r.To == config.ToAny || (ok && r.To == id)):
			hit.Role, hit.Scope = "destination", "device"
		case ok && r.From == id && r.Source == "":
			hit.Role, hit.Scope = "source", "network"
		case ok && r.To == id && r.Destination == "":
			hit.Role, hit.Scope = "destination", "network"
		default:
			continue
		}
		if hit.Scope == "device" {
			rep.Rules = append(rep.Rules, hit)
		} else {
			wide = append(wide, hit)
		}
	}
	rep.Rules = append(rep.Rules, wide...)
	for _, p := range fw.Forwards {
		if p.ToIP == ip {
			rep.Forwards = append(rep.Forwards, ForwardHit{ID: p.ID, Name: p.Name, Text: cfg.ForwardText(p), Disabled: p.Disabled})
		}
	}
	return rep
}

// HandleFirewall: GET /api/v1/clients/{mac}/firewall.
func (s *Service) HandleFirewall(w http.ResponseWriter, r *http.Request) {
	l, ok := s.deviceFor(w, r)
	if !ok {
		return
	}
	if s.Config == nil {
		writeJSON(w, http.StatusOK, FirewallReport{IP: l.IPAddress, Rules: []RuleHit{}, Forwards: []ForwardHit{}, Log: []LogHit{}})
		return
	}
	cfg := s.Config()
	rep := FirewallFor(cfg, l.IPAddress)
	if s.FirewallLog != nil && l.IPAddress != "" {
		rep.LogKnown = true
		evs, err := s.FirewallLog.Query(fwlog.Query{Search: l.IPAddress, Since: s.now().Add(-policyLogDays * 24 * time.Hour).UnixMilli(), Limit: 500})
		if err != nil {
			rep.LogKnown = false
		}
		for _, e := range evs { // the search finds an address inside longer ones (192.168.1.5 in 192.168.1.50): keep the exact ones
			var dir, peer string
			switch l.IPAddress {
			case e.Src:
				dir, peer = "from", e.Dst
			case e.Dst:
				dir, peer = "to", e.Src
			default:
				continue
			}
			h := LogHit{ID: e.ID, Time: e.Time, Last: e.Last, Count: e.Count, Rule: e.Rule, Name: e.Rule, Direction: dir, Peer: peer, Port: e.DstPort, Proto: e.Proto}
			if rule, ok := cfg.FirewallSettings().FindRule(e.Rule); ok {
				h.Name, h.Action = rule.Name, rule.Action
			}
			rep.Log = append(rep.Log, h)
			if len(rep.Log) >= policyLogShown {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, rep)
}

// DNSReport is what a device asked DNS for lately.
type DNSReport struct {
	Filtering bool   `json:"filtering"` // DNS filtering (AdGuard Home) is on for some network
	Filtered  bool   `json:"filtered"`  // ... and for this device's network
	Network   string `json:"network,omitempty"`
	Error     string `json:"error,omitempty"` // AdGuard Home could not be asked
	*filtering.Report
}

// HandleDNS: GET /api/v1/clients/{mac}/dns.
func (s *Service) HandleDNS(w http.ResponseWriter, r *http.Request) {
	l, ok := s.deviceFor(w, r)
	if !ok {
		return
	}
	out := DNSReport{}
	if s.Config != nil {
		cfg := s.Config()
		out.Filtering = cfg.FilterOn()
		if a, err := netip.ParseAddr(l.IPAddress); err == nil {
			if _, name, filtered, ok := networkOf(cfg, a); ok {
				out.Network, out.Filtered = name, filtered
			}
		}
	}
	if out.Filtering && out.Filtered && s.DNS != nil && l.IPAddress != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		rep, err := s.DNS(ctx, l.IPAddress)
		if err != nil {
			out.Error = "AdGuard Home did not answer: " + err.Error()
		} else {
			out.Report = &rep
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// deviceFor answers a request about one device by hardware address (404 when it is not on the list).
func (s *Service) deviceFor(w http.ResponseWriter, r *http.Request) (l struct{ IPAddress string }, ok bool) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return l, false
	}
	mac, ok := macOf(w, r)
	if !ok {
		return l, false
	}
	lease, found := s.find(r.Context(), mac)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "there is no device with this hardware address"})
		return l, false
	}
	l.IPAddress = lease.IPAddress
	return l, true
}

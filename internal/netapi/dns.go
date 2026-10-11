package netapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"peakroute/internal/config"
)

// The DNS API: where the router asks, what each network is told to use, the local names and the forwarded domains. Everything
// is staged like the other editors. docs/dns.md has the model.

// DNSUpstream is where the router asks for the names it does not know.
type DNSUpstream struct {
	Mode     string   `json:"mode"`     // "provider" (what the internet connection hands out) or "custom" (servers set by hand)
	Servers  []string `json:"servers"`  // the servers set by hand (the draft)
	Provider []string `json:"provider"` // what the provider's servers are right now
	Static   bool     `json:"static"`   // the connection has a fixed address: servers must be set
	Pending  bool     `json:"pending"`
}

// DNSNetwork is what one network's devices are told to use for DNS.
type DNSNetwork struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Default bool     `json:"default"`
	Mode    string   `json:"mode"`    // "router" or "custom"
	Servers []string `json:"servers"` // the servers it is told to use when custom
	Router  string   `json:"router"`  // the router's address on the network, what it is told otherwise
	DHCP    bool     `json:"dhcp"`    // the router runs its DHCP server: without it nothing is told anything
	Notes   []string `json:"notes"`   // things worth knowing about the servers chosen
	Pending bool     `json:"pending"`
}

// DNSHost is a local name as the page shows it.
type DNSHost struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	Enabled bool   `json:"enabled"`
	Text    string `json:"text"`
	Pending bool   `json:"pending"`
}

// DNSForward is a forwarded domain as the page shows it.
type DNSForward struct {
	ID      string   `json:"id"`
	Domain  string   `json:"domain"`
	Servers []string `json:"servers"`
	Enabled bool     `json:"enabled"`
	Text    string   `json:"text"`
	Pending bool     `json:"pending"`
}

type dnsResponse struct {
	Upstream DNSUpstream  `json:"upstream"`
	Domain   string       `json:"domain"` // set on System, Settings
	Networks []DNSNetwork `json:"networks"`
	Hosts    []DNSHost    `json:"hosts"`
	Forwards []DNSForward `json:"forwards"`
	Editable bool         `json:"editable"`
}

func sameStrings(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// networkNotes says what is worth knowing about the servers a network is told to use.
func networkNotes(c config.NetworkConfig, id string, servers []string, dhcp bool, internet bool) []string {
	var notes []string
	if len(servers) == 0 {
		return notes
	}
	if !dhcp {
		notes = append(notes, "The router does not run a DHCP server on this network, so nothing tells its devices to use these servers.")
	}
	ids := []string{config.DefaultNetworkID}
	for _, n := range c.Networks {
		ids = append(ids, n.ID)
	}
	for _, s := range servers {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		where := ""
		for _, other := range ids {
			if p, ok := c.NetworkPrefix(other); ok && p.Contains(addr) {
				where = other
			}
		}
		switch {
		case where == id:
		case where != "":
			notes = append(notes, fmt.Sprintf("%s is on %s: a firewall rule must let this network reach it.", s, networkLabel(c, where)))
		case !internet:
			notes = append(notes, fmt.Sprintf("%s is outside your networks and this network has no internet access, so it cannot be reached.", s))
		}
	}
	return notes
}

func networkLabel(c config.NetworkConfig, id string) string {
	if id == config.DefaultNetworkID {
		return "LAN"
	}
	if n, ok := c.FindNetwork(id); ok {
		return strings.TrimSpace(n.Name)
	}
	return id
}

func dnsNetworks(active, draft config.NetworkConfig) []DNSNetwork {
	var out []DNSNetwork
	add := func(id, name string, def bool, router string, dhcp *config.DHCPConfig, internet bool) {
		servers := append([]string{}, draft.NetworkDNSServers(id)...)
		mode := "router"
		if len(servers) > 0 {
			mode = "custom"
		}
		out = append(out, DNSNetwork{ID: id, Name: name, Default: def, Mode: mode, Servers: servers, Router: router,
			DHCP: dhcp != nil && dhcp.Enabled, Notes: networkNotes(draft, id, servers, dhcp != nil && dhcp.Enabled, internet),
			Pending: !sameStrings(active.NetworkDNSServers(id), draft.NetworkDNSServers(id))})
		if out[len(out)-1].Notes == nil {
			out[len(out)-1].Notes = []string{}
		}
	}
	add(config.DefaultNetworkID, "LAN", true, draft.LanIP, draft.DHCP, true)
	for _, n := range draft.Networks {
		add(n.ID, strings.TrimSpace(n.Name), false, n.IP, n.DHCP, n.Internet)
	}
	return out
}

func (a *API) dnsView() dnsResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	d := draft.DNSSettings()

	up := DNSUpstream{Mode: "provider", Servers: append([]string{}, draft.WanDNS...), Provider: []string{}, Static: draft.WanKind() == config.WanStatic,
		Pending: !sameStrings(active.WanDNS, draft.WanDNS)}
	if len(draft.WanDNS) > 0 {
		up.Mode = "custom"
	}
	if a.ProviderDNS != nil {
		up.Provider = append(up.Provider, a.ProviderDNS()...)
	}
	resp := dnsResponse{Upstream: up, Domain: draft.Domain, Networks: dnsNetworks(active, draft), Hosts: []DNSHost{}, Forwards: []DNSForward{}, Editable: a.editable()}
	for _, h := range d.Hosts {
		old, had := active.DNSSettings().FindHost(h.ID)
		resp.Hosts = append(resp.Hosts, hostView(h, !had || old != h))
	}
	for _, f := range d.Forwards {
		resp.Forwards = append(resp.Forwards, forwardViewDNS(f, active))
	}
	return resp
}

func hostView(h config.DNSHost, pending bool) DNSHost {
	return DNSHost{ID: h.ID, Name: h.Name, Address: h.Address, Enabled: !h.Disabled, Text: config.HostText(h), Pending: pending}
}

func forwardViewDNS(f config.DNSForward, active config.NetworkConfig) DNSForward {
	old, had := active.DNSSettings().FindForward(f.ID)
	pending := !had || old.Domain != f.Domain || old.Disabled != f.Disabled || !sameStrings(old.Servers, f.Servers)
	return DNSForward{ID: f.ID, Domain: f.Domain, Servers: append([]string{}, f.Servers...), Enabled: !f.Disabled, Text: config.DNSForwardText(f), Pending: pending}
}

// HandleDNS: GET /api/v1/dns returns the DNS settings (the draft).
func (a *API) HandleDNS(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, a.dnsView())
	}
}

type serversRequest struct {
	Servers []string `json:"servers"`
}

func cleanServers(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// filterErrors keeps the validation problems on one field and renames it.
func filterErrors(err error, field, as string) error {
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out config.ValidationError
	for _, e := range ve {
		if e.Field == field {
			out = append(out, config.FieldError{Field: as, Message: e.Message})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// HandleDNSUpstream: PUT /api/v1/dns/upstream {"servers": [...]} sets the servers the router asks instead of the provider's;
// an empty list goes back to the provider's (a connection with a fixed address needs servers).
func (a *API) HandleDNSUpstream(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) || !a.requireEditable(w) {
		return
	}
	var req serversRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		c.WanDNS = cleanServers(req.Servers)
		if err := filterErrors(c.Validate(), "wan_dns", "servers"); err != nil {
			return err
		}
		if c.WanKind() == config.WanStatic && len(c.WanDNS) == 0 {
			return config.ValidationError{{Field: "servers", Message: "a connection with a fixed address needs at least one DNS server"}}
		}
		return nil
	})
	a.answerStaged(w, err, http.StatusOK, "These servers cannot be saved: ", a.dnsView())
}

// HandleDNSNetwork: PUT /api/v1/dns/networks/{id} {"servers": [...]} sets the servers a network is told to use instead of
// the router; an empty list goes back to the router.
func (a *API) HandleDNSNetwork(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	if id != config.DefaultNetworkID {
		if _, ok := draft.FindNetwork(id); !ok {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no network with this id"})
			return
		}
	}
	if !a.requireEditable(w) {
		return
	}
	var req serversRequest
	if !decode(w, r, &req) {
		return
	}
	servers := cleanServers(req.Servers)
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		field := "lan_dns_servers"
		if id == config.DefaultNetworkID {
			c.LanDNSServers = servers
		} else {
			for i := range c.Networks {
				if c.Networks[i].ID == id {
					c.Networks[i].DNSServers = servers
					field = fmt.Sprintf("networks[%d].dns_servers", i)
				}
			}
		}
		return filterErrors(c.Validate(), field, "servers")
	})
	a.answerStaged(w, err, http.StatusOK, "These servers cannot be saved: ", a.dnsView())
}

// ---- local names -----------------------------------------------------------------------------------------------

type dnsHostRequest struct {
	Name    *string `json:"name"`
	Address *string `json:"address"`
	Enabled *bool   `json:"enabled"`
}

func applyHostRequest(h *config.DNSHost, req dnsHostRequest) {
	if req.Name != nil {
		h.Name = strings.ToLower(trim(req.Name))
	}
	if req.Address != nil {
		h.Address = trim(req.Address)
	}
	if req.Enabled != nil {
		h.Disabled = !*req.Enabled
	}
}

// HandleDNSHosts: POST /api/v1/dns/hosts adds a local name.
func (a *API) HandleDNSHosts(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	var req dnsHostRequest
	if !decode(w, r, &req) {
		return
	}
	var id string
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.DNS == nil {
			c.DNS = &config.DNSConfig{}
		}
		d := c.DNS
		id = nextID("h", func(id string) bool { _, ok := d.FindHost(id); return ok })
		h := config.DNSHost{ID: id}
		applyHostRequest(&h, req)
		d.Hosts = append(d.Hosts, h)
		return errorsForFirewall(c.Validate(), fmt.Sprintf("dns.hosts[%d].", len(d.Hosts)-1), "dns.hosts")
	})
	var body any
	if err == nil {
		draft, _ := a.Manager.StagedNetworkConfig()
		h, _ := draft.DNSSettings().FindHost(id)
		body = hostView(h, true)
	}
	a.answerStaged(w, err, http.StatusCreated, "This name cannot be added: ", body)
}

func tidyDNS(c *config.NetworkConfig) {
	if d := c.DNS; d != nil && len(d.Hosts) == 0 && len(d.Forwards) == 0 {
		c.DNS = nil
	}
}

// HandleDNSHost: PUT /api/v1/dns/hosts/{id} changes a local name; DELETE removes it.
func (a *API) HandleDNSHost(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	if _, ok := draft.DNSSettings().FindHost(id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no local name with this id"})
		return
	}
	if !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			kept := c.DNS.Hosts[:0:0]
			for _, h := range c.DNS.Hosts {
				if h.ID != id {
					kept = append(kept, h)
				}
			}
			c.DNS.Hosts = kept
			tidyDNS(c)
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.dnsView())
		return
	}
	var req dnsHostRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		for i := range c.DNS.Hosts {
			if c.DNS.Hosts[i].ID != id {
				continue
			}
			applyHostRequest(&c.DNS.Hosts[i], req)
			return errorsForFirewall(c.Validate(), fmt.Sprintf("dns.hosts[%d].", i), "dns.hosts")
		}
		return nil
	})
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		h, _ := now.DNSSettings().FindHost(id)
		old, had := a.Manager.NetworkConfig().DNSSettings().FindHost(id)
		body = hostView(h, !had || old != h)
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

// ---- forwarded domains -----------------------------------------------------------------------------------------

type dnsForwardRequest struct {
	Domain  *string   `json:"domain"`
	Servers *[]string `json:"servers"`
	Enabled *bool     `json:"enabled"`
}

func applyDNSForwardRequest(f *config.DNSForward, req dnsForwardRequest) {
	if req.Domain != nil {
		f.Domain = strings.ToLower(trim(req.Domain))
	}
	if req.Servers != nil {
		f.Servers = cleanServers(*req.Servers)
	}
	if req.Enabled != nil {
		f.Disabled = !*req.Enabled
	}
}

// HandleDNSForwards: POST /api/v1/dns/forwards adds a forwarded domain.
func (a *API) HandleDNSForwards(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	var req dnsForwardRequest
	if !decode(w, r, &req) {
		return
	}
	var id string
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.DNS == nil {
			c.DNS = &config.DNSConfig{}
		}
		d := c.DNS
		id = nextID("f", func(id string) bool { _, ok := d.FindForward(id); return ok })
		f := config.DNSForward{ID: id}
		applyDNSForwardRequest(&f, req)
		d.Forwards = append(d.Forwards, f)
		return errorsForFirewall(c.Validate(), fmt.Sprintf("dns.forwards[%d].", len(d.Forwards)-1), "dns.forwards")
	})
	var body any
	if err == nil {
		draft, _ := a.Manager.StagedNetworkConfig()
		f, _ := draft.DNSSettings().FindForward(id)
		body = forwardViewDNS(f, a.Manager.NetworkConfig())
	}
	a.answerStaged(w, err, http.StatusCreated, "This domain cannot be added: ", body)
}

// HandleDNSForward: PUT /api/v1/dns/forwards/{id} changes a forwarded domain; DELETE removes it.
func (a *API) HandleDNSForward(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	if _, ok := draft.DNSSettings().FindForward(id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no forwarded domain with this id"})
		return
	}
	if !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			kept := c.DNS.Forwards[:0:0]
			for _, f := range c.DNS.Forwards {
				if f.ID != id {
					kept = append(kept, f)
				}
			}
			c.DNS.Forwards = kept
			tidyDNS(c)
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.dnsView())
		return
	}
	var req dnsForwardRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		for i := range c.DNS.Forwards {
			if c.DNS.Forwards[i].ID != id {
				continue
			}
			applyDNSForwardRequest(&c.DNS.Forwards[i], req)
			return errorsForFirewall(c.Validate(), fmt.Sprintf("dns.forwards[%d].", i), "dns.forwards")
		}
		return nil
	})
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		f, _ := now.DNSSettings().FindForward(id)
		body = forwardViewDNS(f, a.Manager.NetworkConfig())
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

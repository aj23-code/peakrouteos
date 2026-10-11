package netapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	"peakroute/internal/config"
)

// DHCP is a network's DHCP server settings.
type DHCP struct {
	Enabled    bool   `json:"enabled"`
	RangeStart string `json:"range_start"`
	RangeEnd   string `json:"range_end"`
	LeaseTime  string `json:"lease_time"`
}

// Network is one local network: its address, the ports that carry it, its DHCP server and how it
// is firewalled. The default network (id "lan", always there) is the one every router has; the
// others are VLANs a person adds.
type Network struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Default     bool     `json:"default"`
	VLAN        int      `json:"vlan"` // 1 for the default network
	IP          string   `json:"ip"`
	Netmask     string   `json:"netmask"`
	Ports       []string `json:"ports"`        // the ports that carry it untagged
	TaggedPorts []string `json:"tagged_ports"` // the ports that carry it as a VLAN
	Bridged     bool     `json:"bridged"`      // several ports share the network through a bridge
	DHCP        *DHCP    `json:"dhcp"`         // null: no DHCP server (or, for the default network, not managed yet)
	Internet    bool     `json:"internet"`     // may reach the internet; always true for the default network
	Isolated    bool     `json:"isolated"`     // cannot talk to other networks, and they cannot talk to it
	Pending     bool     `json:"pending"`      // the draft differs from what is in effect
}

type networksResponse struct {
	Networks []Network `json:"networks"`
	Editable bool      `json:"editable"`
}

const lanID = config.DefaultNetworkID

// The fields of the configuration the default network owns (the file's own names).
var networkFields = []string{"lan_ip", "lan_subnet", "lan_interfaces", "lan_isolated", "dhcp_enabled", "dhcp_range_start", "dhcp_range_end", "dhcp_lease_time"}

func toDHCP(d *config.DHCPConfig) *DHCP {
	if d == nil {
		return nil
	}
	return &DHCP{Enabled: d.Enabled, RangeStart: d.RangeStart, RangeEnd: d.RangeEnd, LeaseTime: d.LeaseTime}
}

func fromDHCP(d *DHCP) *config.DHCPConfig {
	if d == nil {
		return nil
	}
	return &config.DHCPConfig{
		Enabled:    d.Enabled,
		RangeStart: strings.TrimSpace(d.RangeStart),
		RangeEnd:   strings.TrimSpace(d.RangeEnd),
		LeaseTime:  strings.TrimSpace(d.LeaseTime),
	}
}

// networkPorts lists which LAN ports carry a network untagged and which tagged, in port order.
func networkPorts(c config.NetworkConfig, id string) (native, tagged []string) {
	native, tagged = []string{}, []string{}
	for _, port := range c.LanInterfaces {
		pn := c.PortNetworks[port]
		if pn.NativeID() == id {
			native = append(native, port)
		}
		for _, t := range pn.Tagged {
			if t == id {
				tagged = append(tagged, port)
			}
		}
	}
	return native, tagged
}

func samePorts(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func networkView(active, draft config.NetworkConfig, id string) (Network, bool) {
	n := Network{ID: id, Default: id == lanID}
	if n.Default {
		n.Name, n.VLAN, n.IP, n.Netmask = "LAN", 1, draft.LanIP, draft.LanSubnet
		n.DHCP, n.Internet, n.Isolated = toDHCP(draft.DHCP), true, draft.LanIsolated
		n.Pending = changedIn(active, draft, networkFields...)
	} else {
		d, ok := draft.FindNetwork(id)
		if !ok {
			return Network{}, false
		}
		n.Name, n.VLAN, n.IP, n.Netmask = d.Name, d.VLAN, d.IP, d.Netmask
		n.DHCP, n.Internet, n.Isolated = toDHCP(d.DHCP), d.Internet, d.Isolated
		a, had := active.FindNetwork(id)
		n.Pending = !had || !a.Equal(d)
	}
	n.Ports, n.TaggedPorts = networkPorts(draft, id)
	if !n.Pending {
		aNative, aTagged := networkPorts(active, id)
		n.Pending = !samePorts(aNative, n.Ports) || !samePorts(aTagged, n.TaggedPorts)
	}
	n.Bridged = len(n.Ports)+len(n.TaggedPorts) > 1
	return n, true
}

func (a *API) networksView() networksResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	resp := networksResponse{Networks: []Network{}, Editable: a.editable()}
	lan, _ := networkView(active, draft, lanID)
	resp.Networks = append(resp.Networks, lan)
	for _, n := range draft.Networks {
		v, _ := networkView(active, draft, n.ID)
		resp.Networks = append(resp.Networks, v)
	}
	return resp
}

// HandleNetworks: GET /api/v1/networks lists the networks; POST adds one.
func (a *API) HandleNetworks(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.networksView())
		return
	}
	a.postNetwork(w, r)
}

// HandleNetwork: GET returns one network; PUT /api/v1/networks/{id} edits it; DELETE removes it.
func (a *API) HandleNetwork(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	if _, ok := networkView(active, draft, id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no network with this id"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		n, _ := networkView(active, draft, id)
		writeJSON(w, http.StatusOK, n)
	case http.MethodPut:
		a.putNetwork(w, r, id)
	case http.MethodDelete:
		a.deleteNetwork(w, id)
	}
}

// networkRequest is what a person sends to create or change a network. A field left out stays as it
// is (when changing) or takes its default (when creating): no internet, isolated, no DHCP.
type networkRequest struct {
	Name     *string `json:"name"`
	VLAN     *int    `json:"vlan"`
	IP       string  `json:"ip"`
	Netmask  string  `json:"netmask"`
	DHCP     *DHCP   `json:"dhcp"`
	Internet *bool   `json:"internet"`
	Isolated *bool   `json:"isolated"`
}

// answer turns the result of staging into the response: field errors, a failure, or the network.
func (a *API) answer(w http.ResponseWriter, err error, status int, id, summary string) {
	var ve config.ValidationError
	if errors.As(err, &ve) {
		fieldErrors(w, http.StatusBadRequest, summary+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	n, _ := networkView(active, draft, id)
	writeJSON(w, status, n)
}

// errorsForNetwork keeps the validation problems that belong to one additional network (renamed to
// the API's own field names) and the ones about the networks as a whole. Another network's own
// problems are not this request's business: they are shown when that network is edited, or at commit.
func errorsForNetwork(err error, index int) error {
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	prefix := fmt.Sprintf("networks[%d].", index)
	var out config.ValidationError
	for _, e := range ve {
		switch {
		case strings.HasPrefix(e.Field, prefix):
			out = append(out, config.FieldError{Field: strings.TrimPrefix(e.Field, prefix), Message: e.Message})
		case e.Field == "networks":
			out = append(out, e)
		case e.Field == "dhcp_enabled":
			out = append(out, config.FieldError{Field: "dhcp.enabled", Message: e.Message})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nextNetworkID(c config.NetworkConfig) string {
	for k := 2; ; k++ {
		id := fmt.Sprintf("net%d", k)
		if _, taken := c.FindNetwork(id); !taken {
			return id
		}
	}
}

func (a *API) postNetwork(w http.ResponseWriter, r *http.Request) {
	if !a.requireEditable(w) {
		return
	}
	var req networkRequest
	if !decode(w, r, &req) {
		return
	}

	var id string
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		id = nextNetworkID(*c)
		n := config.Network{
			ID:       id,
			IP:       strings.TrimSpace(req.IP),
			Netmask:  strings.TrimSpace(req.Netmask),
			DHCP:     fromDHCP(req.DHCP),
			Internet: req.Internet != nil && *req.Internet,
			Isolated: req.Isolated == nil || *req.Isolated, // isolated unless told otherwise
		}
		if req.Name != nil {
			n.Name = strings.TrimSpace(*req.Name)
		}
		if req.VLAN != nil {
			n.VLAN = *req.VLAN
		}
		c.Networks = append(c.Networks, n)
		return errorsForNetwork(c.Validate(), len(c.Networks)-1)
	})
	a.answer(w, err, http.StatusCreated, id, "This network cannot be added: ")
}

func (a *API) putNetwork(w http.ResponseWriter, r *http.Request, id string) {
	if !a.requireEditable(w) {
		return
	}
	var req networkRequest
	if !decode(w, r, &req) {
		return
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if id == lanID {
			return a.editDefault(c, req)
		}
		for i := range c.Networks {
			if c.Networks[i].ID != id {
				continue
			}
			n := &c.Networks[i]
			n.IP, n.Netmask = strings.TrimSpace(req.IP), strings.TrimSpace(req.Netmask)
			if req.Name != nil {
				n.Name = strings.TrimSpace(*req.Name)
			}
			if req.VLAN != nil {
				n.VLAN = *req.VLAN
			}
			if req.DHCP != nil {
				n.DHCP = fromDHCP(req.DHCP)
			}
			if req.Internet != nil {
				n.Internet = *req.Internet
			}
			if req.Isolated != nil {
				n.Isolated = *req.Isolated
			}
			return errorsForNetwork(c.Validate(), i)
		}
		return nil // the id was checked before
	})
	a.answer(w, err, http.StatusOK, id, "These settings cannot be saved: ")
}

// editDefault changes the default network: its address, DHCP and isolation. What makes it the
// default (its name, VLAN 1, and its access to the internet) cannot be changed.
func (a *API) editDefault(c *config.NetworkConfig, req networkRequest) error {
	var problems config.ValidationError
	if req.Name != nil && strings.TrimSpace(*req.Name) != "LAN" {
		problems = append(problems, config.FieldError{Field: "name", Message: "the default network cannot be renamed"})
	}
	if req.VLAN != nil && *req.VLAN != 1 {
		problems = append(problems, config.FieldError{Field: "vlan", Message: "the default network is VLAN 1"})
	}
	if req.Internet != nil && !*req.Internet {
		problems = append(problems, config.FieldError{Field: "internet", Message: "the default network always has internet access"})
	}
	if len(problems) > 0 {
		return problems
	}

	c.LanIP = strings.TrimSpace(req.IP)
	c.LanSubnet = strings.TrimSpace(req.Netmask)
	if req.DHCP != nil {
		c.DHCP = fromDHCP(req.DHCP)
	}
	if req.Isolated != nil {
		c.LanIsolated = *req.Isolated
	}

	// Only this network's own fields are checked here; the rest of the configuration (the WAN, say)
	// is checked when the draft is committed. A disabled DHCP server needs no range, so only the
	// fields that apply are reported. A new address that overlaps another network is reported too.
	return ownedErrorsOrNil(c.Validate(), "lan_ip", "lan_subnet", "dhcp_range_start", "dhcp_range_end", "dhcp_lease_time", "networks")
}

func (a *API) deleteNetwork(w http.ResponseWriter, id string) {
	if !a.requireEditable(w) {
		return
	}
	if id == lanID {
		fieldErrors(w, http.StatusBadRequest, "The default network cannot be deleted.", []config.FieldError{{Field: "id", Message: "the default network cannot be deleted"}})
		return
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		var problems config.ValidationError
		for port, pn := range c.PortNetworks {
			if pn.NativeID() == id {
				problems = append(problems, config.FieldError{Field: "ports",
					Message: fmt.Sprintf("port %s still carries this network untagged; move the port to another network first", port)})
			}
		}
		for _, wn := range c.WifiSettings().Networks {
			if wn.Network == id {
				problems = append(problems, config.FieldError{Field: "wifi",
					Message: fmt.Sprintf("the Wi-Fi network %q still joins this network; move or delete it first", wn.SSID)})
			}
		}
		for _, r := range c.FirewallSettings().Rules {
			if r.From == id || r.To == id {
				problems = append(problems, config.FieldError{Field: "firewall",
					Message: fmt.Sprintf("the firewall rule %q uses this network; delete or change it first", r.Name)})
			}
		}
		if np, ok := c.NetworkPrefix(id); ok {
			for _, p := range c.FirewallSettings().Forwards {
				if ip, err := netip.ParseAddr(p.ToIP); err == nil && np.Contains(ip) {
					problems = append(problems, config.FieldError{Field: "firewall",
						Message: fmt.Sprintf("the port forward %q sends to a device on this network; delete or change it first", p.Name)})
				}
			}
		}
		if len(problems) > 0 {
			return problems
		}

		// a network that is only tagged on ports simply stops being carried
		for port, pn := range c.PortNetworks {
			var keep []string
			for _, t := range pn.Tagged {
				if t != id {
					keep = append(keep, t)
				}
			}
			pn.Tagged = keep
			if pn.IsDefault() {
				delete(c.PortNetworks, port)
			} else {
				c.PortNetworks[port] = pn
			}
		}
		if len(c.PortNetworks) == 0 {
			c.PortNetworks = nil
		}

		kept := c.Networks[:0:0]
		for _, n := range c.Networks {
			if n.ID != id {
				kept = append(kept, n)
			}
		}
		c.Networks = kept
		return ownedErrorsOrNil(c.Validate(), "networks", "port_networks")
	})

	var ve config.ValidationError
	if errors.As(err, &ve) {
		fieldErrors(w, http.StatusBadRequest, "This network cannot be deleted: "+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.networksView())
}

// ownedErrorsOrNil returns the owned validation problems as an error, or nil if there are none.
func ownedErrorsOrNil(err error, fields ...string) error {
	if own := ownedErrors(err, fields...); len(own) > 0 {
		return own
	}
	return nil
}

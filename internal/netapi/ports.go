package netapi

import (
	"errors"
	"net/http"
	"strings"

	"peakroute/internal/config"
)

// Port is a physical network port and the role the draft gives it.
type Port struct {
	Name       string `json:"name"`
	MAC        string `json:"mac"`
	State      string `json:"state"`
	Speed      string `json:"speed"`
	MTU        int    `json:"mtu"`
	Driver     string `json:"driver"`
	Present    bool   `json:"present"`     // false: configured, but the hardware is not there
	Wireless   bool   `json:"wireless"`    // a Wi-Fi adapter: set up on the Wireless page, never a WAN or LAN port
	Role       string `json:"role"`        // what the draft says: wan, lan or unassigned
	ActiveRole string `json:"active_role"` // what is in effect now
	Pending    bool   `json:"pending"`     // the draft differs from what is in effect

	// What a LAN port carries (empty for the other roles): the id of its untagged network and the
	// ids of the networks it carries as VLANs.
	Native string   `json:"native"`
	Tagged []string `json:"tagged"`
}

// NetworkRef names a network a port can carry.
type NetworkRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	VLAN    int    `json:"vlan"`
	Default bool   `json:"default"`
}

type portsResponse struct {
	Ports    []Port       `json:"ports"`
	Networks []NetworkRef `json:"networks"` // what a LAN port can carry
	Editable bool         `json:"editable"`
}

// portNames lists every port that can be given a role: the hardware, in the order the system reports
// it, then any configured port whose hardware has gone missing (so it can still be reassigned).
func portNames(hardware []config.SystemInterface, cfgs ...config.NetworkConfig) []string {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	// A Wi-Fi adapter is not a port. One that is configured as a port anyway (a box set up before this
	// rule, or a laptop used for development) is still listed so that it can be moved off.
	configured := map[string]bool{}
	for _, c := range cfgs {
		configured[c.WanInterface] = true
		for _, l := range c.LanInterfaces {
			configured[l] = true
		}
	}
	for _, h := range hardware {
		if h.Wireless && !configured[h.Name] {
			continue
		}
		add(h.Name)
	}
	for _, c := range cfgs {
		add(c.WanInterface)
		for _, lan := range c.LanInterfaces {
			add(lan)
		}
	}
	return names
}

func (a *API) portsView() (portsResponse, error) {
	hardware, err := a.Ports()
	if err != nil {
		return portsResponse{}, err
	}
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()

	byName := map[string]config.SystemInterface{}
	for _, h := range hardware {
		byName[h.Name] = h
	}

	resp := portsResponse{Ports: []Port{}, Editable: a.editable()}
	for _, name := range portNames(hardware, draft, active) {
		hw, present := byName[name]
		p := Port{
			Name: name, MAC: hw.MAC, State: hw.State, Speed: hw.Speed, MTU: hw.MTU, Driver: hw.Driver,
			Present:    present,
			Wireless:   hw.Wireless,
			Role:       roleOf(draft, name),
			ActiveRole: roleOf(active, name),
		}
		p.Tagged = []string{}
		if p.Role == RoleLAN {
			pn := draft.PortNetworks[name]
			p.Native = pn.NativeID()
			p.Tagged = append(p.Tagged, pn.Tagged...)
		}
		p.Pending = p.Role != p.ActiveRole
		if !p.Pending && p.Role == RoleLAN {
			// the same role, but a different set of networks
			ap := active.PortNetworks[name]
			p.Pending = ap.NativeID() != p.Native || strings.Join(ap.Tagged, ",") != strings.Join(p.Tagged, ",")
		}
		resp.Ports = append(resp.Ports, p)
	}

	resp.Networks = []NetworkRef{{ID: lanID, Name: "LAN", VLAN: 1, Default: true}}
	for _, n := range draft.Networks {
		resp.Networks = append(resp.Networks, NetworkRef{ID: n.ID, Name: n.Name, VLAN: n.VLAN})
	}
	return resp, nil
}

// HandlePorts: GET lists the ports and their roles; PUT assigns roles.
func (a *API) HandlePorts(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodPut {
		a.putPorts(w, r)
		return
	}

	view, err := a.portsView()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *API) putPorts(w http.ResponseWriter, r *http.Request) {
	if !a.requireEditable(w) {
		return
	}
	var req struct {
		Assignments map[string]string `json:"assignments"`
		// Optional: the networks a LAN port carries. A LAN port not mentioned keeps what it has.
		Networks map[string]struct {
			Native string   `json:"native"`
			Tagged []string `json:"tagged"`
		} `json:"networks"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Assignments) == 0 {
		fieldErrors(w, http.StatusBadRequest, "No port assignments were given.", []config.FieldError{{Field: "assignments", Message: "give at least one port a role"}})
		return
	}

	hardware, err := a.Ports()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}

	var problems []config.FieldError
	err = a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		names := portNames(hardware, *c)
		known := map[string]bool{}
		roles := map[string]string{}
		for _, n := range names {
			known[n] = true
			roles[n] = roleOf(*c, n)
		}

		wireless := map[string]bool{}
		for _, h := range hardware {
			if h.Wireless {
				wireless[h.Name] = true
			}
		}

		for name, role := range req.Assignments {
			switch {
			case wireless[name] && role != RoleUnassigned && role != roles[name]:
				problems = append(problems, config.FieldError{Field: "assignments." + name, Message: "a Wi-Fi adapter cannot be a WAN or LAN port: it is set up on the Wireless page"})
			case !known[name]:
				problems = append(problems, config.FieldError{Field: "assignments." + name, Message: "there is no port with this name"})
			case role != RoleWAN && role != RoleLAN && role != RoleUnassigned:
				problems = append(problems, config.FieldError{Field: "assignments." + name, Message: `the role must be "wan", "lan" or "unassigned"`})
			default:
				roles[name] = role
			}
		}
		if len(problems) > 0 {
			return config.ValidationError(problems)
		}

		var wans, lans []string
		for _, n := range names { // stable order: hardware order, so the saved list does not shuffle
			switch roles[n] {
			case RoleWAN:
				wans = append(wans, n)
			case RoleLAN:
				lans = append(lans, n)
			}
		}
		switch {
		case len(wans) == 0:
			problems = append(problems, config.FieldError{Field: "ports", Message: "one port must be the WAN (the internet side)"})
		case len(wans) > 1:
			problems = append(problems, config.FieldError{Field: "ports", Message: "only one port can be the WAN"})
		}
		if len(lans) == 0 {
			problems = append(problems, config.FieldError{Field: "ports", Message: "at least one port must be a LAN port"})
		}
		if len(problems) > 0 {
			return config.ValidationError(problems)
		}

		c.WanInterface = wans[0]
		c.LanInterfaces = lans

		// a port that is no longer a LAN port carries no networks
		for port := range c.PortNetworks {
			if roles[port] != RoleLAN {
				delete(c.PortNetworks, port)
			}
		}
		for port, in := range req.Networks {
			if roles[port] != RoleLAN {
				problems = append(problems, config.FieldError{Field: "networks." + port, Message: "only a LAN port carries networks"})
				continue
			}
			if c.PortNetworks == nil {
				c.PortNetworks = map[string]config.PortNetworks{}
			}
			pn := config.PortNetworks{Native: strings.TrimSpace(in.Native)}
			for _, t := range in.Tagged {
				pn.Tagged = append(pn.Tagged, strings.TrimSpace(t))
			}
			if pn.IsDefault() {
				delete(c.PortNetworks, port)
			} else {
				c.PortNetworks[port] = pn
			}
		}
		if len(c.PortNetworks) == 0 {
			c.PortNetworks = nil
		}
		if len(problems) > 0 {
			return config.ValidationError(problems)
		}

		if own := ownedErrors(c.Validate(), "wan_interface", "lan_interfaces", "port_networks"); len(own) > 0 {
			problems = append(problems, own...)
			return config.ValidationError(problems)
		}
		return nil
	})

	var ve config.ValidationError
	if errors.As(err, &ve) {
		fieldErrors(w, http.StatusBadRequest, "These port roles cannot be saved: "+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}

	view, err := a.portsView()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

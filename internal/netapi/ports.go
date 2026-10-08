package netapi

import (
	"errors"
	"net/http"

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
	Role       string `json:"role"`        // what the draft says: wan, lan or unassigned
	ActiveRole string `json:"active_role"` // what is in effect now
	Pending    bool   `json:"pending"`     // the draft differs from what is in effect
}

type portsResponse struct {
	Ports    []Port `json:"ports"`
	Editable bool   `json:"editable"`
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
	for _, h := range hardware {
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
			Role:       roleOf(draft, name),
			ActiveRole: roleOf(active, name),
		}
		p.Pending = p.Role != p.ActiveRole
		resp.Ports = append(resp.Ports, p)
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

		for name, role := range req.Assignments {
			switch {
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

		if own := ownedErrors(c.Validate(), "wan_interface", "lan_interfaces"); len(own) > 0 {
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

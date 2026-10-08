package netapi

import (
	"errors"
	"net/http"

	"peakroute/internal/config"
)

// DHCP is a network's DHCP server settings.
type DHCP struct {
	Enabled    bool   `json:"enabled"`
	RangeStart string `json:"range_start"`
	RangeEnd   string `json:"range_end"`
	LeaseTime  string `json:"lease_time"`
}

// Network is one network: its address, which ports serve it, and its DHCP server. Today there is
// one (the LAN, id "lan"); VLANs would be more of these.
type Network struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	IP      string   `json:"ip"`
	Netmask string   `json:"netmask"`
	Ports   []string `json:"ports"`
	Bridged bool     `json:"bridged"` // several ports share the network through a bridge
	DHCP    *DHCP    `json:"dhcp"`    // null: not managed by the configuration yet
	Pending bool     `json:"pending"` // the draft differs from what is in effect
}

type networksResponse struct {
	Networks []Network `json:"networks"`
	Editable bool      `json:"editable"`
}

const lanID = "lan"

// The fields of the configuration a network owns (the file's own names).
var networkFields = []string{"lan_ip", "lan_subnet", "lan_interfaces", "dhcp_enabled", "dhcp_range_start", "dhcp_range_end", "dhcp_lease_time"}

func lanNetwork(active, draft config.NetworkConfig) Network {
	n := Network{
		ID:      lanID,
		Name:    "LAN",
		IP:      draft.LanIP,
		Netmask: draft.LanSubnet,
		Ports:   append([]string{}, draft.LanInterfaces...),
		Bridged: len(draft.LanInterfaces) > 1,
		Pending: changedIn(active, draft, networkFields...),
	}
	if d := draft.DHCP; d != nil {
		n.DHCP = &DHCP{Enabled: d.Enabled, RangeStart: d.RangeStart, RangeEnd: d.RangeEnd, LeaseTime: d.LeaseTime}
	}
	return n
}

func (a *API) networksView() networksResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	return networksResponse{Networks: []Network{lanNetwork(active, draft)}, Editable: a.editable()}
}

// HandleNetworks: GET /api/v1/networks lists the networks.
func (a *API) HandleNetworks(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, a.networksView())
	}
}

// HandleNetwork: GET returns one network; PUT /api/v1/networks/{id} edits its address and DHCP.
func (a *API) HandleNetwork(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.PathValue("id") != lanID {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no network with this id"})
		return
	}

	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.networksView().Networks[0])
		return
	}
	a.putLAN(w, r)
}

type putNetworkRequest struct {
	IP      string `json:"ip"`
	Netmask string `json:"netmask"`
	// DHCP is replaced as a whole when given, and left alone when omitted
	DHCP *DHCP `json:"dhcp"`
}

func (a *API) putLAN(w http.ResponseWriter, r *http.Request) {
	if !a.requireEditable(w) {
		return
	}
	var req putNetworkRequest
	if !decode(w, r, &req) {
		return
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		c.LanIP = req.IP
		c.LanSubnet = req.Netmask
		if req.DHCP != nil {
			c.DHCP = &config.DHCPConfig{
				Enabled:    req.DHCP.Enabled,
				RangeStart: req.DHCP.RangeStart,
				RangeEnd:   req.DHCP.RangeEnd,
				LeaseTime:  req.DHCP.LeaseTime,
			}
		}

		// Only this page's own fields are checked here; the rest of the configuration (the WAN, say)
		// is checked when the draft is committed. A disabled DHCP server needs no range, so only the
		// fields that apply are reported.
		return ownedErrorsOrNil(c.Validate(), "lan_ip", "lan_subnet", "dhcp_range_start", "dhcp_range_end", "dhcp_lease_time")
	})

	var ve config.ValidationError
	if errors.As(err, &ve) {
		fieldErrors(w, http.StatusBadRequest, "These settings cannot be saved: "+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.networksView().Networks[0])
}

// ownedErrorsOrNil returns the owned validation problems as an error, or nil if there are none.
func ownedErrorsOrNil(err error, fields ...string) error {
	if own := ownedErrors(err, fields...); len(own) > 0 {
		return own
	}
	return nil
}

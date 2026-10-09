package netapi

import (
	"errors"
	"net/http"
	"strings"

	"peakroute/internal/config"
)

// Live is how the uplink's port is connected right now, as opposed to what the configuration
// asks for. With a DHCP uplink this is where the address the ISP handed out shows up.
type Live struct {
	Up      bool     `json:"up"`
	Address string   `json:"address"` // e.g. "203.0.113.10/24"; empty if the port has none
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

// Uplink is a connection to the internet. Today there is one (id "wan"); a second uplink would be
// another of these.
type Uplink struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Port    string   `json:"port"`    // the physical port it uses; chosen on the Ports page
	Mode    string   `json:"mode"`    // "dhcp" or "static"
	IP      string   `json:"ip"`      // static only
	Netmask string   `json:"netmask"` // static only
	Gateway string   `json:"gateway"` // static only
	DNS     []string `json:"dns"`     // static only
	Pending bool     `json:"pending"` // the draft differs from what is in effect
	Live    *Live    `json:"live,omitempty"`
}

type uplinksResponse struct {
	Uplinks  []Uplink `json:"uplinks"`
	Editable bool     `json:"editable"`
}

const wanID = "wan"

// The fields of the configuration an uplink owns (the file's own names).
var uplinkFields = []string{"wan_mode", "wan_ip", "wan_subnet", "wan_gateway", "wan_dns"}

func wanMode(c config.NetworkConfig) string {
	if strings.EqualFold(c.WanMode, "static") {
		return "static"
	}
	return "dhcp"
}

func wanUplink(active, draft config.NetworkConfig) Uplink {
	return Uplink{
		ID:      wanID,
		Name:    "Internet",
		Port:    draft.WanInterface,
		Mode:    wanMode(draft),
		IP:      draft.WanIP,
		Netmask: draft.WanSubnet,
		Gateway: draft.WanGateway,
		DNS:     append([]string{}, draft.WanDNS...),
		Pending: changedIn(active, draft, uplinkFields...),
	}
}

func (a *API) uplinksView() uplinksResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	u := wanUplink(active, draft)
	// Describe the connection that is in effect, which is the active configuration's port
	if a.Live != nil && active.WanInterface != "" {
		u.Live = a.Live(active.WanInterface)
	}
	return uplinksResponse{Uplinks: []Uplink{u}, Editable: a.editable()}
}

// HandleUplinks: GET /api/v1/uplinks lists the internet connections.
func (a *API) HandleUplinks(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, a.uplinksView())
	}
}

// HandleUplink: GET returns one uplink; PUT /api/v1/uplinks/{id} edits how it gets its address.
func (a *API) HandleUplink(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.PathValue("id") != wanID {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no uplink with this id"})
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.uplinksView().Uplinks[0])
		return
	}
	a.putWAN(w, r)
}

type putUplinkRequest struct {
	Mode    string   `json:"mode"`
	IP      string   `json:"ip"`
	Netmask string   `json:"netmask"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

func (a *API) putWAN(w http.ResponseWriter, r *http.Request) {
	if !a.requireEditable(w) {
		return
	}
	var req putUplinkRequest
	if !decode(w, r, &req) {
		return
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		mode := strings.ToLower(strings.TrimSpace(req.Mode))
		switch mode {
		case "dhcp":
			// The address, gateway and DNS come from the provider: nothing static is kept
			c.WanMode, c.WanIP, c.WanSubnet, c.WanGateway, c.WanDNS = "dhcp", "", "", "", nil
			return nil
		case "static":
			c.WanMode = "static"
			c.WanIP = strings.TrimSpace(req.IP)
			c.WanSubnet = strings.TrimSpace(req.Netmask)
			c.WanGateway = strings.TrimSpace(req.Gateway)
			c.WanDNS = nil
			for _, d := range req.DNS {
				if d = strings.TrimSpace(d); d != "" {
					c.WanDNS = append(c.WanDNS, d)
				}
			}
		default:
			return config.ValidationError{{Field: "mode", Message: "choose DHCP or a static address"}}
		}

		// Only this page's own fields are checked here. A fixed address also needs a gateway and
		// a DNS server to be useful, which the shared validation allows to be empty.
		problems := ownedErrors(c.Validate(), uplinkFields...)
		if c.WanGateway == "" {
			problems = append(problems, config.FieldError{Field: "gateway", Message: "required with a static address"})
		}
		if len(c.WanDNS) == 0 {
			problems = append(problems, config.FieldError{Field: "dns", Message: "enter at least one DNS server"})
		}
		if len(problems) > 0 {
			return problems
		}
		return nil
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
	writeJSON(w, http.StatusOK, a.uplinksView().Uplinks[0])
}

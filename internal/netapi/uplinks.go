package netapi

import (
	"errors"
	"net/http"
	"strings"

	"peakroute/internal/config"
)

// Live is how the uplink is connected right now, as opposed to what the configuration asks for.
// With a DHCP or PPPoE uplink this is where the address the ISP handed out shows up.
type Live struct {
	Up      bool     `json:"up"`
	Address string   `json:"address"` // e.g. "203.0.113.10/24"; empty if there is none
	Gateway string   `json:"gateway"` // with PPPoE: the provider's end of the link
	DNS     []string `json:"dns"`
	Device  string   `json:"device"` // the interface that carries the connection (eth0, eth0.835, ppp0)
	MAC     string   `json:"mac"`    // the hardware address the provider sees
	MTU     int      `json:"mtu"`
}

// PPPoE is the PPPoE login as it is shown: the password is never sent back.
type PPPoE struct {
	Username    string `json:"username"`
	Service     string `json:"service"`
	PasswordSet bool   `json:"password_set"`
}

// Uplink is a connection to the internet. Today there is one (id "wan"); a second uplink would be
// another of these.
type Uplink struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Port     string   `json:"port"`      // the physical port it uses; chosen on the Ports page
	Mode     string   `json:"mode"`      // "dhcp", "static" or "pppoe"
	IP       string   `json:"ip"`        // static only
	Netmask  string   `json:"netmask"`   // static only
	Gateway  string   `json:"gateway"`   // static only
	DNS      []string `json:"dns"`       // static: the servers; DHCP and PPPoE: servers used instead of the provider's
	Vlan     int      `json:"vlan"`      // 0: none
	MTU      int      `json:"mtu"`       // 0: the usual one for the type
	UsualMTU int      `json:"usual_mtu"` // what 0 means for this type
	MAC      string   `json:"mac"`       // "": the port's own address
	Hostname string   `json:"hostname"`  // sent to the provider's DHCP server; DHCP only
	PPPoE    *PPPoE   `json:"pppoe"`     // PPPoE only
	Device   string   `json:"device"`    // the interface that will carry the connection
	Pending  bool     `json:"pending"`   // the draft differs from what is in effect
	Live     *Live    `json:"live,omitempty"`
}

type uplinksResponse struct {
	Uplinks  []Uplink `json:"uplinks"`
	Editable bool     `json:"editable"`
}

const wanID = "wan"

// The fields of the configuration an uplink owns (the file's own names).
var uplinkFields = []string{"wan_mode", "wan_ip", "wan_subnet", "wan_gateway", "wan_dns",
	"wan_vlan", "wan_mtu", "wan_mac", "wan_hostname", "wan_pppoe", "pppoe_username", "pppoe_password", "pppoe_service"}

func wanMode(c config.NetworkConfig) string { return c.WanKind() }

func wanUplink(active, draft config.NetworkConfig) Uplink {
	u := Uplink{
		ID:       wanID,
		Name:     "Internet",
		Port:     draft.WanInterface,
		Mode:     wanMode(draft),
		IP:       draft.WanIP,
		Netmask:  draft.WanSubnet,
		Gateway:  draft.WanGateway,
		DNS:      append([]string{}, draft.WanDNS...),
		Vlan:     draft.WanVlan,
		MTU:      draft.WanMTU,
		UsualMTU: draft.UsualWanMTU(),
		MAC:      draft.WanMAC,
		Hostname: draft.WanHostname,
		Device:   draft.WanDevice(),
		Pending:  changedIn(active, draft, uplinkFields...),
	}
	if p := draft.WanPPPoE; p != nil {
		u.PPPoE = &PPPoE{Username: p.Username, Service: p.Service, PasswordSet: p.Password != ""}
	}
	return u
}

func (a *API) uplinksView() uplinksResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	u := wanUplink(active, draft)
	// Describe the connection that is in effect, which is the active configuration's
	if a.Live != nil && active.WanInterface != "" {
		u.Live = a.Live(active)
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

type putPPPoERequest struct {
	Username *string `json:"username"`
	Password *string `json:"password"` // left out: the saved one stays
	Service  *string `json:"service"`
}

// putUplinkRequest changes how the connection is made. What is left out stays as it is, except for the
// addressing of the type chosen: a static address needs its own four fields, DHCP and PPPoE drop them.
type putUplinkRequest struct {
	Mode     string           `json:"mode"`
	IP       string           `json:"ip"`
	Netmask  string           `json:"netmask"`
	Gateway  string           `json:"gateway"`
	DNS      []string         `json:"dns"` // static: required; DHCP and PPPoE: replaces the provider's servers (an empty list clears them)
	Vlan     *int             `json:"vlan"`
	MTU      *int             `json:"mtu"`
	MAC      *string          `json:"mac"`
	Hostname *string          `json:"hostname"`
	PPPoE    *putPPPoERequest `json:"pppoe"`
}

func trimmedDNS(in []string) []string {
	var out []string
	for _, d := range in {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
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
		before := c.WanKind()
		switch mode {
		case config.WanDHCP, config.WanStatic, config.WanPPPoE:
		default:
			return config.ValidationError{{Field: "mode", Message: "choose DHCP, a static address or PPPoE"}}
		}
		c.WanMode = mode

		// the carrier settings, when sent
		if req.Vlan != nil {
			c.WanVlan = *req.Vlan
		}
		if req.MTU != nil {
			c.WanMTU = *req.MTU
		}
		if req.MAC != nil {
			c.WanMAC = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(*req.MAC, "-", ":")))
		}
		if req.Hostname != nil {
			c.WanHostname = strings.TrimSpace(*req.Hostname)
		}

		// addressing
		switch mode {
		case config.WanStatic:
			c.WanIP = strings.TrimSpace(req.IP)
			c.WanSubnet = strings.TrimSpace(req.Netmask)
			c.WanGateway = strings.TrimSpace(req.Gateway)
			c.WanDNS = trimmedDNS(req.DNS)
			c.WanHostname = "" // only DHCP sends one
			c.WanPPPoE = nil
		default:
			// the address, gateway and mask come from the provider: nothing static is kept
			c.WanIP, c.WanSubnet, c.WanGateway = "", "", ""
			switch {
			case req.DNS != nil:
				c.WanDNS = trimmedDNS(req.DNS)
			case before == config.WanStatic:
				c.WanDNS = nil // the static servers were for the static address
			}
			if mode == config.WanPPPoE {
				c.WanHostname = ""
				p := config.PPPoEConfig{}
				if c.WanPPPoE != nil {
					p = *c.WanPPPoE
				}
				if q := req.PPPoE; q != nil {
					if q.Username != nil {
						p.Username = strings.TrimSpace(*q.Username)
					}
					if q.Password != nil {
						p.Password = *q.Password
					}
					if q.Service != nil {
						p.Service = strings.TrimSpace(*q.Service)
					}
				}
				c.WanPPPoE = &p
			} else {
				c.WanPPPoE = nil
			}
		}

		// Only this page's own fields are checked here. A fixed address also needs a gateway and
		// a DNS server to be useful, which the shared validation allows to be empty.
		problems := ownedErrors(c.Validate(), uplinkFields...)
		if mode == config.WanStatic {
			if c.WanGateway == "" {
				problems = append(problems, config.FieldError{Field: "gateway", Message: "required with a static address"})
			}
			if len(c.WanDNS) == 0 {
				problems = append(problems, config.FieldError{Field: "dns", Message: "enter at least one DNS server"})
			}
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

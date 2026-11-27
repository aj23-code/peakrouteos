package netapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

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

// ProbeInfo is how a connection is watched: the addresses added by the administrator, whether the public ones are used too, and the
// timing. The defaults are what an empty field means.
type ProbeInfo struct {
	Targets          []string `json:"targets"`
	PublicFallbacks  bool     `json:"public_fallbacks"`
	EverySeconds     int      `json:"every_seconds"`
	DownAfter        int      `json:"down_after"`
	PublicTargets    []string `json:"public_targets"` // what the public fallbacks are
	DefaultEvery     int      `json:"default_every_seconds"`
	DefaultDownAfter int      `json:"default_down_after"`
}

func probeInfo(c config.NetworkConfig) ProbeInfo {
	set := c.ProbeSettings()
	info := ProbeInfo{Targets: []string{}, PublicFallbacks: set.Fallbacks, EverySeconds: int(set.Every / time.Second), DownAfter: set.DownAfter,
		PublicTargets: append([]string{}, config.PublicProbeTargets...), DefaultEvery: config.DefaultProbeEvery, DefaultDownAfter: config.DefaultDownAfter}
	for _, a := range set.Targets {
		info.Targets = append(info.Targets, a.String())
	}
	return info
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
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Port      string    `json:"port"`      // the physical port it uses; chosen on the Ports page
	Mode      string    `json:"mode"`      // "dhcp", "static" or "pppoe"
	IP        string    `json:"ip"`        // static only
	Netmask   string    `json:"netmask"`   // static only
	Gateway   string    `json:"gateway"`   // static only
	DNS       []string  `json:"dns"`       // static: the servers; DHCP and PPPoE: servers used instead of the provider's
	Vlan      int       `json:"vlan"`      // 0: none
	MTU       int       `json:"mtu"`       // 0: the usual one for the type
	UsualMTU  int       `json:"usual_mtu"` // what 0 means for this type
	MAC       string    `json:"mac"`       // "": the port's own address
	PPPoE     *PPPoE    `json:"pppoe"`     // PPPoE only
	Device    string    `json:"device"`    // the interface that will carry the connection
	Pending   bool      `json:"pending"`   // the draft differs from what is in effect
	Preferred bool      `json:"preferred"` // the traffic prefers this connection (with two, the other is the one it falls back to)
	Probe     ProbeInfo `json:"probe"`     // how the connection is watched
	Live      *Live     `json:"live,omitempty"`
}

type uplinksResponse struct {
	Uplinks  []Uplink `json:"uplinks"`
	Editable bool     `json:"editable"`
}

const wanID = config.WanFirst

// The fields of the configuration an uplink owns (the file's own names): the first connection's, and the second's.
var uplinkFields = []string{"wan_probe", "wan_mode", "wan_ip", "wan_subnet", "wan_gateway", "wan_dns",
	"wan_vlan", "wan_mtu", "wan_mac", "wan_pppoe", "pppoe_username", "pppoe_password", "pppoe_service"}

var uplink2Fields = []string{"wan2.probe", "wan2", "wan2.mode", "wan2.ip", "wan2.subnet", "wan2.gateway", "wan2.dns", "wan2.vlan", "wan2.mtu", "wan2.mac",
	"wan2.pppoe_username", "wan2.pppoe_password", "wan2.pppoe_service", "wan2.pppoe", "wan2.interface"}

func ownFields(id string) []string {
	if id == config.WanSecond {
		return uplink2Fields
	}
	return uplinkFields
}

func wanMode(c config.NetworkConfig) string { return c.WanKind() }

// wanUplink describes one connection (id) as the draft has it; active is what is in effect.
func wanUplink(active, draft config.NetworkConfig, id string) (Uplink, bool) {
	v, ok := draft.WanView(id)
	if !ok {
		return Uplink{}, false
	}
	u := Uplink{
		ID:        id,
		Name:      "Internet",
		Port:      v.WanInterface,
		Mode:      wanMode(v),
		IP:        v.WanIP,
		Netmask:   v.WanSubnet,
		Gateway:   v.WanGateway,
		DNS:       append([]string{}, v.WanDNS...),
		Vlan:      v.WanVlan,
		MTU:       v.WanMTU,
		UsualMTU:  v.UsualWanMTU(),
		MAC:       v.WanMAC,
		Device:    v.WanDevice(),
		Pending:   changedIn(active, draft, append(append([]string{}, ownFields(id)...), "wan_preferred")...),
		Preferred: draft.WanIsPreferred(id),
		Probe:     probeInfo(v),
	}
	if id == config.WanSecond {
		u.Name = "Internet 2"
	}
	if p := v.WanPPPoE; p != nil {
		u.PPPoE = &PPPoE{Username: p.Username, Service: p.Service, PasswordSet: p.Password != ""}
	}
	return u, true
}

func (a *API) uplinksView() uplinksResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	var list []Uplink
	for _, id := range []string{config.WanFirst, config.WanSecond} {
		u, ok := wanUplink(active, draft, id)
		if !ok {
			continue
		}
		// Describe the connection that is in effect, which is the active configuration's
		if av, ok := active.WanView(id); ok && a.Live != nil && av.WanInterface != "" {
			u.Live = a.Live(av)
		}
		list = append(list, u)
	}
	return uplinksResponse{Uplinks: list, Editable: a.editable()}
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
	id := r.PathValue("id")
	if id != config.WanFirst && id != config.WanSecond {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no uplink with this id"})
		return
	}
	if r.Method == http.MethodGet {
		for _, u := range a.uplinksView().Uplinks {
			if u.ID == id {
				writeJSON(w, http.StatusOK, u)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no second internet connection: give a port the role WAN 2 on the Ports page"})
		return
	}
	a.putWAN(w, r, id)
}

type putPPPoERequest struct {
	Username *string `json:"username"`
	Password *string `json:"password"` // left out: the saved one stays
	Service  *string `json:"service"`
}

// putUplinkRequest changes how the connection is made. What is left out stays as it is, except for the
// addressing of the type chosen: a static address needs its own four fields, DHCP and PPPoE drop them.
type putUplinkRequest struct {
	Mode    string           `json:"mode"`
	IP      string           `json:"ip"`
	Netmask string           `json:"netmask"`
	Gateway string           `json:"gateway"`
	DNS     []string         `json:"dns"` // static: required; DHCP and PPPoE: replaces the provider's servers (an empty list clears them)
	Vlan    *int             `json:"vlan"`
	MTU     *int             `json:"mtu"`
	MAC     *string          `json:"mac"`
	PPPoE   *putPPPoERequest `json:"pppoe"`
	// Probe changes how the connection is watched; what is left out stays.
	Probe *putProbeRequest `json:"probe"`
	// Preferred makes this connection the one the traffic prefers (with two connections: the other is then the one it falls back to).
	Preferred *bool `json:"preferred"`
}

type putProbeRequest struct {
	Targets         *[]string `json:"targets"`
	PublicFallbacks *bool     `json:"public_fallbacks"`
	EverySeconds    *int      `json:"every_seconds"`
	DownAfter       *int      `json:"down_after"`
}

// applyProbe changes a connection's health check settings as asked; a value that is the default is not stored.
func applyProbe(cur *config.ProbeConfig, req *putProbeRequest) *config.ProbeConfig {
	p := config.ProbeConfig{}
	if cur != nil {
		p = *cur
		p.Targets = append([]string(nil), cur.Targets...)
	}
	if req != nil {
		if req.Targets != nil {
			p.Targets = trimmedDNS(*req.Targets)
		}
		if req.PublicFallbacks != nil {
			p.NoFallbacks = !*req.PublicFallbacks
		}
		if req.EverySeconds != nil {
			p.Every = *req.EverySeconds
			if p.Every == config.DefaultProbeEvery {
				p.Every = 0
			}
		}
		if req.DownAfter != nil {
			p.DownAfter = *req.DownAfter
			if p.DownAfter == config.DefaultDownAfter {
				p.DownAfter = 0
			}
		}
	}
	if len(p.Targets) == 0 && !p.NoFallbacks && p.Every == 0 && p.DownAfter == 0 {
		return nil
	}
	return &p
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

func (a *API) putWAN(w http.ResponseWriter, r *http.Request, id string) {
	if !a.requireEditable(w) {
		return
	}
	var req putUplinkRequest
	if !decode(w, r, &req) {
		return
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if id == config.WanSecond && !c.HasWan2() {
			return config.ValidationError{{Field: "ports", Message: "there is no second internet connection: give a port the role WAN 2 on the Ports page"}}
		}
		s := c.WanSettings(id)
		mode := strings.ToLower(strings.TrimSpace(req.Mode))
		before := config.NetworkConfig{WanMode: s.Mode}.WanKind()
		switch mode {
		case config.WanDHCP, config.WanStatic, config.WanPPPoE:
		default:
			return config.ValidationError{{Field: "mode", Message: "choose DHCP, a static address or PPPoE"}}
		}
		s.Mode = mode

		// the carrier settings, when sent
		if req.Vlan != nil {
			s.Vlan = *req.Vlan
		}
		if req.MTU != nil {
			s.MTU = *req.MTU
		}
		if req.MAC != nil {
			s.MAC = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(*req.MAC, "-", ":")))
		}

		// addressing
		switch mode {
		case config.WanStatic:
			s.IP = strings.TrimSpace(req.IP)
			s.Subnet = strings.TrimSpace(req.Netmask)
			s.Gateway = strings.TrimSpace(req.Gateway)
			s.DNS = trimmedDNS(req.DNS)
			s.PPPoE = nil
		default:
			// the address, gateway and mask come from the provider: nothing static is kept
			s.IP, s.Subnet, s.Gateway = "", "", ""
			switch {
			case req.DNS != nil:
				s.DNS = trimmedDNS(req.DNS)
			case before == config.WanStatic:
				s.DNS = nil // the static servers were for the static address
			}
			if mode == config.WanPPPoE {
				p := config.PPPoEConfig{}
				if s.PPPoE != nil {
					p = *s.PPPoE
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
				s.PPPoE = &p
			} else {
				s.PPPoE = nil
			}
		}
		s.Probe = applyProbe(s.Probe, req.Probe)
		c.SetWanSettings(id, s)

		// which connection the traffic prefers (only with two)
		if req.Preferred != nil && *req.Preferred && c.HasWan2() {
			c.WanPreferred = ""
			if id == config.WanSecond {
				c.WanPreferred = config.WanSecond
			}
		}

		// Only this page's own fields are checked here. A fixed address also needs a gateway and
		// a DNS server to be useful, which the shared validation allows to be empty.
		problems := ownedErrors(c.Validate(), append(append([]string{}, ownFields(id)...), "wan_preferred")...)
		if mode == config.WanStatic {
			if s.Gateway == "" {
				problems = append(problems, config.FieldError{Field: "gateway", Message: "required with a static address"})
			}
			if len(s.DNS) == 0 {
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
	for _, u := range a.uplinksView().Uplinks {
		if u.ID == id {
			writeJSON(w, http.StatusOK, u)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such internet connection"})
}

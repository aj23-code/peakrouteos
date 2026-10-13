package netapi

import (
	"errors"
	"net/http"
	"strings"

	"peakroute/internal/config"
)

// Access point mode (docs/access-point.md): one setting says whether the device is a router or an access point, and what the access
// point's network looks like. Like everything else it is a draft until it is committed.

// APPort is a cable port that can join the access point's bridge.
type APPort struct {
	Name  string `json:"name"`
	MAC   string `json:"mac"`
	State string `json:"state"`
}

// APSetup is the access point's network.
type APSetup struct {
	Ports   []string `json:"ports"`
	Mode    string   `json:"mode"` // "dhcp" or "static"
	IP      string   `json:"ip"`
	Netmask string   `json:"netmask"`
	Gateway string   `json:"gateway"`
	DNS     []string `json:"dns"`
}

type apResponse struct {
	// Mode is what the draft says ("router" or "ap"); ActiveMode is what the device is now.
	Mode       string `json:"mode"`
	ActiveMode string `json:"active_mode"`
	// ShownMode is what the pages show the device as ("AP" or "ROUTER"): a page that shows another one loads again.
	ShownMode string   `json:"shown_mode"`
	AP        APSetup  `json:"ap"`
	Ports     []APPort `json:"ports"` // the cable ports of this machine
	Recovery  string   `json:"recovery_address"`
	Pending   bool     `json:"pending"`
	Editable  bool     `json:"editable"`
}

func modeWord(c config.NetworkConfig) string {
	if c.IsAP() {
		return "ap"
	}
	return "router"
}

func apSetupOf(c config.NetworkConfig) APSetup {
	ap := c.APSettings()
	s := APSetup{Ports: append([]string{}, ap.Ports...), Mode: ap.Mode, IP: ap.IP, Netmask: ap.Netmask, Gateway: ap.Gateway, DNS: append([]string{}, ap.DNS...)}
	if s.Mode == "" {
		s.Mode = config.APDHCP
	}
	return s
}

func (a *API) wiredPorts() []config.SystemInterface {
	if a.Ports == nil {
		return nil
	}
	all, err := a.Ports()
	if err != nil {
		return nil
	}
	var out []config.SystemInterface
	for _, p := range all {
		if !p.Wireless {
			out = append(out, p)
		}
	}
	return out
}

func (a *API) apView() apResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	staged, hasDraft := a.Manager.StagedNetworkConfig()
	resp := apResponse{ShownMode: config.ShownMode(active, staged, hasDraft), Mode: modeWord(draft), ActiveMode: modeWord(active), AP: apSetupOf(draft), Ports: []APPort{},
		Recovery: config.APRecoveryAddress, Pending: modeWord(draft) != modeWord(active) || apSetupOf(draft).Mode != apSetupOf(active).Mode ||
			strings.Join(draft.APSettings().Ports, ",") != strings.Join(active.APSettings().Ports, ",") || draft.APSettings().IP != active.APSettings().IP, Editable: a.editable()}
	for _, p := range a.wiredPorts() {
		resp.Ports = append(resp.Ports, APPort{Name: p.Name, MAC: p.MAC, State: p.State})
	}
	return resp
}

// HandleAP: GET /api/v1/ap returns the mode and the access point's network (the draft).
func (a *API) HandleAP(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, a.apView())
	}
}

type apRequest struct {
	Mode    *string  `json:"mode"` // "router" or "ap"
	Ports   []string `json:"ports"`
	APMode  *string  `json:"address_mode"`
	IP      *string  `json:"ip"`
	Netmask *string  `json:"netmask"`
	Gateway *string  `json:"gateway"`
	DNS     []string `json:"dns"`
}

// routerFill gives a device that has never been a router what the first-run wizard would: the first cable port is the internet
// connection and the others are the local network, with the usual address and DHCP range.
func routerFill(c *config.NetworkConfig, wired []config.SystemInterface) error {
	if c.WanInterface == "" {
		if len(wired) < 2 {
			return config.ValidationError{{Field: "mode", Message: "a router needs two cable ports (one for the internet, one for the local network)"}}
		}
		c.WanInterface = wired[0].Name
		c.LanInterfaces = nil
		for _, p := range wired[1:] {
			c.LanInterfaces = append(c.LanInterfaces, p.Name)
		}
	}
	if c.WanMode == "" {
		c.WanMode = "dhcp"
	}
	if c.LanIP == "" {
		c.LanIP, c.LanSubnet = config.DefaultLanIP, config.DefaultLanNetmask
	}
	if c.DHCP == nil {
		c.DHCP = config.DefaultDHCP(c.LanIP, c.LanSubnet)
	}
	return nil
}

// HandleAPSet: PUT /api/v1/ap switches the mode and sets the access point's network.
func (a *API) HandleAPSet(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) || !a.requireEditable(w) {
		return
	}
	var req apRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Mode != nil && *req.Mode != "router" && *req.Mode != "ap" {
		fieldErrors(w, http.StatusBadRequest, "Choose router or access point.", []config.FieldError{{Field: "mode", Message: `must be "router" or "ap"`}})
		return
	}
	wired := a.wiredPorts()
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.AP == nil {
			c.AP = &config.APConfig{}
		}
		ap := c.AP
		if req.Ports != nil {
			ap.Ports = nil
			for _, p := range req.Ports {
				ap.Ports = append(ap.Ports, strings.TrimSpace(p))
			}
		}
		if req.APMode != nil {
			ap.Mode = strings.TrimSpace(*req.APMode)
		}
		if req.IP != nil {
			ap.IP = strings.TrimSpace(*req.IP)
		}
		if req.Netmask != nil {
			ap.Netmask = strings.TrimSpace(*req.Netmask)
		}
		if req.Gateway != nil {
			ap.Gateway = strings.TrimSpace(*req.Gateway)
		}
		if req.DNS != nil {
			ap.DNS = nil
			for _, s := range req.DNS {
				if s = strings.TrimSpace(s); s != "" {
					ap.DNS = append(ap.DNS, s)
				}
			}
		}
		if req.Mode != nil {
			ap.Enabled = *req.Mode == "ap"
		}
		switch {
		case ap.Enabled && len(ap.Ports) == 0 && req.Ports == nil:
			// becoming an access point: every cable port joins the bridge
			for _, p := range wired {
				ap.Ports = append(ap.Ports, p.Name)
			}
		case !ap.Enabled && req.Mode != nil:
			if err := routerFill(c, wired); err != nil {
				return err
			}
		}
		if ap.Enabled {
			// a Wi-Fi uplink means nothing to an access point, and the router's Wi-Fi links keep their place
			return errorsForAP(c.Validate())
		}
		return c.Validate()
	})
	a.answerStaged(w, err, http.StatusOK, "The mode cannot be changed like this: ", a.apView())
}

// errorsForAP keeps the problems an access point's own page can show: its ports, address and mode.
func errorsForAP(err error) error {
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out config.ValidationError
	for _, e := range ve {
		if strings.HasPrefix(e.Field, "ap.") {
			out = append(out, config.FieldError{Field: strings.TrimPrefix(e.Field, "ap."), Message: e.Message})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

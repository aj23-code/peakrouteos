package netapi

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"peakroute/internal/config"
)

// Reservation is an address kept for one device, as the API shows it. Network is the local network the address is in (empty when it is
// in none, which the commit will refuse).
type Reservation struct {
	MAC     string `json:"mac"`
	IP      string `json:"ip"`
	Name    string `json:"name"`
	Network string `json:"network"`
}

type reservationsResponse struct {
	Reservations []Reservation `json:"reservations"` // the draft
	Pending      bool          `json:"pending"`
	Editable     bool          `json:"editable"`
}

// networkNameOf says which local network an address is in ("LAN" for the default network), or "".
func networkNameOf(cfg config.NetworkConfig, ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	in := func(addr, mask string) bool {
		base, err := netip.ParseAddr(addr)
		if err != nil {
			return false
		}
		ones := 0
		m, err := netip.ParseAddr(mask)
		if err != nil {
			return false
		}
		for _, b := range m.As4() {
			for ; b&0x80 != 0; b <<= 1 {
				ones++
			}
		}
		return netip.PrefixFrom(base, ones).Masked().Contains(a)
	}
	if in(cfg.LanIP, cfg.LanSubnet) {
		return "LAN"
	}
	for _, n := range cfg.Networks {
		if in(n.IP, n.Netmask) {
			return n.Name
		}
	}
	return ""
}

func (a *API) reservationsView() reservationsResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	resp := reservationsResponse{Reservations: []Reservation{}, Pending: changedIn(active, draft, "reservations"), Editable: a.editable()}
	for _, r := range draft.Reservations {
		resp.Reservations = append(resp.Reservations, Reservation{MAC: r.MAC, IP: r.IP, Name: r.Name, Network: networkNameOf(draft, r.IP)})
	}
	return resp
}

// HandleReservations: GET /api/v1/dhcp/reservations returns the reserved addresses (the draft); PUT replaces the whole list in the draft.
func (a *API) HandleReservations(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.reservationsView())
		return
	}
	a.putReservations(w, r)
}

type putReservationsRequest struct {
	Reservations []struct {
		MAC  string `json:"mac"`
		IP   string `json:"ip"`
		Name string `json:"name"`
	} `json:"reservations"`
}

func (a *API) putReservations(w http.ResponseWriter, r *http.Request) {
	if !a.requireEditable(w) {
		return
	}
	var req putReservationsRequest
	if !decode(w, r, &req) {
		return
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		c.Reservations = nil
		for _, in := range req.Reservations {
			mac := strings.TrimSpace(in.MAC)
			if m, ok := config.NormalizeMAC(mac); ok {
				mac = m // written the one way, so that the same device is not listed twice under two spellings
			}
			c.Reservations = append(c.Reservations, config.Reservation{MAC: mac, IP: strings.TrimSpace(in.IP), Name: strings.TrimSpace(in.Name)})
		}
		// Only the reservations are judged here; the rest of the configuration is checked at commit
		return ownedErrorsOrNil(c.Validate(), "reservations")
	})

	var ve config.ValidationError
	if errors.As(err, &ve) {
		fieldErrors(w, http.StatusBadRequest, "These reserved addresses cannot be saved: "+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.reservationsView())
}

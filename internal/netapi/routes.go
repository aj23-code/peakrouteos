package netapi

import (
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"peakroute/internal/config"
	"peakroute/internal/network"
)

// Route is a static route as the API shows it. Enabled is the other way round from the file's
// "disabled", so a person reads it the way it is meant.
type Route struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	Gateway     string `json:"gateway"`
	Metric      int    `json:"metric"` // 0 uses the default
	Enabled     bool   `json:"enabled"`
}

// Kinds of entries in the routing table.
const (
	KindDefault   = "default"   // where everything else goes
	KindConnected = "connected" // a network this router is directly on
	KindVia       = "via"       // a network reached through a gateway, set up by something else
	KindStatic    = "static"    // one of the routes configured here
)

// LiveRoute is one entry of the routing table the kernel is using right now.
type LiveRoute struct {
	Destination string `json:"destination"` // CIDR
	Gateway     string `json:"gateway"`     // empty for a connected network
	Interface   string `json:"interface"`
	Metric      int    `json:"metric"`
	Kind        string `json:"kind"`
}

type routesResponse struct {
	Routes   []Route     `json:"routes"` // the draft
	Pending  bool        `json:"pending"`
	Editable bool        `json:"editable"`
	Table    []LiveRoute `json:"table"` // what is in effect now
}

func toAPI(r config.Route) Route {
	return Route{Name: r.Name, Destination: r.Destination, Gateway: r.Gateway, Metric: r.Metric, Enabled: !r.Disabled}
}

func (a *API) routesView() routesResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()

	resp := routesResponse{Routes: []Route{}, Pending: changedIn(active, draft, "routes"), Editable: a.editable(), Table: []LiveRoute{}}
	for _, r := range draft.Routes {
		resp.Routes = append(resp.Routes, toAPI(r))
	}

	if a.RoutingTable != nil {
		ours := map[string]bool{} // the routes in effect that this page manages
		for _, r := range active.Routes {
			if !r.Disabled {
				ours[strings.TrimSpace(r.Destination)+"|"+r.Gateway] = true
			}
		}
		for _, lr := range a.RoutingTable() {
			if lr.Kind == KindVia && ours[lr.Destination+"|"+lr.Gateway] {
				lr.Kind = KindStatic
			}
			resp.Table = append(resp.Table, lr)
		}
	}
	return resp
}

// HandleRoutes: GET /api/v1/routes returns the static routes (the draft) and the routing table in
// effect; PUT replaces the whole list of static routes in the draft.
func (a *API) HandleRoutes(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.routesView())
		return
	}
	a.putRoutes(w, r)
}

type putRoutesRequest struct {
	Routes []struct {
		Name        string `json:"name"`
		Destination string `json:"destination"`
		Gateway     string `json:"gateway"`
		Metric      int    `json:"metric"`
		Enabled     *bool  `json:"enabled"` // omitted means on
	} `json:"routes"`
}

func (a *API) putRoutes(w http.ResponseWriter, r *http.Request) {
	if !a.requireEditable(w) {
		return
	}
	var req putRoutesRequest
	if !decode(w, r, &req) {
		return
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		c.Routes = nil
		for _, in := range req.Routes {
			c.Routes = append(c.Routes, config.Route{
				Name:        strings.TrimSpace(in.Name),
				Destination: strings.TrimSpace(in.Destination),
				Gateway:     strings.TrimSpace(in.Gateway),
				Metric:      in.Metric,
				Disabled:    in.Enabled != nil && !*in.Enabled,
			})
		}
		// Only the routes are judged here; the rest of the configuration is checked at commit
		return ownedErrorsOrNil(c.Validate(), "routes")
	})

	var ve config.ValidationError
	if errors.As(err, &ve) {
		fieldErrors(w, http.StatusBadRequest, "These routes cannot be saved: "+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.routesView())
}

// ClassifyRoutes turns the kernel's routing table into what the page shows: the default route
// first, then the networks this router is directly on, then the routes through a gateway, each
// group in address order.
func ClassifyRoutes(in []network.KernelRoute) []LiveRoute {
	out := make([]LiveRoute, 0, len(in))
	for _, k := range in {
		kind := KindVia
		switch {
		case k.Destination == "0.0.0.0/0":
			kind = KindDefault
		case k.Gateway == "":
			kind = KindConnected
		}
		out = append(out, LiveRoute{Destination: k.Destination, Gateway: k.Gateway, Interface: k.Interface, Metric: k.Metric, Kind: kind})
	}

	rank := map[string]int{KindDefault: 0, KindConnected: 1, KindVia: 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Kind] != rank[out[j].Kind] {
			return rank[out[i].Kind] < rank[out[j].Kind]
		}
		pi, _ := netip.ParsePrefix(out[i].Destination)
		pj, _ := netip.ParsePrefix(out[j].Destination)
		if pi.Addr() != pj.Addr() {
			return pi.Addr().Less(pj.Addr())
		}
		if pi.Bits() != pj.Bits() {
			return pi.Bits() < pj.Bits()
		}
		return out[i].Metric < out[j].Metric
	})
	return out
}

package netapi

import (
	"context"
	"errors"
	"net/http"

	"peakroute/internal/config"
	"peakroute/internal/lldp"
)

// Neighbours of the router's own ports (docs/device-pages.md, phase 2): the switch or access point at the other end of each cable.

type lldpResponse struct {
	Enabled    bool             `json:"enabled"`   // the draft says LLDP is on
	Active     bool             `json:"active"`    // it is on in the configuration in effect
	Pending    bool             `json:"pending"`   // the draft differs
	Running    bool             `json:"running"`   // lldpd answered just now
	Available  bool             `json:"available"` // this system can ask (nil on a development machine)
	Ports      []string         `json:"ports"`     // the LAN ports it listens on
	Neighbours []lldp.Neighbour `json:"neighbours"`
	Editable   bool             `json:"editable"`
}

func (a *API) lldpView(ctx context.Context) lldpResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	out := lldpResponse{Enabled: draft.LLDP, Active: active.LLDP, Pending: draft.LLDP != active.LLDP, Available: a.LLDPNeighbours != nil,
		Ports: append([]string{}, draft.LanInterfaces...), Neighbours: []lldp.Neighbour{}, Editable: a.editable()}
	if active.LLDP && a.LLDPNeighbours != nil {
		if ns, err := a.LLDPNeighbours(ctx); err == nil {
			out.Running = true
			if ns != nil {
				out.Neighbours = ns
			}
		}
	}
	return out
}

// HandleLLDP: GET /api/v1/lldp shows the switch and what lldpd has heard; PUT {"enabled": true|false} switches it (in the draft).
func (a *API) HandleLLDP(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.lldpView(r.Context()))
		return
	}
	if !a.requireEditable(w) {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Enabled == nil {
		fieldErrors(w, http.StatusBadRequest, "Say whether it is on.", []config.FieldError{{Field: "enabled", Message: "required"}})
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		c.LLDP = *req.Enabled
		return nil
	})
	var ve config.ValidationError
	if errors.As(err, &ve) {
		fieldErrors(w, http.StatusBadRequest, "This cannot be saved: "+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.lldpView(r.Context()))
}

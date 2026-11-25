package netapi

import (
	"context"
	"net/http"

	"peakroute/internal/config"
	"peakroute/internal/filtering"
)

// The DNS filtering page's view (docs/dns-filtering.md). The switch itself is one per network (Local Networks); this says which
// networks filter now and in the draft, and whether AdGuard Home is there and answering.

// FilteringNetwork is one network and whether it filters.
type FilteringNetwork struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Filter  bool   `json:"filter"`  // in the draft
	Running bool   `json:"running"` // in effect now
}

type filteringResponse struct {
	filtering.Status
	Enabled  bool               `json:"enabled"` // a network filters now (the committed configuration)
	Pending  bool               `json:"pending"` // the draft differs
	Networks []FilteringNetwork `json:"networks"`
}

func (a *API) filteringView(ctx context.Context) filteringResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	out := filteringResponse{Enabled: active.FilterOn(), Pending: active.FilterOn() != draft.FilterOn(), Networks: []FilteringNetwork{}}
	out.Networks = append(out.Networks, FilteringNetwork{ID: config.DefaultNetworkID, Name: "LAN", Filter: draft.LanFilter, Running: active.LanFilter})
	for _, n := range draft.Networks {
		was, _ := active.FindNetwork(n.ID)
		out.Networks = append(out.Networks, FilteringNetwork{ID: n.ID, Name: n.Name, Filter: n.Filter, Running: was.Filter})
		if n.Filter != was.Filter {
			out.Pending = true
		}
	}
	if draft.LanFilter != active.LanFilter {
		out.Pending = true
	}
	if a.FilteringStatus != nil {
		out.Status = a.FilteringStatus(ctx)
	}
	return out
}

// HandleFiltering answers GET /api/v1/filtering.
func (a *API) HandleFiltering(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	writeJSON(w, http.StatusOK, a.filteringView(r.Context()))
}

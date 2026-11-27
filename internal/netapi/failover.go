package netapi

import (
	"errors"
	"net/http"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/failover"
)

// Failover between two internet connections (docs/wan-failover.md, section 5).

type failoverResponse struct {
	Available       bool              `json:"available"` // there are two connections
	Failback        bool              `json:"failback"`  // go back to the preferred connection when it is up again
	RecoverySeconds int               `json:"recovery_seconds"`
	DefaultRecovery int               `json:"default_recovery_seconds"`
	Balanced        bool              `json:"balanced"`     // the traffic is shared between the connections (not failover)
	WeightFirst     int               `json:"weight_first"` // the weights of the connections when it is shared
	WeightSecond    int               `json:"weight_second"`
	Shares          map[string]int    `json:"shares,omitempty"` // each connection's share of new connections now, in percent (only while shared)
	Pending         bool              `json:"pending"`
	Editable        bool              `json:"editable"`
	Names           map[string]string `json:"names"`
	Status          *failover.Status  `json:"status,omitempty"`
	RecoveringIn    int               `json:"recovering_in_seconds,omitempty"`
}

func (a *API) failoverView() failoverResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	set := draft.FailoverSettings()
	resp := failoverResponse{Available: draft.HasWan2(), Failback: set.Failback, RecoverySeconds: int(set.Recovery / time.Second),
		Balanced: set.Balanced, WeightFirst: set.Weights[config.WanFirst], WeightSecond: set.Weights[config.WanSecond],
		DefaultRecovery: config.DefaultRecoverySeconds, Pending: changedIn(active, draft, "wan_failover"), Editable: a.editable(),
		Names: map[string]string{config.WanFirst: "Internet", config.WanSecond: "Internet 2"}}
	if a.FailoverStatus != nil && active.HasWan2() {
		st, left := a.FailoverStatus(active)
		resp.Status, resp.RecoveringIn = &st, left
		if aset := active.FailoverSettings(); aset.Balanced {
			// the connections that are up share the new connections by weight
			total := 0
			for id, w := range aset.Weights {
				if st.States[id] != failover.StateOffline {
					total += w
				}
			}
			resp.Shares = map[string]int{}
			for id, w := range aset.Weights {
				if total > 0 && st.States[id] != failover.StateOffline {
					resp.Shares[id] = (w*100 + total/2) / total
				} else {
					resp.Shares[id] = 0
				}
			}
		}
	}
	return resp
}

type putFailoverRequest struct {
	Failback        *bool `json:"failback"`
	RecoverySeconds *int  `json:"recovery_seconds"`
	Balanced        *bool `json:"balanced"`
	WeightFirst     *int  `json:"weight_first"`
	WeightSecond    *int  `json:"weight_second"`
}

// HandleFailover: GET shows the failover settings and what is happening; PUT changes the settings (in the draft).
func (a *API) HandleFailover(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.failoverView())
		return
	}
	if !a.requireEditable(w) {
		return
	}
	var req putFailoverRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		f := config.FailoverConfig{}
		if c.WanFailover != nil {
			f = *c.WanFailover
		}
		if req.Failback != nil {
			f.NoFailback = !*req.Failback
		}
		if req.RecoverySeconds != nil {
			f.RecoverySeconds = *req.RecoverySeconds
			if f.RecoverySeconds == config.DefaultRecoverySeconds {
				f.RecoverySeconds = 0
			}
		}
		if req.Balanced != nil {
			f.Balanced = *req.Balanced
		}
		if req.WeightFirst != nil {
			f.WeightFirst = *req.WeightFirst
			if f.WeightFirst == config.DefaultWeight {
				f.WeightFirst = 0
			}
		}
		if req.WeightSecond != nil {
			f.WeightSecond = *req.WeightSecond
			if f.WeightSecond == config.DefaultWeight {
				f.WeightSecond = 0
			}
		}
		c.WanFailover = &f
		if f == (config.FailoverConfig{}) {
			c.WanFailover = nil
		}
		return ownedErrorsOrNil(c.Validate(), "wan_failover")
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
	writeJSON(w, http.StatusOK, a.failoverView())
}

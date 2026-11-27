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
		DefaultRecovery: config.DefaultRecoverySeconds, Pending: changedIn(active, draft, "wan_failover"), Editable: a.editable(),
		Names: map[string]string{config.WanFirst: "Internet", config.WanSecond: "Internet 2"}}
	if a.FailoverStatus != nil && active.HasWan2() {
		st, left := a.FailoverStatus(active)
		resp.Status, resp.RecoveringIn = &st, left
	}
	return resp
}

type putFailoverRequest struct {
	Failback        *bool `json:"failback"`
	RecoverySeconds *int  `json:"recovery_seconds"`
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
		c.WanFailover = &f
		if !f.NoFailback && f.RecoverySeconds == 0 {
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

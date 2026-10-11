package netapi

import (
	"net/http"
	"strings"

	"peakroute/internal/config"
)

// Settings are the router-wide choices that are not about one network or port.
type Settings struct {
	Country   string           `json:"country"`
	Pending   bool             `json:"country_pending"` // differs from the running configuration
	Countries []config.Country `json:"countries"`
	Editable  bool             `json:"editable"`
}

func (a *API) settingsView() Settings {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	return Settings{Country: draft.Country, Pending: draft.Country != active.Country, Countries: config.Countries, Editable: a.editable()}
}

// HandleSettings: GET /api/v1/settings returns the router-wide settings (the draft).
func (a *API) HandleSettings(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, a.settingsView())
	}
}

// HandleSettingsCountry: PUT /api/v1/settings/country {"country": "US"}. An empty string clears it; whether
// Wi-Fi may be switched on without one is decided when the change is committed.
func (a *API) HandleSettingsCountry(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) || !a.requireEditable(w) {
		return
	}
	var req struct {
		Country string `json:"country"`
	}
	if !decode(w, r, &req) {
		return
	}
	country := strings.TrimSpace(req.Country)
	if country != "" && !config.IsCountry(country) {
		fieldErrors(w, http.StatusBadRequest, "That is not a country code.", []config.FieldError{{Field: "country", Message: "choose a country from the list"}})
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		c.Country = country
		return nil
	})
	a.answerStaged(w, err, http.StatusOK, "", a.settingsView())
}

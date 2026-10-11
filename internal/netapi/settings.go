package netapi

import (
	"net/http"
	"strings"

	"peakroute/internal/config"
)

// Settings are the router-wide choices that are not about one network or port.
type Settings struct {
	Country       string           `json:"country"`
	Pending       bool             `json:"country_pending"` // differs from the running configuration
	Countries     []config.Country `json:"countries"`
	Domain        string           `json:"domain"` // the local domain; empty until one is set
	DomainPending bool             `json:"domain_pending"`
	Editable      bool             `json:"editable"`
}

func (a *API) settingsView() Settings {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	return Settings{Country: draft.Country, Pending: draft.Country != active.Country, Countries: config.Countries,
		Domain: draft.Domain, DomainPending: draft.Domain != active.Domain, Editable: a.editable()}
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

// HandleSettingsDomain: PUT /api/v1/settings/domain {"domain": "lan"} sets the router's local domain; an empty string
// clears it. It is kept in lower case.
func (a *API) HandleSettingsDomain(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) || !a.requireEditable(w) {
		return
	}
	var req struct {
		Domain string `json:"domain"`
	}
	if !decode(w, r, &req) {
		return
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if domain != "" && !config.ValidDomain(domain) {
		fieldErrors(w, http.StatusBadRequest, "That is not a usable domain name.", []config.FieldError{{Field: "domain",
			Message: "must be a domain name such as lan or home.example.com (not local, localhost, invalid, test or example)"}})
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		c.Domain = domain
		// a forwarded domain that would now be inside the local domain is refused too
		if errs := ownedErrors(c.Validate(), "domain", "dns"); len(errs) > 0 {
			return errs
		}
		return nil
	})
	a.answerStaged(w, err, http.StatusOK, "", a.settingsView())
}

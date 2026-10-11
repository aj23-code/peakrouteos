package netapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"peakroute/internal/config"
)

// Settings are the router-wide choices that are not about one network or port.
type Settings struct {
	Country         string            `json:"country"`
	Pending         bool              `json:"country_pending"` // differs from the running configuration
	Countries       []config.Country  `json:"countries"`
	Domain          string            `json:"domain"` // the local domain; empty until one is set
	DomainPending   bool              `json:"domain_pending"`
	Hostname        string            `json:"hostname"`      // the router's host name, written to the machine; empty: the machine's name is left as it is
	SendHostname    bool              `json:"send_hostname"` // also tell the provider's DHCP server (an internet connection with DHCP addressing only)
	HostnamePending bool              `json:"hostname_pending"`
	Timezone        string            `json:"timezone"` // "" : not set, the machine keeps its own
	Timezones       []config.Timezone `json:"timezones"`
	Suggested       []string          `json:"suggested_timezones"` // the zones of the chosen country
	NTPEnabled      bool              `json:"ntp_enabled"`
	NTPServers      []string          `json:"ntp_servers"` // the ones chosen; empty: the defaults
	NTPDefault      []string          `json:"ntp_default_servers"`
	TimePending     bool              `json:"time_pending"`
	Time            *TimeInfo         `json:"time"` // the clock as it is right now (nil: it cannot be read)
	Editable        bool              `json:"editable"`
}

// TimeInfo is the router's clock as the Settings page shows it.
type TimeInfo struct {
	Now          int64  `json:"now"`      // Unix milliseconds
	Timezone     string `json:"timezone"` // the zone the machine is in now
	NTPEnabled   bool   `json:"ntp_enabled"`
	Synchronized bool   `json:"synchronized"`
	Server       string `json:"server"`
}

func (a *API) timeInfo() *TimeInfo {
	if a.TimeStatus == nil {
		return nil
	}
	st, ok := a.TimeStatus(context.Background())
	if !ok {
		return nil
	}
	return &TimeInfo{Now: time.Now().UnixMilli(), Timezone: st.Timezone, NTPEnabled: st.NTPEnabled, Synchronized: st.Synchronized, Server: st.Server}
}

func (a *API) settingsView() Settings {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	return Settings{Timezone: draft.Timezone, Timezones: config.Timezones, Suggested: append([]string{}, config.TimezonesOf(draft.Country)...),
		NTPEnabled: !draft.NTPDisabled, NTPServers: append([]string{}, draft.NTPServers...), NTPDefault: config.DefaultNTPServers,
		TimePending: draft.Timezone != active.Timezone || draft.NTPDisabled != active.NTPDisabled || strings.Join(draft.NTPServers, ",") != strings.Join(active.NTPServers, ","),
		Time:        a.timeInfo(), Country: draft.Country, Pending: draft.Country != active.Country, Countries: config.Countries,
		Domain: draft.Domain, DomainPending: draft.Domain != active.Domain,
		Hostname: draft.Hostname, SendHostname: draft.SendHostname,
		HostnamePending: draft.Hostname != active.Hostname || draft.SendHostname != active.SendHostname, Editable: a.editable()}
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
		// a forwarded domain that would now be inside the local domain is refused too, said so that it can be found
		if c.DNS != nil && domain != "" {
			for _, f := range c.DNS.Forwards {
				if f.Domain == domain || strings.HasSuffix(f.Domain, "."+domain) {
					return config.ValidationError{{Field: "domain", Message: "would contain the forwarded domain " + f.Domain +
						", and the router answers a local domain itself, so that forwarding would never be used. Change or remove it on the DNS page first"}}
				}
			}
		}
		if errs := ownedErrors(c.Validate(), "domain", "dns"); len(errs) > 0 {
			return errs
		}
		return nil
	})
	a.answerStaged(w, err, http.StatusOK, "", a.settingsView())
}

// HandleSettingsHostname: PUT /api/v1/settings/hostname {"hostname": "my-router", "send_hostname": true} sets the router's
// host name, which is written to the machine, and whether the provider's DHCP server is told it (only an internet
// connection with DHCP addressing does; the others keep the setting). An empty name clears it (and, unless the request
// says otherwise, the sending); a field left out stays.
func (a *API) HandleSettingsHostname(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) || !a.requireEditable(w) {
		return
	}
	var req struct {
		Hostname *string `json:"hostname"`
		Send     *bool   `json:"send_hostname"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if req.Hostname != nil {
			c.Hostname = strings.TrimSpace(*req.Hostname)
			if c.Hostname == "" && req.Send == nil {
				c.SendHostname = false
			}
		}
		if req.Send != nil {
			c.SendHostname = *req.Send
		}
		if errs := ownedErrors(c.Validate(), "hostname", "send_hostname"); len(errs) > 0 {
			return errs
		}
		return nil
	})
	a.answerStaged(w, err, http.StatusOK, "", a.settingsView())
}

// HandleSettingsTime: PUT /api/v1/settings/time {"timezone": "Europe/Berlin", "ntp_enabled": true, "ntp_servers": ["pool.ntp.org"]}
// sets the time zone (empty: the machine keeps its own), whether the clock follows time servers, and which (an empty list: the
// default). A field left out stays.
func (a *API) HandleSettingsTime(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) || !a.requireEditable(w) {
		return
	}
	var req struct {
		Timezone *string   `json:"timezone"`
		Enabled  *bool     `json:"ntp_enabled"`
		Servers  *[]string `json:"ntp_servers"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if req.Timezone != nil {
			c.Timezone = strings.TrimSpace(*req.Timezone)
		}
		if req.Enabled != nil {
			c.NTPDisabled = !*req.Enabled
		}
		if req.Servers != nil {
			c.NTPServers = nil
			for _, s := range *req.Servers {
				if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
					c.NTPServers = append(c.NTPServers, s)
				}
			}
		}
		if errs := ownedErrors(c.Validate(), "timezone", "ntp_servers"); len(errs) > 0 {
			return errs
		}
		return nil
	})
	a.answerStaged(w, err, http.StatusOK, "", a.settingsView())
}

package netapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"peakroute/internal/config"
	"peakroute/internal/wireless"
)

// WifiRadio is how one adapter is used as an access point.
type WifiRadio struct {
	Name    string `json:"name"`
	Band    string `json:"band"`
	Channel int    `json:"channel"` // 0: automatic
	Width   int    `json:"width"`
	Enabled bool   `json:"enabled"`
	HT      bool   `json:"ht"`
	VHT     bool   `json:"vht"`
	HE      bool   `json:"he"`
	Pending bool   `json:"pending"`
}

// WifiNetwork is one SSID.
type WifiNetwork struct {
	ID              string   `json:"id"`
	SSID            string   `json:"ssid"`
	Passphrase      string   `json:"passphrase"`
	Network         string   `json:"network"` // the local network it joins
	Security        string   `json:"security"`
	Bands           []string `json:"bands"`
	BandSteering    bool     `json:"band_steering"`
	Hidden          bool     `json:"hidden"`
	ClientIsolation bool     `json:"client_isolation"`
	Enabled         bool     `json:"enabled"`
	Pending         bool     `json:"pending"`
}

type wifiResponse struct {
	Country        string           `json:"country"`
	CountryPending bool             `json:"country_pending"`
	Countries      []config.Country `json:"countries"`
	Radios         []WifiRadio      `json:"radios"`
	Networks       []WifiNetwork    `json:"networks"`
	Editable       bool             `json:"editable"`
}

func (a *API) wifiView() wifiResponse {
	active := a.Manager.NetworkConfig().WifiSettings()
	staged, _ := a.Manager.StagedNetworkConfig()
	draft := staged.WifiSettings()

	resp := wifiResponse{Country: draft.Country, CountryPending: draft.Country != active.Country, Countries: config.Countries,
		Radios: []WifiRadio{}, Networks: []WifiNetwork{}, Editable: a.editable()}

	oldRadios := map[string]config.WifiRadio{}
	for _, r := range active.Radios {
		oldRadios[r.Name] = r
	}
	for _, r := range draft.Radios {
		old, had := oldRadios[r.Name]
		resp.Radios = append(resp.Radios, WifiRadio{Name: r.Name, Band: r.Band, Channel: r.Channel, Width: r.Width, Enabled: !r.Disabled,
			HT: r.HT, VHT: r.VHT, HE: r.HE, Pending: !had || old != r})
	}
	for _, n := range draft.Networks {
		old, had := active.FindNetwork(n.ID)
		resp.Networks = append(resp.Networks, wifiNetworkView(n, !had || !reflect.DeepEqual(old, n)))
	}
	return resp
}

func wifiNetworkView(n config.WifiNetwork, pending bool) WifiNetwork {
	bands := append([]string{}, n.Bands...)
	return WifiNetwork{ID: n.ID, SSID: n.SSID, Passphrase: n.Passphrase, Network: n.Network, Security: n.Security, Bands: bands,
		BandSteering: n.BandSteering, Hidden: n.Hidden, ClientIsolation: n.ClientIsolation, Enabled: !n.Disabled, Pending: pending}
}

// wifiOf returns the draft's Wi-Fi section, creating it if there is none yet.
func wifiOf(c *config.NetworkConfig) *config.WifiConfig {
	if c.Wifi == nil {
		c.Wifi = &config.WifiConfig{}
	}
	return c.Wifi
}

// tidyWifi drops an empty Wi-Fi section, so that undoing every change leaves no draft.
func tidyWifi(c *config.NetworkConfig) {
	if w := c.Wifi; w != nil && w.Country == "" && len(w.Radios) == 0 && len(w.Networks) == 0 {
		c.Wifi = nil
	}
}

func (a *API) hardware(ctx context.Context) (wireless.Report, bool) {
	if a.Wireless == nil {
		return wireless.Report{}, false
	}
	return a.Wireless(ctx), true
}

func (a *API) wifiAnswer(w http.ResponseWriter, err error, status int, summary string, body any) {
	var ve config.ValidationError
	if errors.As(err, &ve) {
		fieldErrors(w, http.StatusBadRequest, summary+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, status, body)
}

// HandleWifi: GET /api/v1/wifi returns the Wi-Fi setup (the draft).
func (a *API) HandleWifi(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, a.wifiView())
	}
}

// ---- country ---------------------------------------------------------------------------------------

// HandleWifiCountry: PUT /api/v1/wifi/country {"country": "US"}.
func (a *API) HandleWifiCountry(w http.ResponseWriter, r *http.Request) {
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
		wifiOf(c).Country = country
		tidyWifi(c)
		return nil
	})
	a.wifiAnswer(w, err, http.StatusOK, "", a.wifiView())
}

// ---- adapters --------------------------------------------------------------------------------------

type radioRequest struct {
	Band    string `json:"band"`
	Channel int    `json:"channel"`
	Width   int    `json:"width"`
	Enabled *bool  `json:"enabled"`
}

// HandleWifiRadio: PUT /api/v1/wifi/radios/{name} sets how an adapter is used; DELETE stops using it.
func (a *API) HandleWifiRadio(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) || !a.requireEditable(w) {
		return
	}
	name := r.PathValue("name")
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			wf := wifiOf(c)
			kept := wf.Radios[:0:0]
			for _, x := range wf.Radios {
				if x.Name != name {
					kept = append(kept, x)
				}
			}
			wf.Radios = kept
			tidyWifi(c)
			return nil
		})
		a.wifiAnswer(w, err, http.StatusOK, "", a.wifiView())
		return
	}

	var req radioRequest
	if !decode(w, r, &req) {
		return
	}
	report, haveHardware := a.hardware(r.Context())
	var problems config.ValidationError
	var ht, vht, he bool
	if haveHardware {
		var adapter *wireless.Adapter
		for i := range report.Adapters {
			if report.Adapters[i].Name == name {
				adapter = &report.Adapters[i]
			}
		}
		switch {
		case adapter == nil:
			problems = append(problems, config.FieldError{Field: "name", Message: "there is no Wi-Fi adapter with this name"})
		case !adapter.CapabilitiesKnown:
			problems = append(problems, config.FieldError{Field: "name", Message: "what this adapter can do could not be read (is the iw tool installed?)"})
		case !adapter.APCapable:
			problems = append(problems, config.FieldError{Field: "name", Message: "this adapter cannot act as an access point"})
		default:
			var band *wireless.Band
			for i := range adapter.Bands {
				if adapter.Bands[i].Name == req.Band {
					band = &adapter.Bands[i]
				}
			}
			if band == nil {
				problems = append(problems, config.FieldError{Field: "band", Message: "this adapter does not support the " + req.Band + " band"})
				break
			}
			ht, vht, he = band.HT, band.VHT, band.HE
			okWidth := false
			for _, w := range band.Widths {
				okWidth = okWidth || w == req.Width
			}
			if !okWidth {
				problems = append(problems, config.FieldError{Field: "width", Message: fmt.Sprintf("this adapter cannot use %d MHz in the %s band", req.Width, req.Band)})
			}
			if req.Channel != 0 {
				found := false
				for _, c := range band.Channels {
					if c.Number == req.Channel && !c.Disabled {
						found = true
					}
				}
				if !found {
					problems = append(problems, config.FieldError{Field: "channel", Message: fmt.Sprintf("channel %d is not available on this adapter", req.Channel)})
				}
			}
		}
	} else {
		problems = append(problems, config.FieldError{Field: "name", Message: "Wi-Fi adapters cannot be read on this system"})
	}
	if len(problems) > 0 {
		fieldErrors(w, http.StatusBadRequest, "This adapter cannot be set up like this: "+problems.Error(), problems)
		return
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		wf := wifiOf(c)
		radio := config.WifiRadio{Name: name, Band: req.Band, Channel: req.Channel, Width: req.Width,
			Disabled: req.Enabled != nil && !*req.Enabled, HT: ht, VHT: vht, HE: he}
		replaced := false
		for i := range wf.Radios {
			if wf.Radios[i].Name == name {
				wf.Radios[i], replaced = radio, true
			}
		}
		if !replaced {
			wf.Radios = append(wf.Radios, radio)
		}
		idx := len(wf.Radios) - 1
		for i := range wf.Radios {
			if wf.Radios[i].Name == name {
				idx = i
			}
		}
		if err := errorsForWifi(c.Validate(), fmt.Sprintf("wifi.radios[%d].", idx)); err != nil {
			return err
		}
		return capacityProblems(*c, report, haveHardware, "band")
	})
	a.wifiAnswer(w, err, http.StatusOK, "This adapter cannot be set up like this: ", a.wifiView())
}

// ---- Wi-Fi networks ----------------------------------------------------------------------------------

type wifiNetworkRequest struct {
	SSID            *string  `json:"ssid"`
	Passphrase      *string  `json:"passphrase"`
	Network         *string  `json:"network"`
	Security        *string  `json:"security"`
	Bands           []string `json:"bands"`
	BandSteering    *bool    `json:"band_steering"`
	Hidden          *bool    `json:"hidden"`
	ClientIsolation *bool    `json:"client_isolation"`
	Enabled         *bool    `json:"enabled"`
}

// errorsForWifi keeps the validation problems under a prefix (renamed to the API's own field names) and
// the ones about the list as a whole. Problems with another Wi-Fi network, with the country or with which
// adapters are switched on are not this request's business: they show when that is edited, or at commit.
func errorsForWifi(err error, prefix string) error {
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out config.ValidationError
	for _, e := range ve {
		switch {
		case strings.HasPrefix(e.Field, prefix):
			out = append(out, config.FieldError{Field: strings.TrimPrefix(e.Field, prefix), Message: e.Message})
		case e.Field == "wifi.networks" || e.Field == "wifi.radios":
			out = append(out, config.FieldError{Field: strings.TrimPrefix(e.Field, "wifi."), Message: e.Message})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// capacityProblems checks that no adapter is asked to run more Wi-Fi networks than it can at once.
func capacityProblems(c config.NetworkConfig, report wireless.Report, known bool, field string) error {
	if !known {
		return nil
	}
	radios, nets := c.WifiSettings().Active()
	var out config.ValidationError
	for _, r := range radios {
		for _, ad := range report.Adapters {
			if ad.Name != r.Name || ad.MaxAP <= 0 {
				continue
			}
			count := 0
			for _, n := range nets {
				if n.Has(r.Band) {
					count++
				}
			}
			if count > ad.MaxAP {
				out = append(out, config.FieldError{Field: field, Message: fmt.Sprintf("the adapter %s can run at most %d Wi-Fi %s at once, and %d would use it", r.Name, ad.MaxAP, plural(ad.MaxAP, "network", "networks"), count)})
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func nextWifiID(w *config.WifiConfig) string {
	for k := 1; ; k++ {
		id := fmt.Sprintf("wifi%d", k)
		if _, taken := w.FindNetwork(id); !taken {
			return id
		}
	}
}

func applyWifiRequest(n *config.WifiNetwork, req wifiNetworkRequest) {
	if req.SSID != nil {
		n.SSID = *req.SSID
	}
	if req.Passphrase != nil {
		n.Passphrase = *req.Passphrase
	}
	if req.Network != nil {
		n.Network = strings.TrimSpace(*req.Network)
	}
	if req.Security != nil {
		n.Security = strings.TrimSpace(*req.Security)
	}
	if req.Bands != nil {
		n.Bands = nil
		for _, b := range req.Bands {
			n.Bands = append(n.Bands, strings.TrimSpace(b))
		}
	}
	if req.BandSteering != nil {
		n.BandSteering = *req.BandSteering
	}
	if req.Hidden != nil {
		n.Hidden = *req.Hidden
	}
	if req.ClientIsolation != nil {
		n.ClientIsolation = *req.ClientIsolation
	}
	if req.Enabled != nil {
		n.Disabled = !*req.Enabled
	}
}

// HandleWifiNetworks: POST /api/v1/wifi/networks adds a Wi-Fi network.
func (a *API) HandleWifiNetworks(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	var req wifiNetworkRequest
	if !decode(w, r, &req) {
		return
	}
	report, known := a.hardware(r.Context())

	var id string
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		wf := wifiOf(c)
		id = nextWifiID(wf)
		// what is left out: WPA2/WPA3, the default network, on, band steering when both bands are chosen
		n := config.WifiNetwork{ID: id, Network: config.DefaultNetworkID, Security: config.SecurityWPA2WPA3}
		applyWifiRequest(&n, req)
		if req.BandSteering == nil {
			n.BandSteering = n.Has(config.Band24) && n.Has(config.Band5)
		}
		wf.Networks = append(wf.Networks, n)
		if err := errorsForWifi(c.Validate(), fmt.Sprintf("wifi.networks[%d].", len(wf.Networks)-1)); err != nil {
			return err
		}
		return capacityProblems(*c, report, known, "bands")
	})
	var body any
	if err == nil {
		draft, _ := a.Manager.StagedNetworkConfig()
		n, _ := draft.WifiSettings().FindNetwork(id)
		body = wifiNetworkView(n, true)
	}
	a.wifiAnswer(w, err, http.StatusCreated, "This Wi-Fi network cannot be added: ", body)
}

// HandleWifiNetwork: PUT /api/v1/wifi/networks/{id} changes a Wi-Fi network; DELETE removes it.
func (a *API) HandleWifiNetwork(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	if _, ok := draft.WifiSettings().FindNetwork(id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no Wi-Fi network with this id"})
		return
	}
	if !a.requireEditable(w) {
		return
	}

	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			wf := wifiOf(c)
			kept := wf.Networks[:0:0]
			for _, n := range wf.Networks {
				if n.ID != id {
					kept = append(kept, n)
				}
			}
			wf.Networks = kept
			tidyWifi(c)
			return nil
		})
		a.wifiAnswer(w, err, http.StatusOK, "", a.wifiView())
		return
	}

	var req wifiNetworkRequest
	if !decode(w, r, &req) {
		return
	}
	report, known := a.hardware(r.Context())
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		wf := wifiOf(c)
		for i := range wf.Networks {
			if wf.Networks[i].ID != id {
				continue
			}
			applyWifiRequest(&wf.Networks[i], req)
			if err := errorsForWifi(c.Validate(), fmt.Sprintf("wifi.networks[%d].", i)); err != nil {
				return err
			}
			return capacityProblems(*c, report, known, "bands")
		}
		return nil
	})
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		active := a.Manager.NetworkConfig().WifiSettings()
		n, _ := now.WifiSettings().FindNetwork(id)
		old, had := active.FindNetwork(id)
		body = wifiNetworkView(n, !had || !reflect.DeepEqual(old, n))
	}
	a.wifiAnswer(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

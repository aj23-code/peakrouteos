package netapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

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
	WDS             bool     `json:"wds"`
	VLAN            int      `json:"vlan"` // an access point's tagged network (0: untagged)
	Enabled         bool     `json:"enabled"`
	Pending         bool     `json:"pending"`
}

// WifiStation is an adapter used as a client of another access point.
type WifiStation struct {
	Name       string `json:"name"`
	SSID       string `json:"ssid"`
	Passphrase string `json:"passphrase"`
	Security   string `json:"security"`
	Hidden     bool   `json:"hidden"`
	Role       string `json:"role"`    // "uplink" (the internet connection) or "bridge" (a link into a local network)
	Network    string `json:"network"` // bridge: the local network
	Enabled    bool   `json:"enabled"`
	// InUse: an uplink counts only while the adapter is the internet port (an uplink that is not says which port is).
	InUse   bool `json:"in_use"`
	Pending bool `json:"pending"`
}

type wifiResponse struct {
	// Country is the router-wide country setting (System, Settings), shown here because Wi-Fi needs it.
	Country  string        `json:"country"`
	Radios   []WifiRadio   `json:"radios"`
	Networks []WifiNetwork `json:"networks"`
	Stations []WifiStation `json:"stations"`
	// APMode: the device is an access point in the draft, so a Wi-Fi network is on a VLAN, not a Local Network.
	APMode bool `json:"ap_mode"`
	// WanPort is the internet port in the draft: the adapter of an uplink station is it, or it is a cable port.
	WanPort  string `json:"wan_port"`
	Editable bool   `json:"editable"`
}

func (a *API) wifiView() wifiResponse {
	active := a.Manager.NetworkConfig().WifiSettings()
	staged, _ := a.Manager.StagedNetworkConfig()
	draft := staged.WifiSettings()

	resp := wifiResponse{Country: staged.Country, WanPort: staged.WanInterface, APMode: staged.IsAP(),
		Radios: []WifiRadio{}, Networks: []WifiNetwork{}, Stations: []WifiStation{}, Editable: a.editable()}

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
	for _, st := range draft.Stations {
		old, had := active.FindStation(st.Name)
		resp.Stations = append(resp.Stations, stationView(staged, st, !had || old != st))
	}
	return resp
}

func stationView(c config.NetworkConfig, st config.WifiStation, pending bool) WifiStation {
	inUse := !st.Disabled && (st.Role == config.StationBridge || st.Name == c.WanInterface)
	return WifiStation{Name: st.Name, SSID: st.SSID, Passphrase: st.Passphrase, Security: st.Security, Hidden: st.Hidden, Role: st.Role,
		Network: st.Network, Enabled: !st.Disabled, InUse: inUse, Pending: pending}
}

func wifiNetworkView(n config.WifiNetwork, pending bool) WifiNetwork {
	bands := append([]string{}, n.Bands...)
	return WifiNetwork{ID: n.ID, SSID: n.SSID, Passphrase: n.Passphrase, Network: n.Network, Security: n.Security, Bands: bands,
		BandSteering: n.BandSteering, Hidden: n.Hidden, ClientIsolation: n.ClientIsolation, WDS: n.WDS, VLAN: n.VLAN, Enabled: !n.Disabled, Pending: pending}
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
	if w := c.Wifi; w != nil && len(w.Radios) == 0 && len(w.Networks) == 0 && len(w.Stations) == 0 {
		c.Wifi = nil
	}
}

func (a *API) hardware(ctx context.Context) (wireless.Report, bool) {
	if a.Wireless == nil {
		return wireless.Report{}, false
	}
	return a.Wireless(ctx), true
}

func (a *API) answerStaged(w http.ResponseWriter, err error, status int, summary string, body any) {
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
		a.answerStaged(w, err, http.StatusOK, "", a.wifiView())
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
	a.answerStaged(w, err, http.StatusOK, "This adapter cannot be set up like this: ", a.wifiView())
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
	WDS             *bool    `json:"wds"`
	VLAN            *int     `json:"vlan"`
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
		case e.Field == "wifi.networks" || e.Field == "wifi.radios" || e.Field == "wifi.stations":
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
	if req.WDS != nil {
		n.WDS = *req.WDS
	}
	if req.VLAN != nil {
		n.VLAN = *req.VLAN
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
	a.answerStaged(w, err, http.StatusCreated, "This Wi-Fi network cannot be added: ", body)
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
		a.answerStaged(w, err, http.StatusOK, "", a.wifiView())
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
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

// ---- Wi-Fi client adapters ---------------------------------------------------------------------------

type stationRequest struct {
	SSID       *string `json:"ssid"`
	Passphrase *string `json:"passphrase"`
	Security   *string `json:"security"`
	Hidden     *bool   `json:"hidden"`
	Role       *string `json:"role"`
	Network    *string `json:"network"`
	Enabled    *bool   `json:"enabled"`
}

// freeWiredPort picks a cable port for the internet connection to go back to when the Wi-Fi adapter stops being it: the first one that
// is not a LAN port.
func (a *API) freeWiredPort(c *config.NetworkConfig) (string, bool) {
	if a.Ports == nil {
		return "", false
	}
	ports, err := a.Ports()
	if err != nil {
		return "", false
	}
	used := map[string]bool{}
	for _, p := range c.LanInterfaces {
		used[p] = true
	}
	for _, p := range ports {
		if !p.Wireless && !used[p.Name] {
			return p.Name, true
		}
	}
	return "", false
}

// releaseWan gives the internet connection back to a cable port when the adapter that is it stops being used as one.
func (a *API) releaseWan(c *config.NetworkConfig, adapter string) error {
	if c.WanInterface != adapter {
		return nil
	}
	port, ok := a.freeWiredPort(c)
	if !ok {
		return config.ValidationError{{Field: "role", Message: "no cable port is free to be the internet port again: free one on the Ports page first"}}
	}
	c.WanInterface = port
	return nil
}

// HandleWifiStation: PUT /api/v1/wifi/stations/{name} sets how an adapter is used as a Wi-Fi client; DELETE stops using it.
func (a *API) HandleWifiStation(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) || !a.requireEditable(w) {
		return
	}
	name := r.PathValue("name")
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			wf := wifiOf(c)
			kept := wf.Stations[:0:0]
			for _, x := range wf.Stations {
				if x.Name != name {
					kept = append(kept, x)
				}
			}
			uplink := len(kept) != len(wf.Stations)
			wf.Stations = kept
			tidyWifi(c)
			if uplink {
				return a.releaseWan(c, name)
			}
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.wifiView())
		return
	}

	var req stationRequest
	if !decode(w, r, &req) {
		return
	}
	report, haveHardware := a.hardware(r.Context())
	if haveHardware {
		var adapter *wireless.Adapter
		for i := range report.Adapters {
			if report.Adapters[i].Name == name {
				adapter = &report.Adapters[i]
			}
		}
		var problem string
		switch {
		case adapter == nil:
			problem = "there is no Wi-Fi adapter with this name"
		case adapter.CapabilitiesKnown && !hasMode(adapter.Modes, "managed"):
			problem = "this adapter cannot connect to a Wi-Fi network (it has no client mode)"
		}
		if problem != "" {
			fieldErrors(w, http.StatusBadRequest, "This adapter cannot be set up like this: "+problem,
				config.ValidationError{{Field: "name", Message: problem}})
			return
		}
	}

	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		wf := wifiOf(c)
		idx := -1
		for i := range wf.Stations {
			if wf.Stations[i].Name == name {
				idx = i
			}
		}
		if idx < 0 {
			// what is left out: WPA2/WPA3 (most networks), the internet connection, the default network, on
			wf.Stations = append(wf.Stations, config.WifiStation{Name: name, Security: config.SecurityWPA2WPA3, Role: config.StationUplink, Network: config.DefaultNetworkID})
			idx = len(wf.Stations) - 1
		}
		st := &wf.Stations[idx]
		wasUplink := st.Role == config.StationUplink && !st.Disabled
		if req.SSID != nil {
			st.SSID = *req.SSID
		}
		if req.Passphrase != nil {
			st.Passphrase = *req.Passphrase
		}
		if req.Security != nil {
			st.Security = strings.TrimSpace(*req.Security)
		}
		if req.Hidden != nil {
			st.Hidden = *req.Hidden
		}
		if req.Role != nil {
			st.Role = strings.TrimSpace(*req.Role)
		}
		if req.Network != nil {
			st.Network = strings.TrimSpace(*req.Network)
		}
		if st.Role == config.StationUplink {
			st.Network = ""
		} else if st.Network == "" {
			st.Network = config.DefaultNetworkID
		}
		if req.Enabled != nil {
			st.Disabled = !*req.Enabled
		}
		isUplink := st.Role == config.StationUplink && !st.Disabled
		if isUplink && c.IsAP() {
			return config.ValidationError{{Field: "role", Message: "an access point has no internet connection of its own: choose \"A link into a local network\" to join the main Wi-Fi"}}
		}

		switch {
		case isUplink:
			// the adapter is the internet port now (a cable port that was it is left unassigned); the connection cannot keep what
			// Wi-Fi cannot do
			if c.WanVlan > 0 || c.WanKind() == config.WanPPPoE {
				return config.ValidationError{{Field: "role", Message: "the internet connection uses a VLAN or PPPoE, which cannot run over Wi-Fi: change it on the Network page first"}}
			}
			c.WanInterface = name
			kept := c.LanInterfaces[:0:0]
			for _, p := range c.LanInterfaces {
				if p != name {
					kept = append(kept, p)
				}
			}
			c.LanInterfaces = kept
		case wasUplink:
			if err := a.releaseWan(c, name); err != nil {
				return err
			}
		}
		if err := errorsForWifi(c.Validate(), fmt.Sprintf("wifi.stations[%d].", idx)); err != nil {
			return err
		}
		return nil
	})
	var body any
	if err == nil {
		draft, _ := a.Manager.StagedNetworkConfig()
		st, _ := draft.WifiSettings().FindStation(name)
		old, had := a.Manager.NetworkConfig().WifiSettings().FindStation(name)
		body = stationView(draft, st, !had || old != st)
	}
	a.answerStaged(w, err, http.StatusOK, "This Wi-Fi connection cannot be set up like this: ", body)
}

func hasMode(modes []string, want string) bool {
	for _, m := range modes {
		if m == want {
			return true
		}
	}
	return false
}

type scanResponse struct {
	Adapter  string             `json:"adapter"`
	Networks []wireless.Network `json:"networks"`
	// Cached: the list is what the kernel remembered from an earlier scan (a new one was not allowed).
	Cached bool `json:"cached"`
}

// HandleWifiScan: GET /api/v1/wifi/scan?adapter=wlan0 looks for Wi-Fi networks a client adapter could join. An adapter that is serving
// an access point right now is not scanned: it would drop the devices connected to it.
func (a *API) HandleWifiScan(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	name := r.URL.Query().Get("adapter")
	if !config.IsValidInterfaceName(name) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "choose the adapter to scan with"})
		return
	}
	if a.Scan == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "Wi-Fi networks cannot be scanned for on this system"})
		return
	}
	if report, ok := a.hardware(r.Context()); ok {
		found := false
		for _, ad := range report.Adapters {
			found = found || ad.Name == name
		}
		if !found {
			writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no Wi-Fi adapter with this name"})
			return
		}
	}
	radios, _ := a.Manager.NetworkConfig().WifiSettings().Active()
	for _, rd := range radios {
		if rd.Name == name {
			writeJSON(w, http.StatusConflict, errorBody{Error: "this adapter is serving an access point right now: scanning with it would drop the devices connected to it"})
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	networks, cached, err := a.Scan(ctx, name)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{Error: "The adapter could not be scanned. Is it switched on?"})
		return
	}
	if networks == nil {
		networks = []wireless.Network{}
	}
	writeJSON(w, http.StatusOK, scanResponse{Adapter: name, Networks: networks, Cached: cached})
}

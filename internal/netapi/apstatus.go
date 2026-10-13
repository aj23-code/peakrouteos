package netapi

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/wireless"
)

// The access point's home page: what the device is doing, read from the system (docs/access-point.md).

// APAddress is how the device is reached on the network it is part of.
type APAddress struct {
	Mode      string   `json:"mode"`      // "dhcp" or "static"
	Up        bool     `json:"up"`        // the bridge is up
	Addresses []string `json:"addresses"` // with prefix length; the setup address is not among them
	Recovery  bool     `json:"recovery"`  // the setup address is on the bridge
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
}

// APPortStatus is a cable port or a Wi-Fi link in the bridge.
type APPortStatus struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"` // "port" or "link"
	State string `json:"state"`
	Speed string `json:"speed"`
	SSID  string `json:"ssid,omitempty"` // a link: the network it joins
	// a link: whether it is connected now, and how well
	Connected *bool   `json:"connected,omitempty"`
	SignalDBm *int    `json:"signal_dbm,omitempty"`
	RxMbps    float64 `json:"rx_mbps,omitempty"`
	TxMbps    float64 `json:"tx_mbps,omitempty"`
	Band      string  `json:"band,omitempty"`
}

// APRadioStatus is an adapter hosting Wi-Fi.
type APRadioStatus struct {
	Name    string `json:"name"`
	Band    string `json:"band"`
	Channel int    `json:"channel"` // the channel in use (0: not up)
	Width   int    `json:"width"`
	Up      bool   `json:"up"`
	// How it is doing (nil: not known): transmit power, the noise on the channel, how much of the time the channel is busy, and the devices
	// connected through it with their average signal.
	TxPowerDBm   *float64 `json:"tx_power_dbm"`
	NoiseDBm     *int     `json:"noise_dbm"`
	BusyPercent  *float64 `json:"busy_percent"`
	Clients      int      `json:"clients"`
	AvgSignalDBm *int     `json:"avg_signal_dbm"`
}

// surveyPoint is the last reading of an adapter's channel, to work out how busy it was since.
type surveyPoint struct {
	active, busy uint64
	at           time.Time
}

// busyPercent is the share of the time the channel was busy: since the last reading when there is one, else since the adapter started.
func (a *API) busyPercent(iface string, s wireless.Survey) *float64 {
	a.surveyMu.Lock()
	defer a.surveyMu.Unlock()
	if a.surveyPrev == nil {
		a.surveyPrev = map[string]surveyPoint{}
	}
	prev, had := a.surveyPrev[iface]
	a.surveyPrev[iface] = surveyPoint{active: s.ActiveMS, busy: s.BusyMS, at: time.Now()}
	active, busy := s.ActiveMS, s.BusyMS
	if had && s.ActiveMS > prev.active && s.BusyMS >= prev.busy {
		active, busy = s.ActiveMS-prev.active, s.BusyMS-prev.busy
	}
	if active == 0 {
		return nil
	}
	v := float64(busy) * 100 / float64(active)
	if v > 100 {
		v = 100
	}
	return &v
}

// APNetworkStatus is a Wi-Fi network being hosted.
type APNetworkStatus struct {
	SSID    string   `json:"ssid"`
	Bands   []string `json:"bands"`
	Hidden  bool     `json:"hidden"`
	VLAN    int      `json:"vlan"` // 0: the untagged network
	Clients int      `json:"clients"`
}

type apStatusResponse struct {
	Address  APAddress          `json:"address"`
	Ports    []APPortStatus     `json:"ports"`
	Radios   []APRadioStatus    `json:"radios"`
	Networks []APNetworkStatus  `json:"networks"`
	Devices  []wireless.Station `json:"devices"` // the Wi-Fi devices connected now
	// DevicesKnown is false when the Wi-Fi side could not be read (then the list is empty because it is unknown).
	DevicesKnown bool `json:"devices_known"`
	Pending      bool `json:"pending"`
}

// HandleAPStatus: GET /api/v1/ap/status.
func (a *API) HandleAPStatus(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	active := a.Manager.NetworkConfig()
	if !active.IsAP() {
		writeJSON(w, http.StatusConflict, errorBody{Error: "This device is not an access point."})
		return
	}
	writeJSON(w, http.StatusOK, a.apStatus(r.Context(), active))
}

func (a *API) apStatus(ctx context.Context, active config.NetworkConfig) apStatusResponse {
	ap := active.APSettings()
	resp := apStatusResponse{Ports: []APPortStatus{}, Radios: []APRadioStatus{}, Networks: []APNetworkStatus{}, Devices: []wireless.Station{},
		Pending: len(a.Manager.StagedChanges()) > 0}

	// the address
	resp.Address = APAddress{Mode: ap.Mode, DNS: []string{}, Addresses: []string{}}
	if resp.Address.Mode == "" {
		resp.Address.Mode = config.APDHCP
	}
	if a.Bridge != nil {
		addrs, up := a.Bridge()
		resp.Address.Up = up
		for _, ad := range addrs {
			if ad == config.APRecoveryAddress {
				resp.Address.Recovery = true
				continue
			}
			resp.Address.Addresses = append(resp.Address.Addresses, ad)
		}
	}
	if a.Gateway != nil {
		resp.Address.Gateway = a.Gateway()
	}
	if a.Resolvers != nil {
		if ns := a.Resolvers(); ns != nil {
			resp.Address.DNS = ns
		}
	}

	// the cable ports and the Wi-Fi links in the bridge
	hw := map[string]config.SystemInterface{}
	for _, p := range a.wiredPorts() {
		hw[p.Name] = p
	}
	for _, name := range ap.Ports {
		p := hw[name]
		state := p.State
		if state == "" {
			state = "missing"
		}
		resp.Ports = append(resp.Ports, APPortStatus{Name: name, Kind: "port", State: state, Speed: p.Speed})
	}
	for _, st := range active.WifiSettings().ActiveStations() {
		if st.Role == config.StationBridge {
			ps := APPortStatus{Name: st.Name, Kind: "link", SSID: st.SSID}
			if a.Link != nil {
				if l, ok := a.Link(ctx, st.Name); ok {
					c := l.Connected
					ps.Connected, ps.SignalDBm, ps.RxMbps, ps.TxMbps, ps.Band = &c, l.SignalDBm, l.RxMbps, l.TxMbps, l.Band
				}
			}
			resp.Ports = append(resp.Ports, ps)
		}
	}

	// the Wi-Fi: adapters with the channel they are on, networks with the devices connected
	var ifaces []wireless.Interface
	if a.WifiDev != nil {
		ifaces, _ = a.WifiDev(ctx)
	}
	radios, nets := active.WifiSettings().Active()
	for _, rd := range radios {
		rs := APRadioStatus{Name: rd.Name, Band: rd.Band}
		for _, ifc := range ifaces {
			if ifc.Name == rd.Name && ifc.Type == "AP" {
				rs.Up, rs.Channel, rs.Width = true, ifc.Channel, ifc.Width
				if ifc.HaveTxPower {
					p := ifc.TxPowerDBM
					rs.TxPowerDBm = &p
				}
			}
		}
		if rs.Up && a.Survey != nil {
			if sv, ok := a.Survey(ctx, rd.Name); ok {
				if sv.HaveNoise {
					n := sv.NoiseDBm
					rs.NoiseDBm = &n
				}
				rs.BusyPercent = a.busyPercent(rd.Name, sv)
			}
		}
		resp.Radios = append(resp.Radios, rs)
	}
	if a.Stations != nil {
		rep := a.Stations(ctx)
		resp.DevicesKnown = rep.Known
		resp.Devices = append(resp.Devices, rep.Stations...)
	}
	// the devices connected through each adapter (its first network is on the adapter's own name, the others on name_1, name_2...)
	for i := range resp.Radios {
		rs := &resp.Radios[i]
		sum, n := 0, 0
		for _, d := range resp.Devices {
			if d.Interface == rs.Name || strings.HasPrefix(d.Interface, rs.Name+"_") {
				rs.Clients++
				if d.Signal != nil {
					sum += *d.Signal
					n++
				}
			}
		}
		if n > 0 {
			avg := sum / n
			rs.AvgSignalDBm = &avg
		}
	}
	count := map[string]int{}
	for _, s := range resp.Devices {
		count[s.SSID]++
	}
	for _, n := range nets {
		bands := append([]string{}, n.Bands...)
		resp.Networks = append(resp.Networks, APNetworkStatus{SSID: n.SSID, Bands: bands, Hidden: n.Hidden, VLAN: n.VLAN, Clients: count[n.SSID]})
	}
	sort.SliceStable(resp.Networks, func(i, j int) bool {
		return strings.ToLower(resp.Networks[i].SSID) < strings.ToLower(resp.Networks[j].SSID)
	})
	return resp
}

// RouterOnly lets a handler answer only while the device is a router. An access point has no internet connection, networks, firewall,
// VPN or DHCP of its own: those pages are not in its menu, and their interface says so instead of staging settings nothing uses.
func RouterOnly(isAP func() bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isAP() {
			writeJSON(w, http.StatusConflict, errorBody{Error: "This device is an access point: that belongs to a router. Change the mode on System, Operation mode."})
			return
		}
		h(w, r)
	}
}

package netapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/network"
	"peakroute/internal/speedtest"
)

// Smart queue management (docs/sqm.md): shaping of the internet connection against bufferbloat. The page speaks Mbit/s; the file keeps kbit.

// SQMLink is a kind of connection as the page offers it.
type SQMLink struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

var sqmLinks = []SQMLink{
	{config.SQMEthernet, "Fibre or an Ethernet hand-off"},
	{config.SQMDocsis, "Cable (DOCSIS)"},
	{config.SQMVDSL, "DSL, VDSL2 (PPPoE)"},
	{config.SQMADSL, "DSL, ADSL (PPPoE)"},
}

type sqmResponse struct {
	Enabled        bool               `json:"enabled"`
	DownloadMbit   float64            `json:"download_mbit"`
	UploadMbit     float64            `json:"upload_mbit"`
	Percent        int                `json:"percent"` // the router shapes to this share of the line's speeds
	DefaultPercent int                `json:"default_percent"`
	ShapedDownMbit float64            `json:"shaped_download_mbit"`
	ShapedUpMbit   float64            `json:"shaped_upload_mbit"`
	Link           string             `json:"link"`
	Links          []SQMLink          `json:"links"`
	PerHost        bool               `json:"per_host"`
	Pending        bool               `json:"pending"` // the draft differs from what runs
	Running        bool               `json:"running"` // shaping is in effect now (the committed configuration)
	Status         *network.SQMStatus `json:"status"`  // how the kernel's queues are doing (null: not reported)
	WanDevice      string             `json:"wan_device"`
	Networks       []sqmNetwork       `json:"networks"` // each network's priority and limits, and the share of the line it is assured
	Editable       bool               `json:"editable"`
	Second         *sqmSecond         `json:"second,omitempty"` // the second internet connection's line, while there is one
}

// sqmSecond is the second internet connection's line: its own speeds and kind (the switch, the percentage and the fairness are shared).
type sqmSecond struct {
	DownloadMbit   float64            `json:"download_mbit"`
	UploadMbit     float64            `json:"upload_mbit"`
	ShapedDownMbit float64            `json:"shaped_download_mbit"`
	ShapedUpMbit   float64            `json:"shaped_upload_mbit"`
	Link           string             `json:"link"`
	Running        bool               `json:"running"`
	Status         *network.SQMStatus `json:"status"`
	WanDevice      string             `json:"wan_device"`
}

func (a *API) sqmView(ctx context.Context) sqmResponse {
	draft, _ := a.Manager.StagedNetworkConfig()
	active := a.Manager.NetworkConfig()
	s := draft.SQMSettings()
	out := sqmResponse{
		Enabled: s.Enabled, DownloadMbit: float64(s.DownloadKbit) / 1000, UploadMbit: float64(s.UploadKbit) / 1000,
		Percent: s.EffectivePercent(), DefaultPercent: config.DefaultSQMPercent,
		ShapedDownMbit: float64(s.ShapedDownKbit()) / 1000, ShapedUpMbit: float64(s.ShapedUpKbit()) / 1000,
		Link: s.Link, Links: sqmLinks, PerHost: s.PerHost,
		Pending: !active.SQMSettings().Same(draft.SQMSettings()) || sqmQoSDiffers(active, draft), Running: active.SQMActive(), WanDevice: draft.WanDevice(), Editable: a.editable(),
		Networks: a.sqmNetworks(draft),
	}
	if out.Running && a.SQMStatus != nil {
		st := a.statusOf(ctx, active, config.WanFirst)
		out.Status = &st
	}
	if v, ok := draft.WanView(config.WanSecond); ok {
		s2 := v.SQMSettings()
		out.Second = &sqmSecond{DownloadMbit: float64(s2.DownloadKbit) / 1000, UploadMbit: float64(s2.UploadKbit) / 1000,
			ShapedDownMbit: float64(s2.ShapedDownKbit()) / 1000, ShapedUpMbit: float64(s2.ShapedUpKbit()) / 1000, Link: s2.Link, WanDevice: v.WanDevice()}
		if av, ok := active.WanView(config.WanSecond); ok && av.SQMActive() && a.SQMStatus != nil {
			out.Second.Running = true
			st := a.statusOf(ctx, active, config.WanSecond)
			out.Second.Status = &st
		}
	}
	return out
}

// statusOf reads how the shaping of one connection is doing.
func (a *API) statusOf(ctx context.Context, active config.NetworkConfig, line string) network.SQMStatus {
	st := a.SQMStatus(ctx, line)
	// a line divided between networks is not one queue with one speed: the speeds are the shaped ones
	v, _ := active.WanView(line)
	as := v.SQMSettings()
	if st.Upload.Active && st.Upload.BandwidthKbit == 0 {
		st.Upload.BandwidthKbit = int64(as.ShapedUpKbit())
	}
	if st.Download.Active && st.Download.BandwidthKbit == 0 {
		st.Download.BandwidthKbit = int64(as.ShapedDownKbit())
	}
	return st
}

// sqmNetwork is one network's place in the shaped line.
type sqmNetwork struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Priority      string  `json:"priority"`
	LimitDownMbit float64 `json:"limit_down_mbit"` // 0: no limit
	LimitUpMbit   float64 `json:"limit_up_mbit"`
	// what it is assured when the line is full, and the most it can use, in Mbit/s (0: that direction is not shaped); only while some
	// network has a priority or a limit (the draft's, as it would be)
	AssuredDown float64 `json:"assured_down_mbit"`
	AssuredUp   float64 `json:"assured_up_mbit"`
	CeilDown    float64 `json:"ceil_down_mbit"`
	CeilUp      float64 `json:"ceil_up_mbit"`
	// Inactive says why the setting does nothing right now (shaping is off, or that direction is not shaped); empty when it does
	Inactive string `json:"inactive,omitempty"`
}

func (a *API) sqmNetworks(draft config.NetworkConfig) []sqmNetwork {
	classes := map[string]config.QoSClass{}
	for _, cl := range draft.QoSClasses() {
		classes[cl.ID] = cl
	}
	div := draft.QoSActive()
	mb := func(k int) float64 { return float64(k) / 1000 }
	out := []sqmNetwork{}
	add := func(id, name string) {
		q := draft.NetworkQoS(id)
		n := sqmNetwork{ID: id, Name: name, Priority: q.Priority, LimitDownMbit: mb(q.LimitDownKbit), LimitUpMbit: mb(q.LimitUpKbit)}
		n.Inactive = draft.QoSInactive(id)
		if cl, ok := classes[id]; ok && div {
			n.AssuredDown, n.AssuredUp, n.CeilDown, n.CeilUp = mb(cl.DownRate), mb(cl.UpRate), mb(cl.DownCeil), mb(cl.UpCeil)
		}
		out = append(out, n)
	}
	add(config.DefaultNetworkID, "LAN")
	for _, n := range draft.Networks {
		add(n.ID, strings.TrimSpace(n.Name))
	}
	return out
}

// HandleSQM answers GET /api/v1/sqm and PUT /api/v1/sqm (the settings go to the draft).
func (a *API) HandleSQM(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.sqmView(r.Context()))
		return
	}
	if !a.requireEditable(w) {
		return
	}
	var req struct {
		Enabled      *bool    `json:"enabled"`
		DownloadMbit *float64 `json:"download_mbit"`
		UploadMbit   *float64 `json:"upload_mbit"`
		Percent      *int     `json:"percent"`
		Networks     []struct {
			ID            string   `json:"id"`
			Priority      *string  `json:"priority"`
			LimitDownMbit *float64 `json:"limit_down_mbit"`
			LimitUpMbit   *float64 `json:"limit_up_mbit"`
		} `json:"networks"`
		Link    *string `json:"link"`
		PerHost *bool   `json:"per_host"`
		// Second is the second connection's line (ignored while there is none).
		Second *struct {
			DownloadMbit *float64 `json:"download_mbit"`
			UploadMbit   *float64 `json:"upload_mbit"`
			Link         *string  `json:"link"`
		} `json:"second"`
	}
	if !decode(w, r, &req) {
		return
	}
	kbit := func(m float64) int { return int(m*1000 + 0.5) }
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		s := c.SQMSettings()
		if req.Enabled != nil {
			s.Enabled = *req.Enabled
		}
		if req.DownloadMbit != nil {
			s.DownloadKbit = kbit(*req.DownloadMbit)
		}
		if req.UploadMbit != nil {
			s.UploadKbit = kbit(*req.UploadMbit)
		}
		if req.Percent != nil {
			s.Percent = *req.Percent
			if s.Percent == config.DefaultSQMPercent {
				s.Percent = 0 // the default is not written
			}
		}
		if req.Link != nil {
			s.Link = strings.TrimSpace(*req.Link)
		}
		if req.PerHost != nil {
			s.PerHost = *req.PerHost
		}
		if q := req.Second; q != nil && c.HasWan2() {
			l := config.SQMLine{}
			if s.Second != nil {
				l = *s.Second
			}
			if q.DownloadMbit != nil {
				l.DownloadKbit = kbit(*q.DownloadMbit)
			}
			if q.UploadMbit != nil {
				l.UploadKbit = kbit(*q.UploadMbit)
			}
			if q.Link != nil {
				l.Link = strings.TrimSpace(*q.Link)
			}
			if l == (config.SQMLine{}) {
				s.Second = nil
			} else {
				s.Second = &l
			}
		}
		c.SQM = &s
		for _, in := range req.Networks {
			if id := in.ID; id != config.DefaultNetworkID {
				if _, ok := c.FindNetwork(id); !ok {
					return config.ValidationError{{Field: "networks", Message: "there is no network " + id}}
				}
			}
			q := c.NetworkQoS(in.ID)
			if in.Priority != nil {
				q.Priority = strings.TrimSpace(*in.Priority)
			}
			if in.LimitDownMbit != nil {
				q.LimitDownKbit = kbit(*in.LimitDownMbit)
			}
			if in.LimitUpMbit != nil {
				q.LimitUpKbit = kbit(*in.LimitUpMbit)
			}
			c.SetNetworkQoS(in.ID, q)
		}
		err := ownedErrorsOrNil(c.Validate(), "sqm.download_kbit", "sqm.upload_kbit", "sqm.percent", "sqm.link", "sqm.second.download_kbit", "sqm.second.upload_kbit", "sqm.second.link", "sqm.enabled", "lan_qos_priority", "lan_qos_limit_down_kbit", "lan_qos_limit_up_kbit", "networks")
		return qosErrorsOnly(*c, err)
	})
	var ve config.ValidationError
	if errors.As(err, &ve) {
		// the page speaks Mbit/s
		for i := range ve {
			ve[i].Field = strings.NewReplacer("sqm.second.download_kbit", "second.download_mbit", "sqm.second.upload_kbit", "second.upload_mbit", "sqm.second.link", "second.link", "sqm.download_kbit", "download_mbit", "sqm.upload_kbit", "upload_mbit", "sqm.percent", "percent", "sqm.link", "link", "sqm.enabled", "enabled").Replace(ve[i].Field)
			ve[i].Message = strings.NewReplacer("0 and 10000000 (Mbit/s: 0 to 10000)", "0 and 10000 Mbit/s").Replace(ve[i].Message)
		}
		fieldErrors(w, http.StatusBadRequest, "These settings cannot be saved: "+ve.Error(), ve)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.sqmView(r.Context()))
}

// ---- the speed test ---------------------------------------------------------------------------------------------------------

// speedJob is the state of the speed test, which runs one at a time and whose answer the page fetches.
type speedJob struct {
	mu     sync.Mutex
	state  string // "", "running", "done" or "error"
	phase  string
	line   string // the connection being or last measured
	result *speedtest.Result
	err    string
}

type speedResponse struct {
	State  string            `json:"state"` // "idle", "running", "done" or "error"
	Phase  string            `json:"phase,omitempty"`
	Result *speedtest.Result `json:"result,omitempty"`
	Error  string            `json:"error,omitempty"`
	Line   string            `json:"line,omitempty"` // the connection that was measured: "wan" or "wan2"
	// Pauses: shaping is on and is paused for the test, which would otherwise measure the shaping and not the line.
	Pauses bool `json:"pauses_shaping"`
}

func (a *API) speedView() speedResponse {
	a.speed.mu.Lock()
	defer a.speed.mu.Unlock()
	st := a.speed.state
	if st == "" {
		st = "idle"
	}
	return speedResponse{State: st, Phase: a.speed.phase, Result: a.speed.result, Error: a.speed.err, Line: a.speed.line, Pauses: a.Manager.NetworkConfig().SQMAnyActive()}
}

// HandleSQMSpeedTest answers GET /api/v1/sqm/speedtest (how the test is going or went) and POST (start one). The test changes nothing in
// the configuration: the page puts its numbers into the form, where the person can change them before saving.
func (a *API) HandleSQMSpeedTest(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, a.speedView())
		return
	}
	if a.SpeedTest == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "the speed test is not available on this system"})
		return
	}
	line := r.URL.Query().Get("line")
	switch {
	case line == "":
		line = config.WanFirst
	case line == config.WanSecond && !a.Manager.NetworkConfig().HasWan2():
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "there is no second internet connection to measure"})
		return
	case line != config.WanFirst && line != config.WanSecond:
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "line must be wan or wan2"})
		return
	}
	a.speed.mu.Lock()
	if a.speed.state == "running" {
		a.speed.mu.Unlock()
		writeJSON(w, http.StatusConflict, errorBody{Error: "a speed test is already running"})
		return
	}
	a.speed.state, a.speed.phase, a.speed.result, a.speed.err, a.speed.line = "running", "starting", nil, "", line
	a.speed.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		res, err := a.SpeedTest(ctx, line, func(phase string) {
			a.speed.mu.Lock()
			a.speed.phase = phase
			a.speed.mu.Unlock()
		})
		a.speed.mu.Lock()
		defer a.speed.mu.Unlock()
		a.speed.phase = ""
		if err != nil {
			a.speed.state, a.speed.err = "error", err.Error()
			return
		}
		a.speed.state, a.speed.result = "done", &res
	}()
	writeJSON(w, http.StatusAccepted, a.speedView())
}

// sqmQoSDiffers reports whether any network's priority or limits in the draft differ from those in effect.
func sqmQoSDiffers(active, draft config.NetworkConfig) bool {
	if active.NetworkQoS(config.DefaultNetworkID) != draft.NetworkQoS(config.DefaultNetworkID) {
		return true
	}
	for _, n := range draft.Networks {
		if active.NetworkQoS(n.ID) != draft.NetworkQoS(n.ID) {
			return true
		}
	}
	return false
}

// qosErrorsOnly keeps, of the problems with the networks, only the ones about their priorities and limits (the rest are those pages' own),
// and names the field as the page does: net.<id>.priority, net.<id>.limit_down_mbit, net.<id>.limit_up_mbit.
func qosErrorsOnly(c config.NetworkConfig, err error) error {
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		return err
	}
	var out config.ValidationError
	for _, e := range ve {
		f := e.Field
		if !strings.Contains(f, "qos_") {
			if strings.HasPrefix(f, "sqm.") {
				out = append(out, e)
			}
			continue
		}
		id, rest := config.DefaultNetworkID, strings.TrimPrefix(f, "lan_")
		if strings.HasPrefix(f, "networks[") {
			end := strings.Index(f, "].")
			if end < 0 {
				continue
			}
			i, convErr := strconv.Atoi(f[len("networks["):end])
			if convErr != nil || i < 0 || i >= len(c.Networks) {
				continue
			}
			id, rest = c.Networks[i].ID, f[end+2:]
		}
		rest = strings.NewReplacer("qos_priority", "priority", "qos_limit_down_kbit", "limit_down_mbit", "qos_limit_up_kbit", "limit_up_mbit").Replace(rest)
		out = append(out, config.FieldError{Field: "net." + id + "." + rest, Message: e.Message})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

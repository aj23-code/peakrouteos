package netapi

import (
	"context"
	"errors"
	"net/http"
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
	Editable       bool               `json:"editable"`
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
		Pending: active.SQMSettings() != draft.SQMSettings(), Running: active.SQMActive(), WanDevice: draft.WanDevice(), Editable: a.editable(),
	}
	if out.Running && a.SQMStatus != nil {
		st := a.SQMStatus(ctx)
		out.Status = &st
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
		Link         *string  `json:"link"`
		PerHost      *bool    `json:"per_host"`
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
		c.SQM = &s
		return ownedErrorsOrNil(c.Validate(), "sqm.download_kbit", "sqm.upload_kbit", "sqm.percent", "sqm.link", "sqm.enabled")
	})
	var ve config.ValidationError
	if errors.As(err, &ve) {
		// the page speaks Mbit/s
		for i := range ve {
			ve[i].Field = strings.NewReplacer("sqm.download_kbit", "download_mbit", "sqm.upload_kbit", "upload_mbit", "sqm.percent", "percent", "sqm.link", "link", "sqm.enabled", "enabled").Replace(ve[i].Field)
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
	result *speedtest.Result
	err    string
}

type speedResponse struct {
	State  string            `json:"state"` // "idle", "running", "done" or "error"
	Phase  string            `json:"phase,omitempty"`
	Result *speedtest.Result `json:"result,omitempty"`
	Error  string            `json:"error,omitempty"`
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
	return speedResponse{State: st, Phase: a.speed.phase, Result: a.speed.result, Error: a.speed.err, Pauses: a.Manager.NetworkConfig().SQMActive()}
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
	a.speed.mu.Lock()
	if a.speed.state == "running" {
		a.speed.mu.Unlock()
		writeJSON(w, http.StatusConflict, errorBody{Error: "a speed test is already running"})
		return
	}
	a.speed.state, a.speed.phase, a.speed.result, a.speed.err = "running", "starting", nil, ""
	a.speed.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		res, err := a.SpeedTest(ctx, func(phase string) {
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

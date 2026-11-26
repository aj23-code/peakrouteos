package netapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"peakroute/internal/config"
	"peakroute/internal/network"
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
	Enabled      bool               `json:"enabled"`
	DownloadMbit float64            `json:"download_mbit"`
	UploadMbit   float64            `json:"upload_mbit"`
	Link         string             `json:"link"`
	Links        []SQMLink          `json:"links"`
	PerHost      bool               `json:"per_host"`
	Pending      bool               `json:"pending"` // the draft differs from what runs
	Running      bool               `json:"running"` // shaping is in effect now (the committed configuration)
	Status       *network.SQMStatus `json:"status"`  // how the kernel's queues are doing (null: not reported)
	WanDevice    string             `json:"wan_device"`
	Editable     bool               `json:"editable"`
}

func (a *API) sqmView(ctx context.Context) sqmResponse {
	draft, _ := a.Manager.StagedNetworkConfig()
	active := a.Manager.NetworkConfig()
	s := draft.SQMSettings()
	out := sqmResponse{
		Enabled: s.Enabled, DownloadMbit: float64(s.DownloadKbit) / 1000, UploadMbit: float64(s.UploadKbit) / 1000,
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
		if req.Link != nil {
			s.Link = strings.TrimSpace(*req.Link)
		}
		if req.PerHost != nil {
			s.PerHost = *req.PerHost
		}
		c.SQM = &s
		return ownedErrorsOrNil(c.Validate(), "sqm.download_kbit", "sqm.upload_kbit", "sqm.link", "sqm.enabled")
	})
	var ve config.ValidationError
	if errors.As(err, &ve) {
		// the page speaks Mbit/s
		for i := range ve {
			ve[i].Field = strings.NewReplacer("sqm.download_kbit", "download_mbit", "sqm.upload_kbit", "upload_mbit", "sqm.link", "link", "sqm.enabled", "enabled").Replace(ve[i].Field)
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

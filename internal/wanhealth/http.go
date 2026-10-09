package wanhealth

import (
	"encoding/json"
	"net/http"
	"time"
)

type historyResponse struct {
	Range      string     `json:"range"`
	Status     Status     `json:"status"`
	Summary    Summary    `json:"summary"`
	Timestamps []int64    `json:"timestamps"` // Unix milliseconds
	AvgMS      []*float64 `json:"avg_ms"`     // null where every probe was lost
	MinMS      []*float64 `json:"min_ms"`
	MaxMS      []*float64 `json:"max_ms"`
	LossPct    []float64  `json:"loss_pct"`
}

// HandleHistory serves GET ?range=5m|1h|24h|30d: the status, the summary and the chart points.
func (p *Prober) HandleHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := r.URL.Query().Get("range")
	if name == "" {
		name = "1h"
	}
	rng, ok := Ranges[name]
	if !ok {
		http.Error(w, "range must be 5m, 1h, 24h or 30d", http.StatusBadRequest)
		return
	}

	points, summary, err := p.Store.History(rng, time.Now())
	if err != nil {
		http.Error(w, "could not read the history", http.StatusInternalServerError)
		return
	}

	resp := historyResponse{Range: name, Status: p.Status(), Summary: summary,
		Timestamps: []int64{}, AvgMS: []*float64{}, MinMS: []*float64{}, MaxMS: []*float64{}, LossPct: []float64{}}
	for _, pt := range points {
		resp.Timestamps = append(resp.Timestamps, pt.At)
		resp.AvgMS = append(resp.AvgMS, pt.AvgMS)
		resp.MinMS = append(resp.MinMS, pt.MinMS)
		resp.MaxMS = append(resp.MaxMS, pt.MaxMS)
		resp.LossPct = append(resp.LossPct, pt.LossPct)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

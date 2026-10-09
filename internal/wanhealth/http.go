package wanhealth

import (
	"encoding/json"
	"net/http"
	"time"
)

// seriesJSON is the chart data of one series: parallel arrays, one entry per point.
type seriesJSON struct {
	Summary    Summary    `json:"summary"`
	Timestamps []int64    `json:"timestamps"` // Unix milliseconds
	AvgMS      []*float64 `json:"avg_ms"`     // null where every probe was lost
	MinMS      []*float64 `json:"min_ms"`
	MaxMS      []*float64 `json:"max_ms"`
	LossPct    []float64  `json:"loss_pct"`
}

type historyResponse struct {
	Range   string     `json:"range"`
	Status  Status     `json:"status"`
	DNS     seriesJSON `json:"dns"`
	Gateway seriesJSON `json:"gateway"`
	// How reachable the internet has been over the last day and month, whatever range is charted
	Availability Availability `json:"availability"`
}

func seriesOf(points []Point, sum Summary) seriesJSON {
	out := seriesJSON{Summary: sum, Timestamps: []int64{}, AvgMS: []*float64{}, MinMS: []*float64{}, MaxMS: []*float64{}, LossPct: []float64{}}
	for _, pt := range points {
		out.Timestamps = append(out.Timestamps, pt.At)
		out.AvgMS = append(out.AvgMS, pt.AvgMS)
		out.MinMS = append(out.MinMS, pt.MinMS)
		out.MaxMS = append(out.MaxMS, pt.MaxMS)
		out.LossPct = append(out.LossPct, pt.LossPct)
	}
	return out
}

// HandleHistory serves GET ?range=5m|1h|24h|30d: the status and the chart points of both series.
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

	now := time.Now()
	resp := historyResponse{Range: name, Status: p.Status()}
	for _, series := range allSeries {
		points, summary, err := p.Store.History(series, rng, now)
		if err != nil {
			http.Error(w, "could not read the history", http.StatusInternalServerError)
			return
		}
		if series == SeriesDNS {
			resp.DNS = seriesOf(points, summary)
		} else {
			resp.Gateway = seriesOf(points, summary)
		}
	}

	avail, err := p.availability(now)
	if err != nil {
		http.Error(w, "could not read the history", http.StatusInternalServerError)
		return
	}
	resp.Availability = avail

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(resp)
}

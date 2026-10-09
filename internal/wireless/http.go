package wireless

import (
	"encoding/json"
	"net/http"
)

// Handler serves GET /api/v1/wireless: the adapters found and what they can do.
func (d Detector) Handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(d.Detect(r.Context()))
}

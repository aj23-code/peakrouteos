package network

import (
	"encoding/json"
	"net/http"
)

// HandleGetDevices serves the enriched physical device list to the frontend
func HandleGetDevices(w http.ResponseWriter, r *http.Request) {
	ifaces, err := GetPhysicalInterfaces()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"devices": ifaces,
	})
}

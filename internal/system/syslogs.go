package system

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
)

type LogEntry struct {
	Timestamp  string `json:"timestamp"`
	Priority   string `json:"priority"`
	Identifier string `json:"identifier"`
	Message    string `json:"message"`
}

// HandleGetLogs queries the systemd journal and returns parsed entries JSON
func HandleGetLogs(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if l, err := strconv.Atoi(limitStr); err == nil && l > 0 && l <= 1000 {
		limit = l
	}

	cmd := exec.Command("journalctl", "-n", strconv.Itoa(limit), "--output=json", "--no-pager")
	output, err := cmd.Output()
	if err != nil {
		http.Error(w, "Failed to fetch system logs", http.StatusInternalServerError)
		return
	}

	var entries []LogEntry
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(line), &raw); err == nil {
			msg, _ := raw["MESSAGE"].(string)
			ident, _ := raw["SYSLOG_IDENTIFIER"].(string)
			if ident == "" {
				ident, _ = raw["_COMM"].(string)
			}
			prio, _ := raw["PRIORITY"].(string)
			ts, _ := raw["__REALTIME_TIMESTAMP"].(string)

			entries = append(entries, LogEntry{
				Timestamp:  ts,
				Priority:   prio,
				Identifier: ident,
				Message:    msg,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

package netapi

import (
	"net/http"
	"strconv"
)

// FirewallLogEntry is a logged packet as the Rules page shows it.
type FirewallLogEntry struct {
	Time    int64  `json:"time"` // Unix milliseconds
	Rule    string `json:"rule"`
	Name    string `json:"name"`   // the rule's name; its id when the rule is no longer there
	Action  string `json:"action"` // what the rule does ("" when it is gone)
	Text    string `json:"text"`   // the rule in words
	In      string `json:"in"`
	Out     string `json:"out"`
	Src     string `json:"src"`
	SrcPort int    `json:"sport"`
	Dst     string `json:"dst"`
	DstPort int    `json:"dport"`
	Proto   string `json:"proto"`
}

type firewallLogResponse struct {
	Known   bool               `json:"known"`
	Entries []FirewallLogEntry `json:"entries"`
}

const (
	defaultFirewallLog = 200
	maxFirewallLog     = 1000
)

// HandleFirewallLog: GET /api/v1/firewall/log?limit= returns the most recent packets that rules with "log" on matched,
// newest first, each with the rule that matched it (looked up in the configuration in effect).
func (a *API) HandleFirewallLog(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	limit := defaultFirewallLog
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxFirewallLog {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "limit must be a number from 1 to " + strconv.Itoa(maxFirewallLog)})
			return
		}
		limit = n
	}
	resp := firewallLogResponse{Entries: []FirewallLogEntry{}}
	if a.FirewallLog != nil {
		if entries, ok := a.FirewallLog(limit); ok {
			resp.Known = true
			active := a.Manager.NetworkConfig()
			for i := len(entries) - 1; i >= 0; i-- { // newest first
				e := entries[i]
				out := FirewallLogEntry{Time: e.Time, Rule: e.Rule, Name: e.Rule, In: e.In, Out: e.Out, Src: e.Src, SrcPort: e.SrcPort,
					Dst: e.Dst, DstPort: e.DstPort, Proto: e.Proto}
				if rule, ok := active.FirewallSettings().FindRule(e.Rule); ok {
					out.Name, out.Action, out.Text = rule.Name, rule.Action, active.RuleText(rule)
				}
				resp.Entries = append(resp.Entries, out)
				if len(resp.Entries) == limit {
					break
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

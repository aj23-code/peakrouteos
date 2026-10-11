package netapi

import (
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/fwlog"
)

// FirewallLogStore is where the packets that rules with "log" on matched are kept (see internal/fwlog).
type FirewallLogStore interface {
	Query(fwlog.Query) ([]fwlog.Event, error)
	Stats() (fwlog.Stats, error)
}

// FirewallLogEntry is a logged kind of packet as the Logs page shows it.
type FirewallLogEntry struct {
	ID      int64  `json:"id"`
	Time    int64  `json:"time"`  // Unix milliseconds: the first time it was seen
	Last    int64  `json:"last"`  // the last time
	Count   int    `json:"count"` // how many times (repeats within a minute are one entry)
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
	Known       bool               `json:"known"`
	Entries     []FirewallLogEntry `json:"entries"`
	Next        int64              `json:"next"`         // pass as before= for the next page; 0: that was all
	Total       int                `json:"total"`        // entries in the whole log
	Oldest      int64              `json:"oldest"`       // when the oldest entry was first seen; 0 when empty
	KeepDays    int                `json:"keep_days"`    // how long the log is kept...
	KeepEntries int                `json:"keep_entries"` // ...and how many entries at most
}

const (
	defaultFirewallLog = 200
	maxFirewallLog     = 1000
	maxLogSearch       = 100
)

// logQuery turns the request's filters into a store query. ok is false (and the answer written) for a bad value.
//
//	limit=      how many (1 to 1000, default 200)
//	before=     the entry id to continue after (the "next" of the last page)
//	rule=       one rule, by id
//	action=     allow, block or reject: the rules that do that
//	network=    the rules for traffic from a Local Network (its id) or the internet ("wan")
//	hours=      only what was seen in the last so many hours
//	q=          text to find in an address, interface, protocol or port
//
// Rule, action and network narrow each other; they are checked against the rules in effect, which are the rules the log
// is about. A rule that is no longer there can only be asked for by its id.
func (a *API) logQuery(w http.ResponseWriter, r *http.Request, defaultLimit int) (fwlog.Query, bool) {
	var q fwlog.Query
	bad := func(msg string) (fwlog.Query, bool) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: msg})
		return q, false
	}
	v := r.URL.Query()
	q.Limit = defaultLimit
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxFirewallLog {
			return bad("limit must be a number from 1 to " + strconv.Itoa(maxFirewallLog))
		}
		q.Limit = n
	}
	if s := v.Get("before"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 {
			return bad("before must be an entry number")
		}
		q.Before = n
	}
	if s := v.Get("hours"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 24*fwlog.DefaultKeepDays {
			return bad("hours must be a number from 1 to " + strconv.Itoa(24*fwlog.DefaultKeepDays))
		}
		q.Since = time.Now().Add(-time.Duration(n) * time.Hour).UnixMilli()
	}
	if s := strings.TrimSpace(v.Get("q")); s != "" {
		if len(s) > maxLogSearch {
			return bad("the search text is too long")
		}
		q.Search = s
	}
	rule, action, network := v.Get("rule"), v.Get("action"), v.Get("network")
	if action != "" && action != config.ActionAllow && action != config.ActionBlock && action != config.ActionReject {
		return bad("action must be allow, block or reject")
	}
	if action != "" || network != "" {
		q.RulesSet = true
		q.Rules = []string{}
		for _, fr := range a.Manager.NetworkConfig().FirewallSettings().Rules {
			if (rule == "" || fr.ID == rule) && (action == "" || fr.Action == action) && (network == "" || fr.From == network) {
				q.Rules = append(q.Rules, fr.ID)
			}
		}
	} else if rule != "" {
		q.RulesSet, q.Rules = true, []string{rule}
	}
	return q, true
}

// describe adds what the rules in effect say about an entry.
func describe(active config.NetworkConfig, e fwlog.Event) FirewallLogEntry {
	out := FirewallLogEntry{ID: e.ID, Time: e.Time, Last: e.Last, Count: e.Count, Rule: e.Rule, Name: e.Rule, In: e.In, Out: e.Out,
		Src: e.Src, SrcPort: e.SrcPort, Dst: e.Dst, DstPort: e.DstPort, Proto: e.Proto}
	if rule, ok := active.FirewallSettings().FindRule(e.Rule); ok {
		out.Name, out.Action, out.Text = rule.Name, rule.Action, active.RuleText(rule)
	}
	return out
}

// HandleFirewallLog: GET /api/v1/firewall/log returns the packets that rules with "log" on matched, newest first, each
// with the rule that matched it (looked up in the configuration in effect), a page at a time, with how much the log holds.
func (a *API) HandleFirewallLog(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	q, ok := a.logQuery(w, r, defaultFirewallLog)
	if !ok {
		return
	}
	resp := firewallLogResponse{Entries: []FirewallLogEntry{}}
	if a.FirewallLog != nil {
		events, err := a.FirewallLog.Query(q)
		st, err2 := a.FirewallLog.Stats()
		if err == nil && err2 == nil {
			resp.Known = true
			resp.Total, resp.Oldest, resp.KeepDays, resp.KeepEntries = st.Entries, st.Oldest, st.KeepDays, st.KeepEntries
			active := a.Manager.NetworkConfig()
			for _, e := range events {
				resp.Entries = append(resp.Entries, describe(active, e))
			}
			if len(events) == q.Limit {
				resp.Next = events[len(events)-1].ID
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// csvCell stops a spreadsheet from running a cell as a formula (rule names are typed by people).
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// HandleFirewallLogCSV: GET /api/v1/firewall/log.csv downloads what the same filters select (all of it, not a page).
func (a *API) HandleFirewallLogCSV(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	q, ok := a.logQuery(w, r, fwlog.DefaultKeepEntries)
	if !ok {
		return
	}
	q.Limit = 0
	q.Before = 0
	if a.FirewallLog == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "The firewall log cannot be read."})
		return
	}
	events, err := a.FirewallLog.Query(q)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "The firewall log cannot be read."})
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="firewall-log-`+time.Now().UTC().Format("20060102-1504")+`.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	cw := csv.NewWriter(w)
	cw.Write([]string{"first seen (UTC)", "last seen (UTC)", "times", "rule", "action", "in", "out", "source", "source port", "destination", "destination port", "protocol"})
	active := a.Manager.NetworkConfig()
	stamp := func(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04:05") }
	port := func(p int) string {
		if p == 0 {
			return ""
		}
		return strconv.Itoa(p)
	}
	for _, e := range events {
		d := describe(active, e)
		cw.Write([]string{stamp(d.Time), stamp(d.Last), strconv.Itoa(d.Count), csvCell(d.Name), d.Action, csvCell(d.In), csvCell(d.Out),
			csvCell(d.Src), port(d.SrcPort), csvCell(d.Dst), port(d.DstPort), csvCell(d.Proto)})
	}
	cw.Flush()
}

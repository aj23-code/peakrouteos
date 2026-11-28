package filtering

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Query is one DNS question a device asked, as AdGuard Home's query log has it.
type Query struct {
	Time    int64  `json:"time"` // Unix milliseconds
	Name    string `json:"name"`
	Type    string `json:"type"`
	Blocked bool   `json:"blocked"`
	Reason  string `json:"reason,omitempty"` // AdGuard Home's word for what happened (FilteredBlackList...)
	Rule    string `json:"rule,omitempty"`   // the filter rule that blocked it
	Status  string `json:"status,omitempty"` // the DNS answer code (NOERROR, NXDOMAIN...)
	Cached  bool   `json:"cached,omitempty"`
}

// Domain counts the questions about one name.
type Domain struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Report is what one device asked lately.
type Report struct {
	Queries    []Query  `json:"queries"` // the newest first
	Seen       int      `json:"seen"`    // how many questions the numbers below are about (the newest AdGuard Home has of this device)
	Blocked    int      `json:"blocked"`
	Top        []Domain `json:"top"`
	TopBlocked []Domain `json:"top_blocked"`
}

// QueryLog reads AdGuard Home's query log (loopback, no account).
type QueryLog struct {
	Upstream string       // UIAddr unless a test says otherwise
	HTTP     *http.Client // a short timeout unless a test says otherwise
}

const (
	logWindow  = 500 // how many of a device's newest questions are counted
	logShown   = 40  // how many are listed
	logTop     = 8
	maxLogBody = 16 << 20
)

// ForClient reads the questions the device with this address asked. AdGuard Home's search finds the address as part of other text (192.168.1.5
// is in 192.168.1.50), so the answers are checked for the exact client.
func (q *QueryLog) ForClient(ctx context.Context, ip string) (Report, error) {
	up := q.Upstream
	if up == "" {
		up = UIAddr
	}
	hc := q.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 6 * time.Second}
	}
	v := url.Values{"search": {ip}, "limit": {fmt.Sprint(logWindow * 2)}, "response_status": {"all"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+up+"/control/querylog?"+v.Encode(), nil)
	if err != nil {
		return Report{}, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return Report{}, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxLogBody))
	if res.StatusCode != http.StatusOK {
		return Report{}, fmt.Errorf("AdGuard Home answered %s", res.Status)
	}
	var raw struct {
		Data []struct {
			Client   string `json:"client"`
			Time     string `json:"time"`
			Reason   string `json:"reason"`
			Status   string `json:"status"`
			Cached   bool   `json:"cached"`
			Question struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"question"`
			Rules []struct {
				Text string `json:"text"`
			} `json:"rules"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Report{}, fmt.Errorf("the query log could not be read: %w", err)
	}
	rep := Report{Queries: []Query{}, Top: []Domain{}, TopBlocked: []Domain{}}
	all, blocked := map[string]int{}, map[string]int{}
	for _, e := range raw.Data {
		if e.Client != ip {
			continue
		}
		if rep.Seen >= logWindow {
			break
		}
		name := strings.TrimSuffix(strings.ToLower(e.Question.Name), ".")
		qu := Query{Name: name, Type: e.Question.Type, Reason: e.Reason, Status: e.Status, Cached: e.Cached, Blocked: strings.HasPrefix(e.Reason, "Filtered")}
		if t, err := time.Parse(time.RFC3339Nano, e.Time); err == nil {
			qu.Time = t.UnixMilli()
		}
		if len(e.Rules) > 0 {
			qu.Rule = e.Rules[0].Text
		}
		rep.Seen++
		all[name]++
		if qu.Blocked {
			rep.Blocked++
			blocked[name]++
		}
		if len(rep.Queries) < logShown {
			rep.Queries = append(rep.Queries, qu)
		}
	}
	rep.Top, rep.TopBlocked = top(all), top(blocked)
	return rep, nil
}

func top(m map[string]int) []Domain {
	out := make([]Domain, 0, len(m))
	for n, c := range m {
		out = append(out, Domain{n, c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > logTop {
		out = out[:logTop]
	}
	return out
}

// Package ddns keeps public names pointed at the address the internet connection has: when the address changes, and from time to
// time when it does not, the router tells the name's service. docs/ddns.md describes it.
package ddns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"peakroute/internal/config"
)

// outcome says how one request to a service went.
type outcome int

const (
	updated   outcome = iota // the service now has the address
	unchanged                // it already had it
	retry                    // it did not work and may next time: the service is busy, the network is down
	stop                     // it will not work until something is changed: the password, the name
)

// result is what a provider function reports: the outcome and a sentence for the page.
type result struct {
	outcome outcome
	text    string
}

var errRedirect = errors.New("stopped after too many redirects, or a redirect to an address that is not https")

// get fetches an address with the client and reads at most a few kilobytes of the answer.
func (u *Updater) do(ctx context.Context, method, target string, header map[string]string, user, pass string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", u.userAgent())
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	return resp.StatusCode, data, nil
}

// httpOutcome sorts the answers every service shares: who you are, whether it is busy.
func httpOutcome(code int, who string) (result, bool) {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return result{stop, who + " refused the sign-in: check the user name and password or token."}, true
	case code == http.StatusTooManyRequests:
		return result{retry, who + " says there were too many requests: it will be tried again later."}, true
	case code >= 500:
		return result{retry, fmt.Sprintf("%s is not working right now (HTTP %d): it will be tried again.", who, code)}, true
	case code == http.StatusNotFound:
		return result{stop, who + " does not know that address or name (HTTP 404)."}, true
	}
	return result{}, false
}

func firstWord(b []byte) string {
	f := strings.Fields(strings.TrimSpace(string(b)))
	if len(f) == 0 {
		return ""
	}
	return strings.ToLower(f[0])
}

// dyndns2 speaks the protocol DynDNS, No-IP, Dynu and many others share.
func (u *Updater) dyndns2(ctx context.Context, e config.DDNSEntry, ip netip.Addr) result {
	q := url.Values{"hostname": {e.Hostname}, "myip": {ip.String()}}
	code, body, err := u.do(ctx, http.MethodGet, "https://"+e.Server+"/nic/update?"+q.Encode(), nil, e.Username, e.Password, nil)
	if err != nil {
		return result{retry, "Could not reach " + e.Server + ": " + shortErr(err)}
	}
	if r, ok := httpOutcome(code, e.Server); ok {
		return r
	}
	switch word := firstWord(body); word {
	case "good":
		return result{updated, "updated to " + ip.String()}
	case "nochg":
		return result{unchanged, "already " + ip.String()}
	case "badauth":
		return result{stop, e.Server + " refused the user name or password."}
	case "nohost":
		return result{stop, e.Server + " has no such name under this account."}
	case "notfqdn":
		return result{stop, e.Server + " says the name is not a full name."}
	case "numhost":
		return result{stop, e.Server + " says there are too many names."}
	case "abuse":
		return result{stop, e.Server + " has blocked this account for updating too often. Wait, and change something to try again."}
	case "badagent", "badsys":
		return result{stop, e.Server + " refused this router's updates (" + word + ")."}
	case "!donator":
		return result{stop, e.Server + " says that needs a paid account."}
	case "dnserr", "911":
		return result{retry, e.Server + " has a problem at its end (" + word + "): it will be tried again."}
	}
	return result{retry, e.Server + " answered something unexpected: " + clip(string(body))}
}

// duckdns updates a name under duckdns.org with the account's token.
func (u *Updater) duckdns(ctx context.Context, e config.DDNSEntry, ip netip.Addr) result {
	sub := strings.TrimSuffix(e.Hostname, ".duckdns.org")
	q := url.Values{"domains": {sub}, "token": {e.Password}, "ip": {ip.String()}}
	code, body, err := u.do(ctx, http.MethodGet, u.duckURL()+"?"+q.Encode(), nil, "", "", nil)
	if err != nil {
		return result{retry, "Could not reach DuckDNS: " + shortErr(err)}
	}
	if r, ok := httpOutcome(code, "DuckDNS"); ok {
		return r
	}
	switch firstWord(body) {
	case "ok":
		return result{updated, "updated to " + ip.String()}
	case "ko":
		return result{stop, "DuckDNS refused it: check the name and the token."}
	}
	return result{retry, "DuckDNS answered something unexpected: " + clip(string(body))}
}

// custom calls an address with the placeholders filled in (each value escaped for use in an address).
func (u *Updater) custom(ctx context.Context, e config.DDNSEntry, ip netip.Addr) result {
	target := strings.NewReplacer(
		"{ip}", url.QueryEscape(ip.String()), "{hostname}", url.QueryEscape(e.Hostname),
		"{username}", url.QueryEscape(e.Username), "{password}", url.QueryEscape(e.Password),
	).Replace(e.URL)
	host := e.URL
	if p, err := url.Parse(strings.NewReplacer("{ip}", "x", "{hostname}", "x", "{username}", "x", "{password}", "x").Replace(e.URL)); err == nil {
		host = p.Hostname()
	}
	code, body, err := u.do(ctx, http.MethodGet, target, nil, "", "", nil)
	if err != nil {
		return result{retry, "Could not reach " + host + ": " + shortErr(err)}
	}
	if r, ok := httpOutcome(code, host); ok {
		return r
	}
	if code >= 200 && code < 300 {
		return result{updated, "updated to " + ip.String()}
	}
	return result{stop, fmt.Sprintf("%s answered HTTP %d: %s", host, code, clip(string(body)))}
}

type cfEnvelope struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (u *Updater) cf(ctx context.Context, e config.DDNSEntry, method, path string, query url.Values, payload any) (cfEnvelope, *result) {
	var body []byte
	header := map[string]string{"Authorization": "Bearer " + e.Password, "Accept": "application/json"}
	if payload != nil {
		body, _ = json.Marshal(payload)
		header["Content-Type"] = "application/json"
	}
	target := u.cloudflareURL() + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	code, data, err := u.do(ctx, method, target, header, "", "", body)
	if err != nil {
		return cfEnvelope{}, &result{retry, "Could not reach Cloudflare: " + shortErr(err)}
	}
	var env cfEnvelope
	json.Unmarshal(data, &env)
	if r, ok := httpOutcome(code, "Cloudflare"); ok {
		if len(env.Errors) > 0 && r.outcome == stop {
			r.text += " (" + env.Errors[0].Message + ")"
		}
		return env, &r
	}
	if !env.Success {
		msg := "Cloudflare refused the request"
		if len(env.Errors) > 0 {
			msg += ": " + env.Errors[0].Message
		}
		return env, &result{stop, msg}
	}
	return env, nil
}

// cloudflare keeps an A record of a zone the token may edit: it finds the zone (the name with labels taken off from the
// left until one is found), the record, and changes it, or adds it when there is none.
func (u *Updater) cloudflare(ctx context.Context, e config.DDNSEntry, ip netip.Addr) result {
	labels := strings.Split(e.Hostname, ".")
	var zoneID string
	for i := 0; i+2 <= len(labels) && zoneID == ""; i++ {
		name := strings.Join(labels[i:], ".")
		env, bad := u.cf(ctx, e, http.MethodGet, "/zones", url.Values{"name": {name}, "status": {"active"}}, nil)
		if bad != nil {
			return *bad
		}
		var zones []struct {
			ID string `json:"id"`
		}
		json.Unmarshal(env.Result, &zones)
		if len(zones) > 0 {
			zoneID = zones[0].ID
		}
	}
	if zoneID == "" {
		return result{stop, "Cloudflare has no zone for " + e.Hostname + " that this token can see."}
	}
	env, bad := u.cf(ctx, e, http.MethodGet, "/zones/"+zoneID+"/dns_records", url.Values{"type": {"A"}, "name": {e.Hostname}}, nil)
	if bad != nil {
		return *bad
	}
	var recs []struct {
		ID      string `json:"id"`
		Content string `json:"content"`
		TTL     int    `json:"ttl"`
		Proxied bool   `json:"proxied"`
	}
	json.Unmarshal(env.Result, &recs)
	if len(recs) == 0 {
		_, bad := u.cf(ctx, e, http.MethodPost, "/zones/"+zoneID+"/dns_records", nil, map[string]any{"type": "A", "name": e.Hostname, "content": ip.String(), "ttl": 1, "proxied": false})
		if bad != nil {
			return *bad
		}
		return result{updated, "added the record, " + ip.String()}
	}
	rec := recs[0]
	if rec.Content == ip.String() {
		return result{unchanged, "already " + ip.String()}
	}
	ttl := rec.TTL
	if ttl == 0 {
		ttl = 1
	}
	_, bad = u.cf(ctx, e, http.MethodPut, "/zones/"+zoneID+"/dns_records/"+rec.ID, nil, map[string]any{"type": "A", "name": e.Hostname, "content": ip.String(), "ttl": ttl, "proxied": rec.Proxied})
	if bad != nil {
		return *bad
	}
	return result{updated, "updated to " + ip.String()}
}

func shortErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return clip(err.Error())
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

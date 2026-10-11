package netapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// The DNS lookup test: ask the router's own DNS server a name (or an address) and say what came back and how long it took.
// It asks what is running now, not the draft. docs/dns.md describes it.

const defaultDNSAddr = "127.0.0.1:53"

// dnsLookupTimeout is how long the router's DNS server has to answer (a variable so that a test need not wait three seconds).
var dnsLookupTimeout = 3 * time.Second

// DNSLookup is the result of one question put to the router's DNS server.
type DNSLookup struct {
	Query   string   `json:"query"`   // what was asked, as it was asked
	Kind    string   `json:"kind"`    // "name" (its addresses) or "address" (its names)
	Server  string   `json:"server"`  // who was asked
	Status  string   `json:"status"`  // "answered", "not_found", "timeout" or "error"
	Answers []string `json:"answers"` // addresses, or names for a reverse lookup
	Millis  float64  `json:"millis"`  // how long the answer took
	Message string   `json:"message"` // the result in words
}

// usableLookupName accepts what a person may type into the box: a host name or a full name (letters, digits, hyphens,
// underscores and dots), or an IPv4 address.
func usableLookupName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return !strings.HasPrefix(s, ".") && !strings.Contains(s, "..")
}

// lookup asks the router's DNS server one question.
func (a *API) lookup(ctx context.Context, query string) DNSLookup {
	server := a.DNSAddr
	if server == "" {
		server = defaultDNSAddr
	}
	res := DNSLookup{Query: query, Kind: "name", Server: server, Answers: []string{}}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: dnsLookupTimeout}).DialContext(ctx, network, server)
	}}
	ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()

	start := time.Now()
	var err error
	if ip, perr := netip.ParseAddr(query); perr == nil && ip.Is4() {
		res.Kind = "address"
		var names []string
		names, err = resolver.LookupAddr(ctx, query)
		for _, n := range names {
			res.Answers = append(res.Answers, strings.TrimSuffix(n, "."))
		}
	} else {
		// the final dot makes it the full name: the router's own search list must not change the question
		var ips []net.IP
		ips, err = resolver.LookupIP(ctx, "ip4", query+".")
		for _, ip := range ips {
			res.Answers = append(res.Answers, ip.String())
		}
	}
	res.Millis = float64(time.Since(start).Microseconds()) / 1000
	if res.Answers == nil {
		res.Answers = []string{}
	}

	var dnsErr *net.DNSError
	switch {
	case err == nil && len(res.Answers) > 0:
		res.Status = "answered"
		res.Message = "The router answered."
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		res.Status = "not_found"
		res.Message = "The router answered that there is no such name."
	case errors.As(err, &dnsErr) && dnsErr.IsTimeout, errors.Is(err, context.DeadlineExceeded):
		res.Status = "timeout"
		res.Message = "The router's DNS server did not answer in time. It may be stopped, restarting, or waiting for an upstream server that does not reply."
	case err == nil:
		res.Status = "not_found"
		res.Message = "The router answered that there is no such name."
	case errors.As(err, &dnsErr) && strings.Contains(dnsErr.Err, "misbehaving"):
		res.Status = "error"
		res.Message = "The router's DNS server answered with an error (it could not get an answer from the servers it asks)."
	default:
		res.Status = "error"
		res.Message = "The question could not be asked: " + err.Error()
	}
	return res
}

// HandleDNSLookup is GET /api/v1/dns/lookup?name=...
func (a *API) HandleDNSLookup(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.URL.Query().Get("name")), "."))
	if !usableLookupName(name) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "Type a name such as nas or www.example.com, or an address such as 192.168.1.50."})
		return
	}
	writeJSON(w, http.StatusOK, a.lookup(r.Context(), name))
}

// Package filtering is the router's side of DNS filtering with AdGuard Home (docs/dns-filtering.md): a way into AdGuard Home's own web
// interface behind this router's sign-in, and a look at how it is doing. AdGuard Home itself is configured to have no accounts and to
// listen on the loopback only; what stands in front of it here is the router's session (and its read-only accounts).
package filtering

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"peakroute/internal/auth"
)

const (
	// UIAddr is where AdGuard Home's web interface listens (the image's seed configuration says so); only the proxy reaches it.
	UIAddr = "127.0.0.1:3001"
	// ProxyPort is where the router offers it, signed in with the router's own session.
	ProxyPort = "3000"
	// Binary is the program the image carries.
	Binary = "/usr/lib/peakroute/AdGuardHome"
)

// Sessions is what the proxy needs of the router's sign-in.
type Sessions interface {
	Lookup(r *http.Request) (auth.SessionInfo, bool)
}

// Proxy shows AdGuard Home's web interface to whoever is signed in to the router.
type Proxy struct {
	Sessions Sessions
	Upstream string // UIAddr unless a test says otherwise
	MainPort string // the port of the router's own web interface, for the way back to its sign-in (":443")
}

func (p *Proxy) upstream() string {
	if p.Upstream != "" {
		return p.Upstream
	}
	return UIAddr
}

// sameOrigin is the defence against another web site making a signed-in browser change AdGuard Home's settings: a request that
// changes something must say it comes from this very page. (The router's own pages use a token for that; AdGuard Home's pages cannot.)
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	info, ok := p.Sessions.Lookup(r)
	if !ok {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>Sign in</title><p>Sign in to the router first: <a href=\"https://%s\">https://%s</a>. Then open AdGuard Home from its DNS filtering page.",
			html.EscapeString(net.JoinHostPort(host, strings.TrimPrefix(p.MainPort, ":"))), html.EscapeString(host))
		return
	}
	if !auth.SafeMethod(r.Method) {
		if !auth.CanWrite(info.Role) {
			http.Error(w, "This account is read-only.", http.StatusForbidden)
			return
		}
		if !sameOrigin(r) {
			http.Error(w, "A change must come from this page.", http.StatusForbidden)
			return
		}
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: p.upstream()})
			pr.Out.Host = p.upstream()
			// AdGuard Home has no use for the router's sign-in cookie, and no reason to see it
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "AdGuard Home is not running. It starts when a network turns filtering on and the change is committed.", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// Status is how filtering stands, for the DNS filtering page.
type Status struct {
	Installed bool   `json:"installed"` // the image carries AdGuard Home
	Running   bool   `json:"running"`   // its web interface answers
	Version   string `json:"version"`
	Protected bool   `json:"protection_enabled"` // AdGuard Home's own protection switch (it can be paused in its interface)
	UIPort    string `json:"ui_port"`
}

// ReadStatus asks AdGuard Home (no account is needed on the loopback). binary is where the program should be.
func ReadStatus(ctx context.Context, binary, upstream string) Status {
	st := Status{UIPort: ProxyPort}
	if _, err := os.Stat(binary); err == nil {
		st.Installed = true
	}
	if upstream == "" {
		upstream = UIAddr
	}
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+upstream+"/control/status", nil)
	if err != nil {
		return st
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return st
	}
	defer res.Body.Close()
	var body struct {
		Version string `json:"version"`
		Running bool   `json:"running"`
		Protect bool   `json:"protection_enabled"`
	}
	if res.StatusCode == http.StatusOK && json.NewDecoder(res.Body).Decode(&body) == nil {
		st.Running, st.Version, st.Protected = body.Running, body.Version, body.Protect
	}
	return st
}

// Server offers the proxy over HTTPS on its own port while it is wanted (while any network filters).
type Server struct {
	Proxy *Proxy
	TLS   *tls.Config
	Addr  string // ":3000" unless a test says otherwise
}

// Run serves until ctx ends; it returns by itself when the port cannot be opened (the caller looks again later).
func (s *Server) Run(ctx context.Context) {
	addr := s.Addr
	if addr == "" {
		addr = ":" + ProxyPort
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Proxy,
		TLSConfig:         s.TLS,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute, // AdGuard Home's query log can take a while
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			srv.Shutdown(shutdown)
		case <-done:
		}
	}()
	err := srv.ListenAndServeTLS("", "")
	close(done)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("filtering: AdGuard Home's page cannot be offered on %s: %v", addr, err)
	}
}

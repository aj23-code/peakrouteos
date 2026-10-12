package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	// no script that is not a file of ours: no inline script, no eval, nothing from elsewhere
	for _, want := range []string{"default-src 'self'", "script-src 'self';", "object-src 'none'", "base-uri 'self'", "form-action 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("the policy lacks %q: %s", want, csp)
		}
	}
	scripts := csp[strings.Index(csp, "script-src"):]
	scripts = scripts[:strings.Index(scripts, ";")]
	if strings.Contains(scripts, "unsafe") || strings.Contains(scripts, "*") || strings.Contains(scripts, "http") {
		t.Errorf("script-src is not strict: %s", scripts)
	}
	for header, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Strict-Transport-Security": "max-age=31536000; includeSubDomains"} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s: %q", header, got)
		}
	}
	if rec.Body.String() != "ok" {
		t.Error("the page was not passed on")
	}
}

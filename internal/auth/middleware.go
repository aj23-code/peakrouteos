// SecurityHeadersMiddleware injects defensive HTTP response headers into every request.
package auth

import (
	"net/http"
)

func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Prevent clickjacking by restricting framing
		w.Header().Set("X-Frame-Options", "DENY")

		// Prevent MIME-sniffing
		w.Header().Set("X-Content-Type-Options", "nosniff")

		// Restrict referrer information sent on cross-origin requests
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// Enforce HTTPS-only access for 1 year
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")

		// Restrict sources from which content can be loaded (XSS mitigation)
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'")

		next.ServeHTTP(w, r)
	})
}

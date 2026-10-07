package auth

import (
	"crypto/subtle"
	"net/http"
)

const (
	CSRFHeader    = "X-CSRF-Token"
	CSRFFormField = "csrf_token"
)

// SafeMethod reports whether an HTTP method is read-only and so needs no CSRF token.
func SafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func tokensMatch(got, expected string) bool {
	return expected != "" && subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

// CheckCSRF reports whether the request carries the session's CSRF token in the X-CSRF-Token
// header (how the web UI sends it with fetch).
func CheckCSRF(r *http.Request, expected string) bool {
	return tokensMatch(r.Header.Get(CSRFHeader), expected)
}

// CheckCSRFForm is for plain HTML form posts (such as logout): the token is a hidden field.
// The body is size-limited before it is parsed.
func CheckCSRFForm(w http.ResponseWriter, r *http.Request, expected string) bool {
	if CheckCSRF(r, expected) {
		return true
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return false
	}
	return tokensMatch(r.PostForm.Get(CSRFFormField), expected)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":"` + message + `"}`))
}

// RequireAuth rejects requests without a valid session (401) and, for anything that changes
// state, requests without that session's CSRF token (403), before calling next.
func RequireAuth(sm *SessionManager, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		info, ok := sm.Lookup(r)
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		if !SafeMethod(r.Method) && !CheckCSRF(r, info.CSRFToken) {
			writeJSONError(w, http.StatusForbidden, "missing or invalid CSRF token")
			return
		}

		next(w, r)
	}
}

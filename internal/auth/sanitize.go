package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
)

// LimitPayloadSize wraps the request body to prevent memory exhaustion DoS attacks.
// maxBytes specifies the maximum allowed size (e.g., 1MB for API routes, 10MB for certs).
func LimitPayloadSize(w http.ResponseWriter, r *http.Request, maxBytes int64) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
}

// SanitizeString scrubs basic path traversal, dangerous characters, and trailing whitespace.
func SanitizeString(input string) string {
	// Trim whitespace
	cleaned := strings.TrimSpace(input)

	// Clean path traversal sequences
	cleaned = filepath.Clean(cleaned)

	// Strip null bytes and common shell control characters
	replacer := strings.NewReplacer(
		"\x00", "",
		"`", "",
		";", "",
		"&", "",
		"|", "",
		"$", "",
	)
	return replacer.Replace(cleaned)
}

// DecodeJSONStrict decodes JSON and enforces unknown fields, body size limits, and Content-Type.
func DecodeJSONStrict(w http.ResponseWriter, r *http.Request, dst interface{}, maxBytes int64) error {
	// 1. Enforce Content-Type header
	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") {
		return fmt.Errorf("unsupported content-type: expected application/json")
	}

	// 2. Limit body size
	LimitPayloadSize(w, r, maxBytes)

	// 3. Strict JSON decoding (reject unexpected fields)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return fmt.Errorf("payload too large (max %d bytes)", maxBytes)
		}
		return fmt.Errorf("invalid json payload: %v", err)
	}

	// Ensure there is no leftover trailing garbage after the JSON object
	if decoder.More() {
		return errors.New("body must only contain a single JSON object")
	}

	return nil
}

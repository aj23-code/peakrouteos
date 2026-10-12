package cert

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"peakroute/internal/auth"
)

// The API of the Certificates page. Nothing here is staged and committed like the network settings: a certificate applies at once
// (like an administrator's password), because it is not a change that can lock the administrator out, only one that makes a browser
// ask again whether to trust the router.

type errBody struct {
	Error string `json:"error"`
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[cert] writing response: %v", err)
	}
}

func methods(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", allowed[0])
	write(w, http.StatusMethodNotAllowed, errBody{"method not allowed"})
	return false
}

// HandleStatus: GET /api/v1/certificates.
func (m *Manager) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if methods(w, r, http.MethodGet) {
		write(w, http.StatusOK, m.Status())
	}
}

// HandleExtraNames: PUT /api/v1/certificates/names {"names": [...]} sets the names and addresses added to the built-in certificate;
// the certificate is made again at once when it is the built-in one.
func (m *Manager) HandleExtraNames(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPut) {
		return
	}
	var req struct {
		Names []string `json:"names"`
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 16<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	if _, err := SetExtraNames(req.Names); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	if !Custom() {
		if err := m.issue(); err != nil {
			write(w, http.StatusInternalServerError, errBody{"The certificate could not be made: " + err.Error()})
			return
		}
	}
	write(w, http.StatusOK, m.Status())
}

// HandleRegenerate: POST /api/v1/certificates/regenerate {"replace_custom": bool} makes the built-in certificate now. It takes the place
// of a custom certificate only when the request says so.
func (m *Manager) HandleRegenerate(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	var req struct {
		ReplaceCustom bool `json:"replace_custom"`
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 4<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	if Custom() && !req.ReplaceCustom {
		write(w, http.StatusConflict, errBody{"A custom certificate is installed. Making the built-in one replaces it: say so to go on."})
		return
	}
	if err := m.issue(); err != nil {
		write(w, http.StatusInternalServerError, errBody{"The certificate could not be made: " + err.Error()})
		return
	}
	write(w, http.StatusOK, m.Status())
}

// HandleUpload: POST /api/v1/certificates/upload (multipart: cert and key) installs a certificate and its private key.
func (m *Manager) HandleUpload(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		write(w, http.StatusBadRequest, errBody{"The upload is too big or is not a form."})
		return
	}
	read := func(field string) ([]byte, error) {
		f, _, err := r.FormFile(field)
		if err != nil {
			return nil, errors.New("The " + field + " file is missing.")
		}
		defer f.Close()
		return io.ReadAll(f)
	}
	certPEM, err := read("cert")
	if err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	keyPEM, err := read("key")
	if err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	if err := m.Install(certPEM, keyPEM); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	write(w, http.StatusOK, m.Status())
}

// HandleAuthority: GET /api/v1/certificates/authority.crt is the web authority's certificate (public) for a computer or a phone to trust.
func (m *Manager) HandleAuthority(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodGet) {
		return
	}
	b, err := m.AuthorityPEM()
	if err != nil {
		write(w, http.StatusInternalServerError, errBody{"The authority's certificate could not be read."})
		return
	}
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="peakroute-web-ca.crt"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

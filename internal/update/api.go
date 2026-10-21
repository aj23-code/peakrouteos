package update

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"peakroute/internal/auth"
)

// The API of the Updates page (docs/updates.md, section 6). A read-only account may read the state; everything else is refused by the
// authentication wrapper for it before it gets here.

type errBody struct {
	Error string `json:"error"`
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[update] writing response: %v", err)
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

// longRequest lifts the server's short limits for a request that legitimately takes minutes (the daemon sets them for everything else).
func longRequest(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(d))
	rc.SetWriteDeadline(time.Now().Add(d))
}

// maxUpload is the biggest update file accepted (the system is far smaller; this only stops a runaway upload).
const maxUpload = int64(8) << 30

// HandleState: GET /api/v1/updates.
func (s *Service) HandleState(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodGet) {
		return
	}
	write(w, http.StatusOK, s.State(r.Context()))
}

// HandleCheck: POST /api/v1/updates/check asks GitHub now.
func (s *Service) HandleCheck(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	longRequest(w, time.Minute)
	s.CheckNow(r.Context())
	write(w, http.StatusOK, s.State(r.Context()))
}

// HandleInstall: POST /api/v1/updates/install {"version"} downloads the release the last check found.
func (s *Service) HandleInstall(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Version string `json:"version"`
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 4<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	if err := s.Install(req.Version); err != nil {
		write(w, http.StatusConflict, errBody{capital(err.Error())})
		return
	}
	write(w, http.StatusAccepted, s.State(r.Context()))
}

// HandleCancel: POST /api/v1/updates/cancel stops a download.
func (s *Service) HandleCancel(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	s.Cancel()
	write(w, http.StatusOK, s.State(r.Context()))
}

// HandleUpload: POST /api/v1/updates/upload with the update file as the body. It answers when the file is written.
func (s *Service) HandleUpload(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	longRequest(w, 2*time.Hour)
	body := http.MaxBytesReader(w, r.Body, maxUpload)
	if _, err := s.Upload(r.Context(), body, r.ContentLength); err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, ErrBusy) || errors.Is(err, ErrNotEditable) {
			status = http.StatusConflict
		}
		write(w, status, errBody{capital(err.Error())})
		return
	}
	write(w, http.StatusOK, s.State(r.Context()))
}

// handleAction wraps restart, keep and go back.
func (s *Service) handleAction(do func(r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !methods(w, r, http.MethodPost) {
			return
		}
		if err := do(r); err != nil {
			write(w, http.StatusConflict, errBody{capital(err.Error())})
			return
		}
		write(w, http.StatusOK, s.State(r.Context()))
	}
}

// HandleRestart: POST /api/v1/updates/restart starts the written version on trial.
func (s *Service) HandleRestart(w http.ResponseWriter, r *http.Request) {
	s.handleAction(func(r *http.Request) error { return s.Restart(r.Context()) })(w, r)
}

// HandleKeep: POST /api/v1/updates/keep keeps the version on trial.
func (s *Service) HandleKeep(w http.ResponseWriter, r *http.Request) {
	s.handleAction(func(r *http.Request) error { return s.Keep(r.Context()) })(w, r)
}

// HandleRevert: POST /api/v1/updates/revert goes back to the version that was kept (or tries the earlier one).
func (s *Service) HandleRevert(w http.ResponseWriter, r *http.Request) {
	s.handleAction(func(r *http.Request) error { return s.GoBack(r.Context()) })(w, r)
}

// HandleSettings: PUT /api/v1/updates/settings.
func (s *Service) HandleSettings(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPut) {
		return
	}
	var in Settings
	if err := auth.DecodeJSONStrict(w, r, &in, 4<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	if err := s.SetSettings(in); err != nil {
		write(w, http.StatusBadRequest, errBody{capital(err.Error())})
		return
	}
	write(w, http.StatusOK, s.State(r.Context()))
}

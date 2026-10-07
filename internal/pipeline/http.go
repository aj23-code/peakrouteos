package pipeline

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"peakroute/internal/config"
	"peakroute/internal/translator"
)

// The HTTP layer is deliberately thin: it maps requests onto Service methods and errors onto status
// codes. Authentication and CSRF are applied by the caller (auth.RequireAuth).

type errorBody struct {
	Error  string              `json:"error"`
	Errors []config.FieldError `json:"errors,omitempty"` // per-field problems for a rejected commit
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[pipeline] writing response: %v", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	var ve config.ValidationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "The staged settings are not valid: " + ve.Error(), Errors: ve})
	case errors.Is(err, ErrBusy), errors.Is(err, ErrNothingToCommit), errors.Is(err, ErrNothingToConfirm):
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error()})
	case errors.Is(err, translator.ErrUnsupportedMode):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
	default:
		log.Printf("[pipeline] request failed: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
	}
}

// method rejects anything but the one allowed method, with the Allow header.
func method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method == allowed {
		return true
	}
	w.Header().Set("Allow", allowed)
	writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
	return false
}

// HandleStatus: GET /api/v1/config/status. Polled by the banner and the confirm countdown.
func (s *Service) HandleStatus(w http.ResponseWriter, r *http.Request) {
	if method(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, s.Status())
	}
}

// HandleStaged: GET /api/v1/config/staged. The review shown before committing.
func (s *Service) HandleStaged(w http.ResponseWriter, r *http.Request) {
	if method(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, s.Review())
	}
}

// HandleCommit: POST /api/v1/config/commit. Answers 202 once the apply is under way; poll status.
func (s *Service) HandleCommit(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if err := s.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "status": s.Status()})
}

// HandleConfirm: POST /api/v1/config/confirm.
func (s *Service) HandleConfirm(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if err := s.Confirm(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Status())
}

// HandleRevert: POST /api/v1/config/revert. Undo the change that is waiting for confirmation now.
func (s *Service) HandleRevert(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if err := s.Revert(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "status": s.Status()})
}

// HandleDiscard: POST /api/v1/config/discard. Throw away the staged changes.
func (s *Service) HandleDiscard(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	if err := s.Discard(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.Status())
}

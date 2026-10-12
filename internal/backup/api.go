package backup

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"peakroute/internal/auth"
)

// The API of the Backup and restore page (docs/backup.md, section 6). A backup is made with a POST so that the passphrase is in the body,
// never in an address, and so that a read-only account cannot make one (it would hold every key). One key derivation at a time: each
// costs 64 MiB and a quarter of a second.

type errBody struct {
	Error string `json:"error"`
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[backup] writing response: %v", err)
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

var busy = make(chan struct{}, 1)

func takeTurn(w http.ResponseWriter) bool {
	select {
	case busy <- struct{}{}:
		return true
	default:
		write(w, http.StatusTooManyRequests, errBody{"Another backup is being made or opened: try again in a moment."})
		return false
	}
}

func giveTurn() { <-busy }

type infoResponse struct {
	Copies        []Copy `json:"copies"`
	MinPassphrase int    `json:"min_passphrase"`
	HistoryBytes  int64  `json:"history_bytes"` // about how much the databases weigh now
	Editable      bool   `json:"editable"`
}

// HandleInfo: GET /api/v1/backup.
func (s *Service) HandleInfo(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodGet) {
		return
	}
	copies, err := s.Copies()
	if err != nil {
		write(w, http.StatusInternalServerError, errBody{"The copies could not be listed."})
		return
	}
	var hist int64
	for _, f := range HistoryFiles {
		if info, err := os.Stat(filepath.Join(s.ConfigDir, f)); err == nil {
			hist += info.Size()
		}
	}
	write(w, http.StatusOK, infoResponse{Copies: copies, MinPassphrase: MinPassphrase, HistoryBytes: hist, Editable: s.editable()})
}

// HandleCreate: POST /api/v1/backup/create {"passphrase", "confirm", "history"} answers with the file.
func (s *Service) HandleCreate(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
		Confirm    string `json:"confirm"`
		History    bool   `json:"history"`
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 8<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	if err := CheckPassphrase(req.Passphrase); err != nil {
		write(w, http.StatusBadRequest, errBody{capital(err.Error()) + "."})
		return
	}
	if req.Passphrase != req.Confirm {
		write(w, http.StatusBadRequest, errBody{"The two passphrases are not the same."})
		return
	}
	if !takeTurn(w) {
		return
	}
	defer giveTurn()
	data, name, err := s.Create(req.Passphrase, req.History)
	if err != nil {
		write(w, http.StatusInternalServerError, errBody{"The backup could not be made: " + err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func capital(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// HandleOpen: POST /api/v1/backup/open (multipart: file and passphrase) opens a backup and says what is in it.
func (s *Service) HandleOpen(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxFile+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		write(w, http.StatusBadRequest, errBody{"The file is too big or the upload is not a form."})
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		write(w, http.StatusBadRequest, errBody{"Choose the backup file."})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		write(w, http.StatusBadRequest, errBody{"The file could not be read."})
		return
	}
	if !takeTurn(w) {
		return
	}
	defer giveTurn()
	rep, err := s.Inspect(data, r.FormValue("passphrase"))
	if err != nil {
		write(w, http.StatusBadRequest, errBody{capital(err.Error()) + "."})
		return
	}
	write(w, http.StatusOK, rep)
}

// HandlePreview: POST /api/v1/backup/preview {"token", "keep_ports"} is the report again for this router's own ports kept or not.
func (s *Service) HandlePreview(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Token     string `json:"token"`
		KeepPorts bool   `json:"keep_ports"`
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 4<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	rep, err := s.Preview(req.Token, req.KeepPorts)
	if err != nil {
		write(w, http.StatusNotFound, errBody{capital(err.Error()) + "."})
		return
	}
	write(w, http.StatusOK, rep)
}

// HandlePreviewChoice: POST /api/v1/setup/restore/preview {"token", "wan", "lan"} is the report again with the ports assigned by hand
// (first-run setup, where the router has none yet).
func (s *Service) HandlePreviewChoice(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Token string `json:"token"`
		PortChoice
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 4<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	rep, err := s.PreviewChoice(req.Token, req.PortChoice)
	if err != nil {
		write(w, http.StatusNotFound, errBody{capital(err.Error()) + "."})
		return
	}
	write(w, http.StatusOK, rep)
}

// HandleRestore: POST /api/v1/backup/restore {"token", "settings", "admins", "certificates", "history", "keep_ports"}.
func (s *Service) HandleRestore(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	var req struct {
		Token string `json:"token"`
		Options
	}
	if err := auth.DecodeJSONStrict(w, r, &req, 4<<10); err != nil {
		write(w, http.StatusBadRequest, errBody{err.Error()})
		return
	}
	if _, err := s.get(req.Token); err != nil {
		write(w, http.StatusNotFound, errBody{capital(err.Error()) + "."})
		return
	}
	res, err := s.Restore(req.Token, req.Options)
	if err != nil {
		write(w, http.StatusConflict, errBody{capital(err.Error()) + "."})
		return
	}
	write(w, http.StatusOK, res)
}

// HandleCopy: DELETE /api/v1/backup/copies/{id} removes a copy the router kept of itself.
func (s *Service) HandleCopy(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodDelete) {
		return
	}
	if err := s.DeleteCopy(r.PathValue("id")); err != nil {
		write(w, http.StatusNotFound, errBody{capital(err.Error()) + "."})
		return
	}
	copies, _ := s.Copies()
	write(w, http.StatusOK, infoResponse{Copies: copies, MinPassphrase: MinPassphrase, Editable: s.editable()})
}

// HandleCopyRestore: POST /api/v1/backup/copies/{id}/restore puts a copy back (undoing a restore).
func (s *Service) HandleCopyRestore(w http.ResponseWriter, r *http.Request) {
	if !methods(w, r, http.MethodPost) {
		return
	}
	res, err := s.RestoreCopy(r.PathValue("id"))
	if err != nil {
		status := http.StatusConflict
		if err.Error() == "there is no such copy" {
			status = http.StatusNotFound
		}
		write(w, status, errBody{capital(err.Error()) + "."})
		return
	}
	write(w, http.StatusOK, res)
}

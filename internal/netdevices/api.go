package netdevices

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// SNMPView is a device's SNMP settings as a page sees them: never the secrets, only whether they are set.
type SNMPView struct {
	Version      string `json:"version"`
	Port         int    `json:"port,omitempty"`
	User         string `json:"user,omitempty"`
	AuthProto    string `json:"auth_proto,omitempty"`
	PrivProto    string `json:"priv_proto,omitempty"`
	CommunitySet bool   `json:"community_set"`
	AuthPassSet  bool   `json:"auth_pass_set"`
	PrivPassSet  bool   `json:"priv_pass_set"`
}

// APView is an access point's settings as a page sees them: never the token.
type APView struct {
	Port        int    `json:"port,omitempty"`
	TokenSet    bool   `json:"token_set"`
	Fingerprint string `json:"fingerprint,omitempty"` // of the certificate that is trusted ("" until the first answer)
}

// View is one watched device and what is known of it.
type View struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	Kind        string    `json:"kind"`
	Enabled     bool      `json:"enabled"`
	SNMP        *SNMPView `json:"snmp,omitempty"`
	AP          *APView   `json:"ap,omitempty"`
	Polled      bool      `json:"polled"`    // it has been asked at least once
	Reachable   bool      `json:"reachable"` // it answered the last time
	Error       string    `json:"error,omitempty"`
	LastOK      int64     `json:"last_ok,omitempty"` // Unix seconds
	Since       int64     `json:"since,omitempty"`   // since when it has been reachable, or not
	SystemName  string    `json:"system_name,omitempty"`
	Description string    `json:"description,omitempty"`
	UptimeSec   int64     `json:"uptime_seconds,omitempty"`
	Ports       []Port    `json:"ports"`
	Clients     int       `json:"clients"`
}

func (s *Service) view(d Device) View {
	v := View{ID: d.ID, Name: d.Name, Address: d.Address, Kind: d.Kind, Enabled: d.Enabled, Ports: []Port{}}
	if a := d.SNMP; a != nil {
		v.SNMP = &SNMPView{Version: a.Version, Port: a.Port, User: a.User, AuthProto: a.AuthProto, PrivProto: a.PrivProto,
			CommunitySet: a.Community != "", AuthPassSet: a.AuthPass != "", PrivPassSet: a.PrivPass != ""}
	}
	if a := d.AP; a != nil {
		v.AP = &APView{Port: a.Port, TokenSet: a.Token != "", Fingerprint: a.Fingerprint}
	}
	st := s.StatusOf(d.ID)
	v.Polled, v.Reachable, v.Error = !st.PolledAt.IsZero(), st.Reachable, st.Error
	if !st.LastOK.IsZero() {
		v.LastOK = st.LastOK.Unix()
	}
	if !st.Since.IsZero() {
		v.Since = st.Since.Unix()
	}
	if r := st.Record; r != nil {
		v.SystemName, v.Description, v.UptimeSec, v.Clients = r.Name, r.Description, r.UptimeSec, len(r.Clients)
		if r.Ports != nil {
			v.Ports = r.Ports
		}
	}
	return v
}

// Request is what a page sends to add or change a device. A secret that is left out keeps what is stored.
type Request struct {
	Name    string       `json:"name"`
	Address string       `json:"address"`
	Kind    string       `json:"kind"`
	Enabled *bool        `json:"enabled"`
	SNMP    *SNMPRequest `json:"snmp"`
	AP      *APRequest   `json:"ap"`
}

// APRequest is the access point part of a Request. A token that is left out keeps the stored one; TrustNewCertificate forgets the remembered
// certificate, so that the next answer's is trusted instead (the access point's was renewed).
type APRequest struct {
	Port                int     `json:"port"`
	Token               *string `json:"token"`
	TrustNewCertificate bool    `json:"trust_new_certificate"`
}

// SNMPRequest is the SNMP part of a Request.
type SNMPRequest struct {
	Version   string  `json:"version"`
	Port      int     `json:"port"`
	Community *string `json:"community"`
	User      string  `json:"user"`
	AuthProto string  `json:"auth_proto"`
	AuthPass  *string `json:"auth_pass"`
	PrivProto string  `json:"priv_proto"`
	PrivPass  *string `json:"priv_pass"`
}

func merge(old Device, r Request) Device {
	d := old
	d.Name, d.Address = r.Name, strings.TrimSpace(r.Address)
	if r.Kind != "" {
		d.Kind = r.Kind
	}
	if r.Enabled != nil {
		d.Enabled = *r.Enabled
	}
	if r.SNMP != nil && d.Kind == KindSNMP {
		a := SNMPAuth{Version: r.SNMP.Version, Port: r.SNMP.Port, User: r.SNMP.User, AuthProto: r.SNMP.AuthProto, PrivProto: r.SNMP.PrivProto}
		if old.SNMP != nil {
			a.Community, a.AuthPass, a.PrivPass = old.SNMP.Community, old.SNMP.AuthPass, old.SNMP.PrivPass
		}
		if r.SNMP.Community != nil {
			a.Community = *r.SNMP.Community
		}
		if r.SNMP.AuthPass != nil {
			a.AuthPass = *r.SNMP.AuthPass
		}
		if r.SNMP.PrivPass != nil {
			a.PrivPass = *r.SNMP.PrivPass
		}
		// the settings of the other version, or of a protocol that is not used, are not kept
		if a.Version == "2c" {
			a.User, a.AuthProto, a.AuthPass, a.PrivProto, a.PrivPass = "", "", "", "", ""
		} else {
			a.Community = ""
			if a.AuthProto == "" {
				a.AuthPass = ""
			}
			if a.PrivProto == "" {
				a.PrivPass = ""
			}
		}
		d.SNMP = &a
	}
	if r.AP != nil && d.Kind == KindAP {
		a := APAuth{Port: r.AP.Port}
		if old.AP != nil {
			a.Token, a.Fingerprint = old.AP.Token, old.AP.Fingerprint
		}
		if r.AP.Token != nil {
			a.Token = strings.TrimSpace(*r.AP.Token)
		}
		// a different address or port is a different device: its certificate is not the one that was trusted
		if r.AP.TrustNewCertificate || (old.AP != nil && (old.Address != d.Address || old.AP.Port != a.Port)) {
			a.Fingerprint = ""
		}
		d.AP = &a
	}
	return d
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error  string       `json:"error"`
	Errors []FieldError `json:"errors,omitempty"`
}

func fail(w http.ResponseWriter, err error) {
	var errs Errors
	switch {
	case errors.As(err, &errs):
		fe := make([]FieldError, len(errs))
		copy(fe, errs)
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "These settings cannot be saved: " + errs.Error(), Errors: fe})
	case errors.Is(err, os.ErrNotExist):
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such device"})
	default:
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "the request could not be read: " + err.Error()})
		return false
	}
	return true
}

// Views lists every watched device.
func (s *Service) Views() []View {
	devs := s.Store.List()
	out := make([]View, 0, len(devs))
	for _, d := range devs {
		out = append(out, s.view(d))
	}
	return out
}

// HandleCollection: GET /api/v1/network-devices lists them; POST adds one (and reads it at once).
func (s *Service) HandleCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"devices": s.Views(), "max": MaxDevices})
	case http.MethodPost:
		var req Request
		if !decode(w, r, &req) {
			return
		}
		d := merge(Device{Enabled: true}, req)
		d, err := s.Store.Add(d)
		if err != nil {
			fail(w, err)
			return
		}
		if d.Enabled {
			s.pollSoon(d)
		}
		writeJSON(w, http.StatusCreated, s.view(d))
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// pollSoon reads a device in the background (a page that has just added one wants to see it answer).
func (s *Service) pollSoon(d Device) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.Poll(ctx, d)
	}()
}

// HandleItem: GET, PUT and DELETE /api/v1/network-devices/{id}.
func (s *Service) HandleItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, ok := s.Store.Get(id)
	if !ok {
		fail(w, os.ErrNotExist)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.view(old))
	case http.MethodPut:
		var req Request
		if !decode(w, r, &req) {
			return
		}
		d, err := s.Store.Update(merge(old, req))
		if err != nil {
			fail(w, err)
			return
		}
		if d.Enabled {
			s.pollSoon(d)
		}
		writeJSON(w, http.StatusOK, s.view(d))
	case http.MethodDelete:
		if err := s.Store.Delete(id); err != nil {
			fail(w, err)
			return
		}
		s.mu.Lock()
		delete(s.status, id)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// HandlePoll: POST /api/v1/network-devices/{id}/poll reads the device now and answers with what it found.
func (s *Service) HandlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	d, ok := s.Store.Get(r.PathValue("id"))
	if !ok {
		fail(w, os.ErrNotExist)
		return
	}
	s.Poll(r.Context(), d)
	writeJSON(w, http.StatusOK, s.view(d))
}

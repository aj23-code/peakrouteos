package auth

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"

	"peakroute/internal/config"
)

// The Administrators API: who may sign in to the web interface, with what role, their passwords, and how long a session
// may sit idle. These changes take effect at once (they are not part of the draft, review and commit steps of the network
// configuration: a password change that waited for a commit would be strange, and nothing here can cut the router off).
// docs/administrators.md describes the rules.

// OwnPasswordPath is the one change a read-only account may make.
const OwnPasswordPath = "/api/v1/me/password"

const (
	maxAdmins     = 20
	maxAdminsBody = 16 << 10
)

// AdminAPI serves the Administrators page.
type AdminAPI struct {
	Cfg      *config.Manager
	Sessions *SessionManager
	// Attempts limits how often a password can be tried for one account (nil: not limited).
	Attempts *RateLimiter
	// Logf records who did what (never a password); nil uses the standard logger.
	Logf func(format string, args ...any)
	// Idle says whether a change to the configuration is neither being applied nor waiting for confirmation (nil: always).
	// The router is not restarted or switched off in the middle of one.
	Idle func() bool
	// Power restarts or switches off the machine: action is "reboot" or "poweroff". It returns at once; the action follows
	// a moment later, so that the answer reaches the browser first.
	Power func(action string) error
}

func (a *AdminAPI) logf(format string, args ...any) {
	if a.Logf != nil {
		a.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

type adminUser struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	You      bool   `json:"you"`
	Sessions int    `json:"sessions"` // signed in right now
}

type adminsResponse struct {
	Users            []adminUser `json:"users"` // everyone for an administrator; only yourself otherwise
	You              string      `json:"you"`
	Role             string      `json:"role"` // yours
	CanManage        bool        `json:"can_manage"`
	SessionTimeout   int         `json:"session_timeout_minutes"` // in effect
	TimeoutMin       int         `json:"timeout_min"`
	TimeoutMax       int         `json:"timeout_max"`
	TimeoutDefault   int         `json:"timeout_default"`
	MinPassword      int         `json:"min_password"`
	MaxPasswordBytes int         `json:"max_password_bytes"`
	MaxAdmins        int         `json:"max_accounts"`
}

type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func refuse(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func refuseField(w http.ResponseWriter, status int, field, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "errors": []fieldError{{Field: field, Message: msg}}})
}

// who is the signed-in account (the route is behind RequireAuth; this is the second look and gives the role).
func (a *AdminAPI) who(w http.ResponseWriter, r *http.Request) (SessionInfo, bool) {
	info, ok := a.Sessions.Lookup(r)
	if !ok {
		refuse(w, http.StatusUnauthorized, "unauthorized")
	}
	return info, ok
}

// admin is who, and the account must be an administrator.
func (a *AdminAPI) admin(w http.ResponseWriter, r *http.Request) (SessionInfo, bool) {
	info, ok := a.who(w, r)
	if !ok {
		return info, false
	}
	if !CanWrite(info.Role) {
		refuse(w, http.StatusForbidden, "Only an administrator can do this.")
		return info, false
	}
	return info, true
}

func method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	refuse(w, http.StatusMethodNotAllowed, "method not allowed")
	return false
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := DecodeJSONStrict(w, r, dst, maxAdminsBody); err != nil {
		refuse(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func (a *AdminAPI) view(info SessionInfo) adminsResponse {
	users := a.Cfg.Users()
	counts := a.Sessions.ActiveSessions()
	resp := adminsResponse{
		Users: []adminUser{}, You: info.Username, Role: info.Role, CanManage: CanWrite(info.Role),
		SessionTimeout: int(a.Cfg.SessionTimeout().Minutes()), TimeoutMin: config.MinSessionTimeoutMinutes,
		TimeoutMax: config.MaxSessionTimeoutMinutes, TimeoutDefault: config.DefaultSessionTimeoutMinutes,
		MinPassword: MinPasswordLength, MaxPasswordBytes: MaxPasswordBytes, MaxAdmins: maxAdmins,
	}
	for _, u := range users {
		if !resp.CanManage && u.Username != info.Username {
			continue
		}
		resp.Users = append(resp.Users, adminUser{Username: u.Username, Role: NormalizeRole(u.Role), You: u.Username == info.Username, Sessions: counts[u.Username]})
	}
	sort.SliceStable(resp.Users, func(i, j int) bool {
		if resp.Users[i].You != resp.Users[j].You {
			return resp.Users[i].You // yourself first
		}
		return strings.ToLower(resp.Users[i].Username) < strings.ToLower(resp.Users[j].Username)
	})
	return resp
}

// validPassword checks a new password and its confirmation; field says which box a complaint belongs to.
func validPassword(password, confirm string) (field, msg string) {
	if err := ValidateNewPassword(password, password); err != nil {
		return "password", err.Error()
	}
	if password != confirm {
		return "confirm", "Passwords do not match."
	}
	return "", ""
}

// errors the change functions use to say why they refused
var (
	errNoSuchUser = errors.New("There is no such account.")
	errLastAdmin  = errors.New("There must always be at least one administrator.")
	errDuplicate  = errors.New("There is already an account with that name.")
	errTooMany    = errors.New("There are as many accounts as the router supports.")
)

// HandleAdmins: GET /api/v1/admins lists the accounts; POST /api/v1/admins adds one.
func (a *AdminAPI) HandleAdmins(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodGet {
		if info, ok := a.who(w, r); ok {
			writeJSON(w, http.StatusOK, a.view(info))
		}
		return
	}
	info, ok := a.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Username)
	if err := ValidateUsername(name); err != nil {
		refuseField(w, http.StatusBadRequest, "username", err.Error())
		return
	}
	if req.Role != RoleAdmin && req.Role != RoleReadOnly {
		refuseField(w, http.StatusBadRequest, "role", "Choose administrator or read-only.")
		return
	}
	if field, msg := validPassword(req.Password, req.Confirm); field != "" {
		refuseField(w, http.StatusBadRequest, field, msg)
		return
	}
	hash, err := config.HashPassword(req.Password)
	if err != nil {
		refuse(w, http.StatusInternalServerError, "The password could not be processed.")
		return
	}
	err = a.Cfg.UpdateAuth(func(c *config.AuthConfig) error {
		for _, u := range c.Users {
			if sameName(u.Username, name) {
				return errDuplicate
			}
		}
		if len(c.Users) >= maxAdmins {
			return errTooMany
		}
		c.Users = append(c.Users, config.User{Username: name, PasswordHash: hash, Role: req.Role})
		return nil
	})
	switch {
	case errors.Is(err, errDuplicate):
		refuseField(w, http.StatusConflict, "username", err.Error())
	case errors.Is(err, errTooMany):
		refuse(w, http.StatusConflict, err.Error())
	case err != nil:
		refuse(w, http.StatusInternalServerError, "The account could not be saved.")
	default:
		a.logf("[+] Account %q added by %q (%s)", name, info.Username, req.Role)
		writeJSON(w, http.StatusCreated, a.view(info))
	}
}

// target reads the account named in the path.
func (a *AdminAPI) target(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("username")
	if ValidateUsername(name) != nil {
		refuse(w, http.StatusNotFound, errNoSuchUser.Error())
		return "", false
	}
	return name, true
}

// countAdmins says how many of the accounts are administrators.
func countAdmins(users []config.User) int {
	n := 0
	for _, u := range users {
		if NormalizeRole(u.Role) == RoleAdmin {
			n++
		}
	}
	return n
}

// changeAccount removes the account or gives it a new role, never leaving the router without an administrator (two
// administrators removing each other at the same moment would otherwise get past the checks made before the change).
func changeAccount(c *config.AuthConfig, name string, remove bool, role string) error {
	i := -1
	for k, u := range c.Users {
		if u.Username == name {
			i = k
		}
	}
	if i < 0 {
		return errNoSuchUser
	}
	wasAdmin := NormalizeRole(c.Users[i].Role) == RoleAdmin
	if remove {
		if wasAdmin && countAdmins(c.Users) == 1 {
			return errLastAdmin
		}
		c.Users = append(c.Users[:i:i], c.Users[i+1:]...)
		return nil
	}
	if wasAdmin && role != RoleAdmin && countAdmins(c.Users) == 1 {
		return errLastAdmin
	}
	c.Users[i].Role = role
	return nil
}

// HandleAdmin: PUT /api/v1/admins/{username} {"role": "readonly"} changes the role; DELETE removes the account.
func (a *AdminAPI) HandleAdmin(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	info, ok := a.admin(w, r)
	if !ok {
		return
	}
	name, ok := a.target(w, r)
	if !ok {
		return
	}
	if name == info.Username {
		if r.Method == http.MethodDelete {
			refuse(w, http.StatusConflict, "You cannot remove the account you are signed in with.")
		} else {
			refuse(w, http.StatusConflict, "You cannot change your own role.")
		}
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if r.Method == http.MethodPut {
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Role != RoleAdmin && req.Role != RoleReadOnly {
			refuseField(w, http.StatusBadRequest, "role", "Choose administrator or read-only.")
			return
		}
	}
	err := a.Cfg.UpdateAuth(func(c *config.AuthConfig) error {
		return changeAccount(c, name, r.Method == http.MethodDelete, req.Role)
	})
	switch {
	case errors.Is(err, errNoSuchUser):
		refuse(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errLastAdmin):
		refuse(w, http.StatusConflict, err.Error())
	case err != nil:
		refuse(w, http.StatusInternalServerError, "The change could not be saved.")
	default:
		if r.Method == http.MethodDelete {
			a.Sessions.RevokeUser(name, nil)
			a.logf("[+] Account %q removed by %q", name, info.Username)
		} else {
			a.logf("[+] Account %q is now %s (changed by %q)", name, req.Role, info.Username)
		}
		writeJSON(w, http.StatusOK, a.view(info))
	}
}

// setPassword stores a new password for the account.
func (a *AdminAPI) setPassword(name, password string) error {
	hash, err := config.HashPassword(password)
	if err != nil {
		return err
	}
	return a.Cfg.UpdateAuth(func(c *config.AuthConfig) error {
		for i := range c.Users {
			if c.Users[i].Username == name {
				c.Users[i].PasswordHash = hash
				return nil
			}
		}
		return errNoSuchUser
	})
}

// HandleAdminPassword: POST /api/v1/admins/{username}/password {"password", "confirm"} sets another account's password
// (the person must then sign in again). Your own password is changed with HandleOwnPassword, which asks for the old one.
func (a *AdminAPI) HandleAdminPassword(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	info, ok := a.admin(w, r)
	if !ok {
		return
	}
	name, ok := a.target(w, r)
	if !ok {
		return
	}
	if name == info.Username {
		refuse(w, http.StatusConflict, "Change your own password with the form for your account: it asks for the current one.")
		return
	}
	var req struct {
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if field, msg := validPassword(req.Password, req.Confirm); field != "" {
		refuseField(w, http.StatusBadRequest, field, msg)
		return
	}
	switch err := a.setPassword(name, req.Password); {
	case errors.Is(err, errNoSuchUser):
		refuse(w, http.StatusNotFound, err.Error())
	case err != nil:
		refuse(w, http.StatusInternalServerError, "The password could not be saved.")
	default:
		n := a.Sessions.RevokeUser(name, nil)
		a.logf("[+] Password of %q set by %q (%d sessions ended)", name, info.Username, n)
		writeJSON(w, http.StatusOK, a.view(info))
	}
}

// HandleAdminSessions: DELETE /api/v1/admins/{username}/sessions signs the account out everywhere (for yourself: everywhere
// else, so that the page you are on keeps working).
func (a *AdminAPI) HandleAdminSessions(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodDelete) {
		return
	}
	info, ok := a.admin(w, r)
	if !ok {
		return
	}
	name, ok := a.target(w, r)
	if !ok {
		return
	}
	if _, found := FindUser(a.Cfg.Users(), name); !found {
		refuse(w, http.StatusNotFound, errNoSuchUser.Error())
		return
	}
	var keep *http.Request
	if name == info.Username {
		keep = r
	}
	n := a.Sessions.RevokeUser(name, keep)
	a.logf("[+] %d sessions of %q ended by %q", n, name, info.Username)
	writeJSON(w, http.StatusOK, a.view(info))
}

// HandleOwnPassword: POST /api/v1/me/password {"current", "password", "confirm"}. Any account may change its own password
// with the current one; its other sessions end, this one carries on. Wrong guesses are limited per account.
func (a *AdminAPI) HandleOwnPassword(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	info, ok := a.who(w, r)
	if !ok {
		return
	}
	var req struct {
		Current  string `json:"current"`
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if a.Attempts != nil && !a.Attempts.Allow(info.Username) {
		refuse(w, http.StatusTooManyRequests, "Too many attempts. Wait a couple of minutes and try again.")
		return
	}
	if !Authenticate(a.Cfg.Users(), info.Username, req.Current) {
		refuseField(w, http.StatusBadRequest, "current", "That is not your current password.")
		return
	}
	if field, msg := validPassword(req.Password, req.Confirm); field != "" {
		refuseField(w, http.StatusBadRequest, field, msg)
		return
	}
	if req.Password == req.Current {
		refuseField(w, http.StatusBadRequest, "password", "Choose a password that is different from the current one.")
		return
	}
	if err := a.setPassword(info.Username, req.Password); err != nil {
		refuse(w, http.StatusInternalServerError, "The password could not be saved.")
		return
	}
	n := a.Sessions.RevokeUser(info.Username, r)
	a.logf("[+] Password of %q changed by the account itself (%d other sessions ended)", info.Username, n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ended": n})
}

// HandleSessionTimeout: PUT /api/v1/session/timeout {"minutes": 15} sets how long a session may sit idle.
func (a *AdminAPI) HandleSessionTimeout(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPut) {
		return
	}
	info, ok := a.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		Minutes int `json:"minutes"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Minutes < config.MinSessionTimeoutMinutes || req.Minutes > config.MaxSessionTimeoutMinutes {
		refuseField(w, http.StatusBadRequest, "minutes", "Choose a time from 1 minute to 24 hours (1440 minutes).")
		return
	}
	if err := a.Cfg.UpdateAuth(func(c *config.AuthConfig) error { c.SessionTimeoutMinutes = req.Minutes; return nil }); err != nil {
		refuse(w, http.StatusInternalServerError, "The setting could not be saved.")
		return
	}
	a.logf("[+] Session timeout set to %d minutes by %q", req.Minutes, info.Username)
	writeJSON(w, http.StatusOK, a.view(info))
}

// HandlePower: POST /api/v1/system/power {"action": "reboot"} restarts the router; {"action": "poweroff"} switches it off.
// Administrators only, and not while a configuration change is being applied or waits for confirmation.
func (a *AdminAPI) HandlePower(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	info, ok := a.admin(w, r)
	if !ok {
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Action != "reboot" && req.Action != "poweroff" {
		refuseField(w, http.StatusBadRequest, "action", "Choose restart or power off.")
		return
	}
	if a.Idle != nil && !a.Idle() {
		refuse(w, http.StatusConflict, "A change is being applied or is waiting for confirmation. Confirm or revert it first.")
		return
	}
	if a.Power == nil {
		refuse(w, http.StatusServiceUnavailable, "This router cannot be restarted from here.")
		return
	}
	if err := a.Power(req.Action); err != nil {
		refuse(w, http.StatusInternalServerError, "The request could not be carried out.")
		return
	}
	a.logf("[+] %s requested by %q", map[string]string{"reboot": "Restart", "poweroff": "Power off"}[req.Action], info.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": req.Action})
}

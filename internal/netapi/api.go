// Package netapi serves the endpoints that edit the network configuration: which role each
// physical port has (WAN, LAN or unassigned) and the settings of a network (its address and DHCP).
//
// Saving never changes the running system. Each save stages a draft in the config Manager; the
// banner, review dialog and commit/confirm pipeline take it from there. Each endpoint validates
// only the fields it owns, so a half-configured box can still be edited a page at a time; the full
// check happens when the draft is committed.
package netapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"peakroute/internal/auth"
	"peakroute/internal/config"
)

// PortLister reports the machine's physical network ports.
type PortLister func() ([]config.SystemInterface, error)

type API struct {
	Manager *config.Manager
	Ports   PortLister
	// Editable reports whether edits are allowed right now. They are not while a change is being
	// applied or waiting for confirmation: a revert would overwrite a newer draft. Nil means always.
	Editable func() bool
	// Live reports how the given port is connected right now (its address, gateway and DNS
	// servers). Nil means it is not reported.
	Live func(port string) *Live
	// RoutingTable lists the routes the kernel is using now. Nil means it is not reported.
	RoutingTable func() []LiveRoute
}

const maxBody = 64 << 10

const (
	RoleWAN        = "wan"
	RoleLAN        = "lan"
	RoleUnassigned = "unassigned"
)

// Field names in errors are the API's own (generic, per-network), not the JSON file's.
var errorField = map[string]string{
	"lan_ip":           "ip",
	"lan_subnet":       "netmask",
	"dhcp_enabled":     "dhcp.enabled",
	"dhcp_range_start": "dhcp.range_start",
	"dhcp_range_end":   "dhcp.range_end",
	"dhcp_lease_time":  "dhcp.lease_time",
	"wan_interface":    "ports",
	"lan_interfaces":   "ports",
	"wan_mode":         "mode",
	"wan_ip":           "ip",
	"wan_subnet":       "netmask",
	"wan_gateway":      "gateway",
	"wan_dns":          "dns",
}

type errorBody struct {
	Error  string              `json:"error"`
	Errors []config.FieldError `json:"errors,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[netapi] writing response: %v", err)
	}
}

func fieldErrors(w http.ResponseWriter, status int, summary string, errs []config.FieldError) {
	writeJSON(w, status, errorBody{Error: summary, Errors: errs})
}

func allowMethods(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
	return false
}

func (a *API) editable() bool { return a.Editable == nil || a.Editable() }

// requireEditable answers 409 and returns false while edits are not allowed.
func (a *API) requireEditable(w http.ResponseWriter) bool {
	if a.editable() {
		return true
	}
	writeJSON(w, http.StatusConflict, errorBody{
		Error: "A change is being applied or is waiting for confirmation. Confirm or revert it before editing.",
	})
	return false
}

// ownedErrors keeps only the validation problems on the given fields (using the file's field
// names) and renames them to the API's field names.
func ownedErrors(err error, fields ...string) config.ValidationError {
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	own := map[string]bool{}
	for _, f := range fields {
		own[f] = true
	}
	// "routes" owns "routes[3].gateway" and "port_networks" owns "port_networks.eth1.native"
	owns := func(field string) bool {
		if own[field] {
			return true
		}
		if i := strings.IndexAny(field, "[."); i > 0 {
			return own[field[:i]]
		}
		return false
	}
	var out config.ValidationError
	for _, e := range ve {
		if owns(e.Field) {
			name := errorField[e.Field]
			if name == "" {
				name = e.Field
			}
			out = append(out, config.FieldError{Field: name, Message: e.Message})
		}
	}
	return out
}

// decode reads a JSON request body strictly (right content type, size limit, no unknown fields).
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := auth.DecodeJSONStrict(w, r, dst, maxBody); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return false
	}
	return true
}

// roleOf says what role a port has in a configuration.
func roleOf(c config.NetworkConfig, name string) string {
	if name != "" && name == c.WanInterface {
		return RoleWAN
	}
	for _, lan := range c.LanInterfaces {
		if lan == name {
			return RoleLAN
		}
	}
	return RoleUnassigned
}

// changedIn reports whether any of the fields differs between two configurations.
func changedIn(active, draft config.NetworkConfig, fields ...string) bool {
	for _, c := range config.DiffNetwork(active, draft) {
		for _, f := range fields {
			if c.Field == f {
				return true
			}
		}
	}
	return false
}

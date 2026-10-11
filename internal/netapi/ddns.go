package netapi

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/ddns"
)

// The Dynamic DNS API: the names that are kept pointed at the internet connection's address, how each is going, and an
// "update now". Everything is staged like the other editors; the password (or token) is write-only. docs/ddns.md has the model.

// DDNSStatus is how keeping one name up to date is going (only for a name as it is running).
type DDNSStatus struct {
	Address     string `json:"address"` // the address the service was last given
	OK          bool   `json:"ok"`
	Attention   bool   `json:"attention"` // stopped: it needs something changed, or "update now"
	Message     string `json:"message"`
	LastTry     int64  `json:"last_try"`     // Unix milliseconds; 0: never
	LastSuccess int64  `json:"last_success"` // 0: never
	NextTry     int64  `json:"next_try"`     // 0: not waiting
}

// DDNSEntryView is a name as the page shows it.
type DDNSEntryView struct {
	ID          string      `json:"id"`
	Provider    string      `json:"provider"`
	Hostname    string      `json:"hostname"`
	Server      string      `json:"server"`
	Username    string      `json:"username"`
	URL         string      `json:"url"`
	Source      string      `json:"source"`
	Enabled     bool        `json:"enabled"`
	PasswordSet bool        `json:"password_set"` // a password or token is saved; it is never sent back
	Text        string      `json:"text"`
	Pending     bool        `json:"pending"` // differs from what is running
	Status      *DDNSStatus `json:"status"`  // null for a name that is not running as it is shown (new, changed, switched off)
}

type ddnsResponse struct {
	Entries    []DDNSEntryView `json:"entries"`
	Servers    []DDNSServer    `json:"servers"` // dyndns2 servers the page offers by name
	WANAddress string          `json:"wan_address"`
	WANPublic  bool            `json:"wan_public"` // the connection's address can be reached from the internet
	Editable   bool            `json:"editable"`
}

// DDNSServer is a dyndns2 server with its name.
type DDNSServer struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (a *API) ddnsEntryView(e config.DDNSEntry, active config.NetworkConfig, statuses map[string]ddns.Status) DDNSEntryView {
	old, had := active.DDNSSettings().FindDDNS(e.ID)
	pending := !had || old != e
	v := DDNSEntryView{ID: e.ID, Provider: e.Provider, Hostname: e.Hostname, Server: e.Server, Username: e.Username, URL: e.URL, Source: e.Source,
		Enabled: !e.Disabled, PasswordSet: e.Password != "", Text: config.DDNSText(e), Pending: pending}
	if st, ok := statuses[e.ID]; ok && !pending && !e.Disabled {
		v.Status = &DDNSStatus{Address: st.Address, OK: st.OK, Attention: st.Attention, Message: st.Message, LastTry: ms(st.LastTry), LastSuccess: ms(st.LastSuccess), NextTry: ms(st.NextTry)}
	}
	return v
}

func (a *API) ddnsStatuses() map[string]ddns.Status {
	out := map[string]ddns.Status{}
	if a.DDNSStatuses != nil {
		for _, s := range a.DDNSStatuses() {
			out[s.ID] = s
		}
	}
	return out
}

func (a *API) ddnsView() ddnsResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	resp := ddnsResponse{Entries: []DDNSEntryView{}, Servers: []DDNSServer{}, Editable: a.editable()}
	for _, s := range config.DDNSServers {
		resp.Servers = append(resp.Servers, DDNSServer{Name: s.Name, Host: s.Host})
	}
	statuses := a.ddnsStatuses()
	for _, e := range draft.DDNSSettings().Entries {
		resp.Entries = append(resp.Entries, a.ddnsEntryView(e, active, statuses))
	}
	if a.Live != nil {
		if live := a.Live(active); live != nil && live.Address != "" {
			addr, _, _ := strings.Cut(live.Address, "/")
			resp.WANAddress = addr
			if ip, err := netip.ParseAddr(addr); err == nil {
				resp.WANPublic = ddns.IsPublic(ip)
			}
		}
	}
	return resp
}

// HandleDDNS: GET /api/v1/ddns returns the Dynamic DNS settings (the draft) with how each name is going.
func (a *API) HandleDDNS(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet, http.MethodPost) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, a.ddnsView())
			return
		}
		a.createDDNS(w, r)
	}
}

type ddnsRequest struct {
	Provider *string `json:"provider"`
	Hostname *string `json:"hostname"`
	Server   *string `json:"server"`
	Username *string `json:"username"`
	Password *string `json:"password"` // left out: the saved one stays
	URL      *string `json:"url"`
	Source   *string `json:"source"`
	Enabled  *bool   `json:"enabled"`
}

func applyDDNSRequest(e *config.DDNSEntry, req ddnsRequest) {
	if req.Provider != nil {
		e.Provider = strings.ToLower(trim(req.Provider))
	}
	if req.Hostname != nil {
		e.Hostname = strings.ToLower(trim(req.Hostname))
	}
	if req.Server != nil {
		e.Server = strings.ToLower(trim(req.Server))
	}
	if req.Username != nil {
		e.Username = trim(req.Username)
	}
	if req.Password != nil && *req.Password != "" {
		e.Password = *req.Password
	}
	if req.URL != nil {
		e.URL = trim(req.URL)
	}
	if req.Source != nil {
		e.Source = strings.ToLower(trim(req.Source))
	}
	if req.Enabled != nil {
		e.Disabled = !*req.Enabled
	}
	// what belongs to another kind of service is dropped, so that changing the service needs no clearing
	if e.Provider != config.DDNSDynDNS2 {
		e.Server = ""
	}
	if e.Provider != config.DDNSCustom {
		e.URL = ""
	}
	if e.Provider == config.DDNSDuckDNS || e.Provider == config.DDNSCloudflare {
		e.Username = ""
	}
}

func (a *API) createDDNS(w http.ResponseWriter, r *http.Request) {
	if !a.requireEditable(w) {
		return
	}
	var req ddnsRequest
	if !decode(w, r, &req) {
		return
	}
	var id string
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.DDNS == nil {
			c.DDNS = &config.DDNSConfig{}
		}
		d := c.DDNS
		id = nextID("d", func(id string) bool { _, ok := d.FindDDNS(id); return ok })
		e := config.DDNSEntry{ID: id, Source: config.DDNSSourceWAN}
		applyDDNSRequest(&e, req)
		d.Entries = append(d.Entries, e)
		return errorsForFirewall(c.Validate(), fmt.Sprintf("ddns.entries[%d].", len(d.Entries)-1), "ddns.entries")
	})
	var body any
	if err == nil {
		draft, _ := a.Manager.StagedNetworkConfig()
		e, _ := draft.DDNSSettings().FindDDNS(id)
		body = a.ddnsEntryView(e, a.Manager.NetworkConfig(), a.ddnsStatuses())
	}
	a.answerStaged(w, err, http.StatusCreated, "This name cannot be added: ", body)
}

func tidyDDNS(c *config.NetworkConfig) {
	if d := c.DDNS; d != nil && len(d.Entries) == 0 {
		c.DDNS = nil
	}
}

// HandleDDNSEntry: PUT /api/v1/ddns/{id} changes a name (a field left out stays, a password left out or empty stays); DELETE removes it.
func (a *API) HandleDDNSEntry(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	if _, ok := draft.DDNSSettings().FindDDNS(id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such name"})
		return
	}
	if !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			kept := c.DDNS.Entries[:0:0]
			for _, e := range c.DDNS.Entries {
				if e.ID != id {
					kept = append(kept, e)
				}
			}
			c.DDNS.Entries = kept
			tidyDDNS(c)
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.ddnsView())
		return
	}
	var req ddnsRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		for i := range c.DDNS.Entries {
			if c.DDNS.Entries[i].ID != id {
				continue
			}
			applyDDNSRequest(&c.DDNS.Entries[i], req)
			return errorsForFirewall(c.Validate(), fmt.Sprintf("ddns.entries[%d].", i), "ddns.entries")
		}
		return nil
	})
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		e, _ := now.DDNSSettings().FindDDNS(id)
		body = a.ddnsEntryView(e, a.Manager.NetworkConfig(), a.ddnsStatuses())
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

// HandleDDNSUpdate: POST /api/v1/ddns/{id}/update tells the service the address now, whatever the name was waiting for. It works on
// the name as it is running (not the draft) and answers with how that went.
func (a *API) HandleDDNSUpdate(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	if a.DDNSUpdate == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "Dynamic DNS is not running on this system."})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	st, ok := a.DDNSUpdate(ctx, r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "That name is not running yet. Commit it first (and switch it on), then update."})
		return
	}
	writeJSON(w, http.StatusOK, DDNSStatus{Address: st.Address, OK: st.OK, Attention: st.Attention, Message: st.Message, LastTry: ms(st.LastTry), LastSuccess: ms(st.LastSuccess), NextTry: ms(st.NextTry)})
}

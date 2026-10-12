package netapi

import (
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/diag"
)

// The long diagnostic tools: ping and traceroute run as jobs the page follows, and a TCP connection test answers at once. Like the DNS
// lookup they look at what is running now, not at the draft. They change nothing; starting one is a POST because it sends traffic, so a
// read-only account cannot start them (it can still look at a job). docs/diagnostics.md describes them.

// DiagSource is a place a test can be sent from.
type DiagSource struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"` // the address it is sent from
}

type diagnosticsResponse struct {
	Sources    []DiagSource    `json:"sources"`
	Tools      map[string]bool `json:"tools"` // which of the programs are installed
	MaxPings   int             `json:"max_pings"`
	MaxRunning int             `json:"max_running"`
}

// interfaceAddr is the first IPv4 address of an interface (a variable so that tests need no real interface).
var interfaceAddr = func(name string) (string, bool) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return "", false
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
			return ipn.IP.String(), true
		}
	}
	return "", false
}

func (a *API) diagSources() []DiagSource {
	cfg := a.Manager.NetworkConfig()
	out := []DiagSource{{ID: "auto", Name: "Automatic (the way the routing table says)"}}
	if a.Live != nil {
		if live := a.Live(cfg); live != nil && live.Address != "" {
			addr, _, _ := strings.Cut(live.Address, "/")
			out = append(out, DiagSource{ID: "wan", Name: "Internet connection", Address: addr})
		}
	} else if addr, ok := interfaceAddr(cfg.WanDevice()); ok {
		out = append(out, DiagSource{ID: "wan", Name: "Internet connection", Address: addr})
	}
	for _, t := range firewallTabs(cfg) {
		if t.Kind != "network" {
			continue
		}
		ip := cfg.LanIP
		if t.ID != config.DefaultNetworkID {
			n, ok := cfg.FindNetwork(t.ID)
			if !ok {
				continue
			}
			ip = n.IP
		}
		out = append(out, DiagSource{ID: t.ID, Name: t.Name, Address: ip})
	}
	for _, z := range cfg.VPNZones() {
		if addr, ok := interfaceAddr(z.Iface); ok {
			out = append(out, DiagSource{ID: z.ID, Name: z.Name, Address: addr})
		}
	}
	return out
}

// diagSource turns the id the page sends into the address to send from ("" for automatic).
func (a *API) diagSource(id string) (string, error) {
	if id == "" || id == "auto" {
		return "", nil
	}
	for _, s := range a.diagSources() {
		if s.ID == id && s.Address != "" {
			return s.Address, nil
		}
	}
	return "", fmt.Errorf("there is no address to send from for %q right now", id)
}

func (a *API) diagRunner() *diag.Runner {
	if a.Diag == nil {
		a.Diag = &diag.Runner{}
	}
	return a.Diag
}

// HandleDiagnostics: GET /api/v1/diagnostics lists where tests can be sent from and which programs there are.
func (a *API) HandleDiagnostics(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	tools := map[string]bool{}
	for tool, prog := range map[string]string{diag.Ping: "ping", diag.Traceroute: "tracepath"} {
		_, err := exec.LookPath(prog)
		tools[tool] = err == nil || a.diagRunner().Start != nil
	}
	writeJSON(w, http.StatusOK, diagnosticsResponse{Sources: a.diagSources(), Tools: tools, MaxPings: diag.MaxPings, MaxRunning: diag.MaxJobsRunning})
}

type diagJobRequest struct {
	Tool   string `json:"tool"`
	Target string `json:"target"`
	Count  int    `json:"count"`
	Source string `json:"source"`
}

// HandleDiagnosticsJobs: POST /api/v1/diagnostics/jobs starts a ping or a traceroute.
func (a *API) HandleDiagnosticsJobs(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	var req diagJobRequest
	if !decode(w, r, &req) {
		return
	}
	req.Target = strings.TrimSpace(req.Target)
	src, err := a.diagSource(strings.TrimSpace(req.Source))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Errors: []config.FieldError{{Field: "source", Message: err.Error()}}})
		return
	}
	v, err := a.diagRunner().Run(diag.Request{Tool: req.Tool, Target: req.Target, Count: req.Count, Source: src})
	if err != nil {
		field := "target"
		switch {
		case strings.Contains(err.Error(), "pings"):
			field = "count"
		case strings.Contains(err.Error(), "running already"), strings.Contains(err.Error(), "installed"), strings.Contains(err.Error(), "started"), strings.Contains(err.Error(), "unknown tool"):
			field = ""
		}
		body := errorBody{Error: err.Error()}
		if field != "" {
			body.Errors = []config.FieldError{{Field: field, Message: err.Error()}}
		}
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "running already") {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// HandleDiagnosticsJob: GET /api/v1/diagnostics/jobs/{id} says how a job is going; DELETE stops it.
func (a *API) HandleDiagnosticsJob(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	if r.Method == http.MethodDelete && !a.diagRunner().Stop(id) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such test"})
		return
	}
	v, ok := a.diagRunner().Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no such test"})
		return
	}
	if r.Method == http.MethodDelete {
		// the stop takes a moment to be seen: wait for the job to say so, but not long
		for i := 0; i < 20 && v.Running; i++ {
			time.Sleep(25 * time.Millisecond)
			v, _ = a.diagRunner().Get(id)
		}
	}
	writeJSON(w, http.StatusOK, v)
}

type diagConnectRequest struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Source string `json:"source"`
}

// HandleDiagnosticsConnect: POST /api/v1/diagnostics/connect tries to open a TCP connection and says what happened (it answers within five
// seconds).
func (a *API) HandleDiagnosticsConnect(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) {
		return
	}
	var req diagConnectRequest
	if !decode(w, r, &req) {
		return
	}
	req.Host = strings.TrimSpace(req.Host)
	if !diag.ValidTarget(req.Host) {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "That is not a name or an IPv4 address.", Errors: []config.FieldError{{Field: "host", Message: "that is not a name or an IPv4 address"}}})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "The port is from 1 to 65535.", Errors: []config.FieldError{{Field: "port", Message: "from 1 to 65535"}}})
		return
	}
	src, err := a.diagSource(strings.TrimSpace(req.Source))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error(), Errors: []config.FieldError{{Field: "source", Message: err.Error()}}})
		return
	}
	writeJSON(w, http.StatusOK, diag.TryConnect(r.Context(), req.Host, req.Port, src, 4*time.Second))
}

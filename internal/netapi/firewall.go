package netapi

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"peakroute/internal/config"
)

// The firewall's API: rules (a list per source, applied top to bottom), port forwards, and the hit counters.
// Everything is staged like the other editors. docs/firewall.md has the model.

// FirewallTab is one list on the Rules page: a Local Network, or the internet.
type FirewallTab struct {
	ID      string `json:"id"` // a network id, or "wan"
	Name    string `json:"name"`
	Kind    string `json:"kind"` // "network" or "internet"
	Default bool   `json:"default"`
}

// FirewallRule is a rule as the page shows it.
type FirewallRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	From        string `json:"from"`
	Action      string `json:"action"`
	Protocol    string `json:"protocol"`
	Source      string `json:"source"`
	To          string `json:"to"`
	Destination string `json:"destination"`
	Ports       string `json:"ports"`
	Log         bool   `json:"log"`
	Text        string `json:"text"`        // the rule in words
	Pending     bool   `json:"pending"`     // differs from the running configuration
	ShadowedBy  string `json:"shadowed_by"` // the earlier rule that decides first, so this one never matches
}

// PortForward is a port forward as the page shows it.
type PortForward struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	Protocol      string   `json:"protocol"`
	ExternalPorts string   `json:"external_ports"`
	ToIP          string   `json:"to_ip"`
	InternalPorts string   `json:"internal_ports"`
	Sources       []string `json:"sources"`
	Text          string   `json:"text"`
	Pending       bool     `json:"pending"`
}

// PolicyLine is a fixed line of the rules list: something the router always does, or what a switch on Local Networks
// means. They cannot be edited here.
type PolicyLine struct {
	Text string `json:"text"`
	Kind string `json:"kind"` // "allow", "block" or "off"
}

// FirewallPolicy are the fixed lines of one tab: those before the rules and those after them (docs/firewall.md, section 4).
type FirewallPolicy struct {
	Before []PolicyLine `json:"before"`
	After  []PolicyLine `json:"after"`
}

type firewallResponse struct {
	Tabs     []FirewallTab             `json:"tabs"`
	Rules    []FirewallRule            `json:"rules"`
	Forwards []PortForward             `json:"forwards"`
	Policy   map[string]FirewallPolicy `json:"policy"`
	Editable bool                      `json:"editable"`
}

// Counter is how often a rule or forward has matched since the rules were last loaded.
type Counter struct {
	Packets uint64 `json:"packets"`
	Bytes   uint64 `json:"bytes"`
}

// ---- the view -------------------------------------------------------------------------------------------------

func firewallTabs(c config.NetworkConfig) []FirewallTab {
	tabs := []FirewallTab{{ID: config.DefaultNetworkID, Name: "LAN", Kind: "network", Default: true}}
	for _, n := range c.Networks {
		tabs = append(tabs, FirewallTab{ID: n.ID, Name: strings.TrimSpace(n.Name), Kind: "network"})
	}
	return append(tabs, FirewallTab{ID: config.FromWAN, Name: "Internet", Kind: "internet"})
}

func networkIsolated(c config.NetworkConfig, id string) bool {
	if id == config.DefaultNetworkID {
		return c.LanIsolated
	}
	n, _ := c.FindNetwork(id)
	return n.Isolated
}

// firewallPolicy writes the fixed lines of every tab, in the order of docs/firewall.md section 4.
func firewallPolicy(c config.NetworkConfig) map[string]FirewallPolicy {
	out := map[string]FirewallPolicy{}
	basics := PolicyLine{"Replies to connections that were started are always allowed", "allow"}
	block := PolicyLine{"Block everything else", "block"}

	var nets []FirewallTab
	for _, t := range firewallTabs(c) {
		if t.Kind == "network" {
			nets = append(nets, t)
		}
	}
	for _, t := range nets {
		p := FirewallPolicy{Before: []PolicyLine{basics, {"DHCP requests to this router are always allowed", "allow"}}}
		if t.Default {
			p.Before = append(p.Before, PolicyLine{"This router's web interface and SSH are always reachable from this network (so no rule can lock you out)", "allow"})
		}

		if t.Default {
			p.After = append(p.After, PolicyLine{"Allow everything on this router", "allow"})
		} else {
			p.After = append(p.After, PolicyLine{"Allow this router's DNS and ping", "allow"})
		}
		internet := t.Default
		if n, ok := c.FindNetwork(t.ID); ok {
			internet = n.Internet
		}
		if internet {
			p.After = append(p.After, PolicyLine{"Allow to the internet", "allow"})
		} else {
			p.After = append(p.After, PolicyLine{"No access to the internet (switched off on Local Networks)", "off"})
		}
		if networkIsolated(c, t.ID) {
			p.After = append(p.After, PolicyLine{"Isolated from the other networks (switched on on Local Networks)", "off"})
		} else {
			for _, o := range nets {
				if o.ID != t.ID && !networkIsolated(c, o.ID) {
					p.After = append(p.After, PolicyLine{"Allow to " + o.Name, "allow"})
				}
			}
		}
		p.After = append(p.After, block)
		out[t.ID] = p
	}
	wan := FirewallPolicy{Before: []PolicyLine{basics}, After: []PolicyLine{block}}
	if len(c.FirewallSettings().Forwards) > 0 {
		wan.Before = append(wan.Before, PolicyLine{"The traffic of your port forwards is let in", "allow"})
	}
	out[config.FromWAN] = wan
	return out
}

func ruleView(c, active config.NetworkConfig, r config.FirewallRule, shadow map[string]string) FirewallRule {
	old, had := active.FirewallSettings().FindRule(r.ID)
	return FirewallRule{
		ID: r.ID, Name: r.Name, Enabled: !r.Disabled, From: r.From, Action: r.Action, Protocol: r.Protocol,
		Source: r.Source, To: r.To, Destination: r.Destination, Ports: r.Ports, Log: r.Log,
		Text: c.RuleText(r), Pending: !had || old != r, ShadowedBy: shadow[r.ID],
	}
}

func forwardView(c, active config.NetworkConfig, p config.PortForward) PortForward {
	old, had := active.FirewallSettings().FindForward(p.ID)
	return PortForward{
		ID: p.ID, Name: p.Name, Enabled: !p.Disabled, Protocol: p.Protocol, ExternalPorts: p.ExternalPorts, ToIP: p.ToIP,
		InternalPorts: p.InternalPorts, Sources: append([]string{}, p.Sources...), Text: c.ForwardText(p),
		Pending: !had || !reflect.DeepEqual(old, p),
	}
}

func (a *API) firewallView() firewallResponse {
	active := a.Manager.NetworkConfig()
	draft, _ := a.Manager.StagedNetworkConfig()
	f := draft.FirewallSettings()
	shadow := map[string]string{}
	for _, s := range draft.Shadows() {
		shadow[s.Rule] = s.ShadowBy
	}
	resp := firewallResponse{Tabs: firewallTabs(draft), Policy: firewallPolicy(draft), Rules: []FirewallRule{}, Forwards: []PortForward{}, Editable: a.editable()}
	for _, r := range f.Rules {
		resp.Rules = append(resp.Rules, ruleView(draft, active, r, shadow))
	}
	for _, p := range f.Forwards {
		resp.Forwards = append(resp.Forwards, forwardView(draft, active, p))
	}
	return resp
}

// HandleFirewall: GET /api/v1/firewall returns the tabs, the rules, the forwards and the fixed lines (the draft).
func (a *API) HandleFirewall(w http.ResponseWriter, r *http.Request) {
	if allowMethods(w, r, http.MethodGet) {
		writeJSON(w, http.StatusOK, a.firewallView())
	}
}

// HandleFirewallCounters: GET /api/v1/firewall/counters returns the hit counts of the rules and forwards that are in effect.
func (a *API) HandleFirewallCounters(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	counters := map[string]Counter{}
	known := false
	if a.FirewallCounters != nil {
		if c, ok := a.FirewallCounters(); ok {
			counters, known = c, true
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Known    bool               `json:"known"`
		Counters map[string]Counter `json:"counters"`
	}{known, counters})
}

// errorsForFirewall keeps the validation problems under a prefix (renamed to the API's own field names) and the
// ones about the list as a whole. Problems with other rules, the country or the Wi-Fi are not this request's business.
func errorsForFirewall(err error, prefix, list string) error {
	var ve config.ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	var out config.ValidationError
	for _, e := range ve {
		switch {
		case strings.HasPrefix(e.Field, prefix):
			out = append(out, config.FieldError{Field: strings.TrimPrefix(e.Field, prefix), Message: e.Message})
		case e.Field == list:
			out = append(out, config.FieldError{Field: "list", Message: e.Message})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func nextID(prefix string, taken func(string) bool) string {
	for k := 1; ; k++ {
		if id := fmt.Sprintf("%s%d", prefix, k); !taken(id) {
			return id
		}
	}
}

// ---- rules ----------------------------------------------------------------------------------------------------

type firewallRuleRequest struct {
	Name        *string `json:"name"`
	Enabled     *bool   `json:"enabled"`
	From        *string `json:"from"`
	Action      *string `json:"action"`
	Protocol    *string `json:"protocol"`
	Source      *string `json:"source"`
	To          *string `json:"to"`
	Destination *string `json:"destination"`
	Ports       *string `json:"ports"`
	Log         *bool   `json:"log"`
}

func trim(s *string) string { return strings.TrimSpace(*s) }

func applyRuleRequest(r *config.FirewallRule, req firewallRuleRequest) {
	if req.Name != nil {
		r.Name = trim(req.Name)
	}
	if req.Enabled != nil {
		r.Disabled = !*req.Enabled
	}
	if req.From != nil {
		r.From = trim(req.From)
	}
	if req.Action != nil {
		r.Action = strings.ToLower(trim(req.Action))
	}
	if req.Protocol != nil {
		r.Protocol = strings.ToLower(trim(req.Protocol))
	}
	if req.Source != nil {
		r.Source = trim(req.Source)
	}
	if req.To != nil {
		r.To = trim(req.To)
	}
	if req.Destination != nil {
		r.Destination = trim(req.Destination)
	}
	if req.Ports != nil {
		r.Ports = config.NormalizePorts(trim(req.Ports))
	}
	if req.Log != nil {
		r.Log = *req.Log
	}
}

// HandleFirewallRules: POST /api/v1/firewall/rules adds a rule at the end of the list.
func (a *API) HandleFirewallRules(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	var req firewallRuleRequest
	if !decode(w, r, &req) {
		return
	}
	var id string
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.Firewall == nil {
			c.Firewall = &config.FirewallConfig{}
		}
		f := c.Firewall
		id = nextID("fw", func(id string) bool { _, ok := f.FindRule(id); return ok })
		rule := config.FirewallRule{ID: id, Protocol: config.ProtoAny}
		applyRuleRequest(&rule, req)
		f.Rules = append(f.Rules, rule)
		return errorsForFirewall(c.Validate(), fmt.Sprintf("firewall.rules[%d].", len(f.Rules)-1), "firewall.rules")
	})
	var body any
	if err == nil {
		draft, _ := a.Manager.StagedNetworkConfig()
		rule, _ := draft.FirewallSettings().FindRule(id)
		body = ruleView(draft, a.Manager.NetworkConfig(), rule, shadowMap(draft))
	}
	a.answerStaged(w, err, http.StatusCreated, "This rule cannot be added: ", body)
}

func shadowMap(c config.NetworkConfig) map[string]string {
	m := map[string]string{}
	for _, s := range c.Shadows() {
		m[s.Rule] = s.ShadowBy
	}
	return m
}

func tidyFirewall(c *config.NetworkConfig) {
	if f := c.Firewall; f != nil && len(f.Rules) == 0 && len(f.Forwards) == 0 {
		c.Firewall = nil
	}
}

// HandleFirewallRule: PUT /api/v1/firewall/rules/{id} changes a rule (what is left out stays); DELETE removes it.
func (a *API) HandleFirewallRule(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	if _, ok := draft.FirewallSettings().FindRule(id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no firewall rule with this id"})
		return
	}
	if !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			kept := c.Firewall.Rules[:0:0]
			for _, x := range c.Firewall.Rules {
				if x.ID != id {
					kept = append(kept, x)
				}
			}
			c.Firewall.Rules = kept
			tidyFirewall(c)
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.firewallView())
		return
	}

	var req firewallRuleRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		for i := range c.Firewall.Rules {
			if c.Firewall.Rules[i].ID != id {
				continue
			}
			if req.From != nil && trim(req.From) != c.Firewall.Rules[i].From {
				return config.ValidationError{{Field: "from", Message: "a rule stays on the tab it was made on: make a new rule there instead"}}
			}
			applyRuleRequest(&c.Firewall.Rules[i], req)
			return errorsForFirewall(c.Validate(), fmt.Sprintf("firewall.rules[%d].", i), "firewall.rules")
		}
		return nil
	})
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		rule, _ := now.FirewallSettings().FindRule(id)
		body = ruleView(now, a.Manager.NetworkConfig(), rule, shadowMap(now))
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

type firewallOrderRequest struct {
	From string   `json:"from"`
	IDs  []string `json:"ids"`
}

// HandleFirewallOrder: PUT /api/v1/firewall/order {"from": "lan", "ids": [...]} puts the rules of one tab in this order.
// The ids must be exactly the tab's rules.
func (a *API) HandleFirewallOrder(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut) || !a.requireEditable(w) {
		return
	}
	var req firewallOrderRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		known := false
		for _, t := range firewallTabs(*c) {
			known = known || t.ID == req.From
		}
		if !known {
			return config.ValidationError{{Field: "from", Message: "is not one of the tabs"}}
		}
		f := c.FirewallSettings()
		var slots []int
		have := map[string]bool{}
		for i, x := range f.Rules {
			if x.From == req.From {
				slots = append(slots, i)
				have[x.ID] = true
			}
		}
		seen := map[string]bool{}
		for _, id := range req.IDs {
			if !have[id] || seen[id] {
				return config.ValidationError{{Field: "ids", Message: "must list each rule of this tab exactly once"}}
			}
			seen[id] = true
		}
		if len(req.IDs) != len(slots) {
			return config.ValidationError{{Field: "ids", Message: "must list each rule of this tab exactly once"}}
		}
		byID := map[string]config.FirewallRule{}
		for _, x := range f.Rules {
			byID[x.ID] = x
		}
		for k, slot := range slots {
			c.Firewall.Rules[slot] = byID[req.IDs[k]]
		}
		return nil
	})
	a.answerStaged(w, err, http.StatusOK, "The order cannot be changed: ", a.firewallView())
}

// ---- port forwards --------------------------------------------------------------------------------------------

type portForwardRequest struct {
	Name          *string   `json:"name"`
	Enabled       *bool     `json:"enabled"`
	Protocol      *string   `json:"protocol"`
	ExternalPorts *string   `json:"external_ports"`
	ToIP          *string   `json:"to_ip"`
	InternalPorts *string   `json:"internal_ports"`
	Sources       *[]string `json:"sources"`
}

func applyForwardRequest(p *config.PortForward, req portForwardRequest) {
	if req.Name != nil {
		p.Name = trim(req.Name)
	}
	if req.Enabled != nil {
		p.Disabled = !*req.Enabled
	}
	if req.Protocol != nil {
		p.Protocol = strings.ToLower(trim(req.Protocol))
	}
	if req.ExternalPorts != nil {
		p.ExternalPorts = config.NormalizePorts(trim(req.ExternalPorts))
	}
	if req.ToIP != nil {
		p.ToIP = trim(req.ToIP)
	}
	if req.InternalPorts != nil {
		p.InternalPorts = config.NormalizePorts(trim(req.InternalPorts))
	}
	if req.Sources != nil {
		p.Sources = nil
		for _, s := range *req.Sources {
			if s = strings.TrimSpace(s); s != "" {
				p.Sources = append(p.Sources, s)
			}
		}
	}
}

// HandleFirewallForwards: POST /api/v1/firewall/forwards adds a port forward.
func (a *API) HandleFirewallForwards(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPost) || !a.requireEditable(w) {
		return
	}
	var req portForwardRequest
	if !decode(w, r, &req) {
		return
	}
	var id string
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		if c.Firewall == nil {
			c.Firewall = &config.FirewallConfig{}
		}
		f := c.Firewall
		id = nextID("pf", func(id string) bool { _, ok := f.FindForward(id); return ok })
		p := config.PortForward{ID: id}
		applyForwardRequest(&p, req)
		f.Forwards = append(f.Forwards, p)
		return errorsForFirewall(c.Validate(), fmt.Sprintf("firewall.forwards[%d].", len(f.Forwards)-1), "firewall.forwards")
	})
	var body any
	if err == nil {
		draft, _ := a.Manager.StagedNetworkConfig()
		p, _ := draft.FirewallSettings().FindForward(id)
		body = forwardView(draft, a.Manager.NetworkConfig(), p)
	}
	a.answerStaged(w, err, http.StatusCreated, "This port forward cannot be added: ", body)
}

// HandleFirewallForward: PUT /api/v1/firewall/forwards/{id} changes a port forward; DELETE removes it.
func (a *API) HandleFirewallForward(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	id := r.PathValue("id")
	draft, _ := a.Manager.StagedNetworkConfig()
	if _, ok := draft.FirewallSettings().FindForward(id); !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "there is no port forward with this id"})
		return
	}
	if !a.requireEditable(w) {
		return
	}
	if r.Method == http.MethodDelete {
		err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
			kept := c.Firewall.Forwards[:0:0]
			for _, x := range c.Firewall.Forwards {
				if x.ID != id {
					kept = append(kept, x)
				}
			}
			c.Firewall.Forwards = kept
			tidyFirewall(c)
			return nil
		})
		a.answerStaged(w, err, http.StatusOK, "", a.firewallView())
		return
	}

	var req portForwardRequest
	if !decode(w, r, &req) {
		return
	}
	err := a.Manager.StageNetwork(func(c *config.NetworkConfig) error {
		for i := range c.Firewall.Forwards {
			if c.Firewall.Forwards[i].ID != id {
				continue
			}
			applyForwardRequest(&c.Firewall.Forwards[i], req)
			return errorsForFirewall(c.Validate(), fmt.Sprintf("firewall.forwards[%d].", i), "firewall.forwards")
		}
		return nil
	})
	var body any
	if err == nil {
		now, _ := a.Manager.StagedNetworkConfig()
		p, _ := now.FirewallSettings().FindForward(id)
		body = forwardView(now, a.Manager.NetworkConfig(), p)
	}
	a.answerStaged(w, err, http.StatusOK, "These settings cannot be saved: ", body)
}

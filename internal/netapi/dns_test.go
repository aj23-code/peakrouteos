package netapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"peakroute/internal/config"
)

func (e *env) dns() dnsResponse {
	e.t.Helper()
	return decodeInto[dnsResponse](e.t, e.fw(e.api.HandleDNS, http.MethodGet, "", ""))
}

func (e *env) putUpstream(body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleDNSUpstream, http.MethodPut, "", body)
}

func (e *env) putDNSNetwork(id, body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleDNSNetwork, http.MethodPut, id, body)
}

func (e *env) addHost(body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleDNSHosts, http.MethodPost, "", body)
}

func (e *env) host(method, id, body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleDNSHost, method, id, body)
}

func (e *env) addDNSForward(body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleDNSForwards, http.MethodPost, "", body)
}

func (e *env) dnsForward(method, id, body string) *httptest.ResponseRecorder {
	return e.fw(e.api.HandleDNSForward, method, id, body)
}

// changed says whether the draft changes a field of the configuration
func (e *env) changed(field string) bool {
	for _, c := range e.staged() {
		if c.Field == field {
			return true
		}
	}
	return false
}

func (e *env) network(t *testing.T, id string) DNSNetwork {
	t.Helper()
	for _, n := range e.dns().Networks {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no network %s", id)
	return DNSNetwork{}
}

func TestTheDNSPageStartsWithTheProviderAndTheRouter(t *testing.T) {
	e := fwEnv(t) // LAN and Guest (net2, isolated, no internet, with DHCP)
	e.api.ProviderDNS = func() []string { return []string{"192.168.0.1"} }
	d := e.dns()
	if d.Upstream.Mode != "provider" || len(d.Upstream.Servers) != 0 || d.Upstream.Servers == nil || len(d.Upstream.Provider) != 1 || d.Upstream.Provider[0] != "192.168.0.1" || d.Upstream.Static || d.Upstream.Pending {
		t.Fatalf("%+v", d.Upstream)
	}
	if d.Domain != "" || len(d.Hosts) != 0 || d.Hosts == nil || d.Forwards == nil || len(d.Forwards) != 0 || !d.Editable {
		t.Fatalf("%+v", d)
	}
	if len(d.Networks) != 2 {
		t.Fatalf("%+v", d.Networks)
	}
	lan, guest := d.Networks[0], d.Networks[1]
	if lan.ID != "lan" || !lan.Default || lan.Name != "LAN" || lan.Mode != "router" || lan.Router != "192.168.1.1" || len(lan.Servers) != 0 || lan.Servers == nil || lan.Notes == nil ||
		guest.ID != "net2" || guest.Name != "Guest" || guest.Mode != "router" || guest.Router != "10.20.0.1" || !guest.DHCP {
		t.Fatalf("%+v", d.Networks)
	}
	// without a provider function the list of the provider's servers is empty, not null
	e.api.ProviderDNS = nil
	if got := e.dns().Upstream.Provider; got == nil || len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestSettingTheUpstreamServers(t *testing.T) {
	e := fwEnv(t)
	rec := e.putUpstream(`{"servers":[" 9.9.9.9 ","","149.112.112.112"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := decodeInto[dnsResponse](t, rec)
	if d.Upstream.Mode != "custom" || len(d.Upstream.Servers) != 2 || d.Upstream.Servers[0] != "9.9.9.9" || !d.Upstream.Pending {
		t.Fatalf("%+v", d.Upstream)
	}
	if len(e.mgr.NetworkConfig().WanDNS) != 0 {
		t.Fatal("the active configuration changed")
	}
	// it is the same list the Internet page shows
	if u := decodeInto[Uplink](t, e.uplink(http.MethodGet, "")); len(u.DNS) != 2 {
		t.Fatalf("%+v", u.DNS)
	}
	// back to the provider's
	d = decodeInto[dnsResponse](t, e.putUpstream(`{"servers":[]}`))
	if d.Upstream.Mode != "provider" || d.Upstream.Pending || e.changed("wan_dns") {
		t.Fatalf("%+v", d.Upstream)
	}
}

func TestAFixedAddressNeedsUpstreamServers(t *testing.T) {
	e := fwEnv(t)
	e.saveWAN(staticWAN)
	e.commit()
	if !e.dns().Upstream.Static {
		t.Fatal("not shown as static")
	}
	rec := e.putUpstream(`{"servers":[]}`)
	if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["servers"] {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.putUpstream(`{"servers":["1.1.1.1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestUpstreamServerErrorsNameTheField(t *testing.T) {
	e := fwEnv(t)
	for name, body := range map[string]string{
		"not an address": `{"servers":["dns.example"]}`,
		"loopback":       `{"servers":["127.0.0.1"]}`,
		"four servers":   `{"servers":["1.1.1.1","8.8.8.8","9.9.9.9","8.8.4.4"]}`,
		"twice":          `{"servers":["1.1.1.1","1.1.1.1"]}`,
		"injecting":      `{"servers":["1.1.1.1\nserver=6.6.6.6"]}`,
	} {
		rec := e.putUpstream(body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["servers"] {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(e.draft().WanDNS) != 0 {
		t.Fatal("a refused save left servers in the draft")
	}
}

func TestChoosingTheServersANetworkIsTold(t *testing.T) {
	e := fwEnv(t)
	rec := e.putDNSNetwork("lan", `{"servers":["192.168.1.5","192.168.1.6"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	lan := e.network(t, "lan")
	if lan.Mode != "custom" || len(lan.Servers) != 2 || !lan.Pending || e.draft().LanDNSServers[0] != "192.168.1.5" {
		t.Fatalf("%+v", lan)
	}
	if e.network(t, "net2").Mode != "router" {
		t.Fatal("another network changed")
	}
	// another network
	if rec := e.putDNSNetwork("net2", `{"servers":["1.1.1.1"]}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := e.draft().Networks[0].DNSServers; len(got) != 1 || got[0] != "1.1.1.1" {
		t.Fatalf("%v", got)
	}
	// back to the router: no draft is left
	e.putDNSNetwork("lan", `{"servers":[]}`)
	e.putDNSNetwork("net2", `{"servers":[]}`)
	if d := e.draft(); len(d.LanDNSServers) != 0 || len(d.Networks[0].DNSServers) != 0 || e.network(t, "lan").Mode != "router" || e.network(t, "net2").Mode != "router" {
		t.Fatalf("%+v", d)
	}
	for name, tc := range map[string]struct{ id, body string }{
		"not an address": {"lan", `{"servers":["pihole"]}`},
		"four":           {"net2", `{"servers":["1.1.1.1","8.8.8.8","9.9.9.9","8.8.4.4"]}`},
		"broadcast":      {"lan", `{"servers":["255.255.255.255"]}`},
	} {
		rec := e.putDNSNetwork(tc.id, tc.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["servers"] {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := e.putDNSNetwork("ghost", `{"servers":[]}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown network: %d", rec.Code)
	}
}

func TestTheNotesAboutAnetworksServers(t *testing.T) {
	e := fwEnv(t) // LAN has DHCP; Guest (net2) has DHCP, no internet
	// a server on the network itself needs no note
	e.putDNSNetwork("lan", `{"servers":["192.168.1.5"]}`)
	if n := e.network(t, "lan").Notes; len(n) != 0 {
		t.Errorf("%v", n)
	}
	// a server on another network: the firewall has to allow it
	e.putDNSNetwork("net2", `{"servers":["192.168.1.5"]}`)
	if n := e.network(t, "net2").Notes; len(n) != 1 || !strings.Contains(n[0], "192.168.1.5 is on LAN") || !strings.Contains(n[0], "firewall rule") {
		t.Errorf("%v", n)
	}
	// a public server for a network that cannot reach the internet
	e.putDNSNetwork("net2", `{"servers":["1.1.1.1"]}`)
	if n := e.network(t, "net2").Notes; len(n) != 1 || !strings.Contains(n[0], "no internet access") {
		t.Errorf("%v", n)
	}
	// the same server is fine once the network may reach the internet
	e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","internet":true,"isolated":true}`)
	if n := e.network(t, "net2").Notes; len(n) != 0 {
		t.Errorf("%v", n)
	}
	// no DHCP server on the router for the network: nothing tells the devices
	e.net(http.MethodPut, "net2", `{"ip":"10.20.0.1","netmask":"255.255.255.0","dhcp":{"enabled":false,"range_start":"10.20.0.100","range_end":"10.20.0.200","lease_time":"1h"}}`)
	n := e.network(t, "net2")
	if n.DHCP || len(n.Notes) == 0 || !strings.Contains(n.Notes[0], "does not run a DHCP server") {
		t.Errorf("%+v", n)
	}
	// without servers there is nothing to note
	e.putDNSNetwork("net2", `{"servers":[]}`)
	if n := e.network(t, "net2").Notes; len(n) != 0 {
		t.Errorf("%v", n)
	}
}

func TestLocalNames(t *testing.T) {
	e := fwEnv(t)
	rec := e.addHost(`{"name":" NAS ","address":"192.168.1.50"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	h := decodeInto[DNSHost](t, rec)
	if h.ID != "h1" || h.Name != "nas" || h.Address != "192.168.1.50" || !h.Enabled || !h.Pending || h.Text != "nas is 192.168.1.50" {
		t.Fatalf("%+v", h)
	}
	if e.mgr.NetworkConfig().DNS != nil {
		t.Fatal("the active configuration changed")
	}
	decodeInto[DNSHost](t, e.addHost(`{"name":"home.example.com","address":"192.168.1.50"}`))
	if got := e.dns().Hosts; len(got) != 2 || got[0].ID != "h1" || got[1].ID != "h2" {
		t.Fatalf("%+v", got)
	}
	e.commit()
	if got := e.dns().Hosts; got[0].Pending || got[1].Pending {
		t.Fatalf("%+v", got)
	}
	// changing: what is left out stays
	h = decodeInto[DNSHost](t, e.host(http.MethodPut, "h1", `{"address":"192.168.1.51","enabled":false}`))
	if h.Name != "nas" || h.Address != "192.168.1.51" || h.Enabled || !h.Pending {
		t.Fatalf("%+v", h)
	}
	h = decodeInto[DNSHost](t, e.host(http.MethodPut, "h1", `{"address":"192.168.1.50","enabled":true}`))
	if h.Pending || e.changed("dns_hosts") {
		t.Fatalf("put back, still pending: %+v", h)
	}
	// deleting; the lowest free id is used again; no empty section is left
	rec = e.host(http.MethodDelete, "h1", "")
	if rec.Code != http.StatusOK || len(decodeInto[dnsResponse](t, rec).Hosts) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if h := decodeInto[DNSHost](t, e.addHost(`{"name":"nas","address":"192.168.1.60"}`)); h.ID != "h1" {
		t.Fatalf("%+v", h)
	}
	e.host(http.MethodDelete, "h1", "")
	e.host(http.MethodDelete, "h2", "")
	e.mgr.DiscardNetwork()
	e2 := fwEnv(t)
	e2.addHost(`{"name":"a","address":"192.168.1.5"}`)
	e2.host(http.MethodDelete, "h1", "")
	if e2.draft().DNS != nil {
		t.Fatalf("an empty section is left: %+v", e2.draft().DNS)
	}
	if rec := e.host(http.MethodDelete, "h9", ""); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
	if rec := e.host(http.MethodPut, "h9", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
}

func TestLocalNameErrorsNameTheirFields(t *testing.T) {
	e := fwEnv(t)
	e.addHost(`{"name":"nas","address":"192.168.1.50"}`)
	for name, tc := range map[string]struct{ body, field string }{
		"no name":             {`{"address":"192.168.1.5"}`, "name"},
		"a name with a space": {`{"name":"my nas","address":"192.168.1.5"}`, "name"},
		"the name twice":      {`{"name":"nas","address":"192.168.1.5"}`, "name"},
		"no address":          {`{"name":"x"}`, "address"},
		"a name as address":   {`{"name":"x","address":"nas.lan"}`, "address"},
		"loopback":            {`{"name":"x","address":"127.0.0.1"}`, "address"},
		"ipv6":                {`{"name":"x","address":"fd00::1"}`, "address"},
	} {
		rec := e.addHost(tc.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[tc.field] {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if got := e.dns().Hosts; len(got) != 1 {
		t.Fatalf("a refused name was staged: %+v", got)
	}
	// an edit is judged on its own, not on another entry's problem
	e.addHost(`{"name":"other","address":"192.168.1.60"}`)
	e.mgr.StageNetwork(func(c *config.NetworkConfig) error { c.DNS.Hosts[1].Address = "junk"; return nil })
	if rec := e.host(http.MethodPut, "h1", `{"address":"192.168.1.51"}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestForwardedDomains(t *testing.T) {
	e := fwEnv(t)
	rec := e.addDNSForward(`{"domain":" Corp.Example.com ","servers":[" 10.0.0.5 ","","10.0.0.6"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f := decodeInto[DNSForward](t, rec)
	if f.ID != "f1" || f.Domain != "corp.example.com" || len(f.Servers) != 2 || f.Servers[1] != "10.0.0.6" || !f.Enabled || !f.Pending || f.Text != "corp.example.com is asked of 10.0.0.5, 10.0.0.6" {
		t.Fatalf("%+v", f)
	}
	e.commit()
	if got := e.dns().Forwards; got[0].Pending {
		t.Fatalf("%+v", got)
	}
	// what is left out stays; the servers can be replaced; it can be switched off
	f = decodeInto[DNSForward](t, e.dnsForward(http.MethodPut, "f1", `{"servers":["10.0.0.7"]}`))
	if f.Domain != "corp.example.com" || len(f.Servers) != 1 || f.Servers[0] != "10.0.0.7" || !f.Pending {
		t.Fatalf("%+v", f)
	}
	f = decodeInto[DNSForward](t, e.dnsForward(http.MethodPut, "f1", `{"servers":["10.0.0.5","10.0.0.6"],"enabled":false}`))
	if f.Enabled || !f.Pending {
		t.Fatalf("%+v", f)
	}
	f = decodeInto[DNSForward](t, e.dnsForward(http.MethodPut, "f1", `{"enabled":true}`))
	if f.Pending || e.changed("dns_forwards") {
		t.Fatalf("%+v", f)
	}
	// deleting
	rec = e.dnsForward(http.MethodDelete, "f1", "")
	if rec.Code != http.StatusOK || len(decodeInto[dnsResponse](t, rec).Forwards) != 0 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.dnsForward(http.MethodDelete, "f9", ""); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
	if rec := e.dnsForward(http.MethodPut, "f9", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("%d", rec.Code)
	}
	if e.draft().DNS != nil {
		t.Errorf("an empty section is left: %+v", e.draft().DNS)
	}
}

func TestForwardedDomainErrorsNameTheirFields(t *testing.T) {
	e := fwEnv(t)
	e.addDNSForward(`{"domain":"a.example","servers":["10.0.0.5"]}`)
	for name, tc := range map[string]struct{ body, field string }{
		"no domain":      {`{"servers":["10.0.0.5"]}`, "domain"},
		"a bad domain":   {`{"domain":"a b","servers":["10.0.0.5"]}`, "domain"},
		"the same twice": {`{"domain":"a.example","servers":["10.0.0.6"]}`, "domain"},
		"no servers":     {`{"domain":"b.example"}`, "servers"},
		"a bad server":   {`{"domain":"b.example","servers":["dns"]}`, "servers"},
		"four servers":   {`{"domain":"b.example","servers":["10.0.0.1","10.0.0.2","10.0.0.3","10.0.0.4"]}`, "servers"},
	} {
		rec := e.addDNSForward(tc.body)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)[tc.field] {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if got := e.dns().Forwards; len(got) != 1 {
		t.Fatalf("a refused domain was staged: %+v", got)
	}
}

func TestTheLocalDomainIsSetOnTheSettingsPage(t *testing.T) {
	e := standard(t)
	if s := e.settings(); s.Domain != "" || s.DomainPending {
		t.Fatalf("%+v", s)
	}
	set := func(body string) *httptest.ResponseRecorder {
		return e.do(e.api.HandleSettingsDomain, http.MethodPut, body, "application/json")
	}
	rec := set(`{"domain":" Home.Example.COM "}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if s := decodeInto[Settings](t, rec); s.Domain != "home.example.com" || !s.DomainPending {
		t.Fatalf("%+v", s)
	}
	if e.mgr.NetworkConfig().Domain != "" {
		t.Fatal("the active configuration changed")
	}
	// the DNS page shows it
	if got := e.dns().Domain; got != "home.example.com" {
		t.Fatalf("%q", got)
	}
	e.commit()
	if s := e.settings(); s.Domain != "home.example.com" || s.DomainPending {
		t.Fatalf("%+v", s)
	}
	set(`{"domain":""}`)
	set(`{"domain":"home.example.com"}`)
	if e.changed("domain") {
		t.Fatal("putting it back left a draft")
	}
	for _, bad := range []string{"my lan", "local", "localhost", "lan.", "a_b", "-lan", "x/y"} {
		rec := set(`{"domain":"` + bad + `"}`)
		if rec.Code != http.StatusBadRequest || !errorFields(t, rec)["domain"] {
			t.Errorf("%q: %d %s", bad, rec.Code, rec.Body)
		}
	}
	// a domain that would swallow a forwarded domain is refused
	e2 := fwEnv(t)
	e2.addDNSForward(`{"domain":"lab.home.example.com","servers":["10.0.0.5"]}`)
	rec = e2.do(e2.api.HandleSettingsDomain, http.MethodPut, `{"domain":"home.example.com"}`, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.do(e.api.HandleSettingsDomain, http.MethodPost, `{}`, "application/json"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("method: %d", rec.Code)
	}
	e.editable = false
	if rec := set(`{"domain":"lan"}`); rec.Code != http.StatusConflict {
		t.Errorf("while applying: %d", rec.Code)
	}
}

func TestDNSEditsAreRefusedWhileApplyingAndRequestsAreStrict(t *testing.T) {
	e := fwEnv(t)
	e.addHost(`{"name":"nas","address":"192.168.1.50"}`)
	e.addDNSForward(`{"domain":"a.example","servers":["10.0.0.5"]}`)
	e.editable = false
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"upstream":       e.putUpstream(`{"servers":[]}`),
		"network":        e.putDNSNetwork("lan", `{"servers":[]}`),
		"add host":       e.addHost(`{"name":"x","address":"192.168.1.5"}`),
		"change host":    e.host(http.MethodPut, "h1", `{"name":"y"}`),
		"delete host":    e.host(http.MethodDelete, "h1", ""),
		"add forward":    e.addDNSForward(`{"domain":"b.example","servers":["10.0.0.5"]}`),
		"change forward": e.dnsForward(http.MethodPut, "f1", `{"enabled":false}`),
		"delete forward": e.dnsForward(http.MethodDelete, "f1", ""),
	} {
		if rec.Code != http.StatusConflict {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	if e.dns().Editable {
		t.Fatal("editable while applying")
	}
	e.editable = true
	if rec := e.addHost(`{"name":"x","address":"192.168.1.5","colour":"red"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := e.putUpstream(`{"servers":[],"mode":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", rec.Code)
	}
	if rec := e.fw(e.api.HandleDNS, http.MethodPost, "", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("method: %d", rec.Code)
	}
}

func TestASingleServerIsACustomChoice(t *testing.T) {
	e := fwEnv(t)
	if d := decodeInto[dnsResponse](t, e.putUpstream(`{"servers":["9.9.9.9"]}`)); d.Upstream.Mode != "custom" || len(d.Upstream.Servers) != 1 {
		t.Fatalf("%+v", d.Upstream)
	}
	e.putDNSNetwork("lan", `{"servers":["192.168.1.5"]}`)
	if n := e.network(t, "lan"); n.Mode != "custom" || len(n.Servers) != 1 {
		t.Fatalf("%+v", n)
	}
}

func TestAChangedForwardedDomainIsPending(t *testing.T) {
	e := fwEnv(t)
	e.addDNSForward(`{"domain":"a.example","servers":["10.0.0.5"]}`)
	e.commit()
	if f := decodeInto[DNSForward](t, e.dnsForward(http.MethodPut, "f1", `{"domain":"b.example"}`)); !f.Pending {
		t.Fatalf("a changed domain is not pending: %+v", f)
	}
}

func TestTheServersGoToTheNetworkThatWasAsked(t *testing.T) {
	e := fwEnv(t) // LAN, Guest (net2)
	if rec := e.create(`{"name":"IoT","vlan":30,"ip":"10.30.0.1","netmask":"255.255.255.0"}`); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.putDNSNetwork("net3", `{"servers":["10.30.0.5"]}`); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	d := e.draft()
	if len(d.Networks[0].DNSServers) != 0 || len(d.Networks[1].DNSServers) != 1 || d.Networks[1].ID != "net3" || len(d.LanDNSServers) != 0 {
		t.Fatalf("%+v", d.Networks)
	}
}

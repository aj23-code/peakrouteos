package config

import (
	"encoding/json"
	"strings"
	"testing"
)

const goodCF = "abcdefghijklmnopqrstuvwxyz0123456789ABCD"

func ddnsNet(entries ...DDNSEntry) NetworkConfig {
	c := validNet()
	c.DDNS = &DDNSConfig{Entries: entries}
	return c
}

func dyn(id, host string) DDNSEntry {
	return DDNSEntry{ID: id, Provider: DDNSDynDNS2, Hostname: host, Server: "members.dyndns.org", Username: "me", Password: "secret", Source: DDNSSourceWAN}
}

func withDDNS(f func(*DDNSEntry)) NetworkConfig {
	e := dyn("d1", "home.example.com")
	f(&e)
	return ddnsNet(e)
}

func TestDynamicDNSEntriesThatAreAccepted(t *testing.T) {
	cases := map[string]NetworkConfig{
		"none":           validNet(),
		"an empty block": ddnsNet(),
		"dyndns2":        ddnsNet(dyn("d1", "home.example.com")),
		"no-ip, web ip":  withDDNS(func(e *DDNSEntry) { e.Server = "dynupdate.no-ip.com"; e.Source = DDNSSourceWeb }),
		"switched off":   withDDNS(func(e *DDNSEntry) { e.Disabled = true }),
		"duckdns":        ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSDuckDNS, Hostname: "mine.duckdns.org", Password: "1234abcd-12ab-34cd-56ef-0123456789ab", Source: DDNSSourceWAN}),
		"cloudflare":     ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSCloudflare, Hostname: "home.example.com", Password: goodCF, Source: DDNSSourceWAN}),
		"custom": ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSCustom, Hostname: "home.example.com", Source: DDNSSourceWAN,
			URL: "https://dynamicdns.park-your-domain.com/update?host=@&domain=example.com&password={password}&ip={ip}", Password: "pw"}),
		"custom, no secret": ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSCustom, Hostname: "home.example.com", Source: DDNSSourceWAN, URL: "https://ddns.example.net/set/{hostname}/{ip}"}),
		"custom with user":  ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSCustom, Hostname: "home.example.com", Source: DDNSSourceWAN, URL: "https://ddns.example.net:8443/u/{username}/{password}?ip={ip}", Username: "u", Password: "p"}),
		"two names":         ddnsNet(dyn("d1", "a.example.com"), dyn("d2", "b.example.com")),
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	many := validNet()
	many.DDNS = &DDNSConfig{}
	for i := 1; i <= MaxDDNSEntries; i++ {
		many.DDNS.Entries = append(many.DDNS.Entries, dyn("d"+string(rune('0'+i%10))+string(rune('a'+i)), "h"+string(rune('a'+i))+".example.com"))
	}
	if err := many.Validate(); err != nil {
		t.Errorf("ten names: %v", err)
	}
}

func TestDynamicDNSEntriesThatAreRefused(t *testing.T) {
	custom := func(f func(*DDNSEntry)) NetworkConfig {
		e := DDNSEntry{ID: "d1", Provider: DDNSCustom, Hostname: "home.example.com", Source: DDNSSourceWAN, URL: "https://ddns.example.net/u?h={hostname}&ip={ip}"}
		f(&e)
		return ddnsNet(e)
	}
	cases := []struct {
		name  string
		c     NetworkConfig
		field string
	}{
		{"a bad id", withDDNS(func(e *DDNSEntry) { e.ID = "D 1" }), "ddns.entries[0].id"},
		{"the same id twice", ddnsNet(dyn("d1", "a.example.com"), dyn("d1", "b.example.com")), "ddns.entries[1].id"},
		{"a name without a dot", withDDNS(func(e *DDNSEntry) { e.Hostname = "home" }), "ddns.entries[0].hostname"},
		{"a name in capitals", withDDNS(func(e *DDNSEntry) { e.Hostname = "Home.Example.com" }), "ddns.entries[0].hostname"},
		{"an empty name", withDDNS(func(e *DDNSEntry) { e.Hostname = "" }), "ddns.entries[0].hostname"},
		{"a url as the name", withDDNS(func(e *DDNSEntry) { e.Hostname = "https://home.example.com" }), "ddns.entries[0].hostname"},
		{"the same name twice", ddnsNet(dyn("d1", "a.example.com"), dyn("d2", "a.example.com")), "ddns.entries[1].hostname"},
		{"a made up service", withDDNS(func(e *DDNSEntry) { e.Provider = "dyndns" }), "ddns.entries[0].provider"},
		{"no source", withDDNS(func(e *DDNSEntry) { e.Source = "" }), "ddns.entries[0].source"},
		{"another source", withDDNS(func(e *DDNSEntry) { e.Source = "lan" }), "ddns.entries[0].source"},
		{"no server", withDDNS(func(e *DDNSEntry) { e.Server = "" }), "ddns.entries[0].server"},
		{"an address as the server", withDDNS(func(e *DDNSEntry) { e.Server = "203.0.113.5" }), "ddns.entries[0].server"},
		{"this router as the server", withDDNS(func(e *DDNSEntry) { e.Server = "localhost" }), "ddns.entries[0].server"},
		{"a url as the server", withDDNS(func(e *DDNSEntry) { e.Server = "https://members.dyndns.org" }), "ddns.entries[0].server"},
		{"no user", withDDNS(func(e *DDNSEntry) { e.Username = "" }), "ddns.entries[0].username"},
		{"no password", withDDNS(func(e *DDNSEntry) { e.Password = "" }), "ddns.entries[0].password"},
		{"a password with a line break", withDDNS(func(e *DDNSEntry) { e.Password = "a\nb" }), "ddns.entries[0].password"},
		{"a user with a line break", withDDNS(func(e *DDNSEntry) { e.Username = "a\r\nX-Evil: 1" }), "ddns.entries[0].password"},
		{"a very long password", withDDNS(func(e *DDNSEntry) { e.Password = strings.Repeat("a", 300) }), "ddns.entries[0].password"},
		{"a url on a dyndns2 entry", withDDNS(func(e *DDNSEntry) { e.URL = "https://x.example/{ip}" }), "ddns.entries[0].url"},
		{"duckdns, not under duckdns.org", ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSDuckDNS, Hostname: "mine.example.com", Password: "1234abcd-1234", Source: "wan"}), "ddns.entries[0].hostname"},
		{"duckdns, a name below a name", ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSDuckDNS, Hostname: "a.b.duckdns.org", Password: "1234abcd-1234", Source: "wan"}), "ddns.entries[0].hostname"},
		{"duckdns, a short token", ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSDuckDNS, Hostname: "mine.duckdns.org", Password: "abc", Source: "wan"}), "ddns.entries[0].password"},
		{"duckdns, a bad token", ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSDuckDNS, Hostname: "mine.duckdns.org", Password: "bad token!", Source: "wan"}), "ddns.entries[0].password"},
		{"duckdns, a server", ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSDuckDNS, Hostname: "mine.duckdns.org", Password: "1234abcd-1234", Source: "wan", Server: "x.example"}), "ddns.entries[0].server"},
		{"cloudflare, a short token", ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSCloudflare, Hostname: "home.example.com", Password: "short", Source: "wan"}), "ddns.entries[0].password"},
		{"cloudflare, a token with a space", ddnsNet(DDNSEntry{ID: "d1", Provider: DDNSCloudflare, Hostname: "home.example.com", Password: goodCF[:20] + " " + goodCF[20:], Source: "wan"}), "ddns.entries[0].password"},
		{"custom, no url", custom(func(e *DDNSEntry) { e.URL = "" }), "ddns.entries[0].url"},
		{"custom, http", custom(func(e *DDNSEntry) { e.URL = "http://ddns.example.net/{ip}" }), "ddns.entries[0].url"},
		{"custom, no {ip}", custom(func(e *DDNSEntry) { e.URL = "https://ddns.example.net/update" }), "ddns.entries[0].url"},
		{"custom, a made up placeholder", custom(func(e *DDNSEntry) { e.URL = "https://ddns.example.net/{ip}/{token}" }), "ddns.entries[0].url"},
		{"custom, credentials inside", custom(func(e *DDNSEntry) { e.URL = "https://me:pw@ddns.example.net/{ip}" }), "ddns.entries[0].url"},
		{"custom, a placeholder in the host", custom(func(e *DDNSEntry) { e.URL = "https://{hostname}.example.net/{ip}" }), "ddns.entries[0].url"},
		{"custom, this router", custom(func(e *DDNSEntry) { e.URL = "https://127.0.0.1/{ip}" }), "ddns.entries[0].url"},
		{"custom, localhost", custom(func(e *DDNSEntry) { e.URL = "https://localhost/{ip}" }), "ddns.entries[0].url"},
		{"custom, a space", custom(func(e *DDNSEntry) { e.URL = "https://ddns.example.net/a b/{ip}" }), "ddns.entries[0].url"},
		{"custom, a line break", custom(func(e *DDNSEntry) { e.URL = "https://ddns.example.net/{ip}\nHost: x" }), "ddns.entries[0].url"},
		{"custom, a host in capitals", custom(func(e *DDNSEntry) { e.URL = "https://DDNS.example.net/{ip}" }), "ddns.entries[0].url"},
		{"custom, wants a password and has none", custom(func(e *DDNSEntry) { e.URL = "https://ddns.example.net/{password}/{ip}" }), "ddns.entries[0].password"},
		{"custom, wants a user and has none", custom(func(e *DDNSEntry) { e.URL = "https://ddns.example.net/{username}/{ip}" }), "ddns.entries[0].username"},
		{"custom, too long", custom(func(e *DDNSEntry) { e.URL = "https://ddns.example.net/" + strings.Repeat("a", 600) + "/{ip}" }), "ddns.entries[0].url"},
	}
	for _, tc := range cases {
		if !errorFieldsOf(t, tc.c)[tc.field] {
			t.Errorf("%s: no error on %s (got %v)", tc.name, tc.field, errorFieldsOf(t, tc.c))
		}
	}
	many := validNet()
	many.DDNS = &DDNSConfig{}
	for i := 0; i <= MaxDDNSEntries; i++ {
		many.DDNS.Entries = append(many.DDNS.Entries, dyn("d"+string(rune('a'+i)), "h"+string(rune('a'+i))+".example.com"))
	}
	if !errorFieldsOf(t, many)["ddns.entries"] {
		t.Error("eleven names were accepted")
	}
}

func TestDynamicDNSIsCopiedAndWrittenOnlyWhenUsed(t *testing.T) {
	plain, _ := json.Marshal(validNet())
	if strings.Contains(string(plain), "ddns") {
		t.Errorf("ddns written for a plain configuration: %s", plain)
	}
	c := ddnsNet(dyn("d1", "home.example.com"))
	cp := c.clone()
	cp.DDNS.Entries[0].Hostname = "other.example.com"
	cp.DDNS.Entries[0].Password = "changed"
	if c.DDNS.Entries[0].Hostname != "home.example.com" || c.DDNS.Entries[0].Password != "secret" {
		t.Fatal("the copy shares the entries with the original")
	}
	s := c.DDNSSettings()
	s.Entries[0].Hostname = "x"
	if c.DDNS.Entries[0].Hostname != "home.example.com" {
		t.Fatal("DDNSSettings shares its entries")
	}
	if got, ok := c.DDNSSettings().FindDDNS("d1"); !ok || got.Hostname != "home.example.com" {
		t.Fatal("FindDDNS")
	}
	if _, ok := c.DDNSSettings().FindDDNS("d9"); ok {
		t.Fatal("FindDDNS found a name that is not there")
	}
	if got := validNet().DDNSSettings(); len(got.Entries) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestDynamicDNSReviewLinesNeverShowAPassword(t *testing.T) {
	base := validNet()
	line := func(a, b NetworkConfig, field string) *Change {
		for _, c := range DiffNetwork(a, b) {
			if c.Field == field {
				c := c
				return &c
			}
		}
		return nil
	}
	added := ddnsNet(dyn("d1", "home.example.com"))
	l := line(base, added, "ddns")
	if l == nil || l.Label != "Dynamic DNS home.example.com" || l.Old != "(none)" || l.New != "dyndns2 at members.dyndns.org, address of the internet connection" {
		t.Fatalf("%+v", l)
	}
	// a new entry's password is not a separate line, and no line holds it
	for _, c := range DiffNetwork(base, added) {
		if strings.Contains(c.Old+c.New+c.Label, "secret") {
			t.Fatalf("a password in the review: %+v", c)
		}
	}
	web := withDDNS(func(e *DDNSEntry) { e.Source = DDNSSourceWeb; e.Disabled = true })
	if l := line(added, web, "ddns"); l == nil || !strings.Contains(l.New, "address an internet service reports") || !strings.HasSuffix(l.New, "switched off") {
		t.Fatalf("%+v", l)
	}
	// the password: set, changed, cleared, and nothing else
	if l := line(added, withDDNS(func(e *DDNSEntry) { e.Password = "other" }), "ddns_password"); l == nil || l.Old != "set" || l.New != "changed" || l.Label != "Dynamic DNS password of home.example.com" {
		t.Fatalf("%+v", l)
	}
	if line(added, withDDNS(func(e *DDNSEntry) { e.Password = "other" }), "ddns") != nil {
		t.Error("a password change also shows as a change of the name")
	}
	noPw := withDDNS(func(e *DDNSEntry) { e.Password = "" })
	if l := line(noPw, added, "ddns_password"); l == nil || l.Old != "(none)" || l.New != "set" {
		t.Fatalf("%+v", l)
	}
	if l := line(added, noPw, "ddns_password"); l == nil || l.Old != "set" || l.New != "(none)" {
		t.Fatalf("%+v", l)
	}
	// a rename shows the old name; removing shows a removal
	renamed := withDDNS(func(e *DDNSEntry) { e.Hostname = "new.example.com" })
	if l := line(added, renamed, "ddns"); l == nil || !strings.HasPrefix(l.Old, `"home.example.com": `) || l.Label != "Dynamic DNS new.example.com" {
		t.Fatalf("%+v", l)
	}
	if l := line(added, base, "ddns"); l == nil || l.New != "(removed)" {
		t.Fatalf("%+v", l)
	}
	if got := DiffNetwork(added, added); len(got) != 0 {
		t.Fatalf("no change, but %+v", got)
	}
	for _, e := range []DDNSEntry{{Provider: DDNSDuckDNS, Source: "wan"}, {Provider: DDNSCloudflare, Source: "wan"}, {Provider: DDNSCustom, Source: "wan"}} {
		if txt := DDNSText(e); !strings.Contains(txt, "address of the internet connection") || strings.Contains(txt, "dyndns2") {
			t.Errorf("%q", txt)
		}
	}
}

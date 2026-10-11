package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

// Dynamic DNS: keeping a public name pointed at the address the internet connection has right now. docs/ddns.md has the
// model and the reasons.

const (
	MaxDDNSEntries = 10

	DDNSDynDNS2    = "dyndns2"    // the protocol DynDNS, No-IP, Dynu and many others speak
	DDNSDuckDNS    = "duckdns"    // duckdns.org, with a token
	DDNSCloudflare = "cloudflare" // a record in a Cloudflare zone, with an API token
	DDNSCustom     = "custom"     // any service that updates with one https request

	DDNSSourceWAN = "wan" // the address the internet connection has
	DDNSSourceWeb = "web" // the address an internet service says the router has (behind another router, or a carrier's NAT)
)

// DDNSServers are the dyndns2 servers the page offers by name.
var DDNSServers = []struct{ Name, Host string }{
	{"DynDNS", "members.dyndns.org"},
	{"No-IP", "dynupdate.no-ip.com"},
	{"Dynu", "api.dynu.com"},
}

// DDNSEntry keeps one name up to date.
type DDNSEntry struct {
	ID       string `json:"id"` // d1, d2...
	Provider string `json:"provider"`
	Hostname string `json:"hostname"`           // the name that is kept up to date, such as home.example.com
	Server   string `json:"server,omitempty"`   // dyndns2: the update server
	Username string `json:"username,omitempty"` // dyndns2 (and custom, when the URL asks for {username})
	Password string `json:"password,omitempty"` // the password, the token or the API token; never shown again
	URL      string `json:"url,omitempty"`      // custom: the https address to call, with {ip}, {hostname}, {username}, {password}
	Source   string `json:"source"`             // wan or web
	Disabled bool   `json:"disabled,omitempty"`
}

// DDNSConfig is the Dynamic DNS page's settings.
type DDNSConfig struct {
	Entries []DDNSEntry `json:"entries,omitempty"`
}

func (d DDNSConfig) clone() DDNSConfig {
	d.Entries = append([]DDNSEntry(nil), d.Entries...)
	return d
}

// DDNSSettings returns the Dynamic DNS entries, none when there are none.
func (c NetworkConfig) DDNSSettings() DDNSConfig {
	if c.DDNS == nil {
		return DDNSConfig{}
	}
	return c.DDNS.clone()
}

// FindDDNS returns an entry by id.
func (d DDNSConfig) FindDDNS(id string) (DDNSEntry, bool) {
	for _, e := range d.Entries {
		if e.ID == id {
			return e, true
		}
	}
	return DDNSEntry{}, false
}

var providerNames = map[string]string{DDNSDynDNS2: "dyndns2", DDNSDuckDNS: "DuckDNS", DDNSCloudflare: "Cloudflare", DDNSCustom: "custom URL"}

// DDNSText describes an entry in a line, without any secret: "DuckDNS, address of the internet connection".
func DDNSText(e DDNSEntry) string {
	what := providerNames[e.Provider]
	if e.Provider == DDNSDynDNS2 && e.Server != "" {
		what = "dyndns2 at " + e.Server
	}
	if what == "" {
		what = e.Provider
	}
	src := "address of the internet connection"
	if e.Source == DDNSSourceWeb {
		src = "address an internet service reports"
	}
	t := what + ", " + src
	if e.Disabled {
		t += ", switched off"
	}
	return t
}

var (
	ddnsPlaceholder = regexp.MustCompile(`\{[a-z]*\}`)
	duckTokenRe     = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)
	cfTokenRe       = regexp.MustCompile(`^[A-Za-z0-9_-]{20,100}$`)
)

func plainText(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ddnsTemplateOK checks a custom URL: https, a proper host, no credentials inside, only the four placeholders, and {ip}.
func ddnsTemplateOK(raw string) (needsUser, needsPass bool, problem string) {
	if !plainText(raw, 512) || strings.ContainsAny(raw, " \\\"<>") {
		return false, false, "must be an https address of at most 512 characters, without spaces"
	}
	for _, p := range ddnsPlaceholder.FindAllString(raw, -1) {
		switch p {
		case "{ip}", "{hostname}", "{username}", "{password}":
		default:
			return false, false, fmt.Sprintf("%s is not a placeholder: use {ip}, {hostname}, {username} or {password}", p)
		}
	}
	if !strings.Contains(raw, "{ip}") {
		return false, false, "needs {ip} where the address goes"
	}
	// the placeholders are replaced by plain words to see what the address would be
	probe := strings.NewReplacer("{ip}", "192.0.2.1", "{hostname}", "host.example", "{username}", "user", "{password}", "pass").Replace(raw)
	u, err := url.Parse(probe)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Hostname() != strings.ToLower(u.Hostname()) {
		return false, false, "must be an https address with the server's name in lower case, and no user name or password inside it"
	}
	rest := strings.TrimPrefix(raw, "https://")
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if strings.Contains(authority, "{") {
		return false, false, "the server's name cannot hold a placeholder"
	}
	if !ValidServerHost(u.Hostname()) {
		return false, false, "the server must be a name or an address that is not this router itself"
	}
	return strings.Contains(raw, "{username}"), strings.Contains(raw, "{password}"), ""
}

// validateDDNS checks the Dynamic DNS entries. Errors are reported on ddns.entries[i].field and ddns.entries.
func (c NetworkConfig) validateDDNS(add func(field, format string, args ...any)) {
	d := c.DDNSSettings()
	if len(d.Entries) > MaxDDNSEntries {
		add("ddns.entries", "at most %d names can be kept up to date", MaxDDNSEntries)
	}
	ids, names := map[string]bool{}, map[string]string{}
	for i, e := range d.Entries {
		field := func(name string) string { return fmt.Sprintf("ddns.entries[%d].%s", i, name) }
		switch {
		case !ruleIDPattern.MatchString(e.ID):
			add(field("id"), "must be a short lower-case id such as d1")
		case ids[e.ID]:
			add(field("id"), "another name already has this id")
		}
		ids[e.ID] = true

		if !ValidDNSName(e.Hostname) || !strings.Contains(e.Hostname, ".") {
			add(field("hostname"), "must be a full name in lower case, such as home.example.com")
		} else if other := names[e.Hostname]; other != "" {
			add(field("hostname"), "is already kept up to date by another entry")
		}
		names[e.Hostname] = e.ID

		if e.Source != DDNSSourceWAN && e.Source != DDNSSourceWeb {
			add(field("source"), "must be wan or web")
		}
		if strings.ContainsAny(e.Username, "\r\n") || strings.ContainsAny(e.Password, "\r\n") {
			add(field("password"), "cannot contain a line break")
		}
		if len(e.Username) > 128 || len(e.Password) > 256 {
			add(field("password"), "is too long")
		}
		switch e.Provider {
		case DDNSDynDNS2:
			if !ValidServerHost(e.Server) || IsIPv4(e.Server) {
				add(field("server"), "must be the name of the update server, such as members.dyndns.org")
			}
			if !plainText(e.Username, 128) {
				add(field("username"), "is needed")
			}
			if e.Password == "" {
				add(field("password"), "is needed")
			}
		case DDNSDuckDNS:
			if !strings.HasSuffix(e.Hostname, ".duckdns.org") || strings.Count(e.Hostname, ".") != 2 {
				add(field("hostname"), "must be a name under duckdns.org, such as mine.duckdns.org")
			}
			if !duckTokenRe.MatchString(e.Password) {
				add(field("password"), "must be the token from your DuckDNS account (letters, digits and hyphens)")
			}
		case DDNSCloudflare:
			if !cfTokenRe.MatchString(e.Password) {
				add(field("password"), "must be a Cloudflare API token with permission to edit DNS (letters, digits, hyphen and underscore)")
			}
		case DDNSCustom:
			needUser, needPass, problem := ddnsTemplateOK(e.URL)
			switch {
			case problem != "":
				add(field("url"), "%s", problem)
			default:
				if needUser && !plainText(e.Username, 128) {
					add(field("username"), "is needed: the address asks for {username}")
				}
				if needPass && e.Password == "" {
					add(field("password"), "is needed: the address asks for {password}")
				}
			}
		default:
			add(field("provider"), "must be one of dyndns2, duckdns, cloudflare or custom")
		}
		if e.Provider != DDNSCustom && e.URL != "" {
			add(field("url"), "is only for a custom address")
		}
		if e.Provider != DDNSDynDNS2 && e.Server != "" {
			add(field("server"), "is only for a dyndns2 service")
		}
	}
}

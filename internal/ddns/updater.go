package ddns

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"peakroute/internal/config"
)

// Status is how keeping one name up to date is going.
type Status struct {
	ID          string
	Hostname    string
	Address     string    // the address the service was last given ("" when it has not been given one yet)
	OK          bool      // the last attempt worked (or nothing needed doing)
	Attention   bool      // it stopped, and stays stopped until the entry is changed or "update now" is pressed
	Message     string    // what happened, in words
	LastTry     time.Time // the last attempt
	LastSuccess time.Time // the last time the service was told (or confirmed it had) the address
	NextTry     time.Time // when it tries again after a failure (zero: not waiting)
}

const (
	defaultEvery   = 30 * time.Second // how often the router looks whether something needs doing
	defaultRefresh = 7 * 24 * time.Hour
	webIPEvery     = 5 * time.Minute
	minRetry       = time.Minute
	maxRetry       = 30 * time.Minute
	requestTimeout = 20 * time.Second
)

// Updater tells the services the address of the internet connection.
type Updater struct {
	// Entries are the names to keep up to date, from the configuration in effect.
	Entries func() []config.DDNSEntry
	// WAN is the address of the internet connection; ok is false while there is none.
	WAN func() (addr netip.Addr, ok bool)

	// everything below has a default; the tests change it
	Client        *http.Client
	WebIPURLs     []string      // services that say what address a request came from; tried in order
	DuckURL       string        // https://www.duckdns.org/update
	CloudflareURL string        // https://api.cloudflare.com/client/v4
	UserAgent     string        // PeakRoute-OS/1.0
	Every         time.Duration // how often to look (30 s)
	Refresh       time.Duration // how long a name may go without being told the address, though it has not changed (7 days)
	Now           func() time.Time

	mu    sync.Mutex
	state map[string]*entryState
	web   struct {
		addr netip.Addr
		at   time.Time
	}
}

type entryState struct {
	fingerprint string // what the entry was when this state was made: a change of anything starts afresh
	Status
	backoff time.Duration
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}
func (u *Updater) userAgent() string {
	if u.UserAgent != "" {
		return u.UserAgent
	}
	return "PeakRoute-OS/1.0"
}
func (u *Updater) duckURL() string {
	if u.DuckURL != "" {
		return u.DuckURL
	}
	return "https://www.duckdns.org/update"
}
func (u *Updater) cloudflareURL() string {
	if u.CloudflareURL != "" {
		return strings.TrimSuffix(u.CloudflareURL, "/")
	}
	return "https://api.cloudflare.com/client/v4"
}
func (u *Updater) webIPURLs() []string {
	if len(u.WebIPURLs) > 0 {
		return u.WebIPURLs
	}
	return []string{"https://api.ipify.org", "https://icanhazip.com"}
}
func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return newClient()
}

// newClient is the client the router uses: it follows a few redirects, and only to https addresses.
func newClient() *http.Client {
	return &http.Client{Timeout: requestTimeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || req.URL.Scheme != "https" {
			return errRedirect
		}
		return nil
	}}
}

// IsPublic reports whether an address can be reached from the internet: not private, shared (carrier NAT), loopback,
// link-local, multicast or unspecified.
func IsPublic(a netip.Addr) bool {
	if !a.Is4() {
		return false
	}
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	zero := netip.MustParsePrefix("0.0.0.0/8")
	return !(a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified() || cgnat.Contains(a) || zero.Contains(a) || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}))
}

// fingerprint is what makes an entry "the same entry": all of it, password included.
func fingerprint(e config.DDNSEntry) string { return fmt.Sprintf("%#v", e) }

// Run looks every so often whether a name needs updating, until the context ends.
func (u *Updater) Run(ctx context.Context) {
	every := u.Every
	if every <= 0 {
		every = defaultEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		u.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick looks at every enabled entry once and updates those that need it.
func (u *Updater) Tick(ctx context.Context) {
	entries := u.Entries()
	u.forget(entries)
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		if !e.Disabled {
			u.check(ctx, e, false)
		}
	}
}

// forget drops what is known about entries that are gone.
func (u *Updater) forget(entries []config.DDNSEntry) {
	u.mu.Lock()
	defer u.mu.Unlock()
	keep := map[string]bool{}
	for _, e := range entries {
		keep[e.ID] = true
	}
	for id := range u.state {
		if !keep[id] {
			delete(u.state, id)
		}
	}
}

// UpdateNow updates one entry now, whatever it was waiting for (a failure, a stop). ok is false for an entry that is not there or
// is switched off.
func (u *Updater) UpdateNow(ctx context.Context, id string) (Status, bool) {
	for _, e := range u.Entries() {
		if e.ID == id && !e.Disabled {
			return u.check(ctx, e, true), true
		}
	}
	return Status{}, false
}

// Statuses says how every enabled entry is doing, in the order of the configuration.
func (u *Updater) Statuses() []Status {
	var out []Status
	entries := u.Entries()
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, e := range entries {
		if e.Disabled {
			continue
		}
		if st, ok := u.state[e.ID]; ok && st.fingerprint == fingerprint(e) {
			out = append(out, st.Status)
		} else {
			out = append(out, Status{ID: e.ID, Hostname: e.Hostname, Message: "Waiting to be updated."})
		}
	}
	return out
}

// address finds the address to publish for an entry, or says why there is none.
func (u *Updater) address(ctx context.Context, e config.DDNSEntry) (netip.Addr, string) {
	if e.Source == config.DDNSSourceWeb {
		u.mu.Lock()
		a, at := u.web.addr, u.web.at
		u.mu.Unlock()
		if a.IsValid() && u.now().Sub(at) < webIPEvery {
			return a, ""
		}
		a, msg := u.askTheWeb(ctx)
		if !a.IsValid() {
			return a, msg
		}
		u.mu.Lock()
		u.web.addr, u.web.at = a, u.now()
		u.mu.Unlock()
		return a, ""
	}
	a, ok := u.WAN()
	switch {
	case !ok || !a.IsValid():
		return netip.Addr{}, "The internet connection has no address right now."
	case !IsPublic(a):
		return netip.Addr{}, fmt.Sprintf("The internet connection's address, %s, is not a public address (it is behind another router or a carrier's NAT), so nobody could reach it. Choose the address an internet service reports instead.", a)
	}
	return a, ""
}

func (u *Updater) askTheWeb(ctx context.Context) (netip.Addr, string) {
	var last string
	for _, target := range u.webIPURLs() {
		code, body, err := u.do(ctx, http.MethodGet, target, nil, "", "", nil)
		if err != nil {
			last = "Could not ask " + hostOf(target) + ": " + shortErr(err)
			continue
		}
		if code != http.StatusOK {
			last = fmt.Sprintf("%s answered HTTP %d.", hostOf(target), code)
			continue
		}
		a, err := netip.ParseAddr(strings.TrimSpace(string(body)))
		if err != nil || !IsPublic(a.Unmap()) {
			last = hostOf(target) + " did not answer with a public IPv4 address."
			continue
		}
		return a.Unmap(), ""
	}
	return netip.Addr{}, last
}

func hostOf(target string) string {
	t := strings.TrimPrefix(target, "https://")
	if i := strings.IndexAny(t, "/?"); i >= 0 {
		t = t[:i]
	}
	return t
}

// check updates the entry when it needs it (or when force says it must) and records what happened.
func (u *Updater) check(ctx context.Context, e config.DDNSEntry, force bool) Status {
	fp := fingerprint(e)
	u.mu.Lock()
	if u.state == nil {
		u.state = map[string]*entryState{}
	}
	st := u.state[e.ID]
	if st == nil || st.fingerprint != fp {
		st = &entryState{fingerprint: fp, Status: Status{ID: e.ID, Hostname: e.Hostname}}
		u.state[e.ID] = st
	}
	if force {
		st.Attention, st.NextTry, st.backoff = false, time.Time{}, 0
		u.web.at = time.Time{} // look at the web again too
	}
	now := u.now()
	stopped := st.Attention
	waiting := !st.NextTry.IsZero() && now.Before(st.NextTry)
	u.mu.Unlock()
	if stopped || waiting {
		return u.snapshot(st)
	}

	addr, why := u.address(ctx, e)
	if !addr.IsValid() {
		u.mu.Lock()
		st.Message, st.OK, st.LastTry = why, false, now
		u.mu.Unlock()
		return u.snapshot(st)
	}
	u.mu.Lock()
	due := st.Address != addr.String() || st.LastSuccess.IsZero() || now.Sub(st.LastSuccess) >= u.refresh()
	u.mu.Unlock()
	if !due && !force {
		return u.snapshot(st)
	}

	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var r result
	switch e.Provider {
	case config.DDNSDynDNS2:
		r = u.dyndns2(rctx, e, addr)
	case config.DDNSDuckDNS:
		r = u.duckdns(rctx, e, addr)
	case config.DDNSCloudflare:
		r = u.cloudflare(rctx, e, addr)
	case config.DDNSCustom:
		r = u.custom(rctx, e, addr)
	default:
		r = result{stop, "Unknown service " + e.Provider}
	}

	u.mu.Lock()
	st.LastTry = u.now()
	st.Message = capitalize(r.text)
	switch r.outcome {
	case updated, unchanged:
		changed := st.Address != addr.String()
		st.Address, st.OK, st.Attention, st.backoff, st.NextTry = addr.String(), true, false, 0, time.Time{}
		st.LastSuccess = st.LastTry
		if r.outcome == updated && changed {
			log.Printf("[+] Dynamic DNS: %s is now %s", e.Hostname, addr)
		}
	case retry:
		st.OK = false
		if st.backoff = st.backoff * 2; st.backoff < minRetry {
			st.backoff = minRetry
		}
		if st.backoff > maxRetry {
			st.backoff = maxRetry
		}
		st.NextTry = st.LastTry.Add(st.backoff)
	case stop:
		st.OK, st.Attention = false, true
		log.Printf("[!] Dynamic DNS: %s stopped: %s", e.Hostname, r.text)
	}
	u.mu.Unlock()
	return u.snapshot(st)
}

func (u *Updater) refresh() time.Duration {
	if u.Refresh > 0 {
		return u.Refresh
	}
	return defaultRefresh
}

func (u *Updater) snapshot(st *entryState) Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return st.Status
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

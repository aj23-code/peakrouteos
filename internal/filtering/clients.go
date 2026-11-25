package filtering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"peakroute/internal/fsutil"
)

// Device is one device the router knows: its hardware address and an address it has now.
type Device struct {
	MAC string
	IP  string
}

// ClientSync keeps AdGuard Home's list of named clients in line with the nicknames a person gave the devices on the router's Clients
// page, so that AdGuard Home's query log and statistics say "Alec's laptop" and not just an address or a DHCP host name.
//
// A nickname becomes a persistent client of AdGuard Home with the addresses its device has now (and its hardware address). The
// clients this made are remembered by name in a file; only those are ever changed or removed, so a client a person made in
// AdGuard Home's own interface is left alone, and so are the settings they gave one of ours there (blocked services, say): an update
// only replaces its addresses.
type ClientSync struct {
	Upstream  string                             // UIAddr unless a test says otherwise
	Names     func() map[string]string           // hardware address -> nickname
	Devices   func(ctx context.Context) []Device // what is on the networks
	StatePath string                             // where the names of the clients made here are remembered
	Every     time.Duration                      // 30 s unless a test says otherwise
	HTTP      *http.Client                       // a short timeout unless a test says otherwise
	Log       func(format string, args ...any)

	managed map[string]bool
}

func (c *ClientSync) upstream() string {
	if c.Upstream != "" {
		return c.Upstream
	}
	return UIAddr
}

func (c *ClientSync) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func (c *ClientSync) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(format, args...)
	}
}

func (c *ClientSync) load() {
	c.managed = map[string]bool{}
	b, err := os.ReadFile(c.StatePath)
	if err != nil {
		return
	}
	var names []string
	if json.Unmarshal(b, &names) == nil {
		for _, n := range names {
			c.managed[n] = true
		}
	}
}

func (c *ClientSync) save() {
	names := make([]string, 0, len(c.managed))
	for n := range c.managed {
		names = append(names, n)
	}
	sort.Strings(names)
	b, _ := json.Marshal(names)
	if c.StatePath != "" {
		if err := fsutil.WriteFileAtomic(c.StatePath, b, 0600); err != nil {
			c.logf("filtering: cannot remember the clients made in AdGuard Home: %v", err)
		}
	}
}

// Run syncs every Every until ctx ends. It looks at once, so a new start is right quickly.
func (c *ClientSync) Run(ctx context.Context) {
	every := c.Every
	if every <= 0 {
		every = 30 * time.Second
	}
	c.load()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		if err := c.Once(ctx); err != nil {
			c.logf("filtering: the nicknames could not be given to AdGuard Home: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// agh is AdGuard Home's client as the API sends it; only what is read here is named, and the rest is kept as it came so that an update
// does not lose what a person set in AdGuard Home.
type aghClient map[string]any

func (a aghClient) name() string { n, _ := a["name"].(string); return n }

func (a aghClient) ids() []string {
	var out []string
	if l, ok := a["ids"].([]any); ok {
		for _, v := range l {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func (c *ClientSync) call(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.upstream()+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %s %s", method, path, res.Status, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// desired is what the nicknames ask for: nickname -> the addresses of the devices that carry it (two devices may share a nickname:
// AdGuard Home names are unique, so they become one client with both devices' addresses). A device with no address now is left out of
// the update, not taken out of AdGuard Home.
func (c *ClientSync) desired(ctx context.Context) (names map[string]bool, ids map[string][]string) {
	nick := c.Names()
	names = map[string]bool{}
	ids = map[string][]string{}
	for _, n := range nick {
		names[n] = true
	}
	seen := map[string]map[string]bool{}
	add := func(name, id string) {
		if seen[name] == nil {
			seen[name] = map[string]bool{}
		}
		if id != "" && !seen[name][id] {
			seen[name][id] = true
			ids[name] = append(ids[name], id)
		}
	}
	for _, d := range c.Devices(ctx) {
		mac := strings.ToLower(d.MAC)
		name, ok := nick[mac]
		if !ok || d.IP == "" {
			continue
		}
		add(name, d.IP)
		add(name, mac)
	}
	for n := range ids {
		sort.Strings(ids[n])
	}
	return names, ids
}

func sameIDs(a, b []string) bool {
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, "\n") == strings.Join(y, "\n")
}

// Once makes one pass. It does nothing (and reports nothing) while AdGuard Home does not answer: it is not running yet.
func (c *ClientSync) Once(ctx context.Context) error {
	if c.managed == nil {
		c.load()
	}
	var current struct {
		Clients []aghClient `json:"clients"`
	}
	if err := c.call(ctx, http.MethodGet, "/control/clients", nil, &current); err != nil {
		return nil
	}
	have := map[string]aghClient{}
	for _, cl := range current.Clients {
		have[cl.name()] = cl
	}
	names, ids := c.desired(ctx)
	var problems []string

	for name, want := range ids {
		cl, exists := have[name]
		switch {
		case !exists:
			add := aghClient{"name": name, "ids": want, "tags": []string{}, "use_global_settings": true, "filtering_enabled": true,
				"parental_enabled": false, "safebrowsing_enabled": false, "safesearch_enabled": false, "use_global_blocked_services": true,
				"ignore_querylog": false, "ignore_statistics": false}
			if err := c.call(ctx, http.MethodPost, "/control/clients/add", add, nil); err != nil {
				problems = append(problems, err.Error())
				continue
			}
			c.managed[name] = true
		case !c.managed[name]:
			// a client a person made in AdGuard Home under the same name is theirs
		case !sameIDs(cl.ids(), want):
			cl["ids"] = want
			if err := c.call(ctx, http.MethodPost, "/control/clients/update", map[string]any{"name": name, "data": cl}, nil); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	// a nickname that is gone (taken away, or changed) takes its client with it
	for name := range c.managed {
		if names[name] {
			continue
		}
		if _, exists := have[name]; exists {
			if err := c.call(ctx, http.MethodPost, "/control/clients/delete", map[string]string{"name": name}, nil); err != nil {
				problems = append(problems, err.Error())
				continue
			}
		}
		delete(c.managed, name)
	}
	c.save()
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

package translator

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FileReader reads files from the system being configured. RootFS maps it onto a directory so
// planning can be tested without touching /etc.
type FileReader interface {
	ReadFile(path string) ([]byte, error)
	// ListDir returns the names of the files in a directory (none if it does not exist).
	ListDir(path string) ([]string, error)
}

func (r RootFS) ListDir(path string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(string(r), path))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// RootFS reads absolute paths relative to a root directory ("" or "/" is the real filesystem).
type RootFS string

func (r RootFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(r), path))
}

type ChangeKind string

const (
	Create ChangeKind = "create"
	Update ChangeKind = "update"
	Remove ChangeKind = "remove"
)

// FileChange is a file that will be written or removed.
type FileChange struct {
	Path    string     `json:"path"`
	Kind    ChangeKind `json:"kind"`
	Service string     `json:"service"`
}

// Action is a service operation needed to pick the changes up.
type Action struct {
	Service string `json:"service"`
	Verb    string `json:"verb"` // apply, restart or reload
	Reason  string `json:"reason"`
}

// Plan is what committing would do to the system. An empty plan means the system already
// matches the configuration and nothing needs restarting.
type Plan struct {
	Files   []FileChange `json:"files"`
	Actions []Action     `json:"actions"`
	Notes   []string     `json:"notes,omitempty"`
}

func (p Plan) Empty() bool { return len(p.Files) == 0 && len(p.Actions) == 0 }

var (
	serviceOrder = []string{SvcHostname, SvcTimezone, SvcSysctl, SvcNetworking, SvcRoutes, SvcRegdom, SvcWifi, SvcNftables, SvcDnsmasq, SvcWireGuard, SvcOpenVPN, SvcNTP}
	serviceVerb  = map[string]string{
		SvcHostname:   "apply",
		SvcTimezone:   "apply",
		SvcNTP:        "restart",
		SvcWireGuard:  "apply",
		SvcOpenVPN:    "apply",
		SvcSysctl:     "apply",
		SvcNetworking: "restart",
		SvcRoutes:     "apply",
		SvcRegdom:     "apply",
		SvcWifi:       "restart",
		SvcNftables:   "reload",
		SvcDnsmasq:    "restart",
	}
)

// PlanChanges compares what the translator wants with what is on disk and lists the files that
// differ and the service actions needed, in the order they must run. Only file contents are
// compared; permissions are set when a file is written.
func PlanChanges(fsys FileReader, desired Desired) (Plan, error) {
	// empty lists, not null: the review dialog reads their length
	plan := Plan{Files: []FileChange{}, Actions: []Action{}, Notes: append([]string(nil), desired.Notes...)}
	changedBy := map[string][]string{} // service -> changed paths

	for _, a := range desired.Artifacts {
		current, err := fsys.ReadFile(a.Path)
		missing := os.IsNotExist(err)
		if err != nil && !missing {
			return Plan{}, fmt.Errorf("read %s: %w", a.Path, err)
		}

		var kind ChangeKind
		switch {
		case a.Remove && missing:
			continue
		case a.Remove:
			kind = Remove
		case missing:
			kind = Create
		case !bytes.Equal(current, a.Content):
			kind = Update
		default:
			continue
		}

		plan.Files = append(plan.Files, FileChange{Path: a.Path, Kind: kind, Service: a.Service})
		changedBy[a.Service] = append(changedBy[a.Service], a.Path)
	}

	// Files in a managed directory that nothing accounts for are removed
	wanted := map[string]bool{}
	for _, a := range desired.Artifacts {
		wanted[a.Path] = true
	}
	for _, dir := range desired.ManagedDirs {
		names, err := fsys.ListDir(dir.Path)
		if err != nil {
			return Plan{}, fmt.Errorf("list %s: %w", dir.Path, err)
		}
		for _, name := range names {
			path := dir.Path + "/" + name
			if wanted[path] || (!dir.Any && !strings.HasSuffix(name, ".conf")) || !strings.HasPrefix(name, dir.Prefix) {
				continue
			}
			plan.Files = append(plan.Files, FileChange{Path: path, Kind: Remove, Service: dir.Service})
			changedBy[dir.Service] = append(changedBy[dir.Service], path)
		}
	}

	// Restarting networking takes the LAN interface down and up, which drops the address
	// dnsmasq is bound to, so a managed DHCP server must be restarted along with it
	_, networkingChanged := changedBy[SvcNetworking]
	_, dnsmasqChanged := changedBy[SvcDnsmasq]
	cascadeDnsmasq := networkingChanged && !dnsmasqChanged && dhcpManaged(desired)
	// ...and takes the bridges the access points joined away with it, so the access points start again
	_, wifiChanged := changedBy[SvcWifi]
	cascadeWifi := networkingChanged && !wifiChanged && wifiManaged(desired)

	for _, svc := range serviceOrder {
		switch {
		case len(changedBy[svc]) > 0:
			plan.Actions = append(plan.Actions, Action{
				Service: svc,
				Verb:    serviceVerb[svc],
				Reason:  "changed: " + strings.Join(changedBy[svc], ", "),
			})
		case svc == SvcWifi && cascadeWifi:
			plan.Actions = append(plan.Actions, Action{
				Service: svc,
				Verb:    serviceVerb[svc],
				Reason:  "networking is restarting, which removes the bridges the access points joined",
			})
		case svc == SvcDnsmasq && cascadeDnsmasq:
			plan.Actions = append(plan.Actions, Action{
				Service: svc,
				Verb:    serviceVerb[svc],
				Reason:  "networking is restarting, which drops the LAN address the DHCP server is bound to",
			})
		}
	}

	return plan, nil
}

// dhcpManaged reports whether the DHCP server is configured (and enabled) by this translator.
func dhcpManaged(d Desired) bool {
	for _, a := range d.Artifacts {
		if a.Path == PathDnsmasqDHCP && !a.Remove {
			return true
		}
	}
	return false
}

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
	serviceOrder = []string{SvcSysctl, SvcNetworking, SvcNftables, SvcDnsmasq}
	serviceVerb  = map[string]string{
		SvcSysctl:     "apply",
		SvcNetworking: "restart",
		SvcNftables:   "reload",
		SvcDnsmasq:    "restart",
	}
)

// PlanChanges compares what the translator wants with what is on disk and lists the files that
// differ and the service actions needed, in the order they must run. Only file contents are
// compared; permissions are set when a file is written.
func PlanChanges(fsys FileReader, desired Desired) (Plan, error) {
	plan := Plan{Notes: append([]string(nil), desired.Notes...)}
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

	// Restarting networking takes the LAN interface down and up, which drops the address
	// dnsmasq is bound to, so a managed DHCP server must be restarted along with it
	_, networkingChanged := changedBy[SvcNetworking]
	_, dnsmasqChanged := changedBy[SvcDnsmasq]
	cascadeDnsmasq := networkingChanged && !dnsmasqChanged && dhcpManaged(desired)

	for _, svc := range serviceOrder {
		switch {
		case len(changedBy[svc]) > 0:
			plan.Actions = append(plan.Actions, Action{
				Service: svc,
				Verb:    serviceVerb[svc],
				Reason:  "changed: " + strings.Join(changedBy[svc], ", "),
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

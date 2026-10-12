package translator

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"peakroute/internal/fsutil"
)

// FileSystem is the system being configured: readable, and writable one file at a time.
// RootFS is the implementation; it can be pointed at a sandbox directory.
type FileSystem interface {
	FileReader
	// FileMode returns the permissions of an existing file.
	FileMode(path string) (os.FileMode, error)
	// WriteFile replaces a file atomically, creating parent directories as needed.
	WriteFile(path string, data []byte, mode os.FileMode) error
	// RemoveFile deletes a file; it is not an error if it is already gone.
	RemoveFile(path string) error
}

func (r RootFS) FileMode(path string) (os.FileMode, error) {
	info, err := os.Stat(filepath.Join(string(r), path))
	if err != nil {
		return 0, err
	}
	return info.Mode().Perm(), nil
}

func (r RootFS) WriteFile(path string, data []byte, mode os.FileMode) error {
	full := filepath.Join(string(r), path)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(full, data, mode)
}

func (r RootFS) RemoveFile(path string) error {
	err := os.Remove(filepath.Join(string(r), path))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs real commands with a fixed environment (the daemon may be started with a
// minimal PATH). Output is capped so a chatty command cannot fill memory.
type ExecRunner struct{}

const maxCommandOutput = 8 << 10

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C"}

	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()

	text := strings.TrimSpace(out.String())
	if len(text) > maxCommandOutput {
		text = text[:maxCommandOutput] + "\n[output truncated]"
	}
	if ctx.Err() != nil {
		return text, fmt.Errorf("%s timed out: %w", name, ctx.Err())
	}
	return text, err
}

// LogRunner never executes anything: it logs the command and reports success. It is what -dev
// mode uses, so a developer's own machine is never reconfigured.
type LogRunner struct {
	mu       sync.Mutex
	commands []string
}

func (l *LogRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	l.mu.Lock()
	l.commands = append(l.commands, line)
	l.mu.Unlock()
	log.Printf("[dry-run] would run: %s", line)
	return "(dry run: not executed)", nil
}

// Commands returns every command that was "run", in order.
func (l *LogRunner) Commands() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.commands...)
}

// Step is one thing the applier did, reported to the UI.
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

// FileBackup is one file as it was before an apply.
type FileBackup struct {
	Path    string      `json:"path"`
	Existed bool        `json:"existed"`
	Content []byte      `json:"content,omitempty"`
	Mode    os.FileMode `json:"mode,omitempty"`
}

// Snapshot is everything needed to put the system back the way it was before an apply: the
// previous files and the service actions to re-run afterwards. It is plain data, so it can be
// saved to disk and survive a restart.
type Snapshot struct {
	Files   []FileBackup `json:"files"`
	Actions []Action     `json:"actions"`
}

// Result is the outcome of an Apply or a Restore.
type Result struct {
	OK         bool      `json:"ok"`
	Error      string    `json:"error,omitempty"`
	Steps      []Step    `json:"steps"`
	RolledBack bool      `json:"rolled_back,omitempty"`
	Problems   []string  `json:"rollback_problems,omitempty"` // things that went wrong while undoing
	Snapshot   *Snapshot `json:"-"`                           // set on success: how to undo this apply
}

// Applier puts a Plan on the system. Applies are serialised: two never run at once.
type Applier struct {
	FS  FileSystem
	Run Runner

	mu sync.Mutex
}

const (
	checkTimeout   = 30 * time.Second
	serviceTimeout = 30 * time.Second
	restartTimeout = 90 * time.Second // networking can take a while to bounce interfaces
)

type command struct {
	name string
	args []string
}

func (c command) String() string { return strings.TrimSpace(c.name + " " + strings.Join(c.args, " ")) }

// preChecks validate a service's new configuration before the service is touched.
var preChecks = map[string]command{
	SvcNetworking: {"ifquery", []string{"--list", "--all"}},
	SvcNftables:   {"nft", []string{"-c", "-f", PathNftables}},
	SvcDnsmasq:    {"dnsmasq", []string{"--test"}},
	SvcRoutes:     routesCheckCommand(PathRoutes),
	SvcWireGuard:  wgCheckCommand(),
}

// serviceCommands carry out an action.
var serviceCommands = map[string]command{
	SvcHostname: {"hostname", []string{"-F", PathHostname}},
	// the zone file was validated against the list when it was rendered; it must also exist on this machine
	SvcTimezone: {"sh", []string{"-c", `tz=$(cat ` + PathTimezone + `); if [ -f "/usr/share/zoneinfo/$tz" ]; then ln -sf "/usr/share/zoneinfo/$tz" /etc/localtime; else echo "unknown time zone $tz" >&2; exit 1; fi`}},
	// no file, or one without the "switched off" line: keep the time with time servers; otherwise stop
	SvcNTP:        {"sh", []string{"-c", `if grep -q '` + ntpOffMarker + `' ` + PathTimesyncd + ` 2>/dev/null; then systemctl disable --now systemd-timesyncd; else systemctl enable systemd-timesyncd && systemctl restart systemd-timesyncd; fi`}},
	SvcSysctl:     {"sysctl", []string{"-p", PathSysctl}},
	SvcNetworking: {"systemctl", []string{"restart", "networking"}},
	SvcNftables:   {"systemctl", []string{"reload", "nftables"}},
	SvcDnsmasq:    {"systemctl", []string{"restart", "dnsmasq"}},
	SvcRoutes:     routesApplyCommand(PathRoutes),
	SvcWireGuard:  {"sh", []string{PathWGSync}},
	SvcOpenVPN:    {"sh", []string{PathOVSync}},
	// With no configuration left the service's start condition is not met, so a restart leaves it stopped.
	SvcWifi: {"systemctl", []string{"restart", "peakroute-wifi"}},
}

// routesCheckCommand syntax-checks the routes script. The script may be about to be removed (the
// last route went away), in which case there is nothing to check.
func routesCheckCommand(script string) command {
	return command{"sh", []string{"-c", "[ ! -f " + script + " ] || sh -n " + script}}
}

// routesApplyCommand runs the routes script strictly, so a route the kernel refuses fails the
// commit. With the script gone (the last route was removed) the routes it added are flushed instead.
func routesApplyCommand(script string) command {
	return command{"sh", []string{"-c", "if [ -f " + script + " ]; then sh " + script + " strict; else ip route flush proto " + RouteProto + "; fi"}}
}

// Apply writes the plan's files, validates them, then restarts the affected services. Any
// failure restores the previous files and brings the services back to the previous configuration.
func (a *Applier) Apply(ctx context.Context, desired Desired, plan Plan) Result {
	a.mu.Lock()
	defer a.mu.Unlock()

	var res Result
	if plan.Empty() {
		res.OK = true
		return res
	}
	if err := ctx.Err(); err != nil {
		res.Error = err.Error()
		return res
	}

	artifacts := map[string]Artifact{}
	for _, art := range desired.Artifacts {
		artifacts[art.Path] = art
	}

	// Remember what is there now, so every failure (and a later revert) can put it back
	snap := Snapshot{Actions: append([]Action(nil), plan.Actions...)}
	for _, f := range plan.Files {
		b := FileBackup{Path: f.Path}
		content, err := a.FS.ReadFile(f.Path)
		switch {
		case err == nil:
			b.Existed, b.Content = true, content
			b.Mode, _ = a.FS.FileMode(f.Path)
		case os.IsNotExist(err):
		default:
			res.Error = fmt.Sprintf("cannot read %s to back it up: %v", f.Path, err)
			return res
		}
		snap.Files = append(snap.Files, b)
	}

	fail := func(err error) Result {
		res.Error = err.Error()
		res.RolledBack = true
		res.Problems = a.undo(ctx, snap)
		return res
	}

	// 1. write the files
	for _, f := range plan.Files {
		art, ok := artifacts[f.Path]
		// a removal of a file nothing renders (found in a managed directory) needs no artifact
		if !ok && f.Kind != Remove {
			res.Steps = append(res.Steps, Step{Name: "write " + f.Path, Error: "not part of the rendered configuration"})
			return fail(fmt.Errorf("internal error: %s is in the plan but was not rendered", f.Path))
		}

		var err error
		if f.Kind == Remove {
			err = a.FS.RemoveFile(f.Path)
		} else {
			err = a.FS.WriteFile(f.Path, art.Content, art.Mode)
		}
		step := Step{Name: fmt.Sprintf("%s %s", f.Kind, f.Path), OK: err == nil}
		if err != nil {
			step.Error = err.Error()
			res.Steps = append(res.Steps, step)
			return fail(fmt.Errorf("could not %s %s: %w", f.Kind, f.Path, err))
		}
		res.Steps = append(res.Steps, step)
	}

	// 2. check the new configuration before touching any service
	for _, act := range plan.Actions {
		check, ok := preChecks[act.Service]
		if !ok {
			continue
		}
		out, err := a.exec(ctx, checkTimeout, check)
		step := Step{Name: "check " + act.Service + " configuration (" + check.String() + ")", OK: err == nil, Output: out}
		if err != nil {
			step.Error = err.Error()
			res.Steps = append(res.Steps, step)
			return fail(fmt.Errorf("the new %s configuration was rejected: %s", act.Service, firstLine(out, err)))
		}
		res.Steps = append(res.Steps, step)
	}

	// 3. carry out the actions in order
	for _, act := range plan.Actions {
		cmd := serviceCommands[act.Service]
		timeout := serviceTimeout
		if act.Service == SvcNetworking {
			timeout = restartTimeout
		}
		out, err := a.exec(ctx, timeout, cmd)
		step := Step{Name: fmt.Sprintf("%s %s (%s)", act.Verb, act.Service, cmd.String()), OK: err == nil, Output: out}
		if err != nil {
			step.Error = err.Error()
			res.Steps = append(res.Steps, step)
			return fail(fmt.Errorf("%s %s failed: %s", act.Verb, act.Service, firstLine(out, err)))
		}
		res.Steps = append(res.Steps, step)
	}

	res.OK = true
	res.Snapshot = &snap
	return res
}

// Restore puts the system back to a Snapshot taken by an earlier successful Apply: the previous
// files are written back and the service actions re-run. Used to revert an unconfirmed change.
func (a *Applier) Restore(ctx context.Context, snap Snapshot) Result {
	a.mu.Lock()
	defer a.mu.Unlock()

	res := Result{RolledBack: true}
	res.Problems = a.undo(ctx, snap)
	res.OK = len(res.Problems) == 0
	if !res.OK {
		res.Error = "the previous configuration could not be fully restored"
	}
	for _, f := range snap.Files {
		res.Steps = append(res.Steps, Step{Name: "restore " + f.Path, OK: true})
	}
	for _, act := range snap.Actions {
		res.Steps = append(res.Steps, Step{Name: fmt.Sprintf("%s %s again", act.Verb, act.Service), OK: true})
	}
	return res
}

func (a *Applier) exec(ctx context.Context, timeout time.Duration, c command) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return a.Run.Run(ctx, c.name, c.args...)
}

// undo restores a snapshot's files and re-runs its service actions so the system is running the
// configuration it had before. It returns the problems it ran into, never swallowing them.
func (a *Applier) undo(ctx context.Context, snap Snapshot) []string {
	var problems []string
	problem := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	// Undo in reverse order of the writes
	for i := len(snap.Files) - 1; i >= 0; i-- {
		b := snap.Files[i]
		var err error
		if b.Existed {
			mode := b.Mode
			if mode == 0 {
				mode = 0644
			}
			err = a.FS.WriteFile(b.Path, b.Content, mode)
		} else {
			err = a.FS.RemoveFile(b.Path)
		}
		if err != nil {
			problem("could not restore %s: %v", b.Path, err)
		}
	}

	// A fresh context: the one passed in may be the reason we are here
	rbCtx := context.WithoutCancel(ctx)
	for _, act := range snap.Actions {
		cmd := serviceCommands[act.Service]
		timeout := serviceTimeout
		if act.Service == SvcNetworking {
			timeout = restartTimeout
		}
		if out, err := a.exec(rbCtx, timeout, cmd); err != nil {
			problem("could not bring %s back to the previous configuration: %s", act.Service, firstLine(out, err))
		}
	}
	return problems
}

func firstLine(output string, err error) string {
	if line, _, _ := strings.Cut(strings.TrimSpace(output), "\n"); line != "" {
		return line
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}

// wgCheckCommand reads every tunnel file the way WireGuard will, before anything is touched: a scratch tunnel interface is made and
// given each file in turn (wg-quick strip alone would pass a file with a broken key), then removed.
func wgCheckCommand() command {
	script := "trap 'ip link del wgchk 2>/dev/null' EXIT; for f in " + PathWireGuardDir + "/wg*.conf; do [ -e \"$f\" ] || continue; " +
		"ip link add wgchk type wireguard || exit 1; wg-quick strip \"$f\" | wg setconf wgchk /dev/stdin || exit 1; ip link del wgchk; done"
	return command{"sh", []string{"-c", script}}
}

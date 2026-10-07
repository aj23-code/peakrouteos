// Package pipeline runs the review / commit / confirm / revert workflow for network changes. It
// joins the config Manager (what the user staged), the translator (what that means for the
// system) and the applier (doing it safely), and owns the 60-second safety net.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/fsutil"
	"peakroute/internal/translator"
)

type State string

const (
	StateIdle            State = "idle"
	StateApplying        State = "applying"
	StateAwaitingConfirm State = "awaiting_confirm"
)

var (
	ErrBusy             = errors.New("another change is still being applied")
	ErrNothingToCommit  = errors.New("there are no changes to commit")
	ErrNothingToConfirm = errors.New("there is no change waiting for confirmation")
)

// DefaultConfirmWindow is how long a change that could lock the admin out is kept before it is
// reverted automatically.
const DefaultConfirmWindow = 60 * time.Second

const confirmFile = "confirm.json"

// Timer is the part of *time.Timer the service uses (replaced in tests).
type Timer interface{ Stop() bool }

// Options configure a Service. Manager, FS, Runner and StateDir are required.
type Options struct {
	Manager  *config.Manager
	FS       translator.FileSystem
	Runner   translator.Runner
	StateDir string // where the pending-confirm record is kept (the config directory)

	ConfirmWindow time.Duration
	Mode          func() string // operation mode; defaults to the Manager's sys.json value
	Now           func() time.Time
	AfterFunc     func(d time.Duration, f func()) Timer
}

// Outcome describes how the most recent commit ended.
type Outcome struct {
	Kind     string            `json:"kind"` // committed, confirmed, reverted or failed
	Message  string            `json:"message"`
	At       time.Time         `json:"at"`
	Steps    []translator.Step `json:"steps,omitempty"`
	Problems []string          `json:"problems,omitempty"`
}

// ConfirmInfo describes a change waiting to be confirmed.
type ConfirmInfo struct {
	Deadline    time.Time `json:"deadline"`
	SecondsLeft int       `json:"seconds_left"`
}

// Status is what the banner and the Staged Changes page poll.
type Status struct {
	State       State        `json:"state"`
	HasChanges  bool         `json:"has_changes"` // real, uncommitted differences (not merely a stage file)
	ChangeCount int          `json:"change_count"`
	Confirm     *ConfirmInfo `json:"confirm,omitempty"`
	Last        *Outcome     `json:"last,omitempty"`
}

// Review is what committing would do, shown before the user commits.
type Review struct {
	Changes        []config.Change     `json:"changes"`
	Valid          bool                `json:"valid"`
	Errors         []config.FieldError `json:"errors,omitempty"`  // problems with the staged settings
	Problem        string              `json:"problem,omitempty"` // anything else that stops a commit
	Plan           *translator.Plan    `json:"plan,omitempty"`
	Notes          []string            `json:"notes,omitempty"`
	NeedsConfirm   bool                `json:"needs_confirm"`
	ConfirmSeconds int                 `json:"confirm_seconds"`
}

// pendingConfirm is saved to disk for the whole confirm window, so a restart or power cut during
// it still ends in a revert.
type pendingConfirm struct {
	Deadline  time.Time            `json:"deadline"`
	Previous  config.NetworkConfig `json:"previous"`
	Candidate config.NetworkConfig `json:"candidate"`
	Snapshot  translator.Snapshot  `json:"snapshot"`

	id int64 // identifies this confirm window to its timer
}

type Service struct {
	mgr      *config.Manager
	applier  *translator.Applier
	stateDir string
	window   time.Duration
	mode     func() string
	now      func() time.Time
	after    func(d time.Duration, f func()) Timer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	state    State
	pending  *pendingConfirm
	timer    Timer
	timerSeq int64
	last     *Outcome
}

func New(o Options) *Service {
	if o.ConfirmWindow <= 0 {
		o.ConfirmWindow = DefaultConfirmWindow
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.AfterFunc == nil {
		o.AfterFunc = func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
	}
	if o.Mode == nil {
		mgr := o.Manager
		o.Mode = func() string { return mgr.SysConfig().OperationMode }
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		mgr:      o.Manager,
		applier:  &translator.Applier{FS: o.FS, Run: o.Runner},
		stateDir: o.StateDir,
		window:   o.ConfirmWindow,
		mode:     o.Mode,
		now:      o.Now,
		after:    o.AfterFunc,
		ctx:      ctx,
		cancel:   cancel,
		state:    StateIdle,
	}
}

func (s *Service) fs() translator.FileSystem { return s.applier.FS }

// needsConfirm reports whether a plan can cut the admin off: changing the network or the firewall.
// A DHCP-only change restarts dnsmasq and cannot, so it needs no confirmation.
func needsConfirm(p translator.Plan) bool {
	for _, a := range p.Actions {
		if a.Service == translator.SvcNetworking || a.Service == translator.SvcNftables {
			return true
		}
	}
	return false
}

// Review shows what a commit would change and do, without changing anything.
func (s *Service) Review() Review {
	rev := Review{Changes: s.mgr.StagedChanges(), ConfirmSeconds: int(s.window.Seconds())}
	if len(rev.Changes) == 0 {
		rev.Valid = true
		return rev
	}

	candidate, _ := s.mgr.StagedNetworkConfig()
	if err := candidate.Validate(); err != nil {
		var ve config.ValidationError
		if errors.As(err, &ve) {
			rev.Errors = ve
		} else {
			rev.Problem = err.Error()
		}
		return rev
	}

	plan, err := s.plan(candidate)
	if err != nil {
		rev.Problem = err.Error()
		return rev
	}
	rev.Valid = true
	rev.Plan = &plan.plan
	rev.Notes = plan.plan.Notes
	rev.NeedsConfirm = needsConfirm(plan.plan)
	return rev
}

type planned struct {
	desired translator.Desired
	plan    translator.Plan
}

func (s *Service) plan(candidate config.NetworkConfig) (planned, error) {
	desired, err := translator.Render(candidate, s.mode())
	if err != nil {
		return planned{}, err
	}
	plan, err := translator.PlanChanges(s.fs(), desired)
	if err != nil {
		return planned{}, err
	}
	return planned{desired: desired, plan: plan}, nil
}

// Status reports the current state for polling.
func (s *Service) Status() Status {
	changes := s.mgr.StagedChanges()

	s.mu.Lock()
	defer s.mu.Unlock()

	st := Status{State: s.state, HasChanges: len(changes) > 0, ChangeCount: len(changes), Last: s.last}
	if s.state == StateAwaitingConfirm && s.pending != nil {
		left := s.pending.Deadline.Sub(s.now())
		secs := int((left + time.Second - 1) / time.Second)
		if secs < 0 {
			secs = 0
		}
		st.Confirm = &ConfirmInfo{Deadline: s.pending.Deadline, SecondsLeft: secs}
	}
	return st
}

// Commit validates the staged configuration, makes it the active one and starts applying it to
// the system in the background (poll Status for the result). It returns once the work is under
// way, so the HTTP response goes out before networking is touched.
func (s *Service) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateIdle {
		return ErrBusy
	}
	if len(s.mgr.StagedChanges()) == 0 {
		return ErrNothingToCommit
	}

	candidate, _ := s.mgr.StagedNetworkConfig()
	if err := candidate.Validate(); err != nil {
		return err
	}
	p, err := s.plan(candidate)
	if err != nil {
		return err
	}

	previous := s.mgr.NetworkConfig()
	if err := s.mgr.CommitNetwork(); err != nil {
		return err
	}

	if p.plan.Empty() {
		s.last = &Outcome{Kind: "committed", At: s.now(), Message: "Saved. The system already matched this configuration, so nothing was restarted."}
		return nil
	}

	s.state = StateApplying
	s.wg.Add(1)
	go s.runApply(p, previous, candidate)
	return nil
}

func (s *Service) runApply(p planned, previous, candidate config.NetworkConfig) {
	defer s.wg.Done()
	res := s.applier.Apply(s.ctx, p.desired, p.plan)

	s.mu.Lock()
	defer s.mu.Unlock()

	if !res.OK {
		s.failLocked(previous, candidate, res, "The change could not be applied: "+res.Error+".")
		return
	}

	if !needsConfirm(p.plan) {
		s.state = StateIdle
		s.last = &Outcome{Kind: "committed", At: s.now(), Message: "The change was applied.", Steps: res.Steps}
		return
	}

	pc := &pendingConfirm{Deadline: s.now().Add(s.window), Previous: previous, Candidate: candidate, Snapshot: *res.Snapshot}
	if err := s.persist(pc); err != nil {
		// Without a saved record the revert would not survive a restart, so the safety net
		// cannot be promised: undo the change now instead of leaving it unprotected
		undo := s.applier.Restore(s.ctx, *res.Snapshot)
		res.Steps = append(res.Steps, undo.Steps...)
		res.Problems = append(res.Problems, undo.Problems...)
		s.failLocked(previous, candidate, res, "The change was applied but the safety net could not be armed ("+err.Error()+"), so it was undone.")
		return
	}

	s.pending = pc
	s.state = StateAwaitingConfirm
	s.armLocked(s.window)
	s.last = &Outcome{
		Kind:    "committed",
		At:      s.now(),
		Steps:   res.Steps,
		Message: fmt.Sprintf("The change was applied. Confirm within %d seconds or it will be reverted.", int(s.window.Seconds())),
	}
}

// failLocked records a failed apply and puts the configuration back, keeping the user's edit
// as a pending change so it can be fixed. s.mu must be held.
func (s *Service) failLocked(previous, candidate config.NetworkConfig, res translator.Result, message string) {
	if len(res.Problems) == 0 {
		message += " The previous configuration was restored."
	} else {
		message += " Restoring the previous configuration had problems; check the details below."
	}
	if err := s.mgr.RevertNetwork(previous, &candidate); err != nil {
		message += " The saved network.json could not be restored: " + err.Error()
	}
	s.state = StateIdle
	s.last = &Outcome{Kind: "failed", At: s.now(), Message: message, Steps: res.Steps, Problems: res.Problems}
}

// armLocked starts the countdown for the pending confirm. s.mu must be held.
func (s *Service) armLocked(d time.Duration) {
	s.timerSeq++
	id := s.timerSeq
	s.pending.id = id
	s.timer = s.after(d, func() { s.deadline(id) })
}

func (s *Service) deadline(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A timer from a window that was already confirmed or reverted must do nothing
	if s.pending == nil || s.pending.id != id || s.state != StateAwaitingConfirm {
		return
	}
	s.beginRevertLocked("The change was not confirmed in time, so it was reverted.")
}

// Confirm accepts the change that is waiting: it stays, and the countdown is cancelled.
func (s *Service) Confirm() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateAwaitingConfirm || s.pending == nil {
		return ErrNothingToConfirm
	}
	// If the record cannot be removed, a restart would revert a change the user accepted, so
	// refuse and leave the countdown running rather than pretend
	if err := os.Remove(s.confirmPath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("could not record the confirmation: %w", err)
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.pending, s.timer = nil, nil
	s.state = StateIdle
	s.last = &Outcome{Kind: "confirmed", At: s.now(), Message: "The change was confirmed."}
	return nil
}

// Revert undoes the change that is waiting for confirmation, right now.
func (s *Service) Revert() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != StateAwaitingConfirm || s.pending == nil {
		return ErrNothingToConfirm
	}
	s.beginRevertLocked("The change was reverted at your request.")
	return nil
}

func (s *Service) beginRevertLocked(reason string) {
	pc := s.pending
	if s.timer != nil {
		s.timer.Stop()
	}
	s.pending, s.timer = nil, nil
	s.state = StateApplying
	s.wg.Add(1)
	go s.runRevert(pc, reason)
}

func (s *Service) runRevert(pc *pendingConfirm, reason string) {
	defer s.wg.Done()
	res := s.applier.Restore(s.ctx, pc.Snapshot)

	revertErr := s.mgr.RevertNetwork(pc.Previous, &pc.Candidate)
	// Remove the record only now that a revert was attempted, so a failing revert cannot repeat forever
	removeErr := os.Remove(s.confirmPath())

	message := reason + " Your edit is back in the pending changes."
	if len(res.Problems) > 0 {
		message = reason + " Restoring the previous system configuration had problems; check the details below."
	}
	if revertErr != nil {
		message += " The saved network.json could not be restored: " + revertErr.Error()
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		log.Printf("[pipeline] could not remove %s: %v", confirmFile, removeErr)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = StateIdle
	s.last = &Outcome{Kind: "reverted", At: s.now(), Message: message, Steps: res.Steps, Problems: res.Problems}
}

// Discard throws away the staged (uncommitted) changes.
func (s *Service) Discard() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateIdle {
		return ErrBusy
	}
	return s.mgr.DiscardNetwork()
}

// Resume picks up a confirm window that was open when the daemon last stopped: if its deadline has
// passed the change is reverted at once, otherwise the countdown continues for the time left.
// Call it once at startup.
func (s *Service) Resume() error {
	data, err := os.ReadFile(s.confirmPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", confirmFile, err)
	}

	var pc pendingConfirm
	if err := json.Unmarshal(data, &pc); err != nil {
		os.Remove(s.confirmPath())
		return fmt.Errorf("discarded an unreadable %s: %w", confirmFile, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateIdle {
		return ErrBusy
	}

	s.pending = &pc
	s.state = StateAwaitingConfirm
	if remaining := pc.Deadline.Sub(s.now()); remaining > 0 {
		s.armLocked(remaining)
		return nil
	}
	s.beginRevertLocked("The change was not confirmed before the daemon restarted, so it was reverted.")
	return nil
}

func (s *Service) confirmPath() string { return filepath.Join(s.stateDir, confirmFile) }

func (s *Service) persist(pc *pendingConfirm) error {
	data, err := json.Marshal(pc)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.confirmPath(), data, 0600)
}

// Wait blocks until background applies and reverts have finished (used by tests and shutdown).
func (s *Service) Wait() { s.wg.Wait() }

// Close stops the service. A confirm window that is still open stays on disk, so the next start
// resumes it (and reverts if it has expired).
func (s *Service) Close() {
	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

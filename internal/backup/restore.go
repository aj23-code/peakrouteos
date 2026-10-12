package backup

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"peakroute/internal/auth"
	"peakroute/internal/cert"
	"peakroute/internal/config"
	"peakroute/internal/fsutil"
)

// Restoring (docs/backup.md): a backup is opened and shown before anything changes; what is chosen is then applied in an order that
// leaves the router as it was when a step fails: certificates, then administrators, and the settings last, as the draft (so the
// usual review, commit and confirm still stand between the backup and the running router). Before anything is applied the router
// saves what it has as a copy on itself, and a restore can be undone from it.

// Env is what a restore needs of the router.
type Env struct {
	ConfigDir string
	CertDir   string
	Manager   *config.Manager
	Certs     *cert.Manager
	Sessions  interface{ RevokeAll() int }
	// Ports lists the router's own ports now; Radios the Wi-Fi adapters it has. Either may be nil (then nothing is known of them).
	Ports  func() ([]config.SystemInterface, error)
	Radios func() []string
	// Editable says whether changes may be made now (not while one is being applied or waits for its confirmation).
	Editable func() bool
	Hostname func() string
	Software string
	Now      func() time.Time
}

// Service holds a backup that is open, for a few minutes, between looking at it and restoring from it.
type Service struct {
	Env

	mu      sync.Mutex
	pending map[string]*open // by token
}

type open struct {
	bundle  *Bundle
	expires time.Time
}

const (
	openFor    = 10 * time.Minute
	keepLocal  = 3
	localDir   = "backups"
	pendingDir = "restore-pending"
)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) editable() bool { return s.Editable == nil || s.Editable() }

// ---- making a backup --------------------------------------------------------------------------------------------------------

func (s *Service) source(history bool) Source {
	cfg := s.Manager.NetworkConfig()
	var radios []string
	if cfg.Wifi != nil {
		for _, r := range cfg.Wifi.Radios {
			radios = append(radios, r.Name)
		}
	}
	host := cfg.Hostname
	if host == "" && s.Hostname != nil {
		host = s.Hostname()
	}
	return Source{ConfigDir: s.ConfigDir, CertDir: s.CertDir, History: history, Hostname: host, Software: s.Software, Now: s.now(),
		Ports: PortsInfo{Wan: cfg.WanInterface, Lan: append([]string{}, cfg.LanInterfaces...), Radios: radios}}
}

// Create makes a backup file: the sections wanted, sealed with the passphrase.
func (s *Service) Create(pass string, history bool) (data []byte, name string, err error) {
	if err := CheckPassphrase(pass); err != nil {
		return nil, "", err
	}
	src := s.source(history)
	b, err := Collect(src)
	if err != nil {
		return nil, "", err
	}
	plain, err := b.Encode()
	if err != nil {
		return nil, "", err
	}
	sealed, err := Seal(plain, pass)
	return sealed, FileName(src.Hostname, src.Now), err
}

// ---- looking at a backup ------------------------------------------------------------------------------------------------------

// Problem is something wrong with a part of a backup, in words.
type Problem = string

// SettingsReport is what restoring the settings would do.
type SettingsReport struct {
	Changes      []config.Change `json:"changes"`       // what would differ from what runs now (the usual review lines)
	Problems     []Problem       `json:"problems"`      // why it cannot be restored as it is
	Valid        bool            `json:"valid"`         // there are no problems
	MissingPorts []string        `json:"missing_ports"` // ports of the backup that this router does not have
	KeepPorts    bool            `json:"keep_ports"`    // this report is for keeping this router's own port assignments
	DraftWaits   bool            `json:"draft_waits"`   // there are changes in the draft that restoring the settings replaces
}

// AdminAccount is an account in a backup.
type AdminAccount struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

// AdminsReport is what restoring the administrators would do.
type AdminsReport struct {
	Accounts []AdminAccount `json:"accounts"`
	Problems []Problem      `json:"problems"`
	Valid    bool           `json:"valid"`
	Current  int            `json:"current"` // how many accounts there are now (all are replaced)
}

// CertsReport is what restoring the certificates would do.
type CertsReport struct {
	Web       *cert.Details `json:"web"`
	Authority *cert.Details `json:"authority"`
	Custom    bool          `json:"custom"`
	Problems  []Problem     `json:"problems"`
	Valid     bool          `json:"valid"`
}

// HistoryFile is a database in a backup.
type HistoryFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Report is what is in an open backup and what restoring it would do.
type Report struct {
	Token    string   `json:"token"`
	Created  int64    `json:"created"`
	Hostname string   `json:"hostname"`
	Software string   `json:"software"`
	Sections []string `json:"sections"`

	Settings     *SettingsReport `json:"settings"`
	Admins       *AdminsReport   `json:"admins"`
	Certificates *CertsReport    `json:"certificates"`
	History      []HistoryFile   `json:"history"`
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Inspect opens a backup file with the passphrase, checks it and keeps it open for ten minutes under a token. Nothing changes.
func (s *Service) Inspect(data []byte, pass string) (*Report, error) {
	if len(data) > MaxFile {
		return nil, errors.New("that file is too big to be a backup")
	}
	plain, err := Open(data, pass)
	if err != nil {
		return nil, err
	}
	b, err := Decode(plain)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]*open{}
	}
	now := s.now()
	for t, o := range s.pending { // only a few are kept at a time
		if now.After(o.expires) || len(s.pending) > 4 {
			delete(s.pending, t)
		}
	}
	token := newToken()
	s.pending[token] = &open{bundle: b, expires: now.Add(openFor)}
	s.mu.Unlock()
	return s.report(token, b, s.portsMissing(b), nil), nil
}

// Preview is the report again, with this router's own ports kept or not.
func (s *Service) Preview(token string, keepPorts bool) (*Report, error) {
	b, err := s.get(token)
	if err != nil {
		return nil, err
	}
	return s.report(token, b, keepPorts, nil), nil
}

// PreviewChoice is the report again with the ports assigned by hand (first-run setup).
func (s *Service) PreviewChoice(token string, choice PortChoice) (*Report, error) {
	b, err := s.get(token)
	if err != nil {
		return nil, err
	}
	return s.report(token, b, false, &choice), nil
}

func (s *Service) get(token string) (*Bundle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.pending[token]
	if !ok || s.now().After(o.expires) {
		delete(s.pending, token)
		return nil, errors.New("that backup is no longer open: open the file again")
	}
	return o.bundle, nil
}

// Forget closes an open backup.
func (s *Service) Forget(token string) {
	s.mu.Lock()
	delete(s.pending, token)
	s.mu.Unlock()
}

func (s *Service) portNames() ([]string, bool) {
	if s.Ports == nil {
		return nil, false
	}
	ports, err := s.Ports()
	if err != nil {
		return nil, false
	}
	var names []string
	for _, p := range ports {
		names = append(names, p.Name)
	}
	return names, true
}

// portsMissing says whether the backup uses ports this router does not have (so that its own should be kept).
func (s *Service) portsMissing(b *Bundle) bool {
	cfg, err := parseNetwork(b)
	if err != nil {
		return false
	}
	return len(s.missingPorts(cfg)) > 0
}

func (s *Service) missingPorts(cfg config.NetworkConfig) []string {
	have, known := s.portNames()
	if !known {
		return nil
	}
	set := map[string]bool{}
	for _, n := range have {
		set[n] = true
	}
	used := map[string]bool{}
	if cfg.WanInterface != "" {
		used[cfg.WanInterface] = true
	}
	for _, l := range cfg.LanInterfaces {
		used[l] = true
	}
	for p := range cfg.PortNetworks {
		used[p] = true
	}
	var missing []string
	for p := range used {
		if !set[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	return missing
}

func parseNetwork(b *Bundle) (config.NetworkConfig, error) {
	var cfg config.NetworkConfig
	body, ok := b.Files[FileNetwork]
	if !ok {
		return cfg, errors.New("the backup has no settings")
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return cfg, errors.New("the settings in the backup cannot be read")
	}
	return cfg, nil
}

// chooseOwnPorts puts the ports the page assigned in place of the backup's: which port is the internet connection and which are LAN
// ports (a LAN port keeps what the backup said it carried, as far as those networks exist), and the Wi-Fi adapters that are here.
// Everything else is the backup's.
func (s *Service) chooseOwnPorts(restored config.NetworkConfig, c PortChoice) config.NetworkConfig {
	cur := s.Manager.NetworkConfig()
	restored.WanInterface = c.Wan
	restored.LanInterfaces = append([]string(nil), c.Lan...)
	restored.Interfaces = append([]config.SystemInterface(nil), cur.Interfaces...)
	exists := map[string]bool{"": true, config.DefaultNetworkID: true}
	for _, n := range restored.Networks {
		exists[n.ID] = true
	}
	lan := map[string]bool{}
	for _, p := range c.Lan {
		lan[p] = true
	}
	kept := map[string]config.PortNetworks{}
	for port, pn := range restored.PortNetworks {
		if !lan[port] {
			continue
		}
		if !exists[pn.Native] {
			pn.Native = ""
		}
		var tagged []string
		for _, t := range pn.Tagged {
			if exists[t] {
				tagged = append(tagged, t)
			}
		}
		pn.Tagged = tagged
		if pn.Native != "" || len(pn.Tagged) > 0 {
			kept[port] = pn
		}
	}
	restored.PortNetworks = nil
	if len(kept) > 0 {
		restored.PortNetworks = kept
	}
	return s.onlyRadiosHere(restored)
}

// keepOwnPorts puts this router's own port assignments in place of the backup's: which port is the internet connection, which are
// LAN ports and which networks they carry, and the Wi-Fi adapters. Everything else is the backup's.
func (s *Service) keepOwnPorts(restored config.NetworkConfig) config.NetworkConfig {
	cur := s.Manager.NetworkConfig()
	restored.WanInterface = cur.WanInterface
	restored.LanInterfaces = append([]string(nil), cur.LanInterfaces...)
	restored.Interfaces = append([]config.SystemInterface(nil), cur.Interfaces...)
	// the ports keep what they carry, as far as the restored networks exist
	exists := map[string]bool{"": true, config.DefaultNetworkID: true}
	for _, n := range restored.Networks {
		exists[n.ID] = true
	}
	restored.PortNetworks = nil
	for port, pn := range cur.PortNetworks {
		if !exists[pn.Native] {
			pn.Native = ""
		}
		var tagged []string
		for _, t := range pn.Tagged {
			if exists[t] {
				tagged = append(tagged, t)
			}
		}
		pn.Tagged = tagged
		if pn.Native != "" || len(pn.Tagged) > 0 {
			if restored.PortNetworks == nil {
				restored.PortNetworks = map[string]config.PortNetworks{}
			}
			restored.PortNetworks[port] = pn
		}
	}
	return s.onlyRadiosHere(restored)
}

// onlyRadiosHere leaves out the Wi-Fi adapters this router does not have.
func (s *Service) onlyRadiosHere(restored config.NetworkConfig) config.NetworkConfig {
	if restored.Wifi != nil && s.Radios != nil {
		have := map[string]bool{}
		for _, r := range s.Radios() {
			have[r] = true
		}
		var radios []config.WifiRadio
		for _, r := range restored.Wifi.Radios {
			if have[r.Name] {
				radios = append(radios, r)
			}
		}
		w := *restored.Wifi
		w.Radios = radios
		restored.Wifi = &w
	}
	return restored
}

func problemsOf(err error) []Problem {
	var ve config.ValidationError
	if errors.As(err, &ve) {
		out := make([]Problem, len(ve))
		for i, e := range ve {
			out[i] = e.Field + ": " + e.Message
		}
		return out
	}
	return []Problem{err.Error()}
}

func (s *Service) settingsReport(b *Bundle, keepPorts bool, choice *PortChoice) *SettingsReport {
	r := &SettingsReport{Changes: []config.Change{}, Problems: []Problem{}, MissingPorts: []string{}, KeepPorts: keepPorts}
	cfg, err := parseNetwork(b)
	if err != nil {
		r.Problems = append(r.Problems, err.Error())
		return r
	}
	if m := s.missingPorts(cfg); len(m) > 0 {
		r.MissingPorts = m
	}
	switch {
	case choice != nil:
		if err := s.checkChoice(*choice); err != nil {
			r.Problems = append(r.Problems, err.Error())
		}
		cfg = s.chooseOwnPorts(cfg, *choice)
	case keepPorts:
		cfg = s.keepOwnPorts(cfg)
	}
	if err := cfg.Validate(); err != nil {
		r.Problems = append(r.Problems, problemsOf(err)...)
	}
	r.Changes = config.DiffNetwork(s.Manager.NetworkConfig(), cfg)
	if r.Changes == nil {
		r.Changes = []config.Change{}
	}
	_, draft := s.Manager.StagedNetworkConfig()
	r.DraftWaits = draft
	r.Valid = len(r.Problems) == 0
	return r
}

func parseAuth(b *Bundle) (config.AuthConfig, []Problem) {
	var a config.AuthConfig
	body, ok := b.Files[FileAuth]
	if !ok {
		return a, []Problem{"the backup has no administrators"}
	}
	if err := json.Unmarshal(body, &a); err != nil {
		return a, []Problem{"the administrators in the backup cannot be read"}
	}
	var problems []Problem
	admins := 0
	seen := map[string]bool{}
	for _, u := range a.Users {
		if err := auth.ValidateUsername(u.Username); err != nil {
			problems = append(problems, fmt.Sprintf("the account %q: %v", u.Username, err))
		}
		if seen[strings.ToLower(u.Username)] {
			problems = append(problems, fmt.Sprintf("the account %q is there twice", u.Username))
		}
		seen[strings.ToLower(u.Username)] = true
		if !strings.HasPrefix(u.PasswordHash, "$2") {
			problems = append(problems, fmt.Sprintf("the account %q has no usable password", u.Username))
		}
		if u.Role != auth.RoleAdmin && u.Role != auth.RoleReadOnly {
			problems = append(problems, fmt.Sprintf("the account %q has an unknown role", u.Username))
		}
		if u.Role == auth.RoleAdmin {
			admins++
		}
	}
	if admins == 0 {
		problems = append(problems, "there is no administrator among the accounts: no one could change anything afterwards")
	}
	if a.SessionTimeoutMinutes != 0 && (a.SessionTimeoutMinutes < config.MinSessionTimeoutMinutes || a.SessionTimeoutMinutes > config.MaxSessionTimeoutMinutes) {
		problems = append(problems, "the session timeout is out of range")
	}
	return a, problems
}

func (s *Service) adminsReport(b *Bundle) *AdminsReport {
	a, problems := parseAuth(b)
	r := &AdminsReport{Accounts: []AdminAccount{}, Problems: problems, Current: len(s.Manager.Users())}
	if r.Problems == nil {
		r.Problems = []Problem{}
	}
	for _, u := range a.Users {
		r.Accounts = append(r.Accounts, AdminAccount{Name: u.Username, Role: u.Role})
	}
	r.Valid = len(problems) == 0
	return r
}

// certPair checks that a certificate file and a key file in the backup belong together.
func certPair(b *Bundle, crt, key string) (cert.Details, error) {
	c, k := b.Files[certPrefix+crt], b.Files[certPrefix+key]
	if len(c) == 0 || len(k) == 0 {
		return cert.Details{}, fmt.Errorf("%s or %s is missing", crt, key)
	}
	if _, err := tls.X509KeyPair(c, k); err != nil {
		return cert.Details{}, fmt.Errorf("%s and %s do not belong together", crt, key)
	}
	return cert.DescribePEM(c)
}

func certsReport(b *Bundle) *CertsReport {
	r := &CertsReport{Problems: []Problem{}}
	if _, ok := b.Files[certPrefix+"tls.crt"]; ok {
		if d, err := certPair(b, "tls.crt", "tls.key"); err != nil {
			r.Problems = append(r.Problems, "the web certificate: "+err.Error())
		} else {
			r.Web = &d
		}
	}
	if _, ok := b.Files[certPrefix+"ca.crt"]; ok {
		if d, err := certPair(b, "ca.crt", "ca.key"); err != nil {
			r.Problems = append(r.Problems, "the web authority: "+err.Error())
		} else {
			r.Authority = &d
		}
	}
	_, r.Custom = b.Files[certPrefix+"custom"]
	if r.Web == nil && r.Authority == nil && len(r.Problems) == 0 {
		r.Problems = append(r.Problems, "there is no certificate in the backup")
	}
	r.Valid = len(r.Problems) == 0
	return r
}

func (s *Service) report(token string, b *Bundle, keepPorts bool, choice *PortChoice) *Report {
	m := b.Manifest
	r := &Report{Token: token, Created: m.Created, Hostname: m.Hostname, Software: m.Software, Sections: m.Sections, History: []HistoryFile{}}
	if m.Has(Settings) {
		r.Settings = s.settingsReport(b, keepPorts, choice)
	}
	if m.Has(Admins) {
		r.Admins = s.adminsReport(b)
	}
	if m.Has(Certificates) {
		r.Certificates = certsReport(b)
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, histPrefix) {
			r.History = append(r.History, HistoryFile{Name: strings.TrimPrefix(f.Path, histPrefix), Size: f.Size})
		}
	}
	return r
}

// ---- restoring ----------------------------------------------------------------------------------------------------------------

// PortChoice assigns this router's ports explicitly (first-run setup, where the router has none yet): which port is the internet
// connection and which are LAN ports. It takes the place of KeepPorts.
type PortChoice struct {
	Wan string   `json:"wan"`
	Lan []string `json:"lan"`
}

// Options say what to restore.
type Options struct {
	Settings     bool `json:"settings"`
	Admins       bool `json:"admins"`
	Certificates bool `json:"certificates"`
	History      bool `json:"history"`
	KeepPorts    bool `json:"keep_ports"`
	// Ports, when set, assigns the ports (see PortChoice).
	Ports *PortChoice `json:"ports,omitempty"`
}

// Result says what was done.
type Result struct {
	Done          []string `json:"done"`
	SignedOut     bool     `json:"signed_out"`     // everyone was signed out (the administrators were replaced)
	NeedsRestart  bool     `json:"needs_restart"`  // the history comes back when the daemon starts next
	DraftReplaced bool     `json:"draft_replaced"` // the settings are now the draft, and replaced one that was waiting
	Copy          string   `json:"copy"`           // the id of the copy of the router's own state that was kept
}

// checkChoice says whether the ports assigned are ones this router has, used once each.
func (s *Service) checkChoice(c PortChoice) error {
	if c.Wan == "" {
		return errors.New("choose the port of the internet connection")
	}
	if len(c.Lan) == 0 {
		return errors.New("choose at least one LAN port")
	}
	have, known := s.portNames()
	set := map[string]bool{}
	for _, n := range have {
		set[n] = true
	}
	seen := map[string]bool{c.Wan: true}
	if known && !set[c.Wan] {
		return fmt.Errorf("this router has no port %q", c.Wan)
	}
	for _, p := range c.Lan {
		switch {
		case known && !set[p]:
			return fmt.Errorf("this router has no port %q", p)
		case seen[p]:
			return fmt.Errorf("the port %q is chosen twice", p)
		}
		seen[p] = true
	}
	return nil
}

// RestoreFirstRun restores a backup onto a router that is not set up yet (the caller has made sure of that): the administrators must
// come with it (there would be no one to sign in), nothing is kept as a copy (there is nothing yet to go back to), and the router is
// marked as set up when it is done.
func (s *Service) RestoreFirstRun(token string, o Options) (*Result, error) {
	b, err := s.get(token)
	if err != nil {
		return nil, err
	}
	if !b.Manifest.Has(Admins) {
		return nil, errors.New("this backup has no administrators, so it cannot set up a new router: set one up as a new router and restore the backup there")
	}
	o.Admins = true
	o.Settings = o.Settings && b.Manifest.Has(Settings)
	o.Certificates = o.Certificates && b.Manifest.Has(Certificates)
	o.History = o.History && b.Manifest.Has(History)
	if o.Settings && o.Ports == nil && !o.KeepPorts && s.portsMissing(b) {
		return nil, errors.New("this router does not have the ports the backup uses: choose which of its ports to use")
	}
	res, err := s.apply(b, o, false)
	if err != nil {
		return nil, err
	}
	if err := s.Manager.MarkConfigured(); err != nil {
		return nil, err
	}
	s.Forget(token)
	return res, nil
}

// Restore restores the chosen sections of an open backup.
func (s *Service) Restore(token string, o Options) (*Result, error) {
	b, err := s.get(token)
	if err != nil {
		return nil, err
	}
	res, err := s.apply(b, o, true)
	if err == nil {
		s.Forget(token)
	}
	return res, err
}

func (s *Service) apply(b *Bundle, o Options, keepCopy bool) (*Result, error) {
	if !s.editable() {
		return nil, errors.New("a change is being applied or is waiting for confirmation: confirm or revert it first")
	}
	m := b.Manifest
	if (o.Settings && !m.Has(Settings)) || (o.Admins && !m.Has(Admins)) || (o.Certificates && !m.Has(Certificates)) || (o.History && !m.Has(History)) {
		return nil, errors.New("the backup does not have what you chose")
	}
	if !o.Settings && !o.Admins && !o.Certificates && !o.History {
		return nil, errors.New("choose what to restore")
	}
	// everything is checked before anything changes
	var restored config.NetworkConfig
	if o.Settings {
		cfg, err := parseNetwork(b)
		if err != nil {
			return nil, err
		}
		switch {
		case o.Ports != nil:
			if err := s.checkChoice(*o.Ports); err != nil {
				return nil, err
			}
			cfg = s.chooseOwnPorts(cfg, *o.Ports)
		case o.KeepPorts:
			cfg = s.keepOwnPorts(cfg)
		}
		if err := cfg.Validate(); err != nil {
			return nil, fmt.Errorf("the settings cannot be restored: %s", strings.Join(problemsOf(err), "; "))
		}
		restored = cfg
	}
	var restoredAuth config.AuthConfig
	if o.Admins {
		a, problems := parseAuth(b)
		if len(problems) > 0 {
			return nil, fmt.Errorf("the administrators cannot be restored: %s", strings.Join(problems, "; "))
		}
		restoredAuth = a
	}
	if o.Certificates {
		if r := certsReport(b); !r.Valid {
			return nil, fmt.Errorf("the certificates cannot be restored: %s", strings.Join(r.Problems, "; "))
		}
	}

	res := &Result{Done: []string{}}
	// what the router has now is kept first, so that this can be undone
	if keepCopy {
		id, err := s.saveCopy()
		if err != nil {
			return nil, fmt.Errorf("a copy of what the router has now could not be kept, so nothing was changed: %v", err)
		}
		res.Copy = id
	}
	var undo []func()
	fail := func(err error) (*Result, error) {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		return nil, err
	}
	snapshot := s.currentBundle()

	if o.Certificates {
		if err := s.writeCerts(b); err != nil {
			s.writeCerts(snapshot) // as it was
			return fail(fmt.Errorf("the certificates could not be restored: %v", err))
		}
		undo = append(undo, func() { s.writeCerts(snapshot) })
		res.Done = append(res.Done, Certificates)
	}
	if o.Admins {
		if err := s.writeAdmins(b, restoredAuth); err != nil {
			return fail(fmt.Errorf("the administrators could not be restored: %v", err))
		}
		old, _ := parseAuth(snapshot)
		undo = append(undo, func() { s.writeAdmins(snapshot, old) })
		res.Done = append(res.Done, Admins)
		res.SignedOut = true
	}
	if o.Settings {
		_, hadDraft := s.Manager.StagedNetworkConfig()
		err := s.Manager.StageNetwork(func(c *config.NetworkConfig) error { *c = restored; return nil })
		if err != nil {
			return fail(fmt.Errorf("the settings could not be restored: %v", err))
		}
		res.DraftReplaced = hadDraft
		res.Done = append(res.Done, Settings)
	}
	if o.History {
		if err := s.stageHistory(b); err != nil {
			// the rest is done; only the history failed, and nothing it needed was changed
			res.Done = append(res.Done, "history failed: "+err.Error())
		} else {
			res.NeedsRestart = true
			res.Done = append(res.Done, History)
		}
	}
	if res.SignedOut && s.Sessions != nil {
		s.Sessions.RevokeAll()
	}
	return res, nil
}

// writeCerts puts the certificate files of a bundle in place (the ones it has; a custom marker that it lacks is removed) and reloads.
func (s *Service) writeCerts(b *Bundle) error {
	if err := os.MkdirAll(s.CertDir, 0755); err != nil {
		return err
	}
	write := func(f string) error {
		body := b.Files[certPrefix+f]
		mode := os.FileMode(0644)
		if strings.HasSuffix(f, ".key") {
			mode = 0600
		}
		return fsutil.WriteFileAtomic(filepath.Join(s.CertDir, f), body, mode)
	}
	remove := func(f string) error {
		if err := os.Remove(filepath.Join(s.CertDir, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	// the custom marker comes first when there is one (a certificate that is custom must never be in place unmarked), and goes last when
	// there is none
	if _, ok := b.Files[certPrefix+"custom"]; ok {
		if err := write("custom"); err != nil {
			return err
		}
	}
	for _, f := range CertFiles {
		if f == "custom" {
			continue
		}
		if _, ok := b.Files[certPrefix+f]; ok {
			if err := write(f); err != nil {
				return err
			}
		} else if f == "extra-names.json" {
			if err := remove(f); err != nil {
				return err
			}
		}
	}
	if _, ok := b.Files[certPrefix+"custom"]; !ok {
		if err := remove("custom"); err != nil {
			return err
		}
	}
	if s.Certs != nil {
		if err := s.Certs.Reload(); err != nil {
			return err
		}
		return s.Certs.ReloadAndCheck()
	}
	return nil
}

func (s *Service) writeAdmins(b *Bundle, a config.AuthConfig) error {
	if err := s.Manager.UpdateAuth(func(c *config.AuthConfig) error {
		c.Users = append([]config.User(nil), a.Users...)
		// the console login is a Linux account of this machine: the one that is here stays (a router that has none, a new one,
		// takes the backup's, and the setup makes the account)
		if c.ConsoleUser == "" {
			c.ConsoleUser = a.ConsoleUser
		}
		if a.SessionTimeoutMinutes != 0 {
			c.SessionTimeoutMinutes = a.SessionTimeoutMinutes
		} else {
			c.SessionTimeoutMinutes = config.DefaultSessionTimeoutMinutes
		}
		return nil
	}); err != nil {
		return err
	}
	// the machine's own identity is kept; the mode it runs in comes from the backup
	if body, ok := b.Files[FileSys]; ok {
		var sys config.SysConfig
		if json.Unmarshal(body, &sys) == nil && (sys.OperationMode == "ROUTER" || sys.OperationMode == "AP") {
			return s.Manager.UpdateSys(func(c *config.SysConfig) error { c.OperationMode = sys.OperationMode; return nil })
		}
	}
	return nil
}

// stageHistory puts the databases where the daemon swaps them in when it starts (they are in use now).
func (s *Service) stageHistory(b *Bundle) error {
	dir := filepath.Join(s.ConfigDir, pendingDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, f := range HistoryFiles {
		body, ok := b.Files[histPrefix+f]
		if !ok {
			continue
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, f), body, 0600); err != nil {
			return err
		}
	}
	return nil
}

// ApplyPending swaps in the databases a restore staged. It runs when the daemon starts, before it opens them.
func ApplyPending(configDir string) []string {
	dir := filepath.Join(configDir, pendingDir)
	var done []string
	for _, f := range HistoryFiles {
		src := filepath.Join(dir, f)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(configDir, f)
		os.Remove(dst + "-wal")
		os.Remove(dst + "-shm")
		if err := os.Rename(src, dst); err == nil {
			done = append(done, f)
		}
	}
	os.RemoveAll(dir)
	return done
}

// ---- the copies the router keeps of itself ---------------------------------------------------------------------------------

// Copy is a copy of the router's own state, made before a restore.
type Copy struct {
	ID       string   `json:"id"`
	Created  int64    `json:"created"`
	Hostname string   `json:"hostname"`
	Sections []string `json:"sections"`
	Size     int64    `json:"size"`
}

func (s *Service) copyDir() string { return filepath.Join(s.ConfigDir, localDir) }

// currentBundle reads the router's own settings, administrators and certificates (not the history, which is too big to keep).
func (s *Service) currentBundle() *Bundle {
	b, err := Collect(s.source(false))
	if err != nil {
		return &Bundle{Files: map[string][]byte{}}
	}
	return b
}

func (s *Service) saveCopy() (string, error) {
	src := s.source(false)
	b, err := Collect(src)
	if err != nil {
		return "", err
	}
	data, err := b.Encode()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.copyDir(), 0700); err != nil {
		return "", err
	}
	n := src.Now.UnixMilli()
	for { // never over a copy that is there
		if _, err := os.Stat(filepath.Join(s.copyDir(), fmt.Sprintf("%d.tgz", n))); err != nil {
			break
		}
		n++
	}
	id := fmt.Sprintf("%d", n)
	if err := fsutil.WriteFileAtomic(filepath.Join(s.copyDir(), id+".tgz"), data, 0600); err != nil {
		return "", err
	}
	// only the newest few are kept
	list, _ := s.Copies()
	for i := keepLocal; i < len(list); i++ {
		os.Remove(filepath.Join(s.copyDir(), list[i].ID+".tgz"))
	}
	return id, nil
}

func validCopyID(id string) bool {
	if id == "" || len(id) > 20 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Copies lists the copies, newest first.
func (s *Service) Copies() ([]Copy, error) {
	entries, err := os.ReadDir(s.copyDir())
	if os.IsNotExist(err) {
		return []Copy{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Copy{}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".tgz")
		if id == e.Name() || !validCopyID(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.copyDir(), e.Name()))
		if err != nil {
			continue
		}
		b, err := Decode(data)
		if err != nil {
			continue
		}
		out = append(out, Copy{ID: id, Created: b.Manifest.Created, Hostname: b.Manifest.Hostname, Sections: b.Manifest.Sections, Size: int64(len(data))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// RestoreCopy puts a copy back (everything in it, as it was: it is this router's own state), keeping the state it replaces as a copy
// in turn.
func (s *Service) RestoreCopy(id string) (*Result, error) {
	if !validCopyID(id) {
		return nil, errors.New("there is no such copy")
	}
	data, err := os.ReadFile(filepath.Join(s.copyDir(), id+".tgz"))
	if err != nil {
		return nil, errors.New("there is no such copy")
	}
	b, err := Decode(data)
	if err != nil {
		return nil, err
	}
	return s.apply(b, Options{Settings: b.Manifest.Has(Settings), Admins: b.Manifest.Has(Admins), Certificates: b.Manifest.Has(Certificates)}, true)
}

// DeleteCopy removes a copy.
func (s *Service) DeleteCopy(id string) error {
	if !validCopyID(id) {
		return errors.New("there is no such copy")
	}
	if err := os.Remove(filepath.Join(s.copyDir(), id+".tgz")); err != nil {
		return errors.New("there is no such copy")
	}
	return nil
}

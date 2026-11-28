package netdevices

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync"

	"peakroute/internal/fsutil"
)

// Store keeps the devices to watch in a file of its own (mode 0600: it holds communities and passwords). They are not part of the network
// configuration: watching a switch changes nothing on the router, so there is no draft to commit; a change takes effect at the next poll.
type Store struct {
	path string
	mu   sync.Mutex
	list []Device
	next int
}

// Limits.
const (
	MaxDevices = 32
	maxName    = 40
)

type storeFile struct {
	Devices []Device `json:"devices"`
	Next    int      `json:"next"`
}

// OpenStore reads the file (a missing one is an empty list; an unreadable one is an error, so that it is not written over).
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, next: 1}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("the list of watched devices cannot be read: %w", err)
	}
	s.list, s.next = f.Devices, max(f.Next, 1)
	return s, nil
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(storeFile{Devices: s.list, Next: s.next}, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(s.path, b, 0600)
}

func clone(d Device) Device {
	if d.SNMP != nil {
		a := *d.SNMP
		d.SNMP = &a
	}
	if d.AP != nil {
		a := *d.AP
		d.AP = &a
	}
	return d
}

// List returns a copy of the devices.
func (s *Store) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, len(s.list))
	for i, d := range s.list {
		out[i] = clone(d)
	}
	return out
}

// Get returns one device.
func (s *Store) Get(id string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.list {
		if d.ID == id {
			return clone(d), true
		}
	}
	return Device{}, false
}

// FieldError names what is wrong with a field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Errors is a list of them.
type Errors []FieldError

func (e Errors) Error() string {
	var parts []string
	for _, f := range e {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return strings.Join(parts, "; ")
}

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._()+/-]*$`)
	hostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	plain       = regexp.MustCompile(`^[\x21-\x7e]+$`) // printable, no spaces
)

// Validate checks a device as it would be stored.
func Validate(d Device) Errors {
	var errs Errors
	add := func(f, m string) { errs = append(errs, FieldError{f, m}) }
	if n := strings.TrimSpace(d.Name); n == "" || len(n) > maxName || !namePattern.MatchString(n) {
		add("name", fmt.Sprintf("use 1 to %d letters, digits, spaces or . _ ( ) + - / characters", maxName))
	}
	switch {
	case d.Address == "":
		add("address", "required")
	case len(d.Address) > 253:
		add("address", "too long")
	default:
		if a, err := netip.ParseAddr(d.Address); err == nil {
			if !a.Is4() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
				add("address", "must be an IPv4 address of the device (not 0.0.0.0, loopback or multicast) or its host name")
			}
		} else if !hostPattern.MatchString(d.Address) {
			add("address", "must be an IPv4 address or a host name")
		}
	}
	switch d.Kind {
	case KindSNMP:
		a := d.SNMP
		if a == nil {
			add("snmp", "required for an SNMP device")
			break
		}
		if a.Port < 0 || a.Port > 65535 {
			add("snmp.port", "must be between 1 and 65535")
		}
		switch a.Version {
		case "2c":
			if a.Community == "" || len(a.Community) > 64 || !plain.MatchString(a.Community) {
				add("snmp.community", "required: printable characters, no spaces, up to 64")
			}
		case "3":
			if a.User == "" || len(a.User) > 32 || !plain.MatchString(a.User) {
				add("snmp.user", "required: printable characters, no spaces, up to 32")
			}
			if a.AuthProto == "" && a.PrivProto != "" {
				add("snmp.priv_proto", "encryption needs authentication too")
			}
			if a.AuthProto != "" {
				if !oneOf(strings.ToUpper(a.AuthProto), "MD5", "SHA", "SHA224", "SHA256", "SHA384", "SHA512") {
					add("snmp.auth_proto", "must be MD5, SHA, SHA224, SHA256, SHA384 or SHA512")
				}
				if len(a.AuthPass) < 8 || len(a.AuthPass) > 128 {
					add("snmp.auth_pass", "must be 8 to 128 characters")
				}
			}
			if a.PrivProto != "" {
				if !oneOf(strings.ToUpper(a.PrivProto), "DES", "AES", "AES192", "AES256", "AES192C", "AES256C") {
					add("snmp.priv_proto", "must be DES, AES, AES192 or AES256")
				}
				if len(a.PrivPass) < 8 || len(a.PrivPass) > 128 {
					add("snmp.priv_pass", "must be 8 to 128 characters")
				}
			}
		default:
			add("snmp.version", `must be "2c" or "3"`)
		}
	case KindAP:
		validateAP(d, add)
	default:
		add("kind", `must be "snmp" or "peakroute-ap"`)
	}
	return errs
}

func oneOf(s string, options ...string) bool {
	for _, o := range options {
		if s == o {
			return true
		}
	}
	return false
}

func port(d Device) int {
	switch {
	case d.SNMP != nil && d.SNMP.Port > 0:
		return d.SNMP.Port
	case d.AP != nil && d.AP.Port > 0:
		return d.AP.Port
	}
	return 0
}

// Add stores a new device and gives it an id.
func (s *Store) Add(d Device) (Device, error) {
	d.Name = strings.TrimSpace(d.Name)
	if errs := Validate(d); len(errs) > 0 {
		return Device{}, errs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) >= MaxDevices {
		return Device{}, Errors{{"name", fmt.Sprintf("at most %d devices can be watched", MaxDevices)}}
	}
	if err := s.clash(d, ""); err != nil {
		return Device{}, err
	}
	d.ID = fmt.Sprintf("nd%d", s.next)
	s.next++
	s.list = append(s.list, clone(d))
	if err := s.save(); err != nil {
		s.list = s.list[:len(s.list)-1]
		return Device{}, err
	}
	return d, nil
}

func (s *Store) clash(d Device, self string) error {
	for _, o := range s.list {
		if o.ID == self {
			continue
		}
		if strings.EqualFold(o.Name, d.Name) {
			return Errors{{"name", "another device has this name"}}
		}
		if o.Address == d.Address && port(o) == port(d) && o.Kind == d.Kind {
			return Errors{{"address", "this device is already watched"}}
		}
	}
	return nil
}

// Update replaces a device (keeping its id).
func (s *Store) Update(d Device) (Device, error) {
	d.Name = strings.TrimSpace(d.Name)
	if errs := Validate(d); len(errs) > 0 {
		return Device{}, errs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.list {
		if o.ID != d.ID {
			continue
		}
		if err := s.clash(d, d.ID); err != nil {
			return Device{}, err
		}
		s.list[i] = clone(d)
		if err := s.save(); err != nil {
			s.list[i] = o
			return Device{}, err
		}
		return d, nil
	}
	return Device{}, os.ErrNotExist
}

// SetFingerprint remembers the certificate an access point was first seen with (only when none is set yet).
func (s *Store) SetFingerprint(id, fp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.list {
		if o.ID != id || o.AP == nil || o.AP.Fingerprint != "" {
			continue
		}
		n := clone(o)
		n.AP.Fingerprint = fp
		s.list[i] = n
		if err := s.save(); err != nil {
			s.list[i] = o
			return err
		}
		return nil
	}
	return nil
}

// Delete takes a device out.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, o := range s.list {
		if o.ID == id {
			s.list = append(s.list[:i:i], s.list[i+1:]...)
			if err := s.save(); err != nil {
				s.list = append(s.list[:i:i], append([]Device{o}, s.list[i:]...)...)
				return err
			}
			return nil
		}
	}
	return os.ErrNotExist
}

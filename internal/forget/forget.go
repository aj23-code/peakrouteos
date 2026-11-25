// Package forget removes a device that is gone from the Clients list: its DHCP lease, its entry in the router's neighbour (ARP) table,
// what the presence memory knows of it, and its nickname. A device that is here now cannot be removed (it would be back a moment later, with
// its nickname gone for nothing); one that comes back after being removed is simply a new device with a new lease.
//
// A DHCP reservation is configuration, not a lease: removing the device takes it out of the draft (the person commits it like any change),
// so that a device that is gone does not go on holding an address. If the draft cannot be changed just now, the device is removed all the
// same and the answer says the reservation is still there.
package forget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"peakroute/internal/fsutil"
	"peakroute/internal/network"
)

var macPattern = regexp.MustCompile(`^([0-9a-f]{2}:){5}[0-9a-f]{2}$`)

// Service holds what removing needs. Every field is read at the time of the request.
type Service struct {
	// List is the Clients list as the page shows it, with who is online.
	List func(ctx context.Context) []network.ClientLease
	// Interface is the interface the DHCP server hands out addresses on for the network the device's address is in ("" when unknown).
	Interface func(l network.ClientLease) string
	// Run runs a command and returns its output.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Leases reads the DHCP server's lease file as it is now.
	Leases func() []network.ClientLease
	// LeaseFile is the DHCP server's lease file, edited as a last resort (the server stopped and started around it); "" never edits it.
	LeaseFile string
	// Real is false where nothing may be run or edited (a development machine): only the router's own memory of the device is dropped.
	Real bool
	// ForgetSeen drops the presence memory of the device; DropNickname takes its nickname away.
	// Unreserve takes the device's DHCP reservation out of the draft; it says whether there was one.
	Unreserve    func(mac string) (bool, error)
	ForgetSeen   func(mac string)
	DropNickname func(mac string) error
	// Wait is how long the DHCP server gets to act on a release (400 ms when 0).
	Wait time.Duration
}

type errorBody struct {
	Error string `json:"error"`
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// HandleDevice answers DELETE /api/v1/clients/{mac}.
func (s *Service) HandleDevice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", http.MethodDelete)
		write(w, http.StatusMethodNotAllowed, errorBody{"method not allowed"})
		return
	}
	mac := strings.ToLower(r.PathValue("mac"))
	if !macPattern.MatchString(mac) {
		write(w, http.StatusBadRequest, errorBody{"that is not a hardware address"})
		return
	}
	var dev *network.ClientLease
	for _, l := range s.List(r.Context()) {
		if strings.EqualFold(l.MACAddress, mac) {
			l := l
			dev = &l
			break
		}
	}
	if dev == nil {
		write(w, http.StatusNotFound, errorBody{"this device is not in the list"})
		return
	}
	if dev.Status == "online" {
		write(w, http.StatusConflict, errorBody{"This device is here now. Only devices that are gone can be removed."})
		return
	}
	if err := s.remove(r.Context(), *dev, mac); err != nil {
		write(w, http.StatusInternalServerError, errorBody{err.Error()})
		return
	}
	out := map[string]any{"removed": true, "mac": mac, "reservation_removed": false}
	if s.Unreserve != nil {
		removed, err := s.Unreserve(mac)
		out["reservation_removed"] = removed
		if err != nil {
			out["reservation_note"] = "Its reserved address is still on the DHCP page: " + err.Error()
		}
	}
	write(w, http.StatusOK, out)
}

func (s *Service) remove(ctx context.Context, dev network.ClientLease, mac string) error {
	if s.Real {
		if !dev.Static {
			if err := s.releaseLease(ctx, dev, mac); err != nil {
				return err
			}
		}
		if ip := net.ParseIP(dev.IPAddress); ip != nil {
			// an entry that is already gone is not a failure; nothing else depends on this
			s.Run(ctx, "ip", "neigh", "flush", "to", ip.String())
		}
	}
	if s.ForgetSeen != nil {
		s.ForgetSeen(mac)
	}
	if s.DropNickname != nil {
		if err := s.DropNickname(mac); err != nil {
			return fmt.Errorf("the nickname could not be removed: %w", err)
		}
	}
	return nil
}

func (s *Service) held(mac string) bool {
	if s.Leases == nil {
		return false
	}
	for _, l := range s.Leases() {
		if strings.EqualFold(l.MACAddress, mac) {
			return true
		}
	}
	return false
}

// releaseLease asks the DHCP server to let the lease go (dhcp_release, which says to the server what the device would). When the
// lease is still there afterwards, the server is stopped, the line taken out of its file, and the server started again: a second of no
// DHCP and DNS, which is why it is the second choice.
func (s *Service) releaseLease(ctx context.Context, dev network.ClientLease, mac string) error {
	if iface := s.Interface(dev); iface != "" && net.ParseIP(dev.IPAddress) != nil {
		s.Run(ctx, "dhcp_release", iface, dev.IPAddress, mac)
		wait := s.Wait
		if wait == 0 {
			wait = 400 * time.Millisecond
		}
		time.Sleep(wait)
	}
	if !s.held(mac) {
		return nil
	}
	if s.LeaseFile == "" {
		return errors.New("the DHCP server would not let this lease go")
	}
	if _, err := s.Run(ctx, "systemctl", "stop", "dnsmasq"); err != nil {
		return fmt.Errorf("the DHCP server could not be stopped to remove the lease: %w", err)
	}
	editErr := RemoveLease(s.LeaseFile, mac)
	// start it again whatever happened to the file: the network must not be left without DHCP and DNS
	if _, err := s.Run(ctx, "systemctl", "start", "dnsmasq"); err != nil {
		return fmt.Errorf("the DHCP server could not be started again: %w", err)
	}
	if editErr != nil {
		return fmt.Errorf("the lease could not be removed: %w", editErr)
	}
	return nil
}

// RemoveLease takes the lines of a device out of the lease file. Lines look like "expiry mac address name client-id".
func RemoveLease(path, mac string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && strings.EqualFold(f[1], mac) {
			continue
		}
		keep = append(keep, line)
	}
	return fsutil.WriteFileAtomic(path, []byte(strings.Join(keep, "\n")), info.Mode().Perm())
}

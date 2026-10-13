// Package bootstrap is the setup network (docs/setup.md): a router that has no local network yet has no address anyone could reach its
// welcome page on, so until the first real settings are applied the daemon joins all the wired ports into one temporary bridge, gives it
// an address nobody else is likely to use, and hands out addresses on it. Plug a computer into any port, open the address, and the
// setup wizard is there. It goes away when the first settings with a local network are committed (and comes back if they are reverted).
package bootstrap

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"peakroute/internal/config"
	"peakroute/internal/translator"
)

const (
	// Bridge is the name of the temporary bridge. It starts with "br", so the port lists never offer it.
	Bridge = "brsetup"
	// Address is the router's address on the setup network: unusual on purpose (192.168.1.1 is what most internet providers' routers
	// use, and a router behind one would clash with it).
	Address = "192.168.77.1"
	Prefix  = 24
	// the addresses handed out to the computer plugged in
	RangeStart = "192.168.77.100"
	RangeEnd   = "192.168.77.199"
	Netmask    = "255.255.255.0"
	LeaseTime  = "1h"
)

// Needed says whether the setup network is wanted for these (committed) settings: there is no local network yet (an access point has
// its own bridge, and keeps the same address as a recovery address).
func Needed(cfg config.NetworkConfig) bool {
	return len(cfg.LanInterfaces) == 0 && !cfg.IsAP()
}

// Service runs the setup network.
type Service struct {
	Run translator.Runner
	// Manager gives the committed settings.
	Manager *config.Manager
	// Ports lists the wired ports to join (Wi-Fi adapters are left alone).
	Ports func() ([]config.SystemInterface, error)
	// Dir is where the DHCP server's files go (a directory under /run).
	Dir string
	// Extra lines are added to the DHCP server's file (a test in a namespace has no user to drop to).
	Extra []string

	mu      sync.Mutex
	running bool
}

// Running says whether the setup network is up.
func (s *Service) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *Service) conf() string { return filepath.Join(s.Dir, "dnsmasq.conf") }
func (s *Service) pid() string  { return filepath.Join(s.Dir, "dnsmasq.pid") }

// DHCPConfig is the DHCP server's file: only on the bridge, no DNS (the address is typed), and no router or DNS offered so that a
// computer that also has another connection does not prefer this one for the internet.
func (s *Service) dhcpConfig() string {
	return strings.Join(append([]string{
		"# the setup network of PeakRoute (docs/setup.md), written by the daemon",
		"interface=" + Bridge,
		"bind-interfaces",
		"port=0",
		"dhcp-authoritative",
		fmt.Sprintf("dhcp-range=%s,%s,%s,%s", RangeStart, RangeEnd, Netmask, LeaseTime),
		"dhcp-option=3",
		"dhcp-option=6",
		"dhcp-leasefile=" + filepath.Join(s.Dir, "dnsmasq.leases"),
		"pid-file=" + s.pid(),
	}, append(s.Extra, "")...), "\n")
}

func (s *Service) run(ctx context.Context, name string, args ...string) error {
	if out, err := s.Run.Run(ctx, name, args...); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

func (s *Service) wired() []string {
	if s.Ports == nil {
		return nil
	}
	all, err := s.Ports()
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range all {
		if !p.Wireless {
			out = append(out, p.Name)
		}
	}
	return out
}

// Start brings the setup network up (whatever is left of an earlier one is removed first).
func (s *Service) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.stopLocked(ctx)

	ports := s.wired()
	if len(ports) == 0 {
		return fmt.Errorf("there is no wired port for the setup network")
	}
	if err := os.MkdirAll(s.Dir, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(s.conf(), []byte(s.dhcpConfig()), 0644); err != nil {
		return err
	}
	steps := [][]string{{"ip", "link", "add", Bridge, "type", "bridge"}}
	for _, p := range ports {
		steps = append(steps,
			[]string{"ip", "addr", "flush", "dev", p},
			[]string{"ip", "link", "set", p, "master", Bridge},
			[]string{"ip", "link", "set", p, "up"})
	}
	steps = append(steps,
		[]string{"ip", "addr", "add", fmt.Sprintf("%s/%d", Address, Prefix), "dev", Bridge},
		[]string{"ip", "link", "set", Bridge, "up"},
		[]string{"dnsmasq", "--conf-file=" + s.conf()})
	for _, st := range steps {
		if err := s.run(ctx, st[0], st[1:]...); err != nil {
			s.stopLocked(ctx)
			return err
		}
	}
	s.running = true
	log.Printf("[+] Setup network: %s on %s, joined %s", Address, Bridge, strings.Join(ports, ", "))
	return nil
}

// Stop takes the setup network down and lets go of the ports.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.stopLocked(ctx)
}

func (s *Service) stopLocked(ctx context.Context) {
	// each step is tried whatever the others did: there may be little left to remove
	if _, err := os.Stat(s.pid()); err == nil {
		_ = s.run(ctx, "pkill", "--pidfile", s.pid())
		_ = os.Remove(s.pid())
	}
	if s.running || s.exists(ctx) {
		_ = s.run(ctx, "ip", "link", "del", Bridge)
	}
	if s.running {
		log.Printf("[+] Setup network stopped")
	}
	s.running = false
}

// exists tells whether a bridge of that name is there (left by an earlier run of the daemon).
func (s *Service) exists(ctx context.Context) bool {
	_, err := s.Run.Run(ctx, "ip", "link", "show", Bridge)
	return err == nil
}

// Reconcile makes the setup network match the committed settings: up when there is no local network, down when there is.
func (s *Service) Reconcile() {
	if Needed(s.Manager.NetworkConfig()) {
		if s.Running() {
			return
		}
		if err := s.Start(); err != nil {
			log.Printf("[-] Setup network: %v", err)
		}
		return
	}
	s.Stop()
}

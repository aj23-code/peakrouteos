package timeserver

import (
	"context"
	"log"
	"net"
	"sync"
	"time"
)

// Server listens on the router's own address of each network that serves the time, and keeps the listeners in line with the
// configuration: an address that is added gets one, one that goes away loses it, and one that stopped working (the network was
// restarted under it) is opened again.
type Server struct {
	// Addrs are the router's addresses to answer on (one per network that serves the time), read again every Every.
	Addrs func() []string
	// Clock says where the router's clock stands. It may be slow (it asks the system), so it is read at most once an Every.
	Clock func(ctx context.Context) State
	Every time.Duration
	Now   func() time.Time // time.Now unless a test says otherwise
	Port  string           // "123" unless a test says otherwise

	mu        sync.Mutex
	listeners map[string]*listener
	state     State
	asked     int64
	failed    map[string]string // address -> why it could not be opened, for the log (once per reason)
}

type listener struct {
	conn net.PacketConn
	done chan struct{}
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) {
	if s.Every <= 0 {
		s.Every = 10 * time.Second
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Port == "" {
		s.Port = "123"
	}
	s.listeners = map[string]*listener{}
	s.failed = map[string]string{}
	defer s.closeAll()

	tick := time.NewTicker(s.Every)
	defer tick.Stop()
	for {
		s.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Asked is how many questions have been answered since the server started.
func (s *Server) Asked() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

func (s *Server) reconcile(ctx context.Context) {
	want := map[string]bool{}
	for _, a := range s.Addrs() {
		want[a] = true
	}
	// only ask the system about the clock when someone is to be served
	if len(want) > 0 && s.Clock != nil {
		st := s.Clock(ctx)
		s.mu.Lock()
		s.state = st
		s.mu.Unlock()
	}
	for addr, l := range s.listeners {
		select {
		case <-l.done: // the listener stopped by itself (its address went away)
			delete(s.listeners, addr)
		default:
			if !want[addr] {
				l.conn.Close()
				<-l.done
				delete(s.listeners, addr)
			}
		}
	}
	for addr := range want {
		if _, ok := s.listeners[addr]; ok {
			continue
		}
		conn, err := net.ListenPacket("udp", net.JoinHostPort(addr, s.Port))
		if err != nil {
			if s.failed[addr] != err.Error() {
				s.failed[addr] = err.Error()
				log.Printf("time server: cannot answer on %s: %v", addr, err)
			}
			continue
		}
		delete(s.failed, addr)
		l := &listener{conn: conn, done: make(chan struct{})}
		s.listeners[addr] = l
		go s.serve(l)
	}
}

func (s *Server) serve(l *listener) {
	defer close(l.done)
	buf := make([]byte, 512)
	for {
		n, from, err := l.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		s.mu.Lock()
		st := s.state
		s.mu.Unlock()
		if reply, ok := Reply(buf[:n], s.Now(), st); ok {
			if _, err := l.conn.WriteTo(reply, from); err == nil {
				s.mu.Lock()
				s.asked++
				s.mu.Unlock()
			}
		}
	}
}

func (s *Server) closeAll() {
	for _, l := range s.listeners {
		l.conn.Close()
		<-l.done
	}
	s.listeners = nil
}

package syslogfwd

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"
)

// Server is a syslog server to send to.
type Server struct {
	Host     string
	Port     int
	Protocol string // "udp" or "tcp"
}

// nextBackoff doubles the wait between connection attempts, up to the longest.
func nextBackoff(b time.Duration) time.Duration {
	if b *= 2; b > maxBackoff {
		return maxBackoff
	}
	return b
}

func (s Server) addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// key identifies a server in the forwarder.
func (s Server) key() string { return s.Protocol + "://" + s.addr() }

// ServerStatus is how sending to one server is going.
type ServerStatus struct {
	Server    Server
	Sent      int64
	Dropped   int64     // thrown away because the server could not be reached and the queue was full
	Connected bool      // TCP only: a connection is open now
	LastError string    // the last thing that went wrong ("" when nothing has)
	LastSent  time.Time // the last message that went out
}

const (
	queueSize    = 2000
	dialTimeout  = 5 * time.Second
	writeTimeout = 5 * time.Second
)

// how long to wait before connecting again after a failure: doubling from the first to the second (variables for the tests)
var (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

// sender delivers messages to one server from a queue. UDP is one datagram per message; TCP is one connection kept open, one
// line (LF at the end) per message. A message that cannot be delivered waits in the queue; when the queue is full the oldest
// message goes, so that a server that is down for a day costs memory for the newest few thousand messages and no more.
type sender struct {
	srv    Server
	queue  chan string
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	status ServerStatus
}

func newSender(ctx context.Context, srv Server) *sender {
	ctx, cancel := context.WithCancel(ctx)
	s := &sender{srv: srv, queue: make(chan string, queueSize), cancel: cancel, done: make(chan struct{})}
	s.status.Server = srv
	go s.run(ctx)
	return s
}

func (s *sender) stop() {
	s.cancel()
	<-s.done
}

func (s *sender) enqueue(line string) {
	for {
		select {
		case s.queue <- line:
			return
		default:
		}
		select { // full: the oldest goes
		case <-s.queue:
			s.mu.Lock()
			s.status.Dropped++
			s.mu.Unlock()
		default:
		}
	}
}

func (s *sender) snapshot() ServerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

func (s *sender) note(f func(*ServerStatus)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
}

func (s *sender) run(ctx context.Context) {
	defer close(s.done)
	var conn net.Conn
	closeConn := func() {
		if conn != nil {
			conn.Close()
			conn = nil
			s.note(func(st *ServerStatus) { st.Connected = false })
		}
	}
	defer closeConn()
	backoff := minBackoff
	var pending string
	havePending := false
	for {
		if !havePending {
			select {
			case <-ctx.Done():
				return
			case pending = <-s.queue:
				havePending = true
			}
		}
		if conn == nil {
			d := net.Dialer{Timeout: dialTimeout}
			c, err := d.DialContext(ctx, s.srv.Protocol, s.srv.addr())
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				s.note(func(st *ServerStatus) { st.LastError = err.Error() })
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = nextBackoff(backoff)
				continue
			}
			conn = c
			backoff = minBackoff
			s.note(func(st *ServerStatus) { st.Connected = s.srv.Protocol == "tcp" })
		}
		payload := pending
		if s.srv.Protocol == "tcp" {
			payload += "\n"
		}
		conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := conn.Write([]byte(payload)); err != nil {
			s.note(func(st *ServerStatus) { st.LastError = err.Error() })
			closeConn() // the message stays pending and goes out on the next connection
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		s.note(func(st *ServerStatus) { st.Sent++; st.LastSent = time.Now(); st.LastError = "" })
		havePending = false
	}
}

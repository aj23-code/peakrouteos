package wanhealth

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"
)

// Pinger measures the round trip time to an address.
type Pinger interface {
	Ping(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error)
	// Method names what the last measurement used: "icmp" or "dns".
	Method() string
	// ICMP reports whether real pings are possible (they are until one is found to be forbidden).
	ICMP() bool
}

// ErrNoPermission means this process may not open an ICMP socket.
var ErrNoPermission = errors.New("not allowed to send ICMP echo requests")

// ErrTimeout means no reply arrived in time.
var ErrTimeout = errors.New("no reply")

// AutoPinger sends an ICMP echo (a real ping) when the process is allowed to, and otherwise
// times a small DNS query to the same address, which needs no privileges. The appliance daemon
// runs with the right to ping; a developer running it as a normal user usually does not.
type AutoPinger struct {
	mu         sync.Mutex
	icmpDenied bool
	last       string

	// Replaceable in tests; nil means the real thing
	icmp func(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error)
	dns  func(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error)
}

func (p *AutoPinger) ICMP() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.icmpDenied
}

func (p *AutoPinger) Method() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *AutoPinger) Ping(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error) {
	p.mu.Lock()
	denied := p.icmpDenied
	p.mu.Unlock()

	icmp, dns := p.icmp, p.dns
	if icmp == nil {
		icmp = pingICMP
	}
	if dns == nil {
		dns = pingDNS
	}

	if !denied {
		rtt, err := icmp(ctx, addr, timeout)
		if !errors.Is(err, ErrNoPermission) {
			p.setMethod("icmp")
			return rtt, err
		}
		p.mu.Lock()
		p.icmpDenied = true
		p.mu.Unlock()
	}
	p.setMethod("dns")
	return dns(ctx, addr, timeout)
}

func (p *AutoPinger) setMethod(m string) {
	p.mu.Lock()
	p.last = m
	p.mu.Unlock()
}

// ---- ICMP ----------------------------------------------------------------------------

var icmpID = uint16(os.Getpid())

var seqMu sync.Mutex
var nextSeq uint16

func newSeq() uint16 {
	seqMu.Lock()
	defer seqMu.Unlock()
	nextSeq++
	return nextSeq
}

func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func echoRequest(id, seq uint16) []byte {
	b := make([]byte, 8+16)
	b[0] = 8 // echo request
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	copy(b[8:], "peakroute-wan-hc")
	binary.BigEndian.PutUint16(b[2:], icmpChecksum(b))
	return b
}

// echoReplyMatches reports whether b is an echo reply to our request. A "ping" datagram socket
// rewrites the identifier, so only raw sockets can check it.
func echoReplyMatches(b []byte, id, seq uint16, checkID bool) bool {
	if len(b) < 8 || b[0] != 0 { // echo reply
		return false
	}
	if binary.BigEndian.Uint16(b[6:]) != seq {
		return false
	}
	return !checkID || binary.BigEndian.Uint16(b[4:]) == id
}

// listenICMP opens an unprivileged "ping" socket if the system allows it (net.ipv4.ping_group_range)
// and a raw socket otherwise.
func listenICMP() (conn net.PacketConn, datagram bool, err error) {
	fd, serr := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_ICMP)
	if serr == nil {
		f := os.NewFile(uintptr(fd), "icmp")
		defer f.Close()
		pc, ferr := net.FilePacketConn(f)
		if ferr != nil {
			return nil, false, ferr
		}
		return pc, true, nil
	}
	pc, rerr := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if rerr == nil {
		return pc, false, nil
	}
	if isPermission(serr) || isPermission(rerr) {
		return nil, false, ErrNoPermission
	}
	return nil, false, rerr
}

func isPermission(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) || errors.Is(err, os.ErrPermission)
}

func pingICMP(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error) {
	if !addr.Is4() {
		return 0, fmt.Errorf("only IPv4 addresses can be pinged")
	}
	conn, datagram, err := listenICMP()
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	ip := net.IP(addr.AsSlice())
	var dst net.Addr = &net.IPAddr{IP: ip}
	if datagram {
		dst = &net.UDPAddr{IP: ip}
	}

	seq := newSeq()
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)

	start := time.Now()
	if _, err := conn.WriteTo(echoRequest(icmpID, seq), dst); err != nil {
		if isPermission(err) {
			return 0, ErrNoPermission
		}
		return 0, err // network unreachable and the like: the link is down for our purposes
	}

	// Cancelling the probe (shutdown) wakes the read below
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				return 0, ErrTimeout
			}
			return 0, err
		}
		if !datagram {
			if ia, ok := from.(*net.IPAddr); !ok || !ia.IP.Equal(ip) {
				continue
			}
		}
		if echoReplyMatches(buf[:n], icmpID, seq, !datagram) {
			return time.Since(start), nil
		}
	}
}

// ---- DNS -----------------------------------------------------------------------------

// dnsQuery builds a query for the root name servers (". NS"), which any resolver answers from
// its cache.
func dnsQuery(id uint16) []byte {
	b := make([]byte, 12, 17)
	binary.BigEndian.PutUint16(b[0:], id)
	binary.BigEndian.PutUint16(b[2:], 0x0100) // recursion desired
	binary.BigEndian.PutUint16(b[4:], 1)      // one question
	return append(b, 0, 0, 2, 0, 1)           // root name, type NS, class IN
}

// pingDNS times a DNS query. Any answer counts, even a refusal: the resolver is reachable.
//
// Caveat: some networks redirect all outgoing DNS to their own resolver, which then answers for
// any address. That makes this a weaker check than a real ping, so it is only the fallback.
func pingDNS(ctx context.Context, addr netip.Addr, timeout time.Duration) (time.Duration, error) {
	return queryDNS(ctx, netip.AddrPortFrom(addr, 53), timeout)
}

func queryDNS(ctx context.Context, server netip.AddrPort, timeout time.Duration) (time.Duration, error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "udp", server.String())
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	id := newSeq()
	start := time.Now()
	if _, err := conn.Write(dnsQuery(id)); err != nil {
		return 0, err
	}
	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if ctx.Err() != nil {
					return 0, ctx.Err()
				}
				return 0, ErrTimeout
			}
			return 0, err // for example "connection refused" when the host sends back an ICMP error
		}
		if n >= 12 && binary.BigEndian.Uint16(buf[0:]) == id && buf[2]&0x80 != 0 {
			return time.Since(start), nil
		}
	}
}

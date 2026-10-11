package presence

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"time"
)

// Target is a device to look for: the address its lease gave it and the hardware address it has.
type Target struct {
	MAC string // lower case, colon separated
	IP  netip.Addr
}

// Prober finds out which of the targets answer right now. The result is keyed by MAC address. An error
// means probing is not possible here (no permission for a raw socket), which is not the same as nobody
// answering.
type Prober func(ctx context.Context, targets []Target) (map[string]bool, error)

var broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// arpRequest builds an Ethernet frame holding an ARP "who has target? tell sender".
func arpRequest(srcMAC net.HardwareAddr, srcIP, dstIP netip.Addr) []byte {
	f := make([]byte, 0, 42)
	f = append(f, broadcast...)
	f = append(f, srcMAC...)
	f = append(f, 0x08, 0x06)       // ARP
	f = append(f, 0, 1, 8, 0, 6, 4) // Ethernet, IPv4, 6-byte and 4-byte addresses
	f = append(f, 0, 1)             // request
	f = append(f, srcMAC...)
	s := srcIP.As4()
	f = append(f, s[:]...)
	f = append(f, make([]byte, 6)...)
	d := dstIP.As4()
	f = append(f, d[:]...)
	return f
}

// parseARPReply returns who answered: the sender's hardware and IPv4 address, if the frame is an ARP reply.
func parseARPReply(f []byte) (mac string, ip netip.Addr, ok bool) {
	if len(f) < 42 || f[12] != 0x08 || f[13] != 0x06 {
		return "", netip.Addr{}, false
	}
	if f[14] != 0 || f[15] != 1 || f[16] != 8 || f[17] != 0 || f[18] != 6 || f[19] != 4 {
		return "", netip.Addr{}, false
	}
	if f[20] != 0 || f[21] != 2 {
		return "", netip.Addr{}, false
	}
	ip = netip.AddrFrom4([4]byte(f[28:32]))
	return strings.ToLower(net.HardwareAddr(f[22:28]).String()), ip, true
}

// ifaceInfo is what is needed of a network interface to ask on it.
type ifaceInfo struct {
	Index int
	MAC   net.HardwareAddr
	Addrs []netip.Prefix
	Up    bool
}

// route picks the interface and source address from which a device with this address is reached
// directly: one that is up and has an address in the same subnet.
func route(ifaces []ifaceInfo, dst netip.Addr) (ifaceInfo, netip.Addr, bool) {
	for _, ifc := range ifaces {
		if !ifc.Up || len(ifc.MAC) != 6 {
			continue
		}
		for _, p := range ifc.Addrs {
			if p.Addr().Is4() && p.Contains(dst) && p.Addr() != dst {
				return ifc, p.Addr(), true
			}
		}
	}
	return ifaceInfo{}, netip.Addr{}, false
}

func systemInterfaces() []ifaceInfo {
	var out []ifaceInfo
	list, _ := net.Interfaces()
	for _, ifc := range list {
		info := ifaceInfo{Index: ifc.Index, MAC: ifc.HardwareAddr, Up: ifc.Flags&net.FlagUp != 0}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				info.Addrs = append(info.Addrs, p)
			}
		}
		out = append(out, info)
	}
	return out
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// ARPProber asks each device on the wire "who has this address?" with a raw socket. Unlike a ping this
// is answered by every device that is connected (a phone or a firewalled PC does not answer a ping but
// must answer ARP to be reachable at all), and it needs nothing but the daemon's usual network rights.
type ARPProber struct {
	Attempts int           // requests per device (2 when 0)
	Wait     time.Duration // how long to listen after each round (600 ms when 0)

	interfaces func() []ifaceInfo // systemInterfaces when nil
}

func (p ARPProber) Probe(ctx context.Context, targets []Target) (map[string]bool, error) {
	attempts, wait := p.Attempts, p.Wait
	if attempts == 0 {
		attempts = 2
	}
	if wait == 0 {
		wait = 600 * time.Millisecond
	}
	listIfaces := p.interfaces
	if listIfaces == nil {
		listIfaces = systemInterfaces
	}

	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, int(htons(syscall.ETH_P_ARP)))
	if err != nil {
		return nil, fmt.Errorf("raw socket: %w", err)
	}
	f := os.NewFile(uintptr(fd), "arp")
	defer f.Close()

	ifaces := listIfaces()
	type plan struct {
		t     Target
		frame []byte
		ifidx int
	}
	var plans []plan
	want := map[string]netip.Addr{}
	for _, t := range targets {
		ifc, src, ok := route(ifaces, t.IP)
		if !ok {
			continue // not on a network this router is attached to
		}
		plans = append(plans, plan{t: t, frame: arpRequest(ifc.MAC, src, t.IP), ifidx: ifc.Index})
		want[t.MAC] = t.IP
	}

	answered := map[string]bool{}
	buf := make([]byte, 1600)
	for round := 0; round < attempts && len(answered) < len(plans); round++ {
		for _, pl := range plans {
			if answered[pl.t.MAC] {
				continue
			}
			to := &syscall.SockaddrLinklayer{Protocol: htons(syscall.ETH_P_ARP), Ifindex: pl.ifidx, Halen: 6}
			copy(to.Addr[:], broadcast)
			_ = syscall.Sendto(fd, pl.frame, 0, to) // an interface that went down just now: that device is not answering
		}
		deadline := time.Now().Add(wait)
		for len(answered) < len(plans) && time.Now().Before(deadline) {
			if ctx.Err() != nil {
				return answered, ctx.Err()
			}
			_ = f.SetReadDeadline(deadline)
			n, err := f.Read(buf)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					break
				}
				return answered, err
			}
			if mac, ip, ok := parseARPReply(buf[:n]); ok {
				if wantIP, asked := want[mac]; asked && wantIP == ip {
					answered[mac] = true
				}
			}
		}
	}
	return answered, nil
}

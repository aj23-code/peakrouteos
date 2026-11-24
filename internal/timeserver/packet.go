// Package timeserver answers NTP questions from the local networks that have it switched on (Local Networks, "Serve the time").
// It does not keep time itself: the router's clock follows its time servers (systemd-timesyncd), and this repeats it, as
// stratum one more than the server the router follows. It is the simple kind of server (RFC 5905, section 14 "SNTP"): a client
// asks, the router answers from its own clock; nothing is kept between two questions.
package timeserver

import (
	"crypto/md5"
	"encoding/binary"
	"net"
	"time"
)

// State is what the router knows about its own clock when a question arrives.
type State struct {
	Synchronized bool      // the clock follows a time server; without it clients are told not to trust the answer
	Stratum      int       // the router's stratum (the server it follows plus one), 2 to 15
	Upstream     net.IP    // the server it follows, for the reference identifier (nil: none known)
	LastSync     time.Time // when the clock was last seen to follow a server
}

const (
	packetSize = 48
	ntpEpoch   = 2208988800 // seconds from 1900 to 1970

	modeClient = 3
	modeServer = 4

	// the root delay and dispersion are what a client adds up to judge how far to trust a server; the router does not measure
	// them, so it states a modest, honest figure (about 30 ms each, in 16.16 seconds)
	rootFigure = 0x0000079E
)

// toNTP turns a time into the 64-bit NTP format (32 bits of seconds since 1900, 32 of fraction).
func toNTP(t time.Time) uint64 {
	sec := uint64(t.Unix()) + ntpEpoch
	frac := uint64(t.Nanosecond()) << 32 / 1_000_000_000
	return sec<<32 | frac
}

// Reply builds the answer to one question, or reports false when the packet is not a client's question (too short, or another
// mode: this server answers clients only, never a peer, a control or a monitoring request).
func Reply(req []byte, now time.Time, st State) ([]byte, bool) {
	if len(req) < packetSize {
		return nil, false
	}
	version := int(req[0] >> 3 & 7)
	if req[0]&7 != modeClient || version < 1 || version > 4 {
		return nil, false
	}
	out := make([]byte, packetSize)

	leap := byte(0)
	stratum := byte(st.Stratum)
	if !st.Synchronized || st.Stratum < 2 || st.Stratum > 15 {
		// "the clock is not set": clients that follow the rules ignore the answer
		leap, stratum = 3, 16
	}
	out[0] = leap<<6 | byte(version)<<3 | modeServer
	out[1] = stratum
	out[2] = req[2] // poll: what the client asked for
	out[3] = 0xEC   // precision: 2^-20 s, about a microsecond (an int8 of -20)
	if leap == 0 {
		binary.BigEndian.PutUint32(out[4:], rootFigure)
		binary.BigEndian.PutUint32(out[8:], rootFigure)
		copy(out[12:16], referenceID(st.Upstream))
		binary.BigEndian.PutUint64(out[16:], toNTP(st.LastSync))
	}
	copy(out[24:32], req[40:48]) // origin: the client's own transmit time, so it can match the answer to its question
	stamp := toNTP(now)
	binary.BigEndian.PutUint64(out[32:], stamp) // receive
	binary.BigEndian.PutUint64(out[40:], stamp) // transmit
	return out, true
}

// referenceID names the server the clock follows: an IPv4 address as it is, an IPv6 one by the first four bytes of its MD5
// (RFC 5905), none as zeros.
func referenceID(ip net.IP) []byte {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	if len(ip) == net.IPv6len {
		sum := md5.Sum(ip)
		return sum[:4]
	}
	return []byte{0, 0, 0, 0}
}

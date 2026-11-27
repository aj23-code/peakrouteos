//go:build linux

package speedtest

import (
	"net"

	"golang.org/x/sys/unix"
)

// ackedBytes is how many bytes the other end's kernel has acknowledged on a TCP connection: what has really crossed the line, not what
// the sending program has handed to its own socket buffer (which, at a slow upload, can be seconds of data).
func ackedBytes(c *net.TCPConn) (uint64, bool) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var n uint64
	ok := false
	rc.Control(func(fd uintptr) {
		info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
		if err == nil {
			n, ok = info.Bytes_acked, true
		}
	})
	return n, ok
}

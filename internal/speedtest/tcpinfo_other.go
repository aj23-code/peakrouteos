//go:build !linux

package speedtest

import "net"

func ackedBytes(*net.TCPConn) (uint64, bool) { return 0, false }

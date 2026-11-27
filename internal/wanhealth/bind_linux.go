//go:build linux

package wanhealth

import "syscall"

// bindToDevice makes a socket send through one interface only (SO_BINDTODEVICE), so that a probe of an internet connection goes out
// of that connection and not of whichever the routing table prefers.
func bindToDevice(fd int, dev string) error {
	return syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, dev)
}

// dialControl is a net.Dialer / net.ListenConfig Control function that binds the socket to dev ("" binds nothing).
func dialControl(dev string) func(network, address string, c syscall.RawConn) error {
	if dev == "" {
		return nil
	}
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) { serr = bindToDevice(int(fd), dev) }); err != nil {
			return err
		}
		return serr
	}
}

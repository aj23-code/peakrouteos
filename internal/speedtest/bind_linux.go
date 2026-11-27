//go:build linux

package speedtest

import "syscall"

// dialControl makes the test's connections go out of one interface only (SO_BINDTODEVICE): "" binds nothing.
func dialControl(dev string) func(network, address string, c syscall.RawConn) error {
	if dev == "" {
		return nil
	}
	return func(network, address string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, dev)
		}); err != nil {
			return err
		}
		return serr
	}
}

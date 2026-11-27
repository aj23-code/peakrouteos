//go:build !linux

package speedtest

import "syscall"

func dialControl(dev string) func(network, address string, c syscall.RawConn) error { return nil }

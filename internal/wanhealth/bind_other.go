//go:build !linux

package wanhealth

import "syscall"

// Binding a probe to an interface is only done on Linux, where the router runs; elsewhere (a developer's machine) probes follow the
// routing table.
func bindToDevice(fd int, dev string) error { return nil }

func dialControl(dev string) func(network, address string, c syscall.RawConn) error { return nil }

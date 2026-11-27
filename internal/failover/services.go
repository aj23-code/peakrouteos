package failover

import (
	"context"
	"strings"
)

// MoveServices is what the router does for its services when the traffic moves to the other connection, so that they come back by
// themselves a few seconds later and not at their next timeout:
//
//   - a WireGuard tunnel remembers the address it last sent from: that is the old connection's, so every peer's endpoint is set again (which
//     makes it forget), and its next packet leaves with the address of the connection that carries the traffic;
//   - an OpenVPN tunnel where this router is the client is restarted, to connect again through the new connection (a tunnel where it is
//     the server is reached by the other end at the new connection's address and needs nothing).
//
// The names kept by Dynamic DNS are updated by the caller (internal/ddns), and the connections that were translated to the old
// connection's address are ended by the controller itself.
func MoveServices(ctx context.Context, r Runner) {
	if out, err := r.Run(ctx, "wg", "show", "all", "endpoints"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) == 3 && f[2] != "(none)" {
				r.Run(ctx, "wg", "set", f[0], "peer", f[1], "endpoint", f[2])
			}
		}
	}
	r.Run(ctx, "systemctl", "try-restart", "openvpn-client@*")
}

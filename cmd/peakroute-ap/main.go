// PeakRoute AP: the access point build. It is everything in internal/daemon that every build shares and nothing of the router: no
// internet connection, local networks, firewall rules, DNS or DHCP server, VPN, Dynamic DNS, usage counting or client list, so none of
// it is in the binary or runs (docs/access-point.md, "PeakRoute AP"). The wizard only offers the ways of being an access point, and
// the settings of a router are refused before they are applied. check.sh keeps the router's packages out of this binary.
package main

import (
	"encoding/json"
	"net/http"

	"peakroute/internal/daemon"
)

// routerAPIs are the router's interfaces: here they answer that this device is an access point (as the router does in its access
// point mode) instead of "not found".
var routerAPIs = []string{
	"/api/v1/routes", "/api/v1/dhcp/", "/api/v1/clients/", "/api/v1/firewall", "/api/v1/firewall/", "/api/v1/ddns", "/api/v1/ddns/",
	"/api/v1/vpn", "/api/v1/vpn/", "/api/v1/dns", "/api/v1/dns/upstream", "/api/v1/dns/networks/", "/api/v1/dns/hosts",
	"/api/v1/dns/hosts/", "/api/v1/dns/forwards", "/api/v1/dns/forwards/", "/api/v1/uplinks", "/api/v1/uplinks/", "/api/v1/ports",
	"/api/v1/networks", "/api/v1/networks/",
}

func main() {
	o := daemon.ParseFlags()
	o.APOnly = true
	c := daemon.Start(o)
	notHere := c.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]string{"error": "This is PeakRoute AP, an access point: that belongs to a router."})
	})
	for _, p := range routerAPIs {
		c.Mux.HandleFunc(p, notHere)
	}
	c.Serve()
}

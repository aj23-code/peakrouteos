package netapi

import (
	"net/http"
	"sort"
	"strconv"

	"peakroute/internal/network"
)

// Connection is one connection passing through the router, for the Connections page.
type Connection struct {
	Proto        string `json:"proto"`
	State        string `json:"state"` // the TCP state; empty for other protocols
	ExpiresIn    int    `json:"expires_in"`
	Src          string `json:"src"`
	SrcPort      int    `json:"sport"`
	Dst          string `json:"dst"`
	DstPort      int    `json:"dport"`
	SourceNAT    string `json:"source_nat"` // the address the other side sees, when the router rewrote the source
	DestNAT      string `json:"dest_nat"`   // where it was really sent, when the router rewrote the destination (a port forward)
	DestNATPort  int    `json:"dest_nat_port"`
	Bytes        uint64 `json:"bytes"`       // sent by the initiator
	ReplyBytes   uint64 `json:"reply_bytes"` // sent back
	Packets      uint64 `json:"packets"`
	ReplyPackets uint64 `json:"reply_packets"`
	Unreplied    bool   `json:"unreplied"`
	Assured      bool   `json:"assured"`
}

// ConnectionsReport is what GET /api/v1/firewall/connections answers.
type ConnectionsReport struct {
	Known       bool         `json:"known"`      // false: the table cannot be read here
	Count       int          `json:"count"`      // connections tracked now
	Max         int          `json:"max"`        // how many the table can hold (0: unknown)
	Accounting  bool         `json:"accounting"` // bytes and packets are being counted
	Total       int          `json:"total"`      // how many the table holds
	Shown       int          `json:"shown"`      // how many are listed (the busiest first)
	Connections []Connection `json:"connections"`
}

const (
	defaultConnectionLimit = 1000
	maxConnectionLimit     = 5000
)

func connectionView(c network.Connection) Connection {
	out := Connection{
		Proto: c.Proto, State: c.State, ExpiresIn: c.Timeout, Src: c.Src, SrcPort: c.SrcPort, Dst: c.Dst, DstPort: c.DstPort,
		SourceNAT: c.SourceNAT(), Bytes: c.Bytes, ReplyBytes: c.ReplyBytes, Packets: c.Packets, ReplyPackets: c.ReplyPackets,
		Unreplied: c.Unreplied, Assured: c.Assured,
	}
	out.DestNAT, out.DestNATPort = c.DestinationNAT()
	return out
}

// HandleConnections: GET /api/v1/firewall/connections?limit= lists the connections passing through the router, the
// ones that carried the most data first (or, when the kernel is not counting, the ones that will live longest).
func (a *API) HandleConnections(w http.ResponseWriter, r *http.Request) {
	if !allowMethods(w, r, http.MethodGet) {
		return
	}
	limit := defaultConnectionLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxConnectionLimit {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "limit must be a number from 1 to " + strconv.Itoa(maxConnectionLimit)})
			return
		}
		limit = n
	}
	rep := ConnectionsReport{Connections: []Connection{}}
	if a.Connections != nil {
		if conns, count, max, ok := a.Connections(); ok {
			rep.Known, rep.Count, rep.Max, rep.Total = true, count, max, len(conns)
			for _, c := range conns {
				rep.Accounting = rep.Accounting || c.HasCounters
			}
			sort.SliceStable(conns, func(i, j int) bool {
				bi, bj := conns[i].Bytes+conns[i].ReplyBytes, conns[j].Bytes+conns[j].ReplyBytes
				if bi != bj {
					return bi > bj
				}
				return conns[i].Timeout > conns[j].Timeout
			})
			if len(conns) > limit {
				conns = conns[:limit]
			}
			for _, c := range conns {
				rep.Connections = append(rep.Connections, connectionView(c))
			}
			rep.Shown = len(rep.Connections)
		}
	}
	writeJSON(w, http.StatusOK, rep)
}

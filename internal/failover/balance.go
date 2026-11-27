package failover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"peakroute/internal/config"
	"peakroute/internal/translator"
)

// Sharing the traffic between the two connections, and answering what arrives on one through that same one (docs/wan-failover.md, section 5).
//
// Each connection has a routing table of its own (101, 102) with a default route through it, and a rule that sends packets carrying its
// mark to that table. The mark is in the top byte of the connection mark (0xff000000, apart from the shares of the networks, which use the
// byte below). A table of firewall rules (inet peakroute_lb, written by the controller, not part of the firewall file) does the marking:
//
//   - a connection that arrives from the internet is marked with the connection it arrived on, so that its replies leave the same way
//     (a port forward, a VPN server, the router's own services): needed in failover too, when it arrives on the connection that is waiting;
//   - when the traffic is shared, a new connection from a network is given to one of the connections that are up, by weight;
//   - every packet of a connection carries the connection's mark on its way through the router (and out of it, for the router's own).
//
// The table is not in the firewall file because it follows the health of the connections: the controller rewrites it when one goes down or
// comes back, and puts it back after a commit has flushed the rules.

// The marks, tables and rules.
const (
	LBTable      = "peakroute_lb"
	MarkMask     = 0xff000000
	markFirst    = 0x01000000
	markSecond   = 0x02000000
	tableFirst   = 101
	tableSecond  = 102
	rulePriority = 10100 // + 1 for the first, + 2 for the second
	// suppressPriority is the rule that comes before them: the main table is looked at first with its default routes left out, so that
	// a marked packet for a network behind the router (or any other route that is not a default) still finds it. Without it the table of
	// a connection, which has only its default route, would send such a packet out of the connection.
	suppressPriority = 10050
)

func markOf(id string) int {
	if id == config.WanSecond {
		return markSecond
	}
	return markFirst
}

func tableOf(id string) int {
	if id == config.WanSecond {
		return tableSecond
	}
	return tableFirst
}

// lbRuleset is the text of the firewall table. lans are the interfaces whose new connections are shared, wans the device of each
// connection, up whether each is up, weights how the connections are weighted. With pick false (failover) nothing is shared.
func lbRuleset(lans []string, wans map[string]string, up map[string]bool, weights map[string]int, pick bool) string {
	var b strings.Builder
	b.WriteString("add table inet " + LBTable + "\ndelete table inet " + LBTable + "\n")
	b.WriteString("table inet " + LBTable + " {\n")
	ids := []string{config.WanFirst, config.WanSecond}
	var live []string
	total := 0
	for _, id := range ids {
		if up[id] {
			live = append(live, id)
			total += weights[id]
		}
	}
	if pick && len(live) > 0 && len(lans) > 0 {
		for _, id := range ids {
			fmt.Fprintf(&b, "    chain to_%s {\n        ct mark set ct mark and 0x%08x or 0x%08x\n    }\n", id, ^MarkMask&0xffffffff, markOf(id))
		}
		if len(live) == 1 {
			fmt.Fprintf(&b, "    chain pick {\n        jump to_%s\n    }\n", live[0])
		} else {
			var parts []string
			at := 0
			for _, id := range live {
				w := weights[id]
				rng := fmt.Sprintf("%d", at)
				if w > 1 {
					rng = fmt.Sprintf("%d-%d", at, at+w-1)
				}
				parts = append(parts, fmt.Sprintf("%s : jump to_%s", rng, id))
				at += w
			}
			fmt.Fprintf(&b, "    chain pick {\n        numgen random mod %d vmap { %s }\n    }\n", total, strings.Join(parts, ", "))
		}
	}
	b.WriteString("    chain prerouting {\n        type filter hook prerouting priority mangle; policy accept;\n")
	for _, id := range ids {
		if dev := wans[id]; dev != "" {
			fmt.Fprintf(&b, "        iifname \"%s\" ct mark and 0x%08x == 0 ct mark set ct mark and 0x%08x or 0x%08x\n", dev, MarkMask, ^MarkMask&0xffffffff, markOf(id))
		}
	}
	if pick && len(live) > 0 && len(lans) > 0 {
		fmt.Fprintf(&b, "        iifname { %s } ct mark and 0x%08x == 0 ct state new jump pick\n", quote(lans), MarkMask)
	}
	fmt.Fprintf(&b, "        meta mark set ct mark and 0x%08x\n    }\n", MarkMask)
	fmt.Fprintf(&b, "    chain output {\n        type route hook output priority mangle; policy accept;\n        meta mark set ct mark and 0x%08x\n    }\n}\n", MarkMask)
	return b.String()
}

func quote(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = `"` + n + `"`
	}
	return strings.Join(q, ", ")
}

// syncBalance brings the tables, rules and firewall table to what the configuration and the health of the connections ask for.
func (c *Controller) syncBalance(ctx context.Context, cfg config.NetworkConfig, devices map[string]string, states map[string]string) {
	set := cfg.FailoverSettings()
	// the gateway of each connection is the one its default route has (in whichever metric the controller keeps it)
	out, err := c.Runner.Run(ctx, "ip", "-4", "-o", "route", "show", "default")
	if err != nil {
		return
	}
	main := parseRoutes(out)
	for id, dev := range devices {
		var gw route
		found := false
		for _, r := range main {
			if r.dev == dev {
				gw, found = r, true
				break
			}
		}
		if !found {
			continue // no lease yet: no table to make
		}
		c.ensureTable(ctx, id, gw)
		c.ensureRule(ctx, id)
	}
	c.ensureSuppressRule(ctx)
	up := map[string]bool{}
	for id := range devices {
		up[id] = states[id] != StateOffline
	}
	want := lbRuleset(translator.InternetIfaces(cfg), devices, up, set.Weights, set.Balanced)
	c.mu.Lock()
	same := c.lbApplied == want
	c.mu.Unlock()
	if same {
		if _, err := c.Runner.Run(ctx, "nft", "list", "table", "inet", LBTable); err == nil {
			return
		}
	}
	dir := c.RunDir
	if dir == "" {
		dir = "/run/peakroute"
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	file := filepath.Join(dir, "lb.nft")
	if err := os.WriteFile(file, []byte(want), 0600); err != nil {
		return
	}
	if msg, err := c.Runner.Run(ctx, "nft", "-f", file); err != nil {
		logf("could not load the table that shares the traffic between the connections: %v: %s", err, strings.TrimSpace(msg))
		return
	}
	c.mu.Lock()
	c.lbApplied, c.managed = want, true
	c.mu.Unlock()
}

// ensureTable makes the table of a connection hold a default route through it.
func (c *Controller) ensureTable(ctx context.Context, id string, gw route) {
	table := fmt.Sprint(tableOf(id))
	args := []string{"-4", "route", "replace", "table", table, "default"}
	if gw.via != "" {
		args = append(args, "via", gw.via)
	}
	args = append(args, "dev", gw.dev)
	if cur, err := c.Runner.Run(ctx, "ip", "-4", "-o", "route", "show", "table", table); err == nil {
		for _, r := range parseRoutes(cur) {
			if r.dev == gw.dev && r.via == gw.via {
				return
			}
		}
	}
	c.Runner.Run(ctx, "ip", args...)
}

// ensureSuppressRule makes every lookup start with the main table's routes that are not default routes (see suppressPriority).
func (c *Controller) ensureSuppressRule(ctx context.Context) {
	if cur, err := c.Runner.Run(ctx, "ip", "-4", "rule", "show"); err == nil && strings.Contains(cur, "suppress_prefixlength 0") {
		return
	}
	c.Runner.Run(ctx, "ip", "-4", "rule", "add", "lookup", "main", "suppress_prefixlength", "0", "priority", fmt.Sprint(suppressPriority))
}

// ensureRule makes the packets that carry a connection's mark use its table.
func (c *Controller) ensureRule(ctx context.Context, id string) {
	table := fmt.Sprint(tableOf(id))
	if cur, err := c.Runner.Run(ctx, "ip", "-4", "rule", "show"); err == nil && strings.Contains(cur, "lookup "+table) {
		return
	}
	c.Runner.Run(ctx, "ip", "-4", "rule", "add", "fwmark", fmt.Sprintf("0x%x/0x%x", markOf(id), MarkMask), "lookup", table, "priority", fmt.Sprint(rulePriority+tableOf(id)-100))
}

// cleanup takes away what syncBalance made (the second connection is gone).
func (c *Controller) cleanup(ctx context.Context) {
	c.Runner.Run(ctx, "nft", "delete", "table", "inet", LBTable)
	c.Runner.Run(ctx, "ip", "-4", "rule", "del", "lookup", "main", "suppress_prefixlength", "0", "priority", fmt.Sprint(suppressPriority))
	for _, id := range []string{config.WanFirst, config.WanSecond} {
		table := fmt.Sprint(tableOf(id))
		c.Runner.Run(ctx, "ip", "-4", "rule", "del", "fwmark", fmt.Sprintf("0x%x/0x%x", markOf(id), MarkMask), "lookup", table)
		c.Runner.Run(ctx, "ip", "-4", "route", "flush", "table", table)
	}
	c.mu.Lock()
	c.lbApplied, c.managed = "", false
	c.mu.Unlock()
}

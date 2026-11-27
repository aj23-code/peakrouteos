package config

import (
	"fmt"
)

// Per-network priorities and limits (docs/sqm.md, "Per network"). They live on top of smart queue management: the shaped line is divided
// between the networks, each getting at least a share by its priority (and being served first by it), and none above its limit.

// Priorities of a network.
const (
	QoSLow    = "low"
	QoSNormal = "normal"
	QoSHigh   = "high"
)

// QoSPriorities lists them, lowest first.
var QoSPriorities = []string{QoSLow, QoSNormal, QoSHigh}

// qosWeight is the share of the line a network is assured when the line is full, relative to the others: high 4, normal 2, low 1.
var qosWeight = map[string]int{QoSLow: 1, QoSNormal: 2, QoSHigh: 4}

// qosPrio is the order the queue serves them in (HTB's: lower is first).
var qosPrio = map[string]int{QoSHigh: 0, QoSNormal: 1, QoSLow: 2}

// NetQoS is what a network is set to: its priority, and the most it may use in each direction (0: no limit).
type NetQoS struct {
	Priority      string
	LimitDownKbit int
	LimitUpKbit   int
}

// IsDefault reports whether the network is left as it is by default (normal priority, no limits).
func (q NetQoS) IsDefault() bool {
	return (q.Priority == "" || q.Priority == QoSNormal) && q.LimitDownKbit == 0 && q.LimitUpKbit == 0
}

// ValidQoSPriority reports whether s is one of the priorities (empty counts: it is normal).
func ValidQoSPriority(s string) bool { return s == "" || s == QoSLow || s == QoSNormal || s == QoSHigh }

// NetworkQoS returns the priority and limits of a network (the default network is "lan").
func (c NetworkConfig) NetworkQoS(id string) NetQoS {
	var q NetQoS
	if id == DefaultNetworkID {
		q = NetQoS{c.LanQoSPriority, c.LanQoSLimitDownKbit, c.LanQoSLimitUpKbit}
	} else if n, ok := c.FindNetwork(id); ok {
		q = NetQoS{n.QoSPriority, n.QoSLimitDownKbit, n.QoSLimitUpKbit}
	}
	if q.Priority == "" {
		q.Priority = QoSNormal
	}
	return q
}

// SetNetworkQoS sets them; normal priority is not written.
func (c *NetworkConfig) SetNetworkQoS(id string, q NetQoS) {
	if q.Priority == QoSNormal {
		q.Priority = ""
	}
	if id == DefaultNetworkID {
		c.LanQoSPriority, c.LanQoSLimitDownKbit, c.LanQoSLimitUpKbit = q.Priority, q.LimitDownKbit, q.LimitUpKbit
		return
	}
	for i := range c.Networks {
		if c.Networks[i].ID == id {
			c.Networks[i].QoSPriority, c.Networks[i].QoSLimitDownKbit, c.Networks[i].QoSLimitUpKbit = q.Priority, q.LimitDownKbit, q.LimitUpKbit
		}
	}
}

// QoSSet reports whether any network is set to something other than the default.
func (c NetworkConfig) QoSSet() bool {
	if !c.NetworkQoS(DefaultNetworkID).IsDefault() {
		return true
	}
	for _, n := range c.Networks {
		if !c.NetworkQoS(n.ID).IsDefault() {
			return true
		}
	}
	return false
}

// QoSActive reports whether the line is divided between the networks: shaping is on and some network has a priority or a limit.
func (c NetworkConfig) QoSActive() bool { return c.SQMActive() && c.QoSSet() }

// QoSAnyActive reports whether any internet connection is divided between the networks.
func (c NetworkConfig) QoSAnyActive() bool { return c.SQMAnyActive() && c.QoSSet() }

// QoSClass is one share of the line: a network (or "other": what belongs to none, such as the router's own traffic).
type QoSClass struct {
	ID       string // the network's id, or "other"
	Name     string
	Index    int // 0 for other; the default network is 1, then the added ones in order: its mark is Index << 16
	Priority string
	Prio     int // the queue's order (0 first)
	// Rates and ceilings in kbit/s, per direction; 0 where that direction is not shaped
	UpRate, UpCeil, DownRate, DownCeil int
}

// QoSClasses divides the shaped line between the networks. A network is assured (when the line is full) a share by its weight, never more
// than its limit; it may use more when others leave the line idle, up to its limit (or the whole line without one).
func (c NetworkConfig) QoSClasses() []QoSClass {
	s := c.SQMSettings()
	up, down := s.ShapedUpKbit(), s.ShapedDownKbit()
	type net struct {
		id, name string
	}
	nets := []net{{DefaultNetworkID, "LAN"}}
	for _, n := range c.Networks {
		nets = append(nets, net{n.ID, n.Name})
	}
	classes := make([]QoSClass, 0, len(nets)+1)
	classes = append(classes, QoSClass{ID: "other", Name: "everything else", Index: 0, Priority: QoSNormal, Prio: qosPrio[QoSNormal]})
	for i, n := range nets {
		q := c.NetworkQoS(n.id)
		classes = append(classes, QoSClass{ID: n.id, Name: n.name, Index: i + 1, Priority: q.Priority, Prio: qosPrio[q.Priority]})
	}
	sum := 0
	for _, cl := range classes {
		sum += qosWeight[cl.Priority]
	}
	share := func(total, weight int) int {
		if v := total * weight / sum; v >= 1 {
			return v
		}
		return 1
	}
	ceil := func(total, limit int) int {
		if limit > 0 && limit < total {
			return limit
		}
		return total
	}
	for i := range classes {
		cl := &classes[i]
		q := NetQoS{}
		if cl.ID != "other" {
			q = c.NetworkQoS(cl.ID)
		}
		w := qosWeight[cl.Priority]
		if up > 0 {
			cl.UpCeil = ceil(up, q.LimitUpKbit)
			cl.UpRate = min(share(up, w), cl.UpCeil)
		}
		if down > 0 {
			cl.DownCeil = ceil(down, q.LimitDownKbit)
			cl.DownRate = min(share(down, w), cl.DownCeil)
		}
	}
	return classes
}

// validateQoS checks the priorities and limits; errors are reported on networks[i].qos_priority, networks[i].qos_limit_down_kbit,
// networks[i].qos_limit_up_kbit and lan_qos_... .
func (c NetworkConfig) validateQoS(add func(field, format string, args ...any)) {
	check := func(prefix string, q NetQoS) {
		if !ValidQoSPriority(q.Priority) {
			add(prefix+"qos_priority", "must be low, normal or high")
		}
		for _, l := range []struct {
			field string
			v     int
		}{{"qos_limit_down_kbit", q.LimitDownKbit}, {"qos_limit_up_kbit", q.LimitUpKbit}} {
			if l.v < 0 || l.v > MaxSQMKbit {
				add(prefix+l.field, "must be between 0 and %d Mbit/s (0: no limit)", MaxSQMKbit/1000)
			}
		}
	}
	check("lan_", NetQoS{c.LanQoSPriority, c.LanQoSLimitDownKbit, c.LanQoSLimitUpKbit})
	for i, n := range c.Networks {
		check(fmt.Sprintf("networks[%d].", i), NetQoS{n.QoSPriority, n.QoSLimitDownKbit, n.QoSLimitUpKbit})
	}
}

// QoSInactive says why a network's setting does nothing right now ("" when it does): the settings are kept while shaping is off, and a
// limit in a direction that is not shaped has nothing to limit.
func (c NetworkConfig) QoSInactive(id string) string {
	q := c.NetworkQoS(id)
	if q.IsDefault() {
		return ""
	}
	s := c.SQMSettings()
	switch {
	case !c.SQMActive():
		return "does nothing while smart queue management is off (Internet): the setting is kept"
	case q.LimitDownKbit > 0 && s.DownloadKbit == 0 && q.LimitUpKbit > 0 && s.UploadKbit == 0:
		return "the download and the upload limits do nothing: neither direction is shaped (Internet, smart queue management)"
	case q.LimitDownKbit > 0 && s.DownloadKbit == 0:
		return "the download limit does nothing: the download is not shaped (Internet, smart queue management)"
	case q.LimitUpKbit > 0 && s.UploadKbit == 0:
		return "the upload limit does nothing: the upload is not shaped (Internet, smart queue management)"
	}
	return ""
}

// QoSText describes a network's setting on one line for the review ("" for the default).
func (q NetQoS) QoSText() string {
	if q.IsDefault() {
		return ""
	}
	p := q.Priority
	if p == "" {
		p = QoSNormal
	}
	t := p + " priority"
	lim := func(k int) string {
		if k == 0 {
			return "no limit"
		}
		return fmt.Sprintf("%g Mbit/s", float64(k)/1000)
	}
	return fmt.Sprintf("%s, download limit %s, upload limit %s", t, lim(q.LimitDownKbit), lim(q.LimitUpKbit))
}

package network

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
)

// SQMDirection is how the shaping of one direction is doing: the cake queue as the kernel reports it.
type SQMDirection struct {
	Active        bool  `json:"active"`
	BandwidthKbit int64 `json:"bandwidth_kbit"`
	BytesSent     int64 `json:"bytes_sent"`
	Drops         int64 `json:"drops"`         // packets the queue dropped to keep the delay low (normal under load)
	BacklogBytes  int64 `json:"backlog_bytes"` // waiting in the queue now
	AvgDelayUS    int64 `json:"avg_delay_us"`  // the longest average wait of the queue's traffic classes
	PeakDelayUS   int64 `json:"peak_delay_us"`
}

// SQMStatus is the shaping of the internet connection: what goes out, and what comes in (through the ifb device).
type SQMStatus struct {
	Upload   SQMDirection `json:"upload"`
	Download SQMDirection `json:"download"`
}

// SQMIfb is the device what comes in is redirected to (image/overlay/usr/lib/peakroute/sqm).
const SQMIfb = "ifb-wan"

// SQMIfb2 is the second internet connection's.
const SQMIfb2 = "ifb-wan2"

// ParseSQMQdisc reads `tc -j -s qdisc show dev X` and returns the root cake queue; ok is false when the device has none.
func ParseSQMQdisc(out []byte) (SQMDirection, bool) {
	var qs []struct {
		Kind    string `json:"kind"`
		Root    bool   `json:"root"`
		Options struct {
			Bandwidth int64 `json:"bandwidth"` // bytes a second
		} `json:"options"`
		Bytes   int64 `json:"bytes"`
		Drops   int64 `json:"drops"`
		Backlog int64 `json:"backlog"`
		Tins    []struct {
			AvgDelay  int64 `json:"avg_delay_us"`
			PeakDelay int64 `json:"peak_delay_us"`
		} `json:"tins"`
	}
	if json.Unmarshal(out, &qs) != nil {
		return SQMDirection{}, false
	}
	// a line divided between the networks is an htb tree with a cake queue in each class: the answer is the sum of those
	tree := false
	var sum SQMDirection
	for _, q := range qs {
		switch {
		case q.Kind == "htb" && q.Root:
			tree = true
			sum.Active, sum.BytesSent = true, q.Bytes
		case q.Kind == "cake" && !q.Root:
			sum.Drops += q.Drops
			sum.BacklogBytes += q.Backlog
			for _, t := range q.Tins {
				if t.AvgDelay > sum.AvgDelayUS {
					sum.AvgDelayUS = t.AvgDelay
				}
				if t.PeakDelay > sum.PeakDelayUS {
					sum.PeakDelayUS = t.PeakDelay
				}
			}
		}
	}
	if tree {
		return sum, true // the whole line's speed is not in the queue's own report: the caller knows it
	}
	for _, q := range qs {
		if q.Kind != "cake" || !q.Root {
			continue
		}
		d := SQMDirection{Active: true, BandwidthKbit: q.Options.Bandwidth * 8 / 1000, BytesSent: q.Bytes, Drops: q.Drops, BacklogBytes: q.Backlog}
		for _, t := range q.Tins {
			if t.AvgDelay > d.AvgDelayUS {
				d.AvgDelayUS = t.AvgDelay
			}
			if t.PeakDelay > d.PeakDelayUS {
				d.PeakDelayUS = t.PeakDelay
			}
		}
		return d, true
	}
	return SQMDirection{}, false
}

// ReadSQMStatus asks the kernel how the shaping of the connection's device (and of its ifb) is doing. A device with no cake queue (or a
// machine with no tc) is simply not active.
func ReadSQMStatus(ctx context.Context, dev string) SQMStatus {
	return ReadSQMStatusOf(ctx, dev, SQMIfb)
}

// ReadSQMStatusOf is ReadSQMStatus for a connection whose ifb device is ifb.
func ReadSQMStatusOf(ctx context.Context, dev, ifb string) SQMStatus {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	read := func(d string) SQMDirection {
		if d == "" {
			return SQMDirection{}
		}
		out, err := exec.CommandContext(ctx, "tc", "-j", "-s", "qdisc", "show", "dev", d).Output()
		if err != nil {
			return SQMDirection{}
		}
		dir, _ := ParseSQMQdisc(out)
		return dir
	}
	return SQMStatus{Upload: read(dev), Download: read(ifb)}
}

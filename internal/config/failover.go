package config

import (
	"fmt"
	"time"
)

// Failover between two internet connections (docs/wan-failover.md, section 5). The preferred connection carries the traffic; when it is
// down the other one does, and the traffic goes back to the preferred one once it has been up for the recovery time.

// Limits and defaults.
const (
	DefaultRecoverySeconds = 120
	MinRecoverySeconds     = 10
	MaxRecoverySeconds     = 3600
)

// Weights of the two connections when the traffic is shared between them.
const (
	DefaultWeight = 1
	MinWeight     = 1
	MaxWeight     = 100
)

// FailoverConfig is how the two connections take over from each other, or share the traffic; every field has a default and nothing is
// written for them.
type FailoverConfig struct {
	NoFailback      bool `json:"no_failback,omitempty"`      // stay on the other connection after a failover until it fails (the default is to go back)
	RecoverySeconds int  `json:"recovery_seconds,omitempty"` // how long the preferred connection must be up before the traffic goes back; 0: the default
	// Balanced shares the traffic between both connections: every new connection from a network is given to one of them, by weight, and
	// stays on it. The default is failover: one carries the traffic and the other takes over when it is down.
	Balanced     bool `json:"balanced,omitempty"`
	WeightFirst  int  `json:"weight_first,omitempty"`  // 0: DefaultWeight
	WeightSecond int  `json:"weight_second,omitempty"` // 0: DefaultWeight
}

// FailoverSettings are the settings with the defaults filled in.
type FailoverSettings struct {
	Failback bool
	Recovery time.Duration
	Balanced bool
	Weights  map[string]int // by connection id
}

// FailoverSettings returns the failover settings.
func (c NetworkConfig) FailoverSettings() FailoverSettings {
	s := FailoverSettings{Failback: true, Recovery: DefaultRecoverySeconds * time.Second, Weights: map[string]int{WanFirst: DefaultWeight, WanSecond: DefaultWeight}}
	if f := c.WanFailover; f != nil {
		s.Failback = !f.NoFailback
		s.Balanced = f.Balanced
		if f.RecoverySeconds > 0 {
			s.Recovery = time.Duration(f.RecoverySeconds) * time.Second
		}
		if f.WeightFirst > 0 {
			s.Weights[WanFirst] = f.WeightFirst
		}
		if f.WeightSecond > 0 {
			s.Weights[WanSecond] = f.WeightSecond
		}
	}
	return s
}

// FailoverText describes the settings in a line, for the review of changes.
func (c NetworkConfig) FailoverText() string {
	s := c.FailoverSettings()
	if s.Balanced {
		return fmt.Sprintf("shares the traffic between both connections (weights %d and %d)", s.Weights[WanFirst], s.Weights[WanSecond])
	}
	if !s.Failback {
		return "stays on the other connection until it fails"
	}
	return fmt.Sprintf("goes back to the preferred connection after %d s", int(s.Recovery/time.Second))
}

func (f FailoverConfig) clone() FailoverConfig { return f }

// Balanced reports whether the traffic is shared between two connections (it needs two).
func (c NetworkConfig) Balanced() bool { return c.HasWan2() && c.FailoverSettings().Balanced }

func (c NetworkConfig) validateFailover(add func(field, format string, args ...any)) {
	f := c.WanFailover
	if f == nil {
		return
	}
	if f.RecoverySeconds != 0 && (f.RecoverySeconds < MinRecoverySeconds || f.RecoverySeconds > MaxRecoverySeconds) {
		add("wan_failover.recovery_seconds", "must be between %d and %d seconds", MinRecoverySeconds, MaxRecoverySeconds)
	}
	if f.WeightFirst != 0 && (f.WeightFirst < MinWeight || f.WeightFirst > MaxWeight) {
		add("wan_failover.weight_first", "must be between %d and %d", MinWeight, MaxWeight)
	}
	if f.WeightSecond != 0 && (f.WeightSecond < MinWeight || f.WeightSecond > MaxWeight) {
		add("wan_failover.weight_second", "must be between %d and %d", MinWeight, MaxWeight)
	}
}

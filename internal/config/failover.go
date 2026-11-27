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

// FailoverConfig is how the two connections take over from each other; every field has a default and nothing is written for them.
type FailoverConfig struct {
	NoFailback      bool `json:"no_failback,omitempty"`      // stay on the other connection after a failover until it fails (the default is to go back)
	RecoverySeconds int  `json:"recovery_seconds,omitempty"` // how long the preferred connection must be up before the traffic goes back; 0: the default
}

// FailoverSettings are the settings with the defaults filled in.
type FailoverSettings struct {
	Failback bool
	Recovery time.Duration
}

// FailoverSettings returns the failover settings.
func (c NetworkConfig) FailoverSettings() FailoverSettings {
	s := FailoverSettings{Failback: true, Recovery: DefaultRecoverySeconds * time.Second}
	if f := c.WanFailover; f != nil {
		s.Failback = !f.NoFailback
		if f.RecoverySeconds > 0 {
			s.Recovery = time.Duration(f.RecoverySeconds) * time.Second
		}
	}
	return s
}

// FailoverText describes the settings in a line, for the review of changes.
func (c NetworkConfig) FailoverText() string {
	s := c.FailoverSettings()
	if !s.Failback {
		return "stays on the other connection until it fails"
	}
	return fmt.Sprintf("goes back to the preferred connection after %d s", int(s.Recovery/time.Second))
}

func (f FailoverConfig) clone() FailoverConfig { return f }

func (c NetworkConfig) validateFailover(add func(field, format string, args ...any)) {
	f := c.WanFailover
	if f == nil {
		return
	}
	if f.RecoverySeconds != 0 && (f.RecoverySeconds < MinRecoverySeconds || f.RecoverySeconds > MaxRecoverySeconds) {
		add("wan_failover.recovery_seconds", "must be between %d and %d seconds", MinRecoverySeconds, MaxRecoverySeconds)
	}
}

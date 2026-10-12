package ping

import "time"

// Config contains the system:ping heartbeat settings.
type Config struct {
	// Interval is the period of the system:ping emission ticker. A
	// NON-POSITIVE value disables the event entirely (no emission at all),
	// which is the supported opt-out.
	Interval time.Duration
}

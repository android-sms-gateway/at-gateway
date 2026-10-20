package inbox

import "time"

// defaultReassemblyTimeout is the fallback for a non-positive
// ReassemblyTimeout. Five minutes tolerates a congested service centre whose
// concatenated parts arrive minutes apart, while still bounding the memory a
// never-completed message set can pin.
const defaultReassemblyTimeout = 5 * time.Minute

// defaultSweepInterval is the fallback for a non-positive SweepInterval: the
// reassembly/dedupe eviction cadence. One second preserves the cadence this
// sweep had while it rode the messages poll ticker.
const defaultSweepInterval = time.Second

// Config holds the inbound SMS pipeline settings: the reassembly window and the
// eviction cadence that drives it.
//
// SweepInterval is ONE OF TWO unrelated sweep knobs in at-gateway and must not
// be confused with the other: it paces the reassembly-group and dedupe
// EVICTION below (INBOX__SWEEP_INTERVAL). The modem retained-entry RECOVERY
// sweep - which lists the modem store with AT+CMGL=4 and re-feeds entries that
// were stored but never re-notified - is paced by MODEM__STORE__SWEEP_INTERVAL
// (koanf modem.store.sweep_interval) and lives entirely in the modem module.
type Config struct {
	// ReassemblyTimeout is how long the parts of one concatenated message may
	// stay incomplete before the group is evicted; zero falls back to five
	// minutes. An evicted group is never emitted as a partial message.
	ReassemblyTimeout time.Duration

	// SweepInterval is the period of the reassembly/dedupe eviction sweep; zero
	// falls back to one second.
	SweepInterval time.Duration
}

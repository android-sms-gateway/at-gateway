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

// defaultRetention is the fallback for a non-positive Retention: the
// age-based retention WINDOW of the stored rows, thirty days - the same
// MaxAge the server's inbox:cleanup task uses. The config documents the
// numeric-hour form (720h): the duration parser accepts "720h" but NOT "7d".
const defaultRetention = 720 * time.Hour

// defaultCleanupInterval is the fallback for a non-positive CleanupInterval:
// the CADENCE of the age-based retention cleanup, 24h - the same interval the
// server's inbox:cleanup task uses.
const defaultCleanupInterval = 24 * time.Hour

// inboundStoreTimeout bounds ONE insert into the local inbox store. The
// SQLite connection is shared by every writer (a single connection), and the
// inbound pipeline has exactly ONE consumer, so an unbounded insert could
// stall the consumer loop until the modem hand-off budget sheds the message.
// The bound turns a stuck write into a counted store failure.
const inboundStoreTimeout = 2 * time.Second

// inboxCleanupTimeout bounds one retention delete on the shared SQLite
// connection so a slow delete cannot stall the single inbound consumer.
const inboxCleanupTimeout = 10 * time.Second

// Config holds the inbound SMS pipeline settings: the reassembly window, the
// eviction cadence that drives it, and the age-based retention of the stored
// rows with its cleanup cadence.
//
// FOUR unrelated knobs across at-gateway must not be confused:
//
//   - SweepInterval paces this module's reassembly-group and dedupe EVICTION
//     below;
//   - Retention is the age-based retention WINDOW of the stored rows and
//     CleanupInterval the CADENCE of that cleanup - neither is a sweep;
//   - the modem retained-entry RECOVERY sweep - which lists the modem store
//     with AT+CMGL=4 and re-feeds entries that were stored but never
//     re-notified - is paced by its own knob, lives entirely in the modem
//     module and is unrelated to any field here.
type Config struct {
	// ReassemblyTimeout is how long the parts of one concatenated message may
	// stay incomplete before the group is evicted; zero falls back to five
	// minutes. An evicted group is never emitted as a partial message.
	ReassemblyTimeout time.Duration

	// SweepInterval is the period of the reassembly/dedupe eviction sweep; zero
	// falls back to one second.
	SweepInterval time.Duration

	// Retention is the age-based retention WINDOW of the stored rows: a
	// cleanup pass deletes every row with received_at strictly older than
	// now-Retention (the cutoff is EXCLUSIVE); non-positive falls back to
	// 720h (30 days).
	Retention time.Duration

	// CleanupInterval is the period of that age-based retention cleanup - the
	// SECOND ticker of the Run loop, on the SAME goroutine and never a second
	// consumer of the modem hand-off channel; non-positive falls back to 24h.
	CleanupInterval time.Duration
}

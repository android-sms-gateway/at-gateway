package messages

import "time"

// defaultRegion is the default region used to interpret phone numbers
// without an international prefix (see Config.DefaultRegion).
const defaultRegion = "RU"

// defaultInboundReassemblyTimeout is the fallback for a non-positive
// Inbound.ReassemblyTimeout. Five minutes tolerates a congested service
// centre whose concatenated parts arrive minutes apart, while still bounding
// the memory a never-completed message set can pin.
const defaultInboundReassemblyTimeout = 5 * time.Minute

// InboundConfig bounds the inbound SMS reassembly.
type InboundConfig struct {
	// ReassemblyTimeout is how long the parts of one concatenated message may
	// stay incomplete before the group is evicted; zero falls back to five
	// minutes. An evicted group is never emitted as a partial message.
	ReassemblyTimeout time.Duration
}

// Config holds the messages service and background worker configuration.
type Config struct {
	// PollInterval is how long the worker sleeps after finding an empty
	// queue before polling again; zero falls back to one second.
	PollInterval time.Duration

	// MaxSegments caps the number of concatenated SMS parts a text may be
	// split into; a text that needs more parts is rejected before the modem
	// traffic. Zero disables the cap (the 255-part protocol ceiling always
	// applies).
	MaxSegments int

	// DefaultRegion is the ISO 3166-1 alpha-2 country code used to parse
	// phone numbers without an international prefix during E.164
	// validation; empty falls back to "RU".
	DefaultRegion string

	// Inbound bounds the reassembly of received multi-part messages.
	Inbound InboundConfig
}

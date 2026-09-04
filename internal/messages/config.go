package messages

import "time"

// defaultRegion is the default region used to interpret phone numbers
// without an international prefix (see Config.DefaultRegion).
const defaultRegion = "RU"

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
}

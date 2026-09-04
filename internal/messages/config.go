package messages

import "time"

// maxSegmentsDefault is the default cap on the number of concatenated SMS
// parts a single text may occupy. Carriers commonly limit multi-part
// messages well below the protocol ceiling of 255 parts.
const maxSegmentsDefault = 10

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
}

// Default returns the default messages configuration.
func Default() Config {
	return Config{
		PollInterval: time.Second,
		MaxSegments:  maxSegmentsDefault,
	}
}

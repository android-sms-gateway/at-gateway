package messages

import "time"

// Config holds the messages service and background worker configuration.
type Config struct {
	// PollInterval is how long the worker sleeps after finding an empty
	// queue before polling again; zero falls back to one second.
	PollInterval time.Duration `koanf:"poll_interval"`
}

// Default returns the default messages configuration.
func Default() Config {
	return Config{
		PollInterval: time.Second,
	}
}

package messages

import "time"

// Config holds the messages service and background worker configuration.
type Config struct {
	// PollInterval is how long the worker sleeps after finding an empty
	// queue before polling again; zero falls back to one second.
	PollInterval time.Duration `koanf:"poll_interval"`
	// DeviceID is stored in the messages.device_id column. The default is
	// "default": the MVP is single-device (deviceId on requests is ignored)
	// and the devices registry is a separate lazy module not wired to
	// messages. Env keys are mapped in a later config-polish wave.
	DeviceID string `koanf:"device_id"`
}

// Default returns the default messages configuration.
func Default() Config {
	return Config{
		PollInterval: time.Second,
		DeviceID:     "default",
	}
}

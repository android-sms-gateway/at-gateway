package modem

import "time"

// CmgdConfig is the retry policy for the AT+CMGD command that removes a
// message from the modem store after it was read.
//
// RetryAttempts is the TOTAL number of attempts, not the number of retries
// after the first one: 1 disables retrying. RetryBackoff is the base sleep
// applied EXPONENTIALLY between attempts (2s, 4s, 8s, ...), so the defaults
// (3 attempts, 2s base) spend 6s sleeping. Every attempt additionally costs
// up to the effective command timeout, so the worst-case wall clock per
// message is attempts x cmdTimeout + the sleep budget (3 x 30s + 6s = ~96s at
// the 30s default command timeout).
type CmgdConfig struct {
	RetryAttempts int
	RetryBackoff  time.Duration
}

type Config struct {
	Port           string
	BaudRate       int
	InitTimeout    time.Duration
	CommandTimeout time.Duration

	// Cmgd bounds the store-deletion retries (see CmgdConfig).
	Cmgd CmgdConfig
}

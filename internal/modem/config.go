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

// StoreConfig configures the modem message-store handling.
//
// Memory is the preferred message storage (+CPMS <mem1>) the gateway falls
// back to before the first +CMTI names a store; the store a given message was
// filed in is selected per operation, so this is only the initial/default
// value and the FIRST store the retained-entry sweep lists. Every store a
// +CMTI has named since is swept too, so an entry stays recoverable in the
// store it was filed in.
//
// SweepInterval is the period of the retained-entry sweep (AT+CMGL=4) that
// recovers entries stored but never re-notified. Zero disables the sweep.
type StoreConfig struct {
	Memory        string
	SweepInterval time.Duration
}

type Config struct {
	Port           string
	BaudRate       int
	InitTimeout    time.Duration
	CommandTimeout time.Duration

	// Cmgd bounds the store-deletion retries (see CmgdConfig).
	Cmgd CmgdConfig

	// Store configures the preferred message storage and the retained-entry
	// sweep (see StoreConfig).
	Store StoreConfig
}

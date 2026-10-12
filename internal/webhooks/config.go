package webhooks

import "time"

// Config contains the webhook delivery and queue settings.
type Config struct {
	RetryCount int
	SigningKey string
	Queue      QueueConfig
}

// QueueConfig contains the webhook queue worker settings.
type QueueConfig struct {
	BatchSize              int
	RequestTimeout         time.Duration
	DialTimeout            time.Duration
	RetryBaseDelay         time.Duration
	IdleDelay              time.Duration
	StuckProcessingTimeout time.Duration
	CleanupRetention       time.Duration
}

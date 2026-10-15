package webhooks

import (
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
)

// Webhook is the registry domain entity stored in the webhooks table. The
// service is the sole ID generator (nanoid), so the repository only carries
// IDs through. DeviceID is the scoped device id: nil means registered for
// ALL devices (SQL NULL) and never defaults to the local device; a non-nil
// value must equal the local device id (validated by the service).
type Webhook struct {
	ID       string
	DeviceID *string
	URL      string
	Event    smsgateway.WebhookEvent
}

// QueueStatus is the lifecycle state of a webhook queue item.
type QueueStatus string

const (
	QueueStatusPending           QueueStatus = "pending"
	QueueStatusProcessing        QueueStatus = "processing"
	QueueStatusCompleted         QueueStatus = "completed"
	QueueStatusFailed            QueueStatus = "failed"
	QueueStatusPermanentlyFailed QueueStatus = "permanently_failed"
)

// QueueItem is a queued webhook delivery. CreatedAt and NextAttempt are
// persisted as SQLite DATETIME values and are truncated to microseconds by
// the driver on round-trip. LastError is nil until a delivery attempt fails.
type QueueItem struct {
	ID          string
	WebhookID   string
	URL         string
	Payload     string
	RetryCount  int
	Status      QueueStatus
	CreatedAt   time.Time
	NextAttempt time.Time
	LastError   *string
}

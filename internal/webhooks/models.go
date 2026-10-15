package webhooks

import (
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
)

type webhookModel struct {
	bun.BaseModel `bun:"table:webhooks,alias:w"`
	db.TimedModel

	ID       int64                   `bun:"id,pk,autoincrement"`
	ExtID    string                  `bun:"ext_id,notnull"`
	DeviceID *string                 `bun:"device_id"`
	URL      string                  `bun:"url,notnull"`
	Event    smsgateway.WebhookEvent `bun:"event,notnull"`
}

// newWebhookModel maps a domain webhook onto the persisted model. The caller
// (service) generates ExtID; timestamps are stamped with now.
func newWebhookModel(w Webhook, now time.Time) webhookModel {
	return webhookModel{
		BaseModel: bun.BaseModel{},
		TimedModel: db.TimedModel{
			CreatedAt: now,
			UpdatedAt: now,
		},
		ID:       0,
		ExtID:    w.ID,
		DeviceID: w.DeviceID,
		URL:      w.URL,
		Event:    w.Event,
	}
}

// toDomain maps the persisted model onto the domain webhook; a SQL NULL
// device_id round-trips as DeviceID nil (registered for all devices).
func (m *webhookModel) toDomain() Webhook {
	return Webhook{
		ID:       m.ExtID,
		DeviceID: m.DeviceID,
		URL:      m.URL,
		Event:    m.Event,
	}
}

type queueItemModel struct {
	bun.BaseModel `bun:"table:webhook_queue,alias:q"`

	ID          string      `bun:"id,pk"`
	WebhookID   string      `bun:"webhook_id"`
	URL         string      `bun:"url,notnull"`
	Payload     string      `bun:"payload,notnull"`
	RetryCount  int         `bun:"retry_count,notnull"`
	Status      QueueStatus `bun:"status,notnull"`
	CreatedAt   time.Time   `bun:"created_at"`
	NextAttempt time.Time   `bun:"next_attempt"`
	LastError   *string     `bun:"last_error"`
}

// newQueueItemModel maps a queue domain item onto the persisted model. The
// service supplies the item ID and timestamps; the repository only persists
// the values it receives.
func newQueueItemModel(item QueueItem) queueItemModel {
	return queueItemModel{
		BaseModel:   bun.BaseModel{},
		ID:          item.ID,
		WebhookID:   item.WebhookID,
		URL:         item.URL,
		Payload:     item.Payload,
		RetryCount:  item.RetryCount,
		Status:      item.Status,
		CreatedAt:   item.CreatedAt,
		NextAttempt: item.NextAttempt,
		LastError:   item.LastError,
	}
}

// toDomain maps the persisted queue model onto the domain item, preserving a
// SQL NULL last_error as a nil pointer.
func (m *queueItemModel) toDomain() QueueItem {
	return QueueItem{
		ID:          m.ID,
		WebhookID:   m.WebhookID,
		URL:         m.URL,
		Payload:     m.Payload,
		RetryCount:  m.RetryCount,
		Status:      m.Status,
		CreatedAt:   m.CreatedAt,
		NextAttempt: m.NextAttempt,
		LastError:   m.LastError,
	}
}

package inbox

import (
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
)

// inboxModel is the persisted form of an inbound message.
type inboxModel struct {
	bun.BaseModel `bun:"table:inbox,alias:i"`
	db.TimedModel

	ID        int64  `bun:"id,pk,autoincrement"`
	MessageID string `bun:"message_id,notnull"`
	DeviceID  string `bun:"device_id,notnull"`

	Type    smsgateway.IncomingMessageType `bun:"type,notnull"`
	Sender  string                         `bun:"sender,notnull"`
	Content string                         `bun:"content,notnull"`

	Recipient *string `bun:"recipient"`
	SimNumber *uint8  `bun:"sim_number"`

	ReceivedAt time.Time `bun:"received_at,notnull"`
}

// newInboundModel maps a domain message onto the persisted model. The row
// timestamps are the insertion time; the received stamp is the message's own
// one and never changes.
func newInboundModel(message IncomingMessage, now time.Time) *inboxModel {
	return &inboxModel{
		BaseModel: bun.BaseModel{},
		TimedModel: db.TimedModel{
			CreatedAt: now,
			UpdatedAt: now,
		},
		ID:         0,
		MessageID:  message.ID,
		DeviceID:   message.DeviceID,
		Type:       message.Type,
		Sender:     message.Sender,
		Recipient:  message.Recipient,
		SimNumber:  message.SimNumber,
		Content:    message.Content,
		ReceivedAt: message.ReceivedAt,
	}
}

// toDomain maps the persisted model onto the domain message.
func (m *inboxModel) toDomain() IncomingMessage {
	return IncomingMessage{
		ID:         m.MessageID,
		DeviceID:   m.DeviceID,
		Type:       m.Type,
		Sender:     m.Sender,
		Recipient:  m.Recipient,
		SimNumber:  m.SimNumber,
		Content:    m.Content,
		ReceivedAt: m.ReceivedAt,
	}
}

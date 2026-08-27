package messages

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/samber/lo"
	"github.com/uptrace/bun"
)

type optionsModel struct {
	SimNumber          *uint8 `json:"sim_number,omitempty"`
	WithDeliveryReport *bool  `json:"with_delivery_report,omitempty"`
}

type stateModel struct {
	State smsgateway.ProcessingState `json:"state"`
	At    time.Time                  `json:"at"`
}

func (s stateModel) String() string {
	return string(lo.Must(json.Marshal(s)))
}

type statesModel []stateModel

// toDomainMap collapses the ordered state history onto the domain map; the
// FIRST occurrence per state wins.
func (s statesModel) toDomainMap() map[string]time.Time {
	states := make(map[string]time.Time, len(s))
	for _, entry := range s {
		if _, seen := states[string(entry.State)]; seen {
			continue
		}
		states[string(entry.State)] = entry.At
	}

	return states
}

func (s statesModel) Latest() *stateModel {
	if len(s) == 0 {
		return nil
	}
	return &s[len(s)-1]
}

type messageModel struct {
	bun.BaseModel `bun:"table:messages,alias:m"`
	db.TimedModel

	ID       int64  `bun:"id,pk,autoincrement"`
	ExtID    string `bun:"ext_id,notnull"`
	DeviceID string `bun:"device_id,notnull"`

	Type    ContentType `bun:"type,notnull"`
	Content string      `bun:"content,notnull"`

	Priority    int8         `bun:"priority,notnull"`
	IsEncrypted bool         `bun:"is_encrypted,notnull"`
	ValidUntil  *time.Time   `bun:"valid_until"`
	ScheduleAt  *time.Time   `bun:"schedule_at"`
	Options     optionsModel `bun:"options,notnull"`

	State    smsgateway.ProcessingState `bun:"state,notnull"`
	States   statesModel                `bun:"states,notnull"`
	IsHashed bool                       `bun:"is_hashed,notnull"`

	Recipients []recipientModel `bun:"rel:has-many,join:id=message_id"`
}

// newMessageModel maps a domain message onto the persisted model. The states
// history is initialized with a single entry that matches the state column
// (the sync invariant), and the message timestamps are set to now. ExtID is
// required input - the service generates it before Create.
func newMessageModel(msg *MessageInput, now time.Time) (*messageModel, error) {
	contentType, content, err := msg.Content()
	if err != nil {
		return nil, err
	}

	model := &messageModel{
		BaseModel: bun.BaseModel{},
		TimedModel: db.TimedModel{
			CreatedAt: now,
			UpdatedAt: now,
		},
		ID:          0,
		ExtID:       msg.ExtID,
		DeviceID:    *msg.DeviceID,
		Type:        contentType,
		Content:     content,
		Priority:    int8(msg.Priority),
		IsEncrypted: msg.IsEncrypted,
		IsHashed:    false,
		State:       smsgateway.ProcessingStatePending,
		Options: optionsModel{
			SimNumber:          msg.SimNumber,
			WithDeliveryReport: msg.WithDeliveryReport,
		},
		ValidUntil: msg.ValidUntil,
		ScheduleAt: msg.ScheduleAt,
		States: statesModel{
			{State: smsgateway.ProcessingStatePending, At: now},
		},
		Recipients: nil,
	}

	return model, nil
}

// messageContent maps the type/content columns onto the domain content. Text
// and data bodies are stored as JSON of the wire DTOs; a hashed message
// stores the hash verbatim (matching the server convention).
func (m *messageModel) messageContent() (MessageStateContent, error) {
	if m.IsHashed {
		return MessageStateContent{
			MessageContent: MessageContent{
				TextContent:       nil,
				DataContent:       nil,
				MultimediaContent: nil,
			},
			HashedContent: &smsgateway.HashedMessage{Hash: m.Content},
		}, nil
	}

	switch m.Type {
	case ContentTypeText:
		text := new(smsgateway.TextMessage)
		if err := json.Unmarshal([]byte(m.Content), text); err != nil {
			return MessageStateContent{}, fmt.Errorf("unmarshal text content: %w", err)
		}

		return MessageStateContent{
			MessageContent: MessageContent{
				TextContent:       text,
				DataContent:       nil,
				MultimediaContent: nil,
			},
			HashedContent: nil,
		}, nil
	case ContentTypeData:
		data := new(smsgateway.DataMessage)
		if err := json.Unmarshal([]byte(m.Content), data); err != nil {
			return MessageStateContent{}, fmt.Errorf("unmarshal data content: %w", err)
		}

		return MessageStateContent{
			MessageContent: MessageContent{
				TextContent:       nil,
				DataContent:       data,
				MultimediaContent: nil,
			},
			HashedContent: nil,
		}, nil
	default:
		return MessageStateContent{}, fmt.Errorf("%w: unsupported message type %q", ErrInvalidContent, m.Type)
	}
}

// toDomain maps the persisted message model onto the domain message. The
// device ID is not persisted and must be supplied by the caller.
func (m *messageModel) toDomain() (*Message, error) {
	content, err := m.messageContent()
	if err != nil {
		return nil, err
	}

	var ttl *uint64
	if m.ValidUntil != nil {
		remaining := max(m.ValidUntil.Sub(m.CreatedAt), 0)
		ttl = lo.ToPtr(uint64(remaining / time.Second))
	}

	message := &Message{
		MessageStateContent: content,
		MessageOptions: MessageOptions{
			SimNumber:          m.Options.SimNumber,
			WithDeliveryReport: m.Options.WithDeliveryReport,
			TTL:                ttl,
			ValidUntil:         m.ValidUntil,
			ScheduleAt:         m.ScheduleAt,
			Priority:           smsgateway.MessagePriority(m.Priority),
		},
		ID:    m.ExtID,
		State: m.State,
		Recipients: lo.Map(m.Recipients, func(recipient recipientModel, _ int) Recipient {
			return recipient.toDomain()
		}),
		States:      m.States.toDomainMap(),
		DeviceID:    m.DeviceID,
		IsHashed:    m.IsHashed,
		IsEncrypted: m.IsEncrypted,
	}

	return message, nil
}

type recipientModel struct {
	bun.BaseModel `bun:"table:message_recipients,alias:mr"`

	ID        int64       `bun:"id,pk,autoincrement"`
	RefID     *int        `bun:"ref_id"`
	MessageID int64       `bun:"message_id,notnull,unique:msg_phone"`
	Phone     string      `bun:"phone,notnull,unique:msg_phone"`
	States    statesModel `bun:"states,notnull"`
	Error     *string     `bun:"error"`
}

// newRecipientModels maps the domain recipients onto persisted models; every
// recipient history starts with a single entry matching its current state.
func newRecipientModels(recipients []string, messageID int64, now time.Time) []recipientModel {
	models := make([]recipientModel, 0, len(recipients))
	for _, recipient := range recipients {
		models = append(models, recipientModel{
			BaseModel: bun.BaseModel{},
			ID:        0,
			RefID:     nil,
			MessageID: messageID,
			Phone:     recipient,
			States: statesModel{
				{State: smsgateway.ProcessingStatePending, At: now},
			},
			Error: nil,
		})
	}

	return models
}

// toDomain maps a persisted recipient onto the domain recipient; the current
// state is the last entry of the states history.
func (m *recipientModel) toDomain() Recipient {
	state := smsgateway.ProcessingState("")
	if latest := m.States.Latest(); latest != nil {
		state = latest.State
	}

	recipient := Recipient{
		PhoneNumber: m.Phone,
		State:       state,
		States:      make([]StateChange, 0, len(m.States)),
		RefID:       m.RefID,
		Error:       m.Error,
	}
	for _, entry := range m.States {
		recipient.States = append(recipient.States, StateChange(entry))
	}

	return recipient
}

package messages

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

// State is the processing state of a message; spellings match
// client-go smsgateway.ProcessingState.
type State string

const (
	StatePending   State = "Pending"
	StateSent      State = "Sent"
	StateFailed    State = "Failed"
	StateCancelled State = "Cancelled"
)

// SortOrder is the created_at ordering applied by Repository.List; the zero
// value behaves as ascending.
type SortOrder string

const (
	SortAsc  SortOrder = "asc"
	SortDesc SortOrder = "desc"
)

// StateChange is one entry of the message state audit trail persisted in
// states_json.
type StateChange struct {
	State State     `json:"state"`
	At    time.Time `json:"at"`
}

// Message is the domain representation of a persisted SMS message.
type Message struct {
	ID                 string
	DeviceID           string
	State              State
	IsHashed           bool
	IsEncrypted        bool
	TextMessage        string
	SimNumber          *int
	WithDeliveryReport bool
	Priority           int
	Recipients         []string
	States             []StateChange
	ErrorMessage       *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ProcessedAt        *time.Time
	SentAt             *time.Time
	FailedAt           *time.Time
}

// ListFilter selects the page and ordering for Repository.List. Limit <= 0
// means unbounded; Offset beyond the result set yields an empty page.
type ListFilter struct {
	Limit  int
	Offset int
	State  *State
	Order  SortOrder
}

// messageModel maps the messages table; JSON columns stay opaque strings at
// this layer and are decoded in toDomain.
type messageModel struct {
	bun.BaseModel `bun:"table:messages"`

	ID                 string     `bun:"id,pk"`
	DeviceID           string     `bun:"device_id,notnull"`
	State              string     `bun:"state,notnull"`
	IsHashed           bool       `bun:"is_hashed,notnull"`
	IsEncrypted        bool       `bun:"is_encrypted,notnull"`
	TextMessage        string     `bun:"text_message,notnull"`
	SimNumber          *int       `bun:"sim_number"`
	WithDeliveryReport bool       `bun:"with_delivery_report,notnull"`
	Priority           int        `bun:"priority,notnull"`
	RecipientsJSON     string     `bun:"recipients_json,notnull"`
	StatesJSON         string     `bun:"states_json,notnull"`
	ErrorMessage       *string    `bun:"error_message"`
	CreatedAt          time.Time  `bun:"created_at,nullzero,notnull"`
	UpdatedAt          time.Time  `bun:"updated_at,nullzero,notnull"`
	ProcessedAt        *time.Time `bun:"processed_at"`
	SentAt             *time.Time `bun:"sent_at"`
	FailedAt           *time.Time `bun:"failed_at"`
}

func (m *messageModel) toDomain() (*Message, error) {
	recipients := make([]string, 0)
	if err := json.Unmarshal([]byte(m.RecipientsJSON), &recipients); err != nil {
		return nil, fmt.Errorf("unmarshal recipients: %w", err)
	}

	states := make([]StateChange, 0)
	if err := json.Unmarshal([]byte(m.StatesJSON), &states); err != nil {
		return nil, fmt.Errorf("unmarshal states: %w", err)
	}

	return &Message{
		ID:                 m.ID,
		DeviceID:           m.DeviceID,
		State:              State(m.State),
		IsHashed:           m.IsHashed,
		IsEncrypted:        m.IsEncrypted,
		TextMessage:        m.TextMessage,
		SimNumber:          m.SimNumber,
		WithDeliveryReport: m.WithDeliveryReport,
		Priority:           m.Priority,
		Recipients:         recipients,
		States:             states,
		ErrorMessage:       m.ErrorMessage,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
		ProcessedAt:        m.ProcessedAt,
		SentAt:             m.SentAt,
		FailedAt:           m.FailedAt,
	}, nil
}

func newModel(msg *Message) (*messageModel, error) {
	recipientsJSON, err := json.Marshal(msg.Recipients)
	if err != nil {
		return nil, fmt.Errorf("marshal recipients: %w", err)
	}

	statesJSON, err := json.Marshal(msg.States)
	if err != nil {
		return nil, fmt.Errorf("marshal states: %w", err)
	}

	return &messageModel{
		BaseModel:          bun.BaseModel{},
		ID:                 msg.ID,
		DeviceID:           msg.DeviceID,
		State:              string(msg.State),
		IsHashed:           msg.IsHashed,
		IsEncrypted:        msg.IsEncrypted,
		TextMessage:        msg.TextMessage,
		SimNumber:          msg.SimNumber,
		WithDeliveryReport: msg.WithDeliveryReport,
		Priority:           msg.Priority,
		RecipientsJSON:     string(recipientsJSON),
		StatesJSON:         string(statesJSON),
		ErrorMessage:       msg.ErrorMessage,
		CreatedAt:          msg.CreatedAt,
		UpdatedAt:          msg.UpdatedAt,
		ProcessedAt:        msg.ProcessedAt,
		SentAt:             msg.SentAt,
		FailedAt:           msg.FailedAt,
	}, nil
}

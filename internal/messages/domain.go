package messages

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
)

// SortOrder is the created_at ordering applied by Repository.List; the zero
// value behaves as ascending.
type SortOrder string

const (
	SortAsc  SortOrder = "asc"
	SortDesc SortOrder = "desc"
)

type ContentType string

const (
	ContentTypeText ContentType = "Text"
	ContentTypeData ContentType = "Data"
)

type MessageContent struct {
	TextContent *smsgateway.TextMessage `json:"textContent,omitempty"`
	DataContent *smsgateway.DataMessage `json:"dataContent,omitempty"`
}

type MessageStateContent struct {
	MessageContent

	HashedContent *smsgateway.HashedMessage `json:"hashedContent,omitempty"`
}

type MessageOptions struct {
	SimNumber          *uint8
	WithDeliveryReport *bool
	TTL                *uint64
	ValidUntil         *time.Time
	ScheduleAt         *time.Time
	Priority           smsgateway.MessagePriority
}

type MessageInput struct {
	MessageContent
	MessageOptions

	ExtID    string
	DeviceID *string

	PhoneNumbers []string
	IsEncrypted  bool
}

// Content derives the type/content columns from the domain content.
func (m *MessageInput) Content() (ContentType, string, error) {
	switch {
	case m.TextContent != nil && m.DataContent != nil:
		return "", "", fmt.Errorf("%w: both text and data content are set", ErrInvalidContent)
	case m.TextContent != nil:
		content, err := json.Marshal(m.TextContent)
		if err != nil {
			return "", "", fmt.Errorf("marshal text content: %w", err)
		}

		return ContentTypeText, string(content), nil
	case m.DataContent != nil:
		content, err := json.Marshal(m.DataContent)
		if err != nil {
			return "", "", fmt.Errorf("marshal data content: %w", err)
		}

		return ContentTypeData, string(content), nil
	default:
		return "", "", fmt.Errorf("%w: message content is required", ErrInvalidContent)
	}
}

type Message struct {
	MessageStateContent
	MessageOptions

	ID         string
	State      smsgateway.ProcessingState
	Recipients []Recipient
	States     map[string]time.Time

	DeviceID    string
	IsHashed    bool
	IsEncrypted bool
}

// Recipient is one phone number of a message together with its own state
// history; the current state is the last entry of States.
type Recipient struct {
	PhoneNumber string
	State       smsgateway.ProcessingState
	States      []StateChange
	RefID       *int
	Error       *string
}

// StateChange is one ordered state-history entry; used for both message and
// recipient histories.
type StateChange struct {
	State smsgateway.ProcessingState
	At    time.Time
}

// ListFilter selects the page and ordering for Repository.List. Limit <= 0
// means unbounded; Offset beyond the result set yields an empty page.
type ListFilter struct {
	Limit  int
	Offset int
	State  *smsgateway.ProcessingState
	Order  SortOrder
}

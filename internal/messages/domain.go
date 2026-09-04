package messages

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
)

// SortOrder is the created_at ordering applied by Repository.List; the zero
// value behaves as ascending.
type SortOrder string

const (
	SortAsc  SortOrder = "created_at"
	SortDesc SortOrder = "-created_at"
)

type ContentType string

const (
	ContentTypeText ContentType = "Text"
	ContentTypeData ContentType = "Data"
)

type MessageContent struct {
	TextContent       *smsgateway.TextMessage `json:"textContent,omitempty"`
	DataContent       *smsgateway.DataMessage `json:"dataContent,omitempty"`
	MultimediaContent *smsgateway.MmsMessage  `json:"multimediaContent,omitempty"`
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

func (r Recipient) processable() bool {
	switch r.State {
	case smsgateway.ProcessingStatePending, smsgateway.ProcessingStateProcessed:
		return true
	case smsgateway.ProcessingStateCancelling,
		smsgateway.ProcessingStateCancelled,
		smsgateway.ProcessingStateSent,
		smsgateway.ProcessingStateDelivered,
		smsgateway.ProcessingStateFailed:
		return false
	default:
		panic("unreachable")
	}
}

// StateChange is one ordered state-history entry; used for both message and
// recipient histories.
type StateChange struct {
	State smsgateway.ProcessingState
	At    time.Time
}

type ListOptions struct {
	Filter     *ListFilter
	Order      *ListOrder
	Pagination *ListPagination
	Flags      *ListFlags
}

func (o *ListOptions) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if o == nil {
		return q
	}

	if o.Flags != nil {
		q = o.Flags.apply(q)
	}
	if o.Filter != nil {
		q = o.Filter.apply(q)
	}
	if o.Order != nil {
		q = o.Order.apply(q)
	}
	if o.Pagination != nil {
		q = o.Pagination.apply(q)
	}
	return q
}

type ListFilter struct {
	DeviceID *string
	State    *smsgateway.ProcessingState

	Since *time.Time
	Until *time.Time
}

func (f *ListFilter) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if f == nil {
		return q
	}

	if f.State != nil {
		q = q.Where("state = ?", string(*f.State))
	}

	if f.Since != nil {
		q = q.Where("created_at > ?", f.Since)
	}
	if f.Until != nil {
		q = q.Where("created_at <= ?", f.Until)
	}

	if f.DeviceID != nil {
		q = q.Where("device_id = ?", *f.DeviceID)
	}
	return q
}

type ListOrder struct {
	Order *SortOrder
}

func (o *ListOrder) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if o == nil {
		return q
	}

	if o.Order == nil || *o.Order == SortDesc {
		q = q.OrderExpr(orderDescending)
	} else {
		q = q.OrderExpr(orderAscending)
	}

	return q
}

type ListPagination struct {
	Limit  *int
	Offset *int
}

func (p *ListPagination) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if p == nil {
		return q
	}

	if p.Limit != nil {
		q = q.Limit(*p.Limit)
	}
	if p.Offset != nil {
		q = q.Offset(*p.Offset)
	}
	return q
}

type ListFlags struct {
	IncludeContent *bool
}

func (f *ListFlags) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if f == nil {
		return q
	}

	if f.IncludeContent == nil || !*f.IncludeContent {
		q = q.ExcludeColumn("content")
	}

	return q
}

package inbox

import (
	"fmt"
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
)

// IncomingMessage is one persisted inbound message: the durable record of what
// the modem received. The identity is the content-deterministic message id the
// webhook payload carries, so a consumer can correlate a listed row with the
// event it deduplicates on.
type IncomingMessage struct {
	// ID is the message identity (the webhook messageId), stored in the
	// message_id column.
	ID string
	// DeviceID is the local device the message was received by. It is a READ
	// column: the gateway is single-device and always lists its own rows.
	DeviceID string
	// Type is the message type: SMS or DATA_SMS. MMS is never stored.
	Type smsgateway.IncomingMessageType
	// Sender is the originating address (TP-OA).
	Sender string
	// Recipient is the SIM number that received the message; nil when the SIM
	// reports no MSISDN.
	Recipient *string
	// SimNumber is the receiving SIM index; at-gateway is single-SIM.
	SimNumber *uint8
	// Content is the WIRE-READY body: the assembled text for an SMS, the
	// base64 NO_WRAP encoding of the raw octets for a DATA_SMS.
	Content string
	// ReceivedAt is the service-centre stamp of the first part, or the
	// observation time when the modem reported no usable one.
	ReceivedAt time.Time
}

// ListOptions are the query options of a stored-message listing.
type ListOptions struct {
	// Filter narrows the rows; the device id is always set by the service.
	Filter *ListFilter
	// Pagination bounds the returned page.
	Pagination *ListPagination
}

func (o *ListOptions) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if o == nil {
		return q
	}

	if o.Filter != nil {
		q = o.Filter.apply(q)
	}
	if o.Pagination != nil {
		q = o.Pagination.apply(q)
	}

	return q
}

// ListFilter narrows a listing. The date range is half-open: From is
// INCLUSIVE and To is EXCLUSIVE. An omitted To is UNBOUNDED (server parity).
type ListFilter struct {
	// DeviceID restricts the rows to one device.
	DeviceID *string
	// Type restricts the rows to one message type.
	Type *smsgateway.IncomingMessageType
	// From is the inclusive lower bound of the received_at range.
	From *time.Time
	// To is the exclusive upper bound of the received_at range.
	To *time.Time
}

func (f *ListFilter) apply(q *bun.SelectQuery) *bun.SelectQuery {
	if f == nil {
		return q
	}

	if f.DeviceID != nil {
		q = q.Where("device_id = ?", *f.DeviceID)
	}
	if f.Type != nil {
		q = q.Where("type = ?", string(*f.Type))
	}
	if f.From != nil {
		q = q.Where("received_at >= ?", f.From)
	}
	if f.To != nil {
		q = q.Where("received_at < ?", f.To)
	}

	return q
}

// validate rejects a filter whose own fields cannot be satisfied by the local
// device.
//
// The gateway is SINGLE-DEVICE, so an explicitly named device id is a
// VALIDATION and never a row filter: a foreign id is rejected with
// [ErrDeviceNotFound]. The date range is HALF-OPEN (From inclusive, To
// exclusive), so an inverted range is rejected with [ErrInvalidDateRange].
//
// The device rule is checked FIRST, so a filter that violates both reports the
// device error. A nil filter has no fields to validate and is accepted.
func (f *ListFilter) validate(localDeviceID string) error {
	if f == nil {
		return nil
	}

	if f.DeviceID != nil && *f.DeviceID != localDeviceID {
		return fmt.Errorf("%w: %q", ErrDeviceNotFound, *f.DeviceID)
	}
	if f.From != nil && f.To != nil && f.From.After(*f.To) {
		return fmt.Errorf(
			"%w: from %s is after to %s",
			ErrInvalidDateRange,
			f.From.UTC().Format(time.RFC3339),
			f.To.UTC().Format(time.RFC3339),
		)
	}

	return nil
}

// ListPagination bounds the returned page.
type ListPagination struct {
	// Limit is the maximum number of rows; nil means no limit.
	Limit *int
	// Offset is the number of rows skipped; nil means no offset.
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

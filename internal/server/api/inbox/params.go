package inbox

import (
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// defaultListLimit is the page size used when the client omits the limit
// query parameter. It matches the Swagger annotation of the endpoint.
const defaultListLimit = 50

// listInboxParams are the query parameters of the inbox listing.
//
// The params are defined LOCALLY: the pinned client-go smsgateway.ListInboxOptions
// carries neither query nor validate tags, so binding it would neither parse
// the timestamps nor validate the bounds.
type listInboxParams struct {
	// Type restricts the listing to one message type. The whole shared enum is
	// accepted; MMS and MMS_DOWNLOADED simply match no rows, because this
	// gateway stores SMS and DATA_SMS only.
	Type *smsgateway.IncomingMessageType `query:"type" validate:"omitempty,oneof=SMS DATA_SMS MMS MMS_DOWNLOADED"`
	// Limit is the page size.
	Limit *int `query:"limit" validate:"omitempty,min=1,max=500"`
	// Offset is the number of rows skipped.
	Offset *int `query:"offset" validate:"omitempty,min=0"`
	// From is the INCLUSIVE lower bound of the received_at range.
	From *time.Time `query:"from"`
	// To is the EXCLUSIVE upper bound of the received_at range. An omitted To
	// is UNBOUNDED (server parity).
	To *time.Time `query:"to"`
	// DeviceID names the device the listing belongs to. It is validated
	// against the local device, never used as a row filter.
	DeviceID *string `query:"deviceId"`
}

// toListOptions maps the parsed query parameters onto the domain list
// options. The default page size is applied here, so every caller gets it.
// The type filter follows the `type` parameter: no ?type parameter -> nil ->
// the type filter is DISABLED (every row matches), a named type -> that type
// only.
func (p listInboxParams) toListOptions() inbox.ListOptions {
	limit := defaultListLimit
	if p.Limit != nil {
		limit = *p.Limit
	}

	offset := 0
	if p.Offset != nil {
		offset = *p.Offset
	}

	var types []smsgateway.IncomingMessageType // nil -> the type filter is DISABLED (every row matches)
	if p.Type != nil {
		types = []smsgateway.IncomingMessageType{*p.Type}
	}

	return inbox.ListOptions{
		Filter: &inbox.ListFilter{
			DeviceID: p.DeviceID,
			Types:    types,
			From:     p.From,
			To:       p.To,
		},
		Order: nil,
		Pagination: &inbox.ListPagination{
			Limit:  &limit,
			Offset: &offset,
		},
	}
}

package messages

import "errors"

// The domain types (Message, State, StateChange, ListFilter) live in
// models.go (Wave 1); this file holds the service-layer domain additions.
// State spellings match client-go smsgateway.ProcessingState verbatim
// (Pending/Sent/Failed/Cancelled - double-L), as pinned in models.go.
//
// MessageRef: the +CMGS message reference returned by the modem is consumed
// transiently by the worker (it proves a completed send); it is NOT persisted
// because the messages table has no column for it and the migration is frozen
// in this wave.

// Service-level sentinel errors; repository sentinels (ErrNotFound,
// ErrAlreadyExists, ErrNotPending) are defined in errors.go.
var (
	// ErrNotSupported is returned when Enqueue receives a message type the
	// MVP does not support (data messages).
	ErrNotSupported = errors.New("data messages are not supported")

	// ErrInvalidText is returned when the message text is missing or fails
	// the modem ASCII validation.
	ErrInvalidText = errors.New("invalid message text")

	// ErrInvalidPhoneNumbers is returned when the message carries no phone
	// numbers or contains an empty one.
	ErrInvalidPhoneNumbers = errors.New("message must contain at least one non-empty phone number")
)

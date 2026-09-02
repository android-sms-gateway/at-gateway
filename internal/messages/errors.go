package messages

import "errors"

var (
	ErrNotFound           = errors.New("message not found")
	ErrAlreadyExists      = errors.New("message already exists")
	ErrNotPending         = errors.New("message is not pending")
	ErrDuplicateRecipient = errors.New("recipient already exists for this message")

	// ErrNotSupported is returned when Enqueue receives a message type the
	// MVP does not support (data messages).
	ErrNotSupported = errors.New("data messages are not supported")

	// ErrInvalidText is returned when the message text is missing or fails
	// the modem ASCII validation.
	ErrInvalidText = errors.New("invalid message text")

	// ErrInvalidContent is returned when a message has no usable content
	// (neither text, data nor hash) or carries conflicting content kinds.
	ErrInvalidContent = errors.New("invalid message content")

	// ErrInvalidPhoneNumbers is returned when the message carries no phone
	// numbers or contains an empty one.
	ErrInvalidPhoneNumbers = errors.New("message must contain at least one non-empty phone number")

	// ErrMissingExtID is returned when Create receives a message without an
	// ext_id. The service is the sole ext_id generator and must set it before
	// Create; this sentinel is the repository's defensive guard.
	ErrMissingExtID = errors.New("message ext_id is required")
)

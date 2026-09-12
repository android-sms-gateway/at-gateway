package messages

import (
	"errors"
)

var (
	ErrNotFound           = errors.New("message not found")
	ErrAlreadyExists      = errors.New("message already exists")
	ErrNotPending         = errors.New("message is not pending")
	ErrDuplicateRecipient = errors.New("recipient already exists for this message")
	ErrNotSupported       = errors.New("feature not supported")

	// ErrInvalidText is returned when the message text is missing or fails
	// the modem ASCII validation.
	ErrInvalidText = errors.New("invalid message text")

	// ErrInvalidContent is returned when a message has no usable content
	// (neither text, data nor hash) or carries conflicting content kinds.
	ErrInvalidContent = errors.New("invalid message content")

	ErrInvalidPhoneNumbers = errors.New("invalid phone number")

	ErrDeviceNotFound = errors.New("device not found")
)

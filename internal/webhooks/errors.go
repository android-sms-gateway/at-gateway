package webhooks

import "errors"

var (
	// ErrInvalidEvent is returned when the webhook event is not a known
	// smsgateway webhook event type.
	ErrInvalidEvent = errors.New("invalid event")

	// ErrDeviceNotFound is returned when the webhook targets a device other
	// than the local one.
	ErrDeviceNotFound = errors.New("device not found")
)

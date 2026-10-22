package inbox

import (
	"errors"
)

var (
	// ErrDeviceNotFound is returned when a request names a device that is not
	// the local one. The gateway is single-device, so the device id is a
	// VALIDATION, never a row filter; match it with [errors.Is].
	ErrDeviceNotFound = errors.New("device not found")

	// ErrInvalidDateRange is returned when the lower bound of a date range is
	// after its upper bound; match it with [errors.Is].
	ErrInvalidDateRange = errors.New("invalid date range")
)

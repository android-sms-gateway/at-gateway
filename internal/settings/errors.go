package settings

import "errors"

// ErrInvalidValue reports a section field value that violates its write
// rules (e.g. below the documented minimum); the settings handler maps it
// onto a wire 400. Storage corruption is deliberately NOT this error: a
// malformed stored value stays a plain error and surfaces as 500.
var ErrInvalidValue = errors.New("invalid value")

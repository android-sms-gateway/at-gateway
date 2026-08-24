package messages

import "errors"

// Sentinel errors returned by Repository; inspect with [errors.Is].
var (
	ErrNotFound      = errors.New("message not found")
	ErrAlreadyExists = errors.New("message already exists")
	ErrNotPending    = errors.New("message is not pending")
)

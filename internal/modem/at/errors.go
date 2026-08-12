package at

import "errors"

var (
	ErrTimeout       = errors.New("timeout")
	ErrCommandFailed = errors.New("command failed")
)

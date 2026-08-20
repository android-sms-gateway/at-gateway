package modem

import "errors"

var (
	ErrModemTimeout    = errors.New("modem command timeout")
	ErrModemNotStarted = errors.New("modem not started")
	ErrSIMNotReady     = errors.New("SIM not ready")
	ErrInitFailed      = errors.New("modem initialization failed")
)

package modem

import "errors"

var (
	ErrPortNotFound    = errors.New("serial port not found")
	ErrModemTimeout    = errors.New("modem command timeout")
	ErrModemNotReady   = errors.New("modem not ready")
	ErrModemNotStarted = errors.New("modem not started")
	ErrSIMNotReady     = errors.New("SIM not ready")
	ErrNotRegistered   = errors.New("not registered on network")
	ErrCommandFailed   = errors.New("AT command failed")
	ErrPortBusy        = errors.New("serial port busy")
	ErrInitFailed      = errors.New("modem initialization failed")
	ErrPortClosed      = errors.New("serial port is closed")
)

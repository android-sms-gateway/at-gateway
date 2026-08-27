package modem

import "errors"

var (
	ErrModemTimeout    = errors.New("modem command timeout")
	ErrModemNotStarted = errors.New("modem not started")
	ErrSIMNotReady     = errors.New("SIM not ready")
	ErrInitFailed      = errors.New("modem initialization failed")

	// ErrSendFailed is returned when the modem rejects an SMS send with a generic
	// ERROR or a +CMS/+CME ERROR response.
	ErrSendFailed = errors.New("SMS send failed")

	// ErrInvalidPhone is returned when a phone number contains characters that
	// would corrupt the AT+CMGS command line (a quote, CR or LF). The send is
	// rejected BEFORE any modem traffic.
	ErrInvalidPhone = errors.New("invalid phone number")

	// errNoCMGSLine is returned by SendSMS when the +CMGS response carries no
	// parseable message-reference line.
	errNoCMGSLine = errors.New("no +CMGS line in response")
)

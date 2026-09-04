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

	// ErrInvalidText is returned when an SMS text cannot be sent: it is empty
	// or exceeds the part limits (the configured cap or the 255-part protocol
	// ceiling).
	ErrInvalidText = errors.New("invalid text")

	// errNoCMGSLine is returned by SendSMS when the +CMGS response carries no
	// parseable message-reference line.
	errNoCMGSLine = errors.New("no +CMGS line in response")

	// errNotStatusReport is returned by DecodeDeliveryReport when the +CDS
	// PDU body decodes to a TPDU that is not an SMS-STATUS-REPORT.
	errNotStatusReport = errors.New("unexpected TPDU type")
)

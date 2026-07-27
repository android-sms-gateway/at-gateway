package modem

// State represents the current modem state.
type State int

const (
	StateDisconnected State = iota
	StateConnecting
	StateReady
	StateBusy
	StateError
)

func (s State) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateReady:
		return "ready"
	case StateBusy:
		return "busy"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// Info holds information about the detected modem hardware.
type Info struct {
	Manufacturer string
	Model        string
	IMEI         string
}

// SimInfo holds information about the SIM card.
type SimInfo struct {
	PhoneNumber       string
	ICCID             string
	Carrier           string
	NetworkRegistered bool
	SignalQuality     int
	SignalPercent     int
}

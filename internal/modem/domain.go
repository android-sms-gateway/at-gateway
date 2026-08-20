package modem

// State represents the current modem state.
type State int

// State values: 0-3. The former busy state was removed in the post-migration
// cleanup (never set by any code path); StateError is now 3.
const (
	StateDisconnected State = iota
	StateConnecting
	StateReady
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

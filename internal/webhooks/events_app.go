package webhooks

import (
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// SimCardInfo is the SIM state of ONE slot at the moment the modem became
// ready: the input of EmitAppStarted. It is the modem-side snapshot, NOT the
// wire DTO - the optional attributes are plain strings here and an EMPTY value
// means "the SIM did not report this", which the emitter turns into an omitted
// field rather than an empty string.
type SimCardInfo struct {
	// SlotIndex is the 0-based physical slot index.
	SlotIndex int
	// SimNumber is the 1-based slot number (1, 2 or 3).
	SimNumber int
	// PhoneNumber is the SIM MSISDN; empty when the SIM reports none.
	PhoneNumber string
	// CarrierName is the network operator name; empty when unknown.
	CarrierName string
	// ICCID is the SIM identifier; empty when unknown.
	ICCID string
}

// EmitAppStarted emits an app:started event carrying the SIM snapshot taken
// when the modem reached ready state.
//
// It is FIRE-AND-FORGET: emit() swallows every error and returns nothing, so
// the caller has no delivery obligation to report - exactly like the
// sms:sent/delivered/failed/cancelled and system:ping emitters, and unlike the
// two tracked inbound emitters (see the split documented in events_received.go).
//
// TIMING DIVERGENCE (accepted, deliberate): Android emits app:started after its
// receiver service starts, i.e. once the SIM is actually available, and so does
// at-gateway - the caller emits at the modem's first ready transition, which is
// AFTER the +CPIN?/CNUM/CCID/COPS queries returned. An fx OnStart emission was
// rejected: it runs before the modem initialised, so every SIM field would be
// empty.
//
// A SIM attribute the modem does not report is OMITTED from the payload
// (client-go omitempty pointers), so a partially populated SIM is a partially
// omitted object, not a failure.
func (s *Service) EmitAppStarted(simCards []SimCardInfo) {
	s.emit(
		smsgateway.WebhookEventAppStarted,
		smsgateway.AppStartedPayload{
			SimCards: simCardsPayload(simCards),
		},
	)
}

// simCardsPayload maps the modem SIM snapshots to the wire DTO. An empty
// optional attribute becomes a nil pointer, which the client-go omitempty tags
// drop from the JSON; a nil snapshot list yields an empty list, which the
// simCards omitempty tag drops as well.
func simCardsPayload(simCards []SimCardInfo) []smsgateway.SimCard {
	payload := make([]smsgateway.SimCard, 0, len(simCards))
	for _, sim := range simCards {
		payload = append(payload, smsgateway.SimCard{
			SlotIndex:   sim.SlotIndex,
			SimNumber:   sim.SimNumber,
			PhoneNumber: reported(sim.PhoneNumber),
			CarrierName: reported(sim.CarrierName),
			ICCID:       reported(sim.ICCID),
		})
	}

	return payload
}

// reported maps an unreported (empty) SIM attribute to nil so the wire payload
// OMITS the field instead of publishing an empty string a consumer cannot tell
// apart from a modem that answered with an empty value.
func reported(value string) *string {
	if value == "" {
		return nil
	}

	return &value
}

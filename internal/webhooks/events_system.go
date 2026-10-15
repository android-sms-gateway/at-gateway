package webhooks

import (
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// EmitSystemPing emits a system:ping event: the periodic heartbeat a consumer
// uses to tell a live gateway from a dead one.
//
// FIRE-AND-FORGET, like every non-inbound emitter: emit() swallows every error
// and returns no verdict, because the caller has no delivery obligation to
// report - see the tracked/fire-and-forget split documented in
// events_received.go. With no webhook registered for the event the tick is a
// no-op: emit() selects zero matches and performs no queue write.
//
// PAYLOAD PARITY GAP (accepted, deliberate): the wire payload is the EMPTY JSON
// OBJECT {} because the client-go contract type is
// smsgateway.SystemPingPayload{} (dto_webhooks.go:136), which carries no field.
// The Android gateway publishes {"health": {...}} on the same event
// (EventsReceiver.kt:21-29), so an Android consumer of system:ping cannot read
// a health document from at-gateway. Introducing one would require a client-go
// release chain and would change a published wire type; neither is in scope, so
// the divergence is documented instead.
func (s *Service) EmitSystemPing() {
	s.emit(
		smsgateway.WebhookEventSystemPing,
		smsgateway.SystemPingPayload{},
	)
}

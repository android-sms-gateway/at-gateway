package webhooks

import (
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
)

// TRACKED VS FIRE-AND-FORGET (intentional, not drift):
//
// Only the two INBOUND emitters report an acceptance verdict: EmitSmsReceived
// and EmitSmsDataReceived return the per-webhook enqueue outcome
// (TrackedEmission) because their consumer - the inbox reassembler -
// has to know exactly which webhooks already hold a queue item for a message
// id, so a modem re-notification re-enqueues ONLY the failed ones.
//
// Every other emitter stays fire-and-forget: sms:sent / sms:delivered /
// sms:failed / sms:cancelled (events.go), app:started (events_app.go) and
// system:ping (events_system.go) all delegate to emit(), which swallows every
// error. They have no dedupe obligation: the events describe outgoing messages
// the gateway itself enqueued, so there is no re-notification to damp.

// InboundEvent is the input of the two INBOUND webhook emitters
// (sms:received and sms:data-received). It carries the assembled content of a
// message read from the modem store, never a single part: the reassembler in
// the inbox module has already merged the concatenated parts.
//
// The wire payloads are built from the client-go DTOs, so the field names and
// omission are wire-identical to the Android gateway and the server.
type InboundEvent struct {
	// MessageID is the content-deterministic identifier of the assembled
	// message (first 16 hex chars of a SHA-256 over sender, received stamp,
	// content, port and concat reference). Consumers MUST deduplicate on it:
	// webhook delivery is AT-LEAST-ONCE.
	MessageID string
	// Sender is the originating address (TP-OA) of the received message.
	Sender string
	// Recipient is the SIM number that received the message; nil when the SIM
	// reports no MSISDN, which the payload contract explicitly allows.
	Recipient *string
	// SimNumber is the receiving SIM index; at-gateway is single-SIM.
	SimNumber *uint8
	// Message is the assembled TEXT of the message (sms:received only).
	Message string
	// Data is the assembled DATA payload, base64 NO_WRAP encoded
	// (sms:data-received only).
	Data string
	// At is the service-centre timestamp of the first part, in UTC.
	At time.Time
}

// EmitSmsReceived emits an sms:received event. At is serialized as
// receivedAt; phoneNumber repeats the sender (the field is the deprecated
// alias of sender for the inbound direction).
//
// served lists the webhook ids the caller has ALREADY enqueued this message id
// for; those are skipped and counted in the returned TrackedEmission, so a
// modem re-notification reaches only the webhooks whose enqueue failed.
func (s *Service) EmitSmsReceived(e InboundEvent, served []string) TrackedEmission {
	return s.emitTracked(
		smsgateway.WebhookEventSmsReceived,
		smsgateway.SmsReceivedPayload{
			SmsEventPayload: smsgateway.SmsEventPayload{
				MessageID:   e.MessageID,
				PhoneNumber: e.Sender,
				Sender:      e.Sender,
				Recipient:   e.Recipient,
				SimNumber:   e.SimNumber,
			},
			Message:    e.Message,
			ReceivedAt: e.At,
		},
		served,
	)
}

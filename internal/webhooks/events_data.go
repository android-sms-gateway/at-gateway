package webhooks

import (
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// EmitSmsDataReceived emits an sms:data-received event for a
// PORT-ADDRESSED data SMS: Data carries the assembled post-UDH octets of the
// message base64 NO_WRAP encoded (std encoding with padding, no line breaks -
// Android's Base64.NO_WRAP parity), and At is serialized as receivedAt.
//
// The dispatch decision (a destination port IEI in the user data header means
// data, otherwise text) belongs to the messages consumer; this emitter only
// serializes the contract.
//
// served lists the webhook ids the caller has ALREADY enqueued this message id
// for; see EmitSmsReceived for the semantics.
func (s *Service) EmitSmsDataReceived(e InboundEvent, served []string) TrackedEmission {
	return s.emitTracked(
		smsgateway.WebhookEventSmsDataReceived,
		smsgateway.SmsDataReceivedPayload{
			SmsEventPayload: smsgateway.SmsEventPayload{
				MessageID:   e.MessageID,
				PhoneNumber: e.Sender,
				Sender:      e.Sender,
				Recipient:   e.Recipient,
				SimNumber:   e.SimNumber,
			},
			Data:       e.Data,
			ReceivedAt: e.At,
		},
		served,
	)
}

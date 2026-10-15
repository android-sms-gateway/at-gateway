package webhooks

import (
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
)

// MessageEvent is the shared base input for message-related webhook events:
// SMS today, MMS later. Per-event MMS fields are added by future MMS methods,
// not here.
type MessageEvent struct {
	MessageID   string
	PhoneNumber string
	Sender      string
	Recipient   *string
	SimNumber   *uint8
	At          time.Time
}

// EmitSmsSent emits an sms:sent event. At is serialized as sentAt.
func (s *Service) EmitSmsSent(e MessageEvent) {
	s.emit(
		smsgateway.WebhookEventSmsSent,
		smsgateway.SmsSentPayload{
			SmsEventPayload: smsgateway.SmsEventPayload{
				MessageID:   e.MessageID,
				PhoneNumber: e.PhoneNumber,
				Sender:      e.Sender,
				Recipient:   e.Recipient,
				SimNumber:   e.SimNumber,
			},
			SentAt: e.At,
		},
	)
}

// EmitSmsDelivered emits an sms:delivered event. At is serialized as deliveredAt.
func (s *Service) EmitSmsDelivered(e MessageEvent) {
	s.emit(
		smsgateway.WebhookEventSmsDelivered,
		smsgateway.SmsDeliveredPayload{
			SmsEventPayload: smsgateway.SmsEventPayload{
				MessageID:   e.MessageID,
				PhoneNumber: e.PhoneNumber,
				Sender:      e.Sender,
				Recipient:   e.Recipient,
				SimNumber:   e.SimNumber,
			},
			DeliveredAt: e.At,
		},
	)
}

// EmitSmsFailed emits an sms:failed event. At is serialized as failedAt.
func (s *Service) EmitSmsFailed(e MessageEvent, reason string) {
	s.emit(
		smsgateway.WebhookEventSmsFailed,
		smsgateway.SmsFailedPayload{
			SmsEventPayload: smsgateway.SmsEventPayload{
				MessageID:   e.MessageID,
				PhoneNumber: e.PhoneNumber,
				Sender:      e.Sender,
				Recipient:   e.Recipient,
				SimNumber:   e.SimNumber,
			},
			FailedAt: e.At,
			Reason:   reason,
		},
	)
}

// EmitSmsCancelled emits an sms:cancelled event. At is serialized as cancelledAt.
func (s *Service) EmitSmsCancelled(e MessageEvent) {
	s.emit(
		smsgateway.WebhookEventSmsCancelled,
		smsgateway.SmsCancelledPayload{
			SmsEventPayload: smsgateway.SmsEventPayload{
				MessageID:   e.MessageID,
				PhoneNumber: e.PhoneNumber,
				Sender:      e.Sender,
				Recipient:   e.Recipient,
				SimNumber:   e.SimNumber,
			},
			CancelledAt: e.At,
		},
	)
}

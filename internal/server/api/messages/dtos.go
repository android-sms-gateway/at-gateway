package messages

import (
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/samber/lo"
)

// messageInputFromDTO maps a wire message onto the domain input. The mapping
// is a pure wire-shape conversion: all business validation lives in
// messages.Service.Enqueue; the HTTP edge performs only go-playground/validator
// schema validation of the wire DTO via the validation middleware.
func messageInputFromDTO(req *smsgateway.Message) *messages.MessageInput {
	return &messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent:       req.TextMessage,
			DataContent:       req.DataMessage,
			MultimediaContent: req.MmsMessage,
		},
		MessageOptions: messages.MessageOptions{
			SimNumber:          req.SimNumber,
			WithDeliveryReport: req.WithDeliveryReport,
			TTL:                req.TTL,
			ValidUntil:         req.ValidUntil,
			ScheduleAt:         req.ScheduleAt,
			Priority:           req.Priority,
		},
		ExtID:        req.ID,
		DeviceID:     lo.EmptyableToPtr(req.DeviceID),
		PhoneNumbers: req.PhoneNumbers,
		IsEncrypted:  req.IsEncrypted,
	}
}

func messageToState(m *messages.Message) smsgateway.MessageState {
	return smsgateway.MessageState{
		ID:          m.ID,
		DeviceID:    m.DeviceID,
		State:       m.State,
		IsHashed:    m.IsHashed,
		IsEncrypted: m.IsEncrypted,
		Recipients: lo.Map(m.Recipients, func(recipient messages.Recipient, _ int) smsgateway.RecipientState {
			return smsgateway.RecipientState{
				PhoneNumber: recipient.PhoneNumber,
				State:       recipient.State,
				Error:       recipient.Error,
			}
		}),
		States:        m.States,
		TextMessage:   m.TextContent,
		DataMessage:   m.DataContent,
		MmsMessage:    m.MultimediaContent,
		HashedMessage: m.HashedContent,
	}
}

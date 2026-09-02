package messages

import (
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/samber/lo"
)

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
		HashedMessage: m.HashedContent,
	}
}

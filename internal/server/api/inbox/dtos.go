package inbox

import (
	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// incomingMessageToDTO maps a stored inbound message onto the wire DTO. The
// mapping is pure: the stored content is returned verbatim, so a DATA_SMS
// preview is the base64 NO_WRAP encoding of the FULL payload.
//
// The DTO has no isEncrypted and no attachments field: this gateway has no
// end-to-end encryption layer and stores no attachments. The divergence from
// the server is deliberate.
func incomingMessageToDTO(message inbox.IncomingMessage) smsgateway.IncomingMessage {
	recipient := message.Recipient
	if recipient != nil && *recipient == "" {
		recipient = nil
	}

	return smsgateway.IncomingMessage{
		ID:             message.ID,
		Type:           message.Type,
		Sender:         message.Sender,
		Recipient:      recipient,
		SimNumber:      message.SimNumber,
		ContentPreview: message.Content,
		CreatedAt:      message.ReceivedAt,
	}
}

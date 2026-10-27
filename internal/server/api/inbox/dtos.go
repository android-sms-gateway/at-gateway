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

// refreshRequestFromDTO maps the wire refresh request onto the domain refresh
// request. The mapping is PURE: the delivery mode comes from the DTO's own
// precedence (explicit webhookDelivery > deprecated triggerWebhooks >
// Disabled), the requested message types pass through VERBATIM - they are
// matched EXACTLY against the stored type, and the EMPTY default ([SMS])
// belongs to the service (owner decision D-10) - and no validation runs here
// (the DTO tags and the service own it).
func refreshRequestFromDTO(req *smsgateway.InboxRefreshRequest) inbox.RefreshRequest {
	return inbox.RefreshRequest{
		DeviceID: req.DeviceID,
		Since:    req.Since,
		Until:    req.Until,
		Types:    req.MessageTypes,
		Delivery: req.ResolveWebhookDelivery(),
	}
}

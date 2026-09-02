package messages_test

import (
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/server/api/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// TestMessageInputFromDTO_PureMapping pins that the wire-to-domain mapping
// carries NO business validation: invalid text, an empty phone list and a
// data payload all pass through untouched. Validation belongs to
// messages.Service.Enqueue only; the HTTP edge validates the wire schema via
// the go-playground/validator middleware.
func TestMessageInputFromDTO_PureMapping(t *testing.T) {
	req := &smsgateway.Message{
		ID:           "client-1",
		DeviceID:     "device-1",
		PhoneNumbers: []string{},
		TextMessage: &smsgateway.TextMessage{
			Text: "привет non-ascii text",
		},
		DataMessage: &smsgateway.DataMessage{
			Data: "SGVsbG8=",
			Port: 53739,
		},
	}

	input := messages.MessageInputFromDTO(req)
	if input == nil {
		t.Fatal("MessageInputFromDTO returned nil")
	}
	if input.ExtID != "client-1" {
		t.Fatalf("ExtID = %q, want client-1", input.ExtID)
	}
	if input.DeviceID == nil || *input.DeviceID != "device-1" {
		t.Fatalf("DeviceID = %v, want device-1", input.DeviceID)
	}
	if len(input.PhoneNumbers) != 0 {
		t.Fatalf("PhoneNumbers = %v, want empty list to pass through", input.PhoneNumbers)
	}
	if input.TextContent == nil || input.TextContent.Text != "привет non-ascii text" {
		t.Fatalf("TextContent = %+v, want the invalid text to pass through", input.TextContent)
	}
	if input.DataContent == nil || input.DataContent.Data != "SGVsbG8=" {
		t.Fatalf("DataContent = %+v, want the data payload to pass through", input.DataContent)
	}
}

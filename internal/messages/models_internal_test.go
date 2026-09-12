package messages

import (
	"errors"
	"testing"

	"github.com/android-sms-gateway/client-go/smsgateway"
)

// TestMessageContentEmpty verifies that a message row with omitted content
// (the content column is excluded from List queries) maps to a content-free
// domain value instead of failing to unmarshal empty JSON.
func TestMessageContentEmpty(t *testing.T) {
	m := &messageModel{}

	content, err := m.messageContent()
	if err != nil {
		t.Fatalf("messageContent() error = %v", err)
	}
	if content.TextContent != nil || content.DataContent != nil ||
		content.MultimediaContent != nil || content.HashedContent != nil {
		t.Fatalf("messageContent() = %+v, want all nil content", content)
	}
}

func TestMessageContent(t *testing.T) {
	tests := []struct {
		name      string
		model     *messageModel
		wantErr   error
		wantEmpty bool
		checkText *smsgateway.TextMessage
		checkData *smsgateway.DataMessage
	}{
		{
			name: "hashed content maps to hash verbatim",
			model: &messageModel{
				IsHashed: true,
				Content:  "deadbeef",
			},
			checkText: nil,
		},
		{
			name: "hashed content omitted maps content-free",
			model: &messageModel{
				IsHashed: true,
			},
			wantEmpty: true,
		},
		{
			name: "text content unmarshals to Text DTO",
			model: &messageModel{
				Type:    ContentTypeText,
				Content: `{"text":"hello"}`,
			},
			checkText: &smsgateway.TextMessage{Text: "hello"},
		},
		{
			name: "data content unmarshals to Data DTO",
			model: &messageModel{
				Type:    ContentTypeData,
				Content: `{"data":"aGVsbG8=","port":53739}`,
			},
			checkData: &smsgateway.DataMessage{Data: "aGVsbG8=", Port: 53739},
		},
		{
			name: "unsupported type is invalid content",
			model: &messageModel{
				Type:    "Unknown",
				Content: `{}`,
			},
			wantErr: ErrInvalidContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := tt.model.messageContent()
			if want := tt.wantErr; want != nil {
				if !errors.Is(err, want) {
					t.Fatalf("messageContent() error = %v, want %v", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("messageContent() error = %v", err)
			}

			switch {
			case tt.wantEmpty:
				if content.TextContent != nil || content.DataContent != nil ||
					content.MultimediaContent != nil || content.HashedContent != nil {
					t.Fatalf("messageContent() = %+v, want all nil content", content)
				}
			case tt.checkText != nil:
				if content.TextContent == nil || *content.TextContent != *tt.checkText {
					t.Fatalf("messageContent() text = %+v, want %+v", content.TextContent, tt.checkText)
				}
			case tt.checkData != nil:
				if content.DataContent == nil || *content.DataContent != *tt.checkData {
					t.Fatalf("messageContent() data = %+v, want %+v", content.DataContent, tt.checkData)
				}
			default:
				if content.HashedContent == nil || content.HashedContent.Hash != tt.model.Content {
					t.Fatalf("messageContent() hash = %+v, want %q", content.HashedContent, tt.model.Content)
				}
			}
		})
	}
}

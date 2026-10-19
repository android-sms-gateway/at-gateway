package messages

import (
	"errors"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
)

// TestMessageContentEmpty verifies that a message row with omitted content
// (the content column is excluded from List queries) maps to a content-free
// domain value instead of failing to unmarshal empty JSON.
func TestMessageContentEmpty(t *testing.T) {
	t.Parallel()

	m := &messageModel{
		BaseModel: bun.BaseModel{},
		TimedModel: db.TimedModel{
			CreatedAt: time.Time{},
			UpdatedAt: time.Time{},
		},
		ID:          0,
		ExtID:       "",
		DeviceID:    "",
		Type:        "",
		Content:     "",
		Priority:    0,
		IsEncrypted: false,
		ValidUntil:  nil,
		ScheduleAt:  nil,
		Options: optionsModel{
			SimNumber:          nil,
			WithDeliveryReport: nil,
		},
		State:      "",
		States:     nil,
		IsHashed:   false,
		Recipients: nil,
	}

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
	t.Parallel()

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
				BaseModel: bun.BaseModel{},
				TimedModel: db.TimedModel{
					CreatedAt: time.Time{},
					UpdatedAt: time.Time{},
				},
				ID:          0,
				ExtID:       "",
				DeviceID:    "",
				Type:        "",
				Priority:    0,
				IsEncrypted: false,
				ValidUntil:  nil,
				ScheduleAt:  nil,
				Options: optionsModel{
					SimNumber:          nil,
					WithDeliveryReport: nil,
				},
				State:      "",
				States:     nil,
				Recipients: nil,
				IsHashed:   true,
				Content:    "deadbeef",
			},
			wantErr:   nil,
			wantEmpty: false,
			checkText: nil,
			checkData: nil,
		},
		{
			name: "hashed content omitted maps content-free",
			model: &messageModel{
				BaseModel: bun.BaseModel{},
				TimedModel: db.TimedModel{
					CreatedAt: time.Time{},
					UpdatedAt: time.Time{},
				},
				ID:          0,
				ExtID:       "",
				DeviceID:    "",
				Type:        "",
				Content:     "",
				Priority:    0,
				IsEncrypted: false,
				ValidUntil:  nil,
				ScheduleAt:  nil,
				Options: optionsModel{
					SimNumber:          nil,
					WithDeliveryReport: nil,
				},
				State:      "",
				States:     nil,
				IsHashed:   true,
				Recipients: nil,
			},
			wantErr:   nil,
			wantEmpty: true,
			checkText: nil,
			checkData: nil,
		},
		{
			name: "text content unmarshals to Text DTO",
			model: &messageModel{
				BaseModel: bun.BaseModel{},
				TimedModel: db.TimedModel{
					CreatedAt: time.Time{},
					UpdatedAt: time.Time{},
				},
				ID:          0,
				ExtID:       "",
				DeviceID:    "",
				Priority:    0,
				IsEncrypted: false,
				ValidUntil:  nil,
				ScheduleAt:  nil,
				Options: optionsModel{
					SimNumber:          nil,
					WithDeliveryReport: nil,
				},
				State:      "",
				States:     nil,
				IsHashed:   false,
				Recipients: nil,
				Type:       ContentTypeText,
				Content:    `{"text":"hello"}`,
			},
			wantErr:   nil,
			wantEmpty: false,
			checkText: &smsgateway.TextMessage{Text: "hello"},
			checkData: nil,
		},
		{
			name: "data content unmarshals to Data DTO",
			model: &messageModel{
				BaseModel: bun.BaseModel{},
				TimedModel: db.TimedModel{
					CreatedAt: time.Time{},
					UpdatedAt: time.Time{},
				},
				ID:          0,
				ExtID:       "",
				DeviceID:    "",
				Priority:    0,
				IsEncrypted: false,
				ValidUntil:  nil,
				ScheduleAt:  nil,
				Options: optionsModel{
					SimNumber:          nil,
					WithDeliveryReport: nil,
				},
				State:      "",
				States:     nil,
				IsHashed:   false,
				Recipients: nil,
				Type:       ContentTypeData,
				Content:    `{"data":"aGVsbG8=","port":53739}`,
			},
			wantErr:   nil,
			wantEmpty: false,
			checkText: nil,
			checkData: &smsgateway.DataMessage{Data: "aGVsbG8=", Port: 53739},
		},
		{
			name: "unsupported type is invalid content",
			model: &messageModel{
				BaseModel: bun.BaseModel{},
				TimedModel: db.TimedModel{
					CreatedAt: time.Time{},
					UpdatedAt: time.Time{},
				},
				ID:          0,
				ExtID:       "",
				DeviceID:    "",
				Priority:    0,
				IsEncrypted: false,
				ValidUntil:  nil,
				ScheduleAt:  nil,
				Options: optionsModel{
					SimNumber:          nil,
					WithDeliveryReport: nil,
				},
				State:      "",
				States:     nil,
				IsHashed:   false,
				Recipients: nil,
				Type:       "Unknown",
				Content:    `{}`,
			},
			wantErr:   ErrInvalidContent,
			wantEmpty: false,
			checkText: nil,
			checkData: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

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

package messages_test

import (
	"errors"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

func TestContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		build     func() *messages.MessageInput
		wantType  messages.ContentType
		wantValue string
		wantErr   error
	}{
		{
			name: "text content marshals to Text DTO",
			build: func() *messages.MessageInput {
				return &messages.MessageInput{
					MessageContent: messages.MessageContent{
						TextContent:       &smsgateway.TextMessage{Text: "hello"},
						DataContent:       nil,
						MultimediaContent: nil,
					},
					MessageOptions: messages.MessageOptions{
						SimNumber:          nil,
						WithDeliveryReport: nil,
						ValidUntil:         nil,
						ScheduleAt:         nil,
						Priority:           0,
					},
					ExtID:        "",
					DeviceID:     nil,
					PhoneNumbers: nil,
					IsEncrypted:  false,
				}
			},
			wantType:  messages.ContentTypeText,
			wantValue: `{"text":"hello"}`,
			wantErr:   nil,
		},
		{
			name: "data content marshals to Data DTO",
			build: func() *messages.MessageInput {
				return &messages.MessageInput{
					MessageContent: messages.MessageContent{
						TextContent:       nil,
						DataContent:       &smsgateway.DataMessage{Data: "aGVsbG8=", Port: 53739},
						MultimediaContent: nil,
					},
					MessageOptions: messages.MessageOptions{
						SimNumber:          nil,
						WithDeliveryReport: nil,
						ValidUntil:         nil,
						ScheduleAt:         nil,
						Priority:           0,
					},
					ExtID:        "",
					DeviceID:     nil,
					PhoneNumbers: nil,
					IsEncrypted:  false,
				}
			},
			wantType:  messages.ContentTypeData,
			wantValue: `{"data":"aGVsbG8=","port":53739}`,
			wantErr:   nil,
		},
		{
			name: "multimedia content is not supported",
			build: func() *messages.MessageInput {
				return &messages.MessageInput{
					MessageContent: messages.MessageContent{
						TextContent: nil,
						DataContent: nil,
						MultimediaContent: &smsgateway.MmsMessage{
							Text: ptr("world"),
						},
					},
					MessageOptions: messages.MessageOptions{
						SimNumber:          nil,
						WithDeliveryReport: nil,
						ValidUntil:         nil,
						ScheduleAt:         nil,
						Priority:           0,
					},
					ExtID:        "",
					DeviceID:     nil,
					PhoneNumbers: nil,
					IsEncrypted:  false,
				}
			},
			wantType:  "",
			wantValue: "",
			wantErr:   messages.ErrNotSupported,
		},
		{
			name: "missing content is invalid",
			build: func() *messages.MessageInput {
				return &messages.MessageInput{
					MessageContent: messages.MessageContent{
						TextContent:       nil,
						DataContent:       nil,
						MultimediaContent: nil,
					},
					MessageOptions: messages.MessageOptions{
						SimNumber:          nil,
						WithDeliveryReport: nil,
						ValidUntil:         nil,
						ScheduleAt:         nil,
						Priority:           0,
					},
					ExtID:        "",
					DeviceID:     nil,
					PhoneNumbers: nil,
					IsEncrypted:  false,
				}
			},
			wantType:  "",
			wantValue: "",
			wantErr:   messages.ErrInvalidContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotType, gotValue, err := tt.build().Content()
			if want := tt.wantErr; want != nil {
				if !errors.Is(err, want) {
					t.Fatalf("Content() error = %v, want %v", err, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Content() error = %v", err)
			}
			if gotType != tt.wantType {
				t.Errorf("Content() type = %q, want %q", gotType, tt.wantType)
			}
			if gotValue != tt.wantValue {
				t.Errorf("Content() value = %q, want %q", gotValue, tt.wantValue)
			}
		})
	}
}

func ptr(s string) *string {
	return &s
}

package messages_test

import (
	"errors"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

func TestContent(t *testing.T) {
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
						TextContent: &smsgateway.TextMessage{Text: "hello"},
					},
				}
			},
			wantType:  messages.ContentTypeText,
			wantValue: `{"text":"hello"}`,
		},
		{
			name: "data content marshals to Data DTO",
			build: func() *messages.MessageInput {
				return &messages.MessageInput{
					MessageContent: messages.MessageContent{
						DataContent: &smsgateway.DataMessage{Data: "aGVsbG8=", Port: 53739},
					},
				}
			},
			wantType:  messages.ContentTypeData,
			wantValue: `{"data":"aGVsbG8=","port":53739}`,
		},
		{
			name: "multimedia content is not supported",
			build: func() *messages.MessageInput {
				return &messages.MessageInput{
					MessageContent: messages.MessageContent{
						MultimediaContent: &smsgateway.MmsMessage{
							Text: ptr("world"),
						},
					},
				}
			},
			wantErr: messages.ErrNotSupported,
		},
		{
			name: "missing content is invalid",
			build: func() *messages.MessageInput {
				return &messages.MessageInput{}
			},
			wantErr: messages.ErrInvalidContent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

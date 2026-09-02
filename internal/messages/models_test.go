package messages_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

const (
	testPhone1 = "+79161234567"
	testPhone2 = "+79167654321"
	testText   = "Hello, world!"
)

// newInput builds a text message with two recipients and default options.
func newInput() *messages.MessageInput {
	return &messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		PhoneNumbers: []string{testPhone1, testPhone2},
	}
}

func TestNewMessageModel_ExtID(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

	explicit, err := messages.NewMessageModel(&messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		ExtID: "client-id-1",
	}, now)
	if err != nil {
		t.Fatalf("NewMessageModel with explicit ExtID: %v", err)
	}
	if explicit.ExtID != "client-id-1" {
		t.Fatalf("ExtID = %q, want %q", explicit.ExtID, "client-id-1")
	}

	// Generation is service-only: an empty ExtID stays empty in the mapper
	// and the repository rejects it (ErrMissingExtID).
	empty, err := messages.NewMessageModel(newInput(), now)
	if err != nil {
		t.Fatalf("NewMessageModel with empty ExtID: %v", err)
	}
	if empty.ExtID != "" {
		t.Fatalf("ExtID = %q, want empty (generation is service-only)", empty.ExtID)
	}

	// The autoincrement PK is assigned by the database, not the mapper.
	if empty.ID != 0 {
		t.Fatalf("ID = %d, want 0 (assigned on insert)", empty.ID)
	}
}

func TestNewMessageModel_InitState(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	model, err := messages.NewMessageModel(newInput(), now)
	if err != nil {
		t.Fatalf("NewMessageModel: %v", err)
	}

	if model.State != smsgateway.ProcessingStatePending {
		t.Fatalf("State = %q, want Pending", model.State)
	}
	if len(model.States) != 1 {
		t.Fatalf("States length = %d, want 1", len(model.States))
	}
	if got := model.States[0]; got.State != smsgateway.ProcessingStatePending || !got.At.Equal(now) {
		t.Fatalf("States[0] = %+v, want {Pending, now}", got)
	}
	if model.CreatedAt != now || model.UpdatedAt != now {
		t.Fatalf("CreatedAt/UpdatedAt = %v/%v, want %v", model.CreatedAt, model.UpdatedAt, now)
	}
	if model.Priority != 0 || model.IsEncrypted || model.IsHashed {
		t.Fatalf("defaults: Priority=%d IsEncrypted=%v IsHashed=%v, want zero values",
			model.Priority, model.IsEncrypted, model.IsHashed)
	}
}

func TestNewMessageModel_ContentStorage(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	model, err := messages.NewMessageModel(newInput(), now)
	if err != nil {
		t.Fatalf("NewMessageModel: %v", err)
	}

	if model.Type != messages.ContentTypeText {
		t.Fatalf("Type = %q, want Text", model.Type)
	}

	var decoded smsgateway.TextMessage
	if unmarshalErr := json.Unmarshal([]byte(model.Content), &decoded); unmarshalErr != nil {
		t.Fatalf("Content is not text JSON: %v", unmarshalErr)
	}
	if decoded.Text != testText {
		t.Fatalf("decoded text = %q, want %q", decoded.Text, testText)
	}
}

func TestMessageModel_ToDomainRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	model, err := messages.NewMessageModel(&messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: testText},
		},
		MessageOptions: messages.MessageOptions{
			SimNumber:          func() *uint8 { v := uint8(2); return &v }(),
			WithDeliveryReport: func() *bool { v := true; return &v }(),
			Priority:           smsgateway.MessagePriority(5),
		},
		ExtID:        "roundtrip-1",
		IsEncrypted:  true,
		PhoneNumbers: []string{testPhone1, testPhone2},
	}, now)
	if err != nil {
		t.Fatalf("NewMessageModel: %v", err)
	}
	model.Recipients = messages.NewRecipientModels([]string{testPhone1, testPhone2}, 0, now)

	message, err := model.ToDomain()
	if err != nil {
		t.Fatalf("ToDomain: %v", err)
	}

	if message.ID != "roundtrip-1" {
		t.Fatalf("ID = %q, want roundtrip-1", message.ID)
	}
	if message.DeviceID != "" {
		t.Fatalf("DeviceID = %q, want empty", message.DeviceID)
	}
	if message.State != smsgateway.ProcessingStatePending {
		t.Fatalf("State = %q, want Pending", message.State)
	}
	if message.TextContent == nil || message.TextContent.Text != testText {
		t.Fatalf("TextContent = %+v, want text %q", message.TextContent, testText)
	}
	if message.Priority != smsgateway.MessagePriority(5) {
		t.Fatalf("Priority = %d, want 5", message.Priority)
	}
	if message.SimNumber == nil || *message.SimNumber != 2 {
		t.Fatalf("SimNumber = %v, want 2", message.SimNumber)
	}
	if message.WithDeliveryReport == nil || !*message.WithDeliveryReport {
		t.Fatalf("WithDeliveryReport = %v, want true", message.WithDeliveryReport)
	}
	if !message.IsEncrypted {
		t.Fatal("IsEncrypted = false, want true")
	}
	firstAt, ok := message.States[string(smsgateway.ProcessingStatePending)]
	if !ok || !firstAt.Equal(now) {
		t.Fatalf("States[Pending] = %v (ok=%v), want %v", firstAt, ok, now)
	}
	if len(message.Recipients) != 2 {
		t.Fatalf("Recipients length = %d, want 2", len(message.Recipients))
	}
}

func TestMessageModel_ToDomainHashed(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	model, err := messages.NewMessageModel(newInput(), now)
	if err != nil {
		t.Fatalf("NewMessageModel: %v", err)
	}
	model.IsHashed = true
	model.Content = "abc123"

	message, err := model.ToDomain()
	if err != nil {
		t.Fatalf("ToDomain: %v", err)
	}
	if !message.IsHashed {
		t.Fatal("IsHashed = false, want true")
	}
	if message.HashedContent == nil || message.HashedContent.Hash != "abc123" {
		t.Fatalf("HashedContent = %+v, want hash abc123", message.HashedContent)
	}
	if message.TextContent != nil {
		t.Fatal("TextContent is set for a hashed message, want nil")
	}
}

func TestStatesModel_ToDomainMapFirstOccurrence(t *testing.T) {
	first := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	later := first.Add(time.Minute)

	states := messages.NewStatesModel(
		messages.StateChange{State: smsgateway.ProcessingStatePending, At: first},
		messages.StateChange{State: smsgateway.ProcessingStateSent, At: later},
		messages.StateChange{State: smsgateway.ProcessingStatePending, At: later},
	)

	got := states.ToDomainMap()
	if len(got) != 2 {
		t.Fatalf("map length = %d, want 2", len(got))
	}
	if at := got[string(smsgateway.ProcessingStatePending)]; !at.Equal(first) {
		t.Fatalf("Pending mapped to %v, want first occurrence %v", at, first)
	}
	if at := got[string(smsgateway.ProcessingStateSent)]; !at.Equal(later) {
		t.Fatalf("Sent mapped to %v, want %v", at, later)
	}
}

func TestRecipientModels_Mapping(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	models := messages.NewRecipientModels([]string{testPhone1, testPhone2}, 42, now)

	if len(models) != 2 {
		t.Fatalf("models length = %d, want 2", len(models))
	}
	for i, phone := range []string{testPhone1, testPhone2} {
		model := models[i]
		if model.MessageID != 42 {
			t.Fatalf("models[%d].MessageID = %d, want 42", i, model.MessageID)
		}
		if model.Phone != phone {
			t.Fatalf("models[%d].Phone = %q, want %q", i, model.Phone, phone)
		}
		if model.RefID != nil || model.Error != nil {
			t.Fatalf("models[%d] RefID/Error = %v/%v, want nil", i, model.RefID, model.Error)
		}
		if len(model.States) != 1 || model.States[0].State != smsgateway.ProcessingStatePending {
			t.Fatalf("models[%d] States = %+v, want single Pending entry", i, model.States)
		}

		recipient := model.ToDomain()
		if recipient.PhoneNumber != phone {
			t.Fatalf("recipient.PhoneNumber = %q, want %q", recipient.PhoneNumber, phone)
		}
		if recipient.State != smsgateway.ProcessingStatePending {
			t.Fatalf("recipient.State = %q, want Pending", recipient.State)
		}
		if len(recipient.States) != 1 {
			t.Fatalf("recipient.States length = %d, want 1", len(recipient.States))
		}
	}
}

func TestRecipientModel_ToDomainLatestState(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	models := messages.NewRecipientModels([]string{testPhone1}, 1, now)
	models[0].States = messages.NewStatesModel(
		messages.StateChange{State: smsgateway.ProcessingStatePending, At: now},
		messages.StateChange{State: smsgateway.ProcessingStateFailed, At: now.Add(time.Minute)},
	)

	recipient := models[0].ToDomain()
	if recipient.State != smsgateway.ProcessingStateFailed {
		t.Fatalf("State = %q, want Failed (last history entry)", recipient.State)
	}
	if len(recipient.States) != 2 {
		t.Fatalf("States length = %d, want 2", len(recipient.States))
	}
}

func TestMessageInput_Content(t *testing.T) {
	t.Run("text", func(t *testing.T) {
		contentType, content, err := newInput().Content()
		if err != nil {
			t.Fatalf("Content(): %v", err)
		}
		if contentType != messages.ContentTypeText {
			t.Fatalf("content type = %q, want Text", contentType)
		}
		if !strings.Contains(content, testText) {
			t.Fatalf("content = %q, want it to contain %q", content, testText)
		}
	})

	t.Run("data", func(t *testing.T) {
		input := &messages.MessageInput{
			MessageContent: messages.MessageContent{
				DataContent: &smsgateway.DataMessage{Data: "SGVsbG8=", Port: 53739},
			},
			PhoneNumbers: []string{testPhone1},
		}
		contentType, content, err := input.Content()
		if err != nil {
			t.Fatalf("Content(): %v", err)
		}
		if contentType != messages.ContentTypeData {
			t.Fatalf("content type = %q, want Data", contentType)
		}
		if !strings.Contains(content, "SGVsbG8=") {
			t.Fatalf("content = %q, want it to contain the payload", content)
		}
	})

	t.Run("both error", func(t *testing.T) {
		input := &messages.MessageInput{
			MessageContent: messages.MessageContent{
				TextContent: &smsgateway.TextMessage{Text: testText},
				DataContent: &smsgateway.DataMessage{Data: "SGVsbG8=", Port: 53739},
			},
			PhoneNumbers: []string{testPhone1},
		}
		_, _, err := input.Content()
		if !errors.Is(err, messages.ErrInvalidContent) {
			t.Fatalf("Content() error = %v, want ErrInvalidContent", err)
		}
	})

	t.Run("none error", func(t *testing.T) {
		input := &messages.MessageInput{PhoneNumbers: []string{testPhone1}}
		_, _, err := input.Content()
		if !errors.Is(err, messages.ErrInvalidContent) {
			t.Fatalf("Content() error = %v, want ErrInvalidContent", err)
		}
	})
}

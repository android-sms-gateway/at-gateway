package messages_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

// newService builds the full persistence graph (see newRepository) plus the
// storage-backed devices service and a plain-constructor metrics registry,
// then returns a *messages.Service ready for Enqueue calls.
func newService(t *testing.T) *messages.Service {
	return newServiceWithConfig(t, messages.Config{})
}

func newServiceWithConfig(t *testing.T, config messages.Config) *messages.Service {
	t.Helper()

	repo, _ := newRepository(t)

	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}

	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())

	metrics := &messages.Metrics{
		EnqueuedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_enqueued_total", Help: "Test counter"},
		),
		SentTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_sent_total", Help: "Test counter"},
		),
		FailedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_failed_total", Help: "Test counter"},
		),
		CancelledTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_cancelled_total", Help: "Test counter"},
		),
	}

	return messages.NewService(config, repo, devicesSvc, nil, metrics, zap.NewNop())
}

func newEnqueueInput(extID string, phones ...string) messages.MessageInput {
	return messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: "hello"},
		},
		ExtID:        extID,
		PhoneNumbers: phones,
	}
}

// TestEnqueue_PhoneValidation pins the default E.164 validation and
// normalization of phone numbers at enqueue time.
func TestEnqueue_PhoneValidation(t *testing.T) {
	tests := []struct {
		name  string
		phone string
		want  string
	}{
		{"e164 with plus", "+79990001234", "+79990001234"},
		{"russian national without plus", "79990001234", "+79990001234"},
		{"foreign mobile", "+4915123456789", "+4915123456789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(t)

			msg, err := svc.Enqueue(
				context.Background(),
				newEnqueueInput("validate-"+tt.name, tt.phone),
				messages.EnqueueOptions{},
			)
			if err != nil {
				t.Fatalf("enqueue message: %v", err)
			}

			if got := msg.Recipients[0].PhoneNumber; got != tt.want {
				t.Errorf("phone number = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEnqueue_ConfiguredDefaultRegion pins the configurable region used to
// parse national phone numbers: a German mobile in national format is valid
// under DefaultRegion "DE" but rejected under the default "RU" fallback.
func TestEnqueue_ConfiguredDefaultRegion(t *testing.T) {
	const germanMobile = "015123456789"
	const germanMobileE164 = "+4915123456789"

	svc := newServiceWithConfig(t, messages.Config{DefaultRegion: "DE"})

	msg, err := svc.Enqueue(
		context.Background(),
		newEnqueueInput("region-de", germanMobile),
		messages.EnqueueOptions{},
	)
	if err != nil {
		t.Fatalf("enqueue message with DE region: %v", err)
	}
	if got := msg.Recipients[0].PhoneNumber; got != germanMobileE164 {
		t.Errorf("phone number = %q, want %q", got, germanMobileE164)
	}

	ruSvc := newService(t)

	_, err = ruSvc.Enqueue(
		context.Background(),
		newEnqueueInput("region-ru", germanMobile),
		messages.EnqueueOptions{},
	)
	if !errors.Is(err, messages.ErrInvalidPhoneNumbers) {
		t.Errorf("error = %v, want ErrInvalidPhoneNumbers under the RU fallback", err)
	}
}

// TestEnqueue_PhoneValidationError pins the per-phone validation error
// surface: errors wrap ErrInvalidPhoneNumbers with the failing row and a
// server-parity reason, and nothing is persisted.
func TestEnqueue_PhoneValidationError(t *testing.T) {
	tests := []struct {
		name    string
		phones  []string
		wantErr string
	}{
		{
			"landline rejected",
			[]string{"+74951234567"},
			"not mobile phone number",
		},
		{
			"invalid number rejected",
			[]string{"+79990001"},
			"invalid phone number",
		},
		{
			"unparseable rejected",
			[]string{"abc"},
			"the phone number supplied is not a number",
		},
		{
			"row index points at failing recipient",
			[]string{"+79990001234", "+74951234567"},
			"not mobile phone number",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(t)

			_, err := svc.Enqueue(
				context.Background(),
				newEnqueueInput("invalid-"+tt.name, tt.phones...),
				messages.EnqueueOptions{},
			)
			if err == nil {
				t.Fatal("enqueue message succeeded, want validation error")
			}

			if !errors.Is(err, messages.ErrInvalidPhoneNumbers) {
				t.Fatalf("error = %T, want messages.ErrInvalidPhoneNumbers", err)
			}

			// The wrap chain pins the failing row and the reason.
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("message = %q, want substring %q", err.Error(), tt.wantErr)
			}

			wantRow := fmt.Sprintf("failed to use phone in row %d", len(tt.phones))
			if !strings.Contains(err.Error(), wantRow) {
				t.Errorf("message = %q, want substring %q", err.Error(), wantRow)
			}

			if _, getErr := svc.Get(
				context.Background(),
				"invalid-"+tt.name,
			); !errors.Is(
				getErr,
				messages.ErrNotFound,
			) {
				t.Errorf("get message = %v, want ErrNotFound", getErr)
			}
		})
	}
}

// TestEnqueue_SkipPhoneValidation verifies that the option stores phone
// numbers verbatim without validation or normalization.
func TestEnqueue_SkipPhoneValidation(t *testing.T) {
	phones := []string{"+74951234567", "abc", "8 999 000-12-34"}

	svc := newService(t)

	msg, err := svc.Enqueue(
		context.Background(),
		newEnqueueInput("skip-validation", phones...),
		messages.EnqueueOptions{SkipPhoneValidation: true},
	)
	if err != nil {
		t.Fatalf("enqueue message: %v", err)
	}

	for i, want := range phones {
		if got := msg.Recipients[i].PhoneNumber; got != want {
			t.Errorf("recipient %d = %q, want verbatim %q", i, got, want)
		}
	}
}

// TestEnqueue_EmptyPhoneNumbers verifies that the empty-phone invariant is
// enforced regardless of the skip option (mirroring the wire DTO tags).
func TestEnqueue_EmptyPhoneNumbers(t *testing.T) {
	tests := []struct {
		name   string
		opts   messages.EnqueueOptions
		phones []string
	}{
		{"no recipients", messages.EnqueueOptions{}, nil},
		{"empty recipient", messages.EnqueueOptions{}, []string{""}},
		{"no recipients with skip", messages.EnqueueOptions{SkipPhoneValidation: true}, nil},
		{"empty recipient with skip", messages.EnqueueOptions{SkipPhoneValidation: true}, []string{""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newService(t)

			_, err := svc.Enqueue(
				context.Background(),
				newEnqueueInput("empty-"+tt.name, tt.phones...),
				tt.opts,
			)
			if !errors.Is(err, messages.ErrInvalidPhoneNumbers) {
				t.Errorf("error = %v, want ErrInvalidPhoneNumbers", err)
			}
		})
	}
}

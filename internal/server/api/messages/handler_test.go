package messages_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	apimessages "github.com/android-sms-gateway/at-gateway/internal/server/api/messages"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/fiberfx"
	fiberval "github.com/go-core-fx/fiberfx/validation"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// newRepository boots the full persistence graph against an in-memory SQLite
// database so embedded migrations are applied before the repository is used.
func newRepository(t *testing.T) *messages.Repository {
	t.Helper()

	var bunDB *bun.DB

	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Supply(sqlfx.Config{
			URL:             "sqlite://:memory:",
			ConnMaxIdleTime: 0,
			ConnMaxLifetime: 0,
			MaxOpenConns:    1,
			MaxIdleConns:    1,
		}),
		db.Module(),
		sqlfx.Module(),
		goosefx.Module(),
		bunfx.Module(),
		fx.Invoke(func(b *bun.DB) {
			bunDB = b
		}),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelStop()
		if stopErr := app.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop app: %v", stopErr)
		}
	})

	return messages.NewRepository(bunDB)
}

// newHandlerApp boots the messages handler the way module.go wires it: the
// fiberfx validation middleware wraps the handler group, and the handler is
// backed by the full persistence graph, storage-backed devices service and a
// plain-constructor metrics registry.
func newHandlerApp(t *testing.T) *fiber.App {
	t.Helper()

	repo := newRepository(t)

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
			prometheus.CounterOpts{Name: "handler_test_enqueued_total", Help: "Test counter"},
		),
		SentTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "handler_test_sent_total", Help: "Test counter"},
		),
		FailedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "handler_test_failed_total", Help: "Test counter"},
		),
		CancelledTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "handler_test_cancelled_total", Help: "Test counter"},
		),
	}

	messagesSvc := messages.NewService(messages.Config{}, repo, devicesSvc, nil, metrics, zap.NewNop())
	handler := apimessages.NewHandler(messagesSvc, zap.NewNop(), validator.New())

	app := fiber.New(fiber.Config{
		ErrorHandler: fiberfx.NewJSONErrorHandler(zap.NewNop()),
	})
	app.Use(fiberval.Middleware)
	handler.Register(app.Group("/api/v1"))

	return app
}

func postEnqueue(t *testing.T, app *fiber.App, query, phone string) (*http.Response, map[string]any) {
	t.Helper()

	body, err := json.Marshal(smsgateway.Message{
		PhoneNumbers: []string{phone},
		TextMessage:  &smsgateway.TextMessage{Text: "hello"},
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, "/api/v1/messages"+query, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
	req.Host = "localhost"

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	t.Cleanup(func() {
		_ = resp.Body.Close()
	})

	var parsed map[string]any
	if resp.StatusCode < 500 {
		if decErr := json.Unmarshal(respBody, &parsed); decErr != nil {
			t.Fatalf("decode response body %q: %v", respBody, decErr)
		}
	}

	return resp, parsed
}

func recipientPhone(t *testing.T, parsed map[string]any) string {
	t.Helper()

	recipients, ok := parsed["recipients"].([]any)
	if !ok || len(recipients) == 0 {
		t.Fatalf("recipients = %v, want at least one entry", parsed["recipients"])
	}
	first, ok := recipients[0].(map[string]any)
	if !ok {
		t.Fatalf("recipient = %T, want object", recipients[0])
	}

	phone, _ := first["phoneNumber"].(string)
	return phone
}

// TestPostEnqueue_PhoneValidation pins the default E.164 validation: valid
// numbers are normalized, invalid ones are rejected with a 400 whose message
// carries the failing row and reason.
func TestPostEnqueue_PhoneValidation(t *testing.T) {
	tests := []struct {
		name  string
		phone string
		want  string
	}{
		{"valid number normalized", "79990001234", "+79990001234"},
		{"invalid number rejected", "+74951234567", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newHandlerApp(t)

			resp, parsed := postEnqueue(t, app, "", tt.phone)

			if tt.want == "" {
				if resp.StatusCode != fiber.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, resp.Body)
				}
				wantMessage := "enqueue message: failed to use phone in row 1: invalid phone number: not mobile phone number"
				if got := parsed["message"]; got != wantMessage {
					t.Errorf("message = %q, want %q", got, wantMessage)
				}
				return
			}

			if resp.StatusCode != fiber.StatusAccepted {
				t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, resp.Body)
			}
			if got := recipientPhone(t, parsed); got != tt.want {
				t.Errorf("phoneNumber = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPostEnqueue_SkipPhoneValidation pins the query option: validation and
// normalization are bypassed and the phone number is stored verbatim.
func TestPostEnqueue_SkipPhoneValidation(t *testing.T) {
	app := newHandlerApp(t)

	resp, parsed := postEnqueue(t, app, "?skipPhoneValidation=true", "+74951234567")
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, resp.Body)
	}
	if got := recipientPhone(t, parsed); got != "+74951234567" {
		t.Errorf("phoneNumber = %q, want verbatim +74951234567", got)
	}
}

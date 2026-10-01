package webhooks_test

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
	apiwebhooks "github.com/android-sms-gateway/at-gateway/internal/server/api/webhooks"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/fiberfx"
	fiberval "github.com/go-core-fx/fiberfx/validation"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	testStartTimeout = 10 * time.Second
	testStopTimeout  = 5 * time.Second
	testSingleConn   = 1
)

// newPersistence boots the full persistence graph against an in-memory SQLite
// database so embedded migrations (incl. the webhooks registry) apply before
// the repository is used.
func newPersistence(t *testing.T) *webhooks.Repository {
	t.Helper()

	var bunDB *bun.DB

	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Supply(sqlfx.Config{
			URL:             "sqlite://:memory:",
			ConnMaxIdleTime: 0,
			ConnMaxLifetime: 0,
			MaxOpenConns:    testSingleConn,
			MaxIdleConns:    testSingleConn,
		}),
		db.Module(),
		sqlfx.Module(),
		goosefx.Module(),
		bunfx.Module(),
		fx.Invoke(func(b *bun.DB) {
			bunDB = b
		}),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), testStopTimeout)
		defer cancelStop()
		if stopErr := app.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop app: %v", stopErr)
		}
	})

	return webhooks.NewRepository(bunDB)
}

// newHandlerApp boots the webhooks handler the way module.go wires it: the
// fiberfx validation middleware wraps the handler group, the global error
// handler formats wire 4xx bodies as {message, code}, and the service is
// backed by the full persistence graph plus a storage-backed devices service.
// Basic auth is NOT installed (same harness choice as the messages
// handler_test - the v1 group auth is exercised by module wiring, not here).
func newHandlerApp(t *testing.T) (*fiber.App, *devices.Service) {
	t.Helper()

	repo := newPersistence(t)

	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}

	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())
	webhooksSvc, err := webhooks.NewService(
		webhooks.Config{SigningKey: "webhooks-handler-test-key"},
		repo,
		devicesSvc,
		storageSvc,
		nil,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create webhooks service: %v", err)
	}
	handler := apiwebhooks.NewHandler(webhooksSvc, zap.NewNop(), validator.New())

	app := fiber.New(fiber.Config{
		ErrorHandler: fiberfx.NewJSONErrorHandler(zap.NewNop()),
	})
	app.Use(fiberval.Middleware)
	handler.Register(app.Group("/api/v1"))

	return app, devicesSvc
}

// doRequest performs a request against the in-memory app and returns the
// status plus the raw response body.
func doRequest(t *testing.T, app *fiber.App, method, path string, body []byte) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(method, path, bytes.NewReader(body))
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
	t.Cleanup(func() {
		_ = resp.Body.Close()
	})

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	return resp.StatusCode, respBody
}

// decodeBody unmarshals a JSON response body into a generic map.
func decodeBody(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return parsed
}

// decodeList unmarshals a JSON array response body.
func decodeList(t *testing.T, body []byte) []map[string]any {
	t.Helper()

	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list body %q: %v", body, err)
	}
	return list
}

func webhookBody(url string, event smsgateway.WebhookEvent, deviceID *string) []byte {
	body, _ := json.Marshal(smsgateway.Webhook{
		DeviceID: deviceID,
		URL:      url,
		Event:    event,
	})
	return body
}

// TestGetWebhooks_EmptyRegistry pins AC2: an idle registry answers 200 with
// an empty JSON array (not null, not an error).
func TestGetWebhooks_EmptyRegistry(t *testing.T) {
	app, _ := newHandlerApp(t)

	status, body := doRequest(t, app, http.MethodGet, "/api/v1/webhooks", nil)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if string(body) != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
}

// TestPostWebhooks_SuccessEchoesGeneratedIDNullDeviceID pins owner rule 1:
// an omitted deviceId key, JSON null and empty string all answer 201 with a
// generated id and deviceId null - the local device id is NEVER filled -
// across three distinct events/urls (required_test_first).
func TestPostWebhooks_SuccessEchoesGeneratedIDNullDeviceID(t *testing.T) {
	tests := []struct {
		name  string
		body  []byte
		url   string
		event smsgateway.WebhookEvent
	}{
		{
			name:  "omitted device id",
			body:  []byte(`{"url":"https://example.com/hook","event":"sms:sent"}`),
			url:   "https://example.com/hook",
			event: smsgateway.WebhookEventSmsSent,
		},
		{
			name:  "json null device id",
			body:  webhookBody("https://example.org/ping", smsgateway.WebhookEventSystemPing, nil),
			url:   "https://example.org/ping",
			event: smsgateway.WebhookEventSystemPing,
		},
		{
			name:  "empty device id",
			body:  webhookBody("https://example.com/empty", smsgateway.WebhookEventSmsReceived, new(string)),
			url:   "https://example.com/empty",
			event: smsgateway.WebhookEventSmsReceived,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, devicesSvc := newHandlerApp(t)

			status, body := doRequest(t, app, http.MethodPost, "/api/v1/webhooks", tt.body)
			if status != fiber.StatusCreated {
				t.Fatalf("status = %d, want 201; body %s", status, body)
			}

			parsed := decodeBody(t, body)
			id, _ := parsed["id"].(string)
			if id == "" {
				t.Fatalf("id = %q, want generated nanoid in 201 body %s", id, body)
			}
			deviceID, hasDeviceID := parsed["deviceId"]
			if !hasDeviceID {
				t.Fatal("deviceId key missing from 201 body, want null")
			}
			if deviceID != nil {
				localID := devicesSvc.Get().ID
				t.Fatalf("deviceId = %v, want null (never fill local id %q)", deviceID, localID)
			}
			if got, _ := parsed["url"].(string); got != tt.url {
				t.Errorf("url = %q, want %q", got, tt.url)
			}
			if got, _ := parsed["event"].(string); got != tt.event {
				t.Errorf("event = %q, want %q", got, tt.event)
			}
		})
	}
}

// TestPostWebhooks_SuppliedLocalDeviceIDEchoed pins owner rule 2 at the wire:
// a deviceId equal to the local device is accepted; the 201 body and a
// subsequent GET list both echo that supplied id (not null).
func TestPostWebhooks_SuppliedLocalDeviceIDEchoed(t *testing.T) {
	app, devicesSvc := newHandlerApp(t)
	localID := devicesSvc.Get().ID

	status, body := doRequest(
		t,
		app,
		http.MethodPost,
		"/api/v1/webhooks",
		webhookBody("https://example.com/hook", smsgateway.WebhookEventSmsSent, &localID),
	)
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", status, body)
	}

	parsed := decodeBody(t, body)
	if id, _ := parsed["id"].(string); id == "" {
		t.Fatalf("id = %q, want generated nanoid in 201 body %s", id, body)
	}
	if got, _ := parsed["deviceId"].(string); got != localID {
		t.Fatalf("deviceId = %q, want supplied local id %q", got, localID)
	}

	status, body = doRequest(t, app, http.MethodGet, "/api/v1/webhooks", nil)
	if status != fiber.StatusOK {
		t.Fatalf("list status = %d, want 200; body %s", status, body)
	}
	list := decodeList(t, body)
	if len(list) != 1 {
		t.Fatalf("list has %d rows, want 1", len(list))
	}
	if got, _ := list[0]["deviceId"].(string); got != localID {
		t.Fatalf("list deviceId = %q, want supplied local id %q", got, localID)
	}
}

// TestPostWebhooks_InvalidEvent pins AC3: Webhook.Validate rejects an invalid
// event at the edge before Service.Replace runs. The response is 400 with the
// validation error in {message} and {details}, marshaled exactly the way the
// fiberfx validation middleware builds it.
func TestPostWebhooks_InvalidEvent(t *testing.T) {
	tests := []struct {
		name  string
		event smsgateway.WebhookEvent
	}{
		{"unknown event", "bogus:event"},
		{"case variant rejected", "SMS:RECEIVED"},
		{"prefix lookalike", "sms:received:v2"},
	}

	validationErrs := fiberval.Errors{{Field: "_", Message: "validation failed: invalid event type"}}
	want, err := json.Marshal(fiberfx.NewErrorResponse(
		validationErrs.Error(),
		fiber.StatusBadRequest,
		validationErrs,
	))
	if err != nil {
		t.Fatalf("marshal expected body: %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := newHandlerApp(t)

			status, body := doRequest(
				t,
				app,
				http.MethodPost,
				"/api/v1/webhooks",
				webhookBody("https://example.com/hook", tt.event, nil),
			)
			if status != fiber.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", status, body)
			}

			if !bytes.Equal(body, want) {
				t.Fatalf("body = %s, want %s", body, want)
			}
		})
	}
}

// TestPostWebhooks_ForeignDeviceID pins owner rule 2: a deviceId that differs
// from the local device answers 400 with the full wrapped device-not-found
// text.
func TestPostWebhooks_ForeignDeviceID(t *testing.T) {
	tests := []struct {
		name    string
		foreign string
	}{
		{"short foreign id", "other-device"},
		{"nanoid-shaped foreign id", "PyDmBQZZXYmyxMwED8Fzy"},
		{"another foreign id", "dev-999"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := newHandlerApp(t)
			foreign := tt.foreign

			status, body := doRequest(
				t,
				app,
				http.MethodPost,
				"/api/v1/webhooks",
				webhookBody("https://example.com/hook", smsgateway.WebhookEventSmsSent, &foreign),
			)
			if status != fiber.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", status, body)
			}

			want := `{"message":"replace webhook: device not found: \"` + tt.foreign + `\"","code":400}`
			if string(body) != want {
				t.Fatalf("body = %s, want %s", body, want)
			}
		})
	}
}

// TestPostWebhooks_ValidatorRejected pins AC3: a body that fails the edge
// validator tags (http_url / required) answers 400 via the validation
// middleware with {message, code} - message carries the unwrapped
// validation.Errors.Error() string.
func TestPostWebhooks_ValidatorRejected(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		wantMessage string
	}{
		{
			name:        "invalid url rejected by http_url",
			body:        []byte(`{"url":"not-a-url","event":"sms:sent"}`),
			wantMessage: "validation failed: URL: URL is invalid",
		},
		{
			name:        "missing url and event rejected by required",
			body:        []byte(`{}`),
			wantMessage: "validation failed: URL: URL is required; Event: Event is required",
		},
		{
			name:        "malformed json rejected by body parser",
			body:        []byte(`{`),
			wantMessage: "validation failed: _: unexpected end of JSON input",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := newHandlerApp(t)

			status, body := doRequest(t, app, http.MethodPost, "/api/v1/webhooks", tt.body)
			if status != fiber.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", status, body)
			}

			parsed := decodeBody(t, body)
			if got, _ := parsed["message"].(string); got != tt.wantMessage {
				t.Errorf("message = %q, want %q", got, tt.wantMessage)
			}
			if got, ok := parsed["code"].(float64); !ok || int(got) != fiber.StatusBadRequest {
				t.Errorf("code = %v, want 400", parsed["code"])
			}
		})
	}
}

// TestGetWebhooks_ListAfterCreate pins owner rule 1 on the list wire: after a
// create that omitted deviceId, the list answers 200 with the row and
// deviceId null (registered for all devices), across successive listings.
func TestGetWebhooks_ListAfterCreate(t *testing.T) {
	app, _ := newHandlerApp(t)

	status, _ := doRequest(
		t,
		app,
		http.MethodPost,
		"/api/v1/webhooks",
		webhookBody("https://example.com/hook", smsgateway.WebhookEventSmsSent, nil),
	)
	if status != fiber.StatusCreated {
		t.Fatalf("create status = %d, want 201", status)
	}

	status, body := doRequest(t, app, http.MethodGet, "/api/v1/webhooks", nil)
	if status != fiber.StatusOK {
		t.Fatalf("list status = %d, want 200; body %s", status, body)
	}

	list := decodeList(t, body)
	if len(list) != 1 {
		t.Fatalf("list has %d rows, want 1", len(list))
	}
	deviceID, hasDeviceID := list[0]["deviceId"]
	if !hasDeviceID {
		t.Error("deviceId key missing from list row, want null")
	} else if deviceID != nil {
		t.Errorf("deviceId = %v, want null for unscoped row", deviceID)
	}
	if got, _ := list[0]["id"].(string); got == "" {
		t.Errorf("id = %q, want present", got)
	}
	if got, _ := list[0]["url"].(string); got != "https://example.com/hook" {
		t.Errorf("url = %q, want https://example.com/hook", got)
	}
	if got, _ := list[0]["event"].(string); got != smsgateway.WebhookEventSmsSent {
		t.Errorf("event = %q, want %q", got, smsgateway.WebhookEventSmsSent)
	}
}

// TestDeleteWebhooks_Idempotent pins AC4: known id -> 204, same id again ->
// 204, unknown id -> 204 (server parity: no 404 case, required_test_first).
func TestDeleteWebhooks_Idempotent(t *testing.T) {
	app, _ := newHandlerApp(t)

	status, body := doRequest(
		t,
		app,
		http.MethodPost,
		"/api/v1/webhooks",
		webhookBody("https://example.com/hook", smsgateway.WebhookEventSmsSent, nil),
	)
	if status != fiber.StatusCreated {
		t.Fatalf("create status = %d, want 201; body %s", status, body)
	}
	created := decodeBody(t, body)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("created id empty, body %s", body)
	}

	for _, path := range []string{
		"/api/v1/webhooks/" + id,
		"/api/v1/webhooks/" + id, // second delete of the same id
		"/api/v1/webhooks/does-not-exist",
	} {
		delStatus, delBody := doRequest(t, app, http.MethodDelete, path, nil)
		if delStatus != fiber.StatusNoContent {
			t.Fatalf("DELETE %s status = %d, want 204; body %s", path, delStatus, delBody)
		}
		if len(delBody) != 0 {
			t.Errorf("DELETE %s body = %q, want empty", path, delBody)
		}
	}

	status, body = doRequest(t, app, http.MethodGet, "/api/v1/webhooks", nil)
	if status != fiber.StatusOK {
		t.Fatalf("list status = %d, want 200; body %s", status, body)
	}
	if list := decodeList(t, body); len(list) != 0 {
		t.Fatalf("list has %d rows after deletes, want 0", len(list))
	}
}

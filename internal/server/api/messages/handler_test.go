package messages_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
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
func newRepository(t *testing.T) (*messages.Repository, *bun.DB) {
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

	return messages.NewRepository(bunDB), bunDB
}

// newHandlerApp boots the messages handler the way module.go wires it: the
// fiberfx validation middleware wraps the handler group, and the handler is
// backed by the full persistence graph, storage-backed devices service and a
// plain-constructor metrics registry.
func newHandlerApp(t *testing.T) *fiber.App {
	t.Helper()

	app, _, _ := newHandlerWithRepo(t)
	return app
}

// newHandlerWithRepo is newHandlerApp plus the repository and raw bun handle
// so tests can seed rows before issuing requests.
func newHandlerWithRepo(t *testing.T) (*fiber.App, *messages.Repository, *bun.DB) {
	t.Helper()

	repo, bunDB := newRepository(t)

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
		DeliveredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "handler_test_delivered_total", Help: "Test counter"},
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

	return app, repo, bunDB
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

// TestPostEnqueue_DeviceNotFound pins the server-parity response for a request
// that explicitly names a non-existent device: 400 Bad Request, not 500.
func TestPostEnqueue_DeviceNotFound(t *testing.T) {
	app := newHandlerApp(t)

	body, err := json.Marshal(smsgateway.Message{
		PhoneNumbers: []string{"+79990001234"},
		TextMessage:  &smsgateway.TextMessage{Text: "hello"},
		DeviceID:     "other-device",
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, "/api/v1/messages", bytes.NewReader(body))
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
		t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, respBody)
	}

	var parsed map[string]any
	if decErr := json.Unmarshal(respBody, &parsed); decErr != nil {
		t.Fatalf("decode body: %v", decErr)
	}
	if msg, _ := parsed["message"].(string); !strings.Contains(msg, "device not found") {
		t.Errorf("message = %q, want substring 'device not found'", msg)
	}
}

// newListInput builds a minimal domain input for list-history seeding.
func newListInput(extID string) *messages.MessageInput {
	deviceID := "device-1"
	return &messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: "hello"},
		},
		ExtID:        extID,
		DeviceID:     &deviceID,
		PhoneNumbers: []string{"+79990001111"},
	}
}

// seedMessages inserts count messages with created_at = base + (i+1)*time.Second
// ascending by insertion order, so under the default -created_at (descending)
// sort the newest insertion is first and the oldest is last - deterministic
// enough to assert default-limit truncation.
func seedMessages(t *testing.T, repo *messages.Repository, bunDB *bun.DB, count int, base time.Time) {
	t.Helper()
	ctx := context.Background()
	for i := range count {
		extID := fmt.Sprintf("m-%02d", i+1)
		if err := repo.Create(ctx, newListInput(extID)); err != nil {
			t.Fatalf("create %s: %v", extID, err)
		}
		at := base.Add(time.Duration(i+1) * time.Second)
		if _, err := bunDB.ExecContext(
			ctx,
			"UPDATE messages SET created_at = ? WHERE ext_id = ?",
			at,
			extID,
		); err != nil {
			t.Fatalf("set created_at for %s: %v", extID, err)
		}
	}
}

// getListBody performs a GET /api/v1/messages request and returns the raw body.
func getListBody(t *testing.T, app *fiber.App, query string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "/api/v1/messages"+query, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Host = "localhost"

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("perform request: %v", err)
	}
	t.Cleanup(func() {
		_ = resp.Body.Close()
	})

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp, body
}

func decodeList(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list body %q: %v", body, err)
	}
	return list
}

func decodeError(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return parsed
}

func listIDs(list []map[string]any) []string {
	ids := make([]string, 0, len(list))
	for _, row := range list {
		if id, ok := row["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// TestGetMessages_NilLimit_DefaultsTo50 pins the server-parity default page
// size: when the client omits limit, at most 50 rows are returned and the
// 51st (oldest) row is excluded (AC-GAP2-1).
func TestGetMessages_NilLimit_DefaultsTo50(t *testing.T) {
	app, repo, bunDB := newHandlerWithRepo(t)
	base := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	seedMessages(t, repo, bunDB, 51, base)

	resp, body := getListBody(t, app, "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	list := decodeList(t, body)
	if len(list) != 50 {
		t.Fatalf("returned %d rows, want default limit 50", len(list))
	}
	if got := resp.Header.Get("X-Total-Count"); got != "51" {
		t.Fatalf("X-Total-Count = %q, want 51", got)
	}
	for _, id := range listIDs(list) {
		if id == "m-01" {
			t.Fatal("list contains m-01 (oldest row), want default limit 50 to exclude it")
		}
	}
}

// TestGetMessages_ExplicitLimitOverridesDefault pins that an explicit limit
// takes precedence over the default (AC-GAP2-2).
func TestGetMessages_ExplicitLimitOverridesDefault(t *testing.T) {
	app, repo, bunDB := newHandlerWithRepo(t)
	seedMessages(t, repo, bunDB, 12, time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC))

	resp, body := getListBody(t, app, "?limit=10")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	list := decodeList(t, body)
	if len(list) != 10 {
		t.Fatalf("returned %d rows, want 10", len(list))
	}
	if got := resp.Header.Get("X-Total-Count"); got != "12" {
		t.Fatalf("X-Total-Count = %q, want 12", got)
	}
}

// TestGetMessages_LimitZeroRejected pins the query validation: an explicit
// limit below the minimum (1) is rejected with 400 (AC-GAP2-7).
func TestGetMessages_LimitZeroRejected(t *testing.T) {
	app, _, _ := newHandlerWithRepo(t)

	resp, body := getListBody(t, app, "?limit=0")
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := "validation failed: Limit: Limit must be at least 1"
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestGetMessages_LimitTooLargeRejected pins the query validation: an explicit
// limit above the maximum (100) is rejected with 400 (AC-GAP2-8).
func TestGetMessages_LimitTooLargeRejected(t *testing.T) {
	app, _, _ := newHandlerWithRepo(t)

	resp, body := getListBody(t, app, "?limit=101")
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := "validation failed: Limit: Limit must be at most 100"
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestGetMessages_EmptyResult pins that a listing with no matching rows yields
// 200 with an empty list ([]), not an error (AC-GAP2-9).
func TestGetMessages_EmptyResult(t *testing.T) {
	app, _, _ := newHandlerWithRepo(t)

	resp, body := getListBody(t, app, "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	list := decodeList(t, body)
	if len(list) != 0 {
		t.Fatalf("returned %d rows, want 0", len(list))
	}
	if got := resp.Header.Get("X-Total-Count"); got != "0" {
		t.Fatalf("X-Total-Count = %q, want 0", got)
	}
}

// TestGetMessages_FromToBounds verifies the from/to query parameters pass
// through the query parser into inclusive-start / exclusive-end SQL boundaries
// at millisecond precision (AC-GAP2-3/4/10).
func TestGetMessages_FromToBounds(t *testing.T) {
	app, repo, bunDB := newHandlerWithRepo(t)
	ctx := context.Background()

	from := time.Date(2026, 9, 9, 10, 0, 0, 123_000_000, time.UTC)
	between := from.Add(time.Minute)
	to := from.Add(2 * time.Minute)
	for _, row := range []struct {
		extID string
		at    time.Time
	}{
		{"m-at-from", from},
		{"m-between", between},
		{"m-at-to", to},
	} {
		if err := repo.Create(ctx, newListInput(row.extID)); err != nil {
			t.Fatalf("create %s: %v", row.extID, err)
		}
		if _, err := bunDB.ExecContext(
			ctx,
			"UPDATE messages SET created_at = ? WHERE ext_id = ?",
			row.at,
			row.extID,
		); err != nil {
			t.Fatalf("set created_at for %s: %v", row.extID, err)
		}
	}

	query := "?from=" + from.Format(time.RFC3339Nano) + "&to=" + to.Format(time.RFC3339Nano)
	resp, body := getListBody(t, app, query)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	ids := listIDs(decodeList(t, body))
	for _, want := range []string{"m-at-from", "m-between"} {
		if !slices.Contains(ids, want) {
			t.Fatalf("list = %v, want %s included (inclusive from)", ids, want)
		}
	}
	if slices.Contains(ids, "m-at-to") {
		t.Fatalf("list = %v, want m-at-to excluded (exclusive to)", ids)
	}
	if got := resp.Header.Get("X-Total-Count"); got != "2" {
		t.Fatalf("X-Total-Count = %q, want 2", got)
	}
}

// TestGetMessage_ScheduleAtRoundTrip pins GAP5 (Android-to-Go parity): a
// message enqueued with scheduleAt reads the timestamp back from
// GET /messages/{id} (AC-GAP5-2, AC-GAP5-4). Sub-second inputs pin the actual
// precision persisted by the SQLite DATETIME column (AC-GAP5-5).
func TestGetMessage_ScheduleAtRoundTrip(t *testing.T) {
	tests := []struct {
		name       string
		scheduleAt time.Time
		want       time.Time
	}{
		{
			name:       "whole-second value round-trips",
			scheduleAt: time.Date(2030, 9, 10, 12, 30, 45, 0, time.UTC),
			want:       time.Date(2030, 9, 10, 12, 30, 45, 0, time.UTC),
		},
		{
			// bun persists time.Time as 2006-01-02 15:04:05.999999-07:00, so
			// nanosecond input is expected to be truncated to the microsecond
			// the column stores (AC-GAP5-5 pins the stored precision).
			name:       "nanosecond input truncated to stored precision",
			scheduleAt: time.Date(2030, 9, 10, 12, 30, 45, 123_456_789, time.UTC),
			want:       time.Date(2030, 9, 10, 12, 30, 45, 123_456_000, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newHandlerApp(t)

			body, err := json.Marshal(smsgateway.Message{
				PhoneNumbers: []string{"+79990001234"},
				TextMessage:  &smsgateway.TextMessage{Text: "scheduled"},
				ScheduleAt:   &tt.scheduleAt,
			})
			if err != nil {
				t.Fatalf("marshal request body: %v", err)
			}

			req, err := http.NewRequest(http.MethodPost, "/api/v1/messages", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("create request: %v", err)
			}
			req.Header.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
			req.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
			req.Host = "localhost"

			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("perform enqueue: %v", err)
			}
			enqBody, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				t.Fatalf("read enqueue body: %v", readErr)
			}
			if resp.StatusCode != fiber.StatusAccepted {
				t.Fatalf("enqueue status = %d, want 202; body %s", resp.StatusCode, enqBody)
			}

			var enqueued map[string]any
			if decErr := json.Unmarshal(enqBody, &enqueued); decErr != nil {
				t.Fatalf("decode enqueue body: %v", decErr)
			}
			id, _ := enqueued["id"].(string)

			getReq, err := http.NewRequest(http.MethodGet, "/api/v1/messages/"+id, nil)
			if err != nil {
				t.Fatalf("create get request: %v", err)
			}
			getReq.Host = "localhost"

			getResp, err := app.Test(getReq)
			if err != nil {
				t.Fatalf("perform get: %v", err)
			}
			getBody, readErr := io.ReadAll(getResp.Body)
			_ = getResp.Body.Close()
			if readErr != nil {
				t.Fatalf("read get body: %v", readErr)
			}
			if getResp.StatusCode != fiber.StatusOK {
				t.Fatalf("get status = %d, want 200; body %s", getResp.StatusCode, getBody)
			}

			var got map[string]any
			if decErr := json.Unmarshal(getBody, &got); decErr != nil {
				t.Fatalf("decode get body: %v", decErr)
			}

			gotStr, ok := got["scheduleAt"].(string)
			if !ok {
				t.Fatalf("scheduleAt absent in GET body %s", getBody)
			}
			gotTime, parseErr := time.Parse(time.RFC3339Nano, gotStr)
			if parseErr != nil {
				t.Fatalf("parse scheduleAt %q: %v", gotStr, parseErr)
			}
			if !gotTime.Equal(tt.want) {
				t.Errorf("scheduleAt = %s, want %s", gotStr, tt.want)
			}
		})
	}
}

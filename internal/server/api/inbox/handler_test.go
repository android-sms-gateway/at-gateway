package inbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/auth"
	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	apiinbox "github.com/android-sms-gateway/at-gateway/internal/server/api/inbox"
	"github.com/android-sms-gateway/at-gateway/internal/server/middlewares/userauth"
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
	// testUsername and testPassword are the basic credentials of the fixture.
	testUsername = "sms"
	testPassword = "inbox-handler-test-password"
)

// replayQueueLimit is the read bound of the UN-CAPPED replay fixtures: the
// refresh replay has no row cap (D-3), so the queue read must not invent one.
const replayQueueLimit = 1024

// refreshTestTimeout is the Fiber app.Test timeout used for refresh requests.
// The refresh endpoint's documented behaviour is SYNCHRONOUS with a duration
// PROPORTIONAL to the requested window (no row cap, Swagger @Description), so
// Fiber's 1000ms default is not a valid expectation for it: a legitimate
// 501-row replay under -race exceeds 1s while still completing. 30s keeps
// headroom so a true hang still fails fast. The assertions are status and
// emission count, never latency. (Fiber v2's app.Test takes the timeout as
// MILLISECONDS, so the Duration is converted at the call site.)
const refreshTestTimeout = 30 * time.Second

// refreshInboxBody is the valid refresh body of the fixture window: every test
// varies only the field under test.
const refreshInboxBody = `{"since":"2026-10-22T00:00:00Z","until":"2026-10-23T00:00:00Z"`

// handlerMetricsOnce guards the single registration of the inbox metrics: the
// production constructor registers on the default registerer and panics on a
// duplicate registration.
var (
	handlerMetricsOnce sync.Once
	handlerMetrics     *inbox.Metrics
)

// handlerFixture is the wired inbox handler: the fiber app, the repository the
// tests seed rows with, the webhook service/queue the refresh replays into and
// the local device the rows belong to.
type handlerFixture struct {
	app      *fiber.App
	repo     *inbox.Repository
	webhooks *webhooks.Service
	queue    *webhooks.Repository
	deviceID string
}

// newHandlerFixture boots the inbox handler the way module.go wires it: the
// basic auth middleware guards the version 1 group, the fiberfx validation
// middleware wraps the handler group, and the handler is backed by the full
// persistence graph and a storage-backed devices service.
func newHandlerFixture(t *testing.T) handlerFixture {
	t.Helper()

	bunDB := newHandlerDB(t)

	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}

	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())

	authSvc, err := auth.NewService(
		auth.Config{Basic: auth.BasicConfig{Username: testUsername, Password: testPassword}},
		storageSvc,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create auth service: %v", err)
	}

	webhooksRepo := webhooks.NewRepository(bunDB)
	webhooksSvc, err := webhooks.NewService(
		webhooks.Config{
			SigningKey: "inbox-handler-test-key",
			RetryCount: 0,
			Queue: webhooks.QueueConfig{
				BatchSize:              0,
				RequestTimeout:         0,
				DialTimeout:            0,
				RetryBaseDelay:         0,
				IdleDelay:              0,
				StuckProcessingTimeout: 0,
				CleanupRetention:       0,
			},
		},
		webhooksRepo,
		devicesSvc,
		storageSvc,
		nil,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create webhooks service: %v", err)
	}

	// The modem service is never run: the consumer only reads its cached SIM
	// state, which is empty here.
	modemSvc := modem.NewService(modem.Config{
		Port:           "",
		BaudRate:       0,
		InitTimeout:    0,
		CommandTimeout: 0,
		Cmgd: modem.CmgdConfig{
			RetryAttempts: 0,
			RetryBackoff:  0,
		},
		Store: modem.StoreConfig{
			Memory:        "",
			SweepInterval: 0,
		},
	}, zap.NewNop(), nil, webhooksSvc)

	repo := inbox.NewRepository(bunDB)
	service := inbox.NewService(
		inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0},
		repo,
		devicesSvc,
		modemSvc,
		webhooksSvc,
		newHandlerMetrics(),
		zap.NewNop(),
	)

	handler := apiinbox.NewHandler(service, zap.NewNop(), validator.New())

	app := fiber.New(fiber.Config{
		ErrorHandler: fiberfx.NewJSONErrorHandler(zap.NewNop()),
	})

	v1 := app.Group("/api/v1", userauth.NewBasic(authSvc))
	v1.Use(fiberval.Middleware)
	handler.Register(v1)

	return handlerFixture{
		app:      app,
		repo:     repo,
		webhooks: webhooksSvc,
		queue:    webhooksRepo,
		deviceID: devicesSvc.Get().ID,
	}
}

// seed stores one inbound message of the local device.
func (f handlerFixture) seed(
	t *testing.T,
	id string,
	messageType smsgateway.IncomingMessageType,
	content string,
	receivedAt time.Time,
) {
	t.Helper()

	if _, err := f.repo.Insert(context.Background(), inbox.IncomingMessage{
		ID:         id,
		DeviceID:   f.deviceID,
		Type:       messageType,
		Sender:     "+79990001234",
		Recipient:  nil,
		SimNumber:  nil,
		Content:    content,
		ReceivedAt: receivedAt,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// registerWebhook registers a webhook for an event.
func (f handlerFixture) registerWebhook(t *testing.T, id string, event smsgateway.WebhookEvent) {
	t.Helper()

	if err := f.webhooks.Replace(context.Background(), &smsgateway.Webhook{
		ID:    id,
		URL:   "https://example.com/" + id,
		Event: event,
	}); err != nil {
		t.Fatalf("register webhook %s: %v", id, err)
	}
}

// queuedCount counts the stored queue items within the replay read bound.
func (f handlerFixture) queuedCount(t *testing.T) int {
	t.Helper()

	items, err := f.queue.DueItems(context.Background(), time.Now().Add(time.Hour), replayQueueLimit)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	return len(items)
}

// queuedPayloads returns the payload sub-documents grouped by webhook id
// WITHOUT re-marshaling them; one webhook may hold many replayed items.
func (f handlerFixture) queuedPayloads(t *testing.T) map[string][]json.RawMessage {
	t.Helper()

	items, err := f.queue.DueItems(context.Background(), time.Now().Add(time.Hour), replayQueueLimit)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	grouped := make(map[string][]json.RawMessage)
	for _, item := range items {
		var envelope struct {
			Payload json.RawMessage `json:"payload"`
		}
		if unmarshalErr := json.Unmarshal([]byte(item.Payload), &envelope); unmarshalErr != nil {
			t.Fatalf("decode envelope for %s: %v", item.WebhookID, unmarshalErr)
		}
		grouped[item.WebhookID] = append(grouped[item.WebhookID], envelope.Payload)
	}

	return grouped
}

// TestMain warms the GSM charsets on the main goroutine before any fixture
// builds a modem service: warthog618/sms initialises them lazily, and the
// parallel tests below would otherwise race on that first construction.
func TestMain(m *testing.M) {
	modem.WarmGSMCharsets()
	os.Exit(m.Run())
}

// TestList_ReturnsTheStoredMessages pins the 200 contract: the JSON array of
// the wire DTOs and the total count in the X-Total-Count header.
func TestList_ReturnsTheStoredMessages(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)
	receivedAt := time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC)
	f.seed(t, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "hello", receivedAt)
	f.seed(t, "fedcba9876543210", smsgateway.IncomingMessageTypeSMS, "world", receivedAt.Add(time.Minute))

	resp, body := getInbox(t, f.app, "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Total-Count"); got != "2" {
		t.Fatalf("X-Total-Count = %q, want 2", got)
	}

	list := decodeList(t, body)
	if len(list) != 2 {
		t.Fatalf("returned %d rows, want 2", len(list))
	}

	first := list[0]
	if got, _ := first["id"].(string); got != "fedcba9876543210" {
		t.Errorf("id = %q, want the newest row first", got)
	}
	if got, _ := first["type"].(string); got != "SMS" {
		t.Errorf("type = %q, want SMS", got)
	}
	if got, _ := first["sender"].(string); got != "+79990001234" {
		t.Errorf("sender = %q, want +79990001234", got)
	}
	if got, _ := first["contentPreview"].(string); got != "world" {
		t.Errorf("contentPreview = %q, want world", got)
	}
	if got, _ := first["createdAt"].(string); got != "2026-10-22T09:01:00Z" {
		t.Errorf("createdAt = %q, want the receive stamp", got)
	}
	if _, present := first["recipient"]; present {
		t.Error("recipient is present, want it omitted while the SIM reports no MSISDN")
	}
	if _, present := first["simNumber"]; present {
		t.Error("simNumber is present, want it omitted")
	}
}

// TestList_EmptyResult pins that a listing without matching rows answers 200
// with an empty array.
func TestList_EmptyResult(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	resp, body := getInbox(t, f.app, "")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Total-Count"); got != "0" {
		t.Fatalf("X-Total-Count = %q, want 0", got)
	}
	if string(body) != "[]" {
		t.Fatalf("body = %s, want an empty array", body)
	}
}

// TestList_FiltersByTypeAndDate pins the query filters: the type filter and
// the half-open date range.
func TestList_FiltersByTypeAndDate(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)
	base := time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC)
	f.seed(t, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "text", base)
	f.seed(t, "fedcba9876543210", smsgateway.IncomingMessageTypeDataSMS, "ZGF0YQ==", base.Add(time.Minute))

	resp, body := getInbox(t, f.app, "?type=DATA_SMS")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	list := decodeList(t, body)
	if len(list) != 1 {
		t.Fatalf("returned %d rows, want 1", len(list))
	}
	if got, _ := list[0]["type"].(string); got != "DATA_SMS" {
		t.Errorf("type = %q, want DATA_SMS", got)
	}

	// The From bound is inclusive, the To bound exclusive.
	query := "?from=" + base.Format(time.RFC3339) + "&to=" + base.Add(time.Minute).Format(time.RFC3339)
	resp, body = getInbox(t, f.app, query)
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Total-Count"); got != "1" {
		t.Fatalf("X-Total-Count = %q, want 1", got)
	}

	// The whole shared enum is accepted; a type this gateway never stores
	// matches no rows.
	resp, body = getInbox(t, f.app, "?type=MMS")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if mms := decodeList(t, body); len(mms) != 0 {
		t.Fatalf("returned %d rows, want none for MMS", len(mms))
	}
}

// TestList_PaginatesAndCountsBeforePagination pins that a page limit below
// the total truncates the body but not the reported total.
func TestList_PaginatesAndCountsBeforePagination(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)
	base := time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC)
	for i := range 3 {
		f.seed(
			t,
			string(rune('a'+i)),
			smsgateway.IncomingMessageTypeSMS,
			"hello",
			base.Add(time.Duration(i)*time.Minute),
		)
	}

	resp, body := getInbox(t, f.app, "?limit=1")
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", resp.StatusCode, body)
	}
	if list := decodeList(t, body); len(list) != 1 {
		t.Fatalf("returned %d rows, want 1", len(list))
	}
	if got := resp.Header.Get("X-Total-Count"); got != "3" {
		t.Fatalf("X-Total-Count = %q, want the full count 3", got)
	}
}

// TestList_RejectsAForeignDevice pins the device id validation: a foreign
// device id is a 400, not a row filter.
func TestList_RejectsAForeignDevice(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	resp, body := getInbox(t, f.app, "?deviceId=000000000000000000000")
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := `list inbox messages: device not found: "000000000000000000000"`
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestList_RejectsAnInvertedDateRange pins the date range validation.
func TestList_RejectsAnInvertedDateRange(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	from := time.Date(2026, time.October, 22, 10, 0, 0, 0, time.UTC)
	to := from.Add(-time.Hour)
	query := "?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)

	resp, body := getInbox(t, f.app, query)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := "list inbox messages: invalid date range: " +
		"from 2026-10-22T10:00:00Z is after to 2026-10-22T09:00:00Z"
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestList_RejectsOutOfBoundsPagination pins the query validation of the
// pagination bounds.
func TestList_RejectsOutOfBoundsPagination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "limit below the minimum",
			query: "?limit=0",
			want:  "validation failed: Limit: Limit must be at least 1",
		},
		{
			name:  "limit above the maximum",
			query: "?limit=501",
			want:  "validation failed: Limit: Limit must be at most 500",
		},
		{
			name:  "negative offset",
			query: "?offset=-1",
			want:  "validation failed: Offset: Offset must be at least 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newHandlerFixture(t)

			resp, body := getInbox(t, f.app, tt.query)
			if resp.StatusCode != fiber.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
			}
			parsed := decodeError(t, body)
			if got, _ := parsed["message"].(string); got != tt.want {
				t.Fatalf("message = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestList_RejectsAnUnknownType pins the type validation: the whole shared
// enum is accepted, an unknown name is not.
func TestList_RejectsAnUnknownType(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	resp, body := getInbox(t, f.app, "?type=BOGUS")
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := "validation failed: Type: Type must be one of: SMS, DATA_SMS, MMS, MMS_DOWNLOADED"
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestList_RequiresAuthentication pins that the endpoint is guarded: without
// credentials the request is rejected with 401.
func TestList_RequiresAuthentication(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	req, err := http.NewRequest(http.MethodGet, "/api/v1/inbox", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Host = "localhost"

	resp, err := f.app.Test(req)
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

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", resp.StatusCode, body)
	}
}

// TestRefresh_Returns202AcceptedWithTheTextPlainBody pins AC6/AC8's success
// contract: 202 with the body "Accepted" (text/plain) produced by
// c.SendStatus(fiber.StatusAccepted) - exact server parity - and the replay
// enqueues the stored row.
func TestRefresh_Returns202AcceptedWithTheTextPlainBody(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	f.seed(t, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "hello",
		time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC))

	resp, body := postInboxRefresh(t, f.app,
		refreshInboxBody+`,"webhookDelivery":"Individual"}`)
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	if string(body) != "Accepted" {
		t.Errorf("body = %q, want the plain text %q", body, "Accepted")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want a text/plain response", ct)
	}
	if got := f.queuedCount(t); got != 1 {
		t.Errorf("queued items = %d, want 1 replayed event", got)
	}
}

// TestRefresh_DisabledAndEmptyWindowStayAtZeroEmissions pins the two zero
// cases of AC1/AC6 over HTTP: webhookDelivery Disabled emits nothing and an
// empty window emits nothing - both still answer 202.
func TestRefresh_DisabledAndEmptyWindowStayAtZeroEmissions(t *testing.T) {
	t.Parallel()

	t.Run("Disabled", func(t *testing.T) {
		t.Parallel()

		f := newHandlerFixture(t)
		f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
		f.seed(t, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "hello",
			time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC))

		resp, body := postInboxRefresh(t, f.app,
			refreshInboxBody+`,"webhookDelivery":"Disabled"}`)
		if resp.StatusCode != fiber.StatusAccepted {
			t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
		}
		if got := f.queuedCount(t); got != 0 {
			t.Errorf("queued items = %d, want 0 for the Disabled mode", got)
		}
	})

	t.Run("empty window", func(t *testing.T) {
		t.Parallel()

		f := newHandlerFixture(t)
		f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
		f.seed(t, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "hello",
			time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC))

		// A window that matches no stored row.
		resp, body := postInboxRefresh(t, f.app,
			`{"since":"2026-10-24T00:00:00Z","until":"2026-10-25T00:00:00Z","webhookDelivery":"Individual"}`)
		if resp.StatusCode != fiber.StatusAccepted {
			t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
		}
		if got := f.queuedCount(t); got != 0 {
			t.Errorf("queued items = %d, want 0 for an empty window", got)
		}
	})
}

// TestRefresh_BatchReturns400 pins the Batch rejection over HTTP: this gateway
// has no batch inbound emitters, so the mode is a 400 instead of a silent
// degradation to Individual.
func TestRefresh_BatchReturns400(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	resp, body := postInboxRefresh(t, f.app,
		refreshInboxBody+`,"webhookDelivery":"Batch"}`)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := "refresh inbox messages: batch delivery not supported"
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestRefresh_ForeignDeviceReturns400 pins AC5/AC8 over HTTP: a device id that
// is not the local one is a 400 validation, never a row filter.
func TestRefresh_ForeignDeviceReturns400(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	resp, body := postInboxRefresh(t, f.app,
		refreshInboxBody+`,"deviceId":"000000000000000000000","webhookDelivery":"Individual"}`)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := `refresh inbox messages: device not found: "000000000000000000000"`
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestRefresh_SinceAfterUntilRejectedByTheDTOValidator pins AC6: since>until
// never reaches the service - the pinned DTO validator (ltefield/gtefield)
// rejects the body with 400.
func TestRefresh_SinceAfterUntilRejectedByTheDTOValidator(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	resp, body := postInboxRefresh(t, f.app,
		`{"since":"2026-10-23T00:00:00Z","until":"2026-10-22T00:00:00Z","webhookDelivery":"Individual"}`)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := "validation failed: Since: Since is invalid; Until: Until is invalid"
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestRefresh_RejectsLiteralDataSMSClass pins the EXACT-match wire contract:
// the pinned DTO validator accepts only the values SMS and MMS, so a literal
// DATA_SMS is a 400 - DATA_SMS rows are listable but can never be named for a
// replay over HTTP.
func TestRefresh_RejectsLiteralDataSMSClass(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	resp, body := postInboxRefresh(t, f.app,
		refreshInboxBody+`,"messageTypes":["DATA_SMS"],"webhookDelivery":"Individual"}`)
	if resp.StatusCode != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", resp.StatusCode, body)
	}
	parsed := decodeError(t, body)
	want := "validation failed: MessageTypes[0]: MessageTypes[0] must be one of: SMS, MMS"
	if got, _ := parsed["message"].(string); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

// TestRefresh_RequiresAuthentication pins that the endpoint is guarded: without
// credentials the request is rejected with 401.
func TestRefresh_RequiresAuthentication(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)

	req, err := http.NewRequest(http.MethodPost, "/api/v1/inbox/refresh", strings.NewReader(refreshInboxBody+"}"))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = "localhost"

	resp, err := f.app.Test(req)
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

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", resp.StatusCode, body)
	}
}

// TestRefresh_DefaultTypesReplayOnlyTheSMSRow pins EXACT matching over HTTP
// (D-10 amended): with messageTypes omitted the default ([SMS]) replays ONLY
// the stored SMS row - one sms:received - while the stored DATA_SMS row
// produces ZERO sms:data-received events. DATA_SMS cannot be named in this
// request (the shared DTO validates oneof=SMS MMS), so its rows are listable
// but NOT replayable over HTTP.
func TestRefresh_DefaultTypesReplayOnlyTheSMSRow(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	f.registerWebhook(t, "data", smsgateway.WebhookEventSmsDataReceived)
	f.seed(t, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "hello",
		time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC))
	f.seed(t, "fedcba9876543210", smsgateway.IncomingMessageTypeDataSMS, "yv66vg==",
		time.Date(2026, time.October, 22, 9, 1, 0, 0, time.UTC))

	resp, body := postInboxRefresh(t, f.app,
		refreshInboxBody+`,"webhookDelivery":"Individual"}`)
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	if string(body) != "Accepted" {
		t.Errorf("body = %q, want %q", body, "Accepted")
	}

	payloads := f.queuedPayloads(t)
	got := payloads["text"]
	if len(got) != 1 {
		t.Fatalf("sms:received payloads = %d, want 1 - the default [SMS] replays the SMS row", len(got))
	}

	var payload struct {
		MessageID string `json:"messageId"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(got[0], &payload); err != nil {
		t.Fatalf("decode sms:received payload: %v", err)
	}
	if payload.MessageID != "0123456789abcdef" {
		t.Errorf("messageId = %q, want the SMS row id", payload.MessageID)
	}
	if payload.Message != "hello" {
		t.Errorf("message = %q, want the stored text %q", payload.Message, "hello")
	}

	// NEGATIVE half: exact matching means the stored DATA_SMS row is NOT
	// replayed through the default - no grouping, no expansion.
	if data := payloads["data"]; len(data) != 0 {
		t.Errorf("sms:data-received payloads = %d, want 0 - DATA_SMS must not replay via the default", len(data))
	}
}

// TestRefresh_DefaultDeliveryDisabled pins decision 2 (D-3 clause): when the
// request carries NEITHER webhookDelivery NOR triggerWebhooks, delivery
// defaults to Disabled - the refresh still answers 202 with body "Accepted"
// and NOTHING is emitted.
func TestRefresh_DefaultDeliveryDisabled(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	f.seed(t, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "hello",
		time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC))

	// Only the base body: no webhookDelivery, no triggerWebhooks.
	resp, body := postInboxRefresh(t, f.app, refreshInboxBody+`}`)
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	if string(body) != "Accepted" {
		t.Errorf("body = %q, want %q", body, "Accepted")
	}
	if got := f.queuedCount(t); got != 0 {
		t.Errorf("queued items = %d, want 0 - with neither field present the default delivery is Disabled", got)
	}
}

// TestRefresh_NoRowCap501RowsReturns202 pins AC7 end to end: a window with 501
// matching rows triggers exactly 501 emissions and still answers 202 - the
// inverse of a capped replay.
func TestRefresh_NoRowCap501RowsReturns202(t *testing.T) {
	t.Parallel()

	const seededRows = 501

	f := newHandlerFixture(t)
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)

	base := time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC)
	for i := range seededRows {
		f.seed(
			t,
			fmt.Sprintf("msg-%03d", i),
			smsgateway.IncomingMessageTypeSMS,
			"hello",
			base.Add(time.Duration(i)*time.Second),
		)
	}

	resp, body := postInboxRefresh(t, f.app,
		refreshInboxBody+`,"webhookDelivery":"Individual"}`)
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	if string(body) != "Accepted" {
		t.Errorf("body = %q, want %q", body, "Accepted")
	}
	if got := f.queuedCount(t); got != seededRows {
		t.Errorf("emissions = %d, want exactly %d - the replay must not be capped", got, seededRows)
	}
}

// TestRefresh_LegacyTriggerWebhooksEnablesIndividualEmission pins the legacy
// precedence branch over HTTP: with no webhookDelivery field, the pinned
// ResolveWebhookDelivery maps triggerWebhooks=true to Individual (explicit
// webhookDelivery > triggerWebhooks=true > Disabled), so the replay emits one
// event per stored row of the window.
func TestRefresh_LegacyTriggerWebhooksEnablesIndividualEmission(t *testing.T) {
	t.Parallel()

	f := newHandlerFixture(t)
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	f.seed(t, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "hello",
		time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC))
	f.seed(t, "23456789abcdef01", smsgateway.IncomingMessageTypeSMS, "world",
		time.Date(2026, time.October, 22, 10, 0, 0, 0, time.UTC))

	resp, body := postInboxRefresh(t, f.app,
		refreshInboxBody+`,"triggerWebhooks":true}`)
	if resp.StatusCode != fiber.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", resp.StatusCode, body)
	}
	if string(body) != "Accepted" {
		t.Errorf("body = %q, want %q", body, "Accepted")
	}
	if got := f.queuedCount(t); got != 2 {
		t.Errorf("emissions = %d, want 2 - triggerWebhooks=true must resolve to Individual", got)
	}
}

// newHandlerDB boots the full persistence graph against an in-memory SQLite
// database so the embedded migrations are applied.
func newHandlerDB(t *testing.T) *bun.DB {
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
		t.Fatalf("start persistence app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelStop()
		if stopErr := app.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop persistence app: %v", stopErr)
		}
	})

	return bunDB
}

// newHandlerMetrics builds the inbox metrics ONCE per test binary. The
// fixtures share the instance: no assertion reads a counter, and swapping the
// process-global registerer would race between the parallel fixtures.
func newHandlerMetrics() *inbox.Metrics {
	handlerMetricsOnce.Do(func() {
		handlerMetrics = inbox.NewMetrics()
	})

	return handlerMetrics
}

// getInbox performs a GET /api/v1/inbox request with the fixture credentials.
func getInbox(t *testing.T, app *fiber.App, query string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, "/api/v1/inbox"+query, nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.SetBasicAuth(testUsername, testPassword)
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

// decodeList decodes the JSON array body of a listing.
func decodeList(t *testing.T, body []byte) []map[string]any {
	t.Helper()

	var list []map[string]any
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("decode list body %q: %v", body, err)
	}

	return list
}

// decodeError decodes the JSON error body.
func decodeError(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}

	return parsed
}

// postInboxRefresh performs a POST /api/v1/inbox/refresh request with the
// fixture credentials. Every refresh test goes through this helper, so the
// raised timeout applies to the whole refresh surface.
func postInboxRefresh(t *testing.T, app *fiber.App, body string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, "/api/v1/inbox/refresh", strings.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(testUsername, testPassword)
	req.Host = "localhost"

	resp, err := app.Test(req, int(refreshTestTimeout.Milliseconds()))
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

	return resp, respBody
}

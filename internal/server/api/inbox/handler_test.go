package inbox_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
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

// handlerMetricsOnce guards the single registration of the inbox metrics: the
// production constructor registers on the default registerer and panics on a
// duplicate registration.
var (
	handlerMetricsOnce sync.Once
	handlerMetrics     *inbox.Metrics
)

// handlerFixture is the wired inbox handler: the fiber app, the repository the
// tests seed rows with and the local device the rows belong to.
type handlerFixture struct {
	app      *fiber.App
	repo     *inbox.Repository
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
		webhooks.NewRepository(bunDB),
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

	if err := f.repo.Insert(context.Background(), inbox.IncomingMessage{
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

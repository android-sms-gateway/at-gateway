//nolint:testpackage // in-package tests call the unexported mapEnqueueError helper.
package messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/auth"
	"github.com/android-sms-gateway/at-gateway/internal/db/migrations"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/server/middlewares/userauth"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/fiberfx"
	"github.com/go-core-fx/fiberfx/validation"
	"github.com/go-core-fx/goosefx"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.uber.org/zap"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

const (
	testMaxConns       = 1
	testUsername       = "test"
	testPassword       = "secret"
	testPageLimit      = 2
	testTotalMessages  = 3
	testPendingCount   = 2
	testLongTextLength = 161
	testSeedBaseYear   = 2026
)

// newTestRepository builds a Repository over a fresh in-memory SQLite
// database with the embedded goose migration applied.
func newTestRepository(t *testing.T) *messages.Repository {
	t.Helper()

	sqldb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqldb.SetMaxOpenConns(testMaxConns)
	sqldb.SetMaxIdleConns(testMaxConns)
	t.Cleanup(func() {
		if closeErr := sqldb.Close(); closeErr != nil {
			t.Errorf("close sqlite: %v", closeErr)
		}
	})

	provider, providerErr := goose.NewProvider(database.DialectSQLite3, sqldb, goosefx.Storage(migrations.FS))
	if providerErr != nil {
		t.Fatalf("init goose provider: %v", providerErr)
	}
	if _, upErr := provider.Up(context.Background()); upErr != nil {
		t.Fatalf("apply migrations: %v", upErr)
	}

	return messages.NewRepository(bunfx.New(sqldb, sqlitedialect.New(), zap.NewNop()))
}

// newTestApp builds the same stack as internal/server/module.go: Basic auth
// on /api/v1, the validation middleware and the messages handler registered
// on the v1 group. The messages worker is not started; the modem Service is
// only a constructor argument and never opens a port.
func newTestApp(t *testing.T) (*fiber.App, *messages.Repository) {
	t.Helper()

	repo := newTestRepository(t)

	svc := messages.New(
		messages.Default(),
		repo,
		modem.NewService(modem.Config{}, zap.NewNop(), nil),
		&messages.Metrics{
			EnqueuedTotal: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "test_messages_enqueued_total",
				Help: "total messages enqueued (test)",
			}),
			SentTotal: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "test_messages_sent_total",
				Help: "total messages sent (test)",
			}),
			FailedTotal: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "test_messages_failed_total",
				Help: "total messages failed (test)",
			}),
			CancelledTotal: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "test_messages_cancelled_total",
				Help: "total messages cancelled (test)",
			}),
		},
		zap.NewNop(),
	)

	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("init storage: %v", err)
	}

	authSvc, err := auth.NewService(
		auth.Config{Basic: auth.BasicConfig{Username: testUsername, Password: testPassword}},
		storageSvc,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("init auth: %v", err)
	}

	app := fiber.New(fiber.Config{ErrorHandler: fiberfx.NewJSONErrorHandler(zap.NewNop())})
	v1 := app.Group("/api/v1", userauth.NewBasic(authSvc))
	v1.Use(validation.Middleware)
	NewHandler(svc, zap.NewNop(), validator.New(validator.WithRequiredStructEnabled())).Register(v1)

	return app, repo
}

type testResponse struct {
	status int
	header http.Header
	body   []byte
}

// doRequest performs an authenticated request against the test app and
// returns the raw response.
func doRequest(t *testing.T, app *fiber.App, method, target, body string) testResponse {
	t.Helper()

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(testUsername, testPassword)

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}

	return testResponse{status: resp.StatusCode, header: resp.Header, body: raw}
}

func unmarshalState(t *testing.T, raw []byte) smsgateway.MessageState {
	t.Helper()

	var state smsgateway.MessageState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("unmarshal message state: %v", err)
	}

	return state
}

func errorMessage(t *testing.T, raw []byte) string {
	t.Helper()

	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}

	return body.Message
}

// newSeedMessage builds a persisted message with a fixed created_at so
// ordering assertions are deterministic.
func newSeedMessage(id string, createdAt time.Time, state messages.State) *messages.Message {
	msg := &messages.Message{
		ID:                 id,
		DeviceID:           "device-under-test",
		State:              state,
		IsHashed:           false,
		IsEncrypted:        false,
		TextMessage:        "hello world",
		SimNumber:          nil,
		WithDeliveryReport: false,
		Priority:           0,
		Recipients:         []string{"+15550000001"},
		States:             []messages.StateChange{{State: state, At: createdAt}},
		ErrorMessage:       nil,
		CreatedAt:          createdAt,
		UpdatedAt:          createdAt,
		ProcessedAt:        nil,
		SentAt:             nil,
		FailedAt:           nil,
	}
	if state == messages.StateSent {
		msg.SentAt = &createdAt
	}

	return msg
}

func TestPost_Enqueue_202WithLocationAndBody(t *testing.T) {
	app, _ := newTestApp(t)

	resp := doRequest(t, app, http.MethodPost, "/api/v1/messages",
		`{"textMessage":{"text":"Hello world"},"phoneNumbers":["+15550000001"]}`)
	if resp.status != fiber.StatusAccepted {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusAccepted, resp.body)
	}

	state := unmarshalState(t, resp.body)
	if state.ID == "" {
		t.Error("response state ID is empty")
	}
	if location := resp.header.Get("Location"); location != "/api/v1/messages/"+state.ID {
		t.Errorf("Location = %q, want %q", location, "/api/v1/messages/"+state.ID)
	}
	if state.State != smsgateway.ProcessingStatePending {
		t.Errorf("State = %q, want %q", state.State, smsgateway.ProcessingStatePending)
	}
	if len(state.Recipients) != 1 || state.Recipients[0].PhoneNumber != "+15550000001" {
		t.Errorf("Recipients = %+v, want [+15550000001]", state.Recipients)
	}
	if _, ok := state.States[string(smsgateway.ProcessingStatePending)]; !ok {
		t.Errorf("States history = %v, want a Pending entry", state.States)
	}
}

func TestPost_NonASCII_400(t *testing.T) {
	app, _ := newTestApp(t)

	resp := doRequest(t, app, http.MethodPost, "/api/v1/messages",
		`{"textMessage":{"text":"Привет мир"},"phoneNumbers":["+15550000001"]}`)
	if resp.status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusBadRequest, resp.body)
	}
	if msg := errorMessage(t, resp.body); !strings.Contains(msg, "invalid message text") {
		t.Errorf("error message = %q, want it to mention invalid message text", msg)
	}
}

func TestPost_TooLongText_400(t *testing.T) {
	app, _ := newTestApp(t)

	longText := strings.Repeat("a", testLongTextLength)
	resp := doRequest(t, app, http.MethodPost, "/api/v1/messages",
		`{"textMessage":{"text":"`+longText+`"},"phoneNumbers":["+15550000001"]}`)
	if resp.status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusBadRequest, resp.body)
	}
	if msg := errorMessage(t, resp.body); !strings.Contains(msg, "invalid message text") {
		t.Errorf("error message = %q, want it to mention invalid message text", msg)
	}
}

func TestPost_DataMessage_400(t *testing.T) {
	app, _ := newTestApp(t)

	resp := doRequest(t, app, http.MethodPost, "/api/v1/messages",
		`{"dataMessage":{"data":"AQID","port":1},"phoneNumbers":["+15550000001"]}`)
	if resp.status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusBadRequest, resp.body)
	}
	if msg := errorMessage(t, resp.body); !strings.Contains(msg, "not supported") {
		t.Errorf("error message = %q, want it to mention not supported", msg)
	}
}

func TestPost_EmptyBody_400(t *testing.T) {
	app, _ := newTestApp(t)

	resp := doRequest(t, app, http.MethodPost, "/api/v1/messages", "")
	if resp.status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusBadRequest, resp.body)
	}
	if msg := errorMessage(t, resp.body); msg == "" {
		t.Error("error message is empty")
	}
}

func TestPost_MissingPhoneNumbers_400(t *testing.T) {
	app, _ := newTestApp(t)

	resp := doRequest(t, app, http.MethodPost, "/api/v1/messages",
		`{"textMessage":{"text":"Hello"}}`)
	if resp.status != fiber.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusBadRequest, resp.body)
	}
}

// TestMapEnqueueError pins the service-error to HTTP-status mapping,
// including the duplicate-ID 409 branch that is not reachable over HTTP
// because Enqueue always generates fresh IDs.
func TestMapEnqueueError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "not supported", err: messages.ErrNotSupported, want: fiber.StatusBadRequest},
		{name: "invalid text", err: messages.ErrInvalidText, want: fiber.StatusBadRequest},
		{name: "invalid phone numbers", err: messages.ErrInvalidPhoneNumbers, want: fiber.StatusBadRequest},
		{name: "duplicate id", err: messages.ErrAlreadyExists, want: fiber.StatusConflict},
		{name: "internal", err: errors.New("boom"), want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mapped := mapEnqueueError(tc.err)
			if tc.want == 0 {
				if mapped != nil {
					t.Errorf("mapEnqueueError(%v) = %v, want nil", tc.err, mapped)
				}
				return
			}
			if mapped == nil {
				t.Fatalf("mapEnqueueError(%v) = nil, want status %d", tc.err, tc.want)
			}
			var fiberErr *fiber.Error
			if !errors.As(mapped, &fiberErr) {
				t.Fatalf("mapped error %v is not a *fiber.Error", mapped)
			}
			if fiberErr.Code != tc.want {
				t.Errorf("mapped status = %d, want %d", fiberErr.Code, tc.want)
			}
		})
	}
}

func TestGet_ByID_200(t *testing.T) {
	app, _ := newTestApp(t)

	created := doRequest(t, app, http.MethodPost, "/api/v1/messages",
		`{"textMessage":{"text":"Hello world"},"phoneNumbers":["+15550000001"]}`)
	state := unmarshalState(t, created.body)

	resp := doRequest(t, app, http.MethodGet, "/api/v1/messages/"+state.ID, "")
	if resp.status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusOK, resp.body)
	}
	got := unmarshalState(t, resp.body)
	if got.ID != state.ID {
		t.Errorf("ID = %q, want %q", got.ID, state.ID)
	}
	if got.State != smsgateway.ProcessingStatePending {
		t.Errorf("State = %q, want %q", got.State, smsgateway.ProcessingStatePending)
	}
}

func TestGet_Missing_404(t *testing.T) {
	app, _ := newTestApp(t)

	resp := doRequest(t, app, http.MethodGet, "/api/v1/messages/does-not-exist", "")
	if resp.status != fiber.StatusNotFound {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusNotFound, resp.body)
	}
	if msg := errorMessage(t, resp.body); !strings.Contains(msg, "not found") {
		t.Errorf("error message = %q, want it to mention not found", msg)
	}
}

func TestList_Empty_200WithTotalCount(t *testing.T) {
	app, _ := newTestApp(t)

	resp := doRequest(t, app, http.MethodGet, "/api/v1/messages", "")
	if resp.status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusOK, resp.body)
	}
	if total := resp.header.Get("X-Total-Count"); total != "0" {
		t.Errorf("X-Total-Count = %q, want 0", total)
	}

	var list []smsgateway.MessageState
	if err := json.Unmarshal(resp.body, &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("list = %+v, want empty", list)
	}
}

// seedListData creates three messages with distinct created_at values:
// two Pending (oldest and middle) and one Sent (newest).
func seedListData(t *testing.T, repo *messages.Repository) []string {
	t.Helper()

	base := time.Date(testSeedBaseYear, 8, 21, 12, 0, 0, 0, time.UTC)
	ids := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	}
	for i, id := range ids {
		state := messages.StatePending
		if i == len(ids)-1 {
			state = messages.StateSent
		}
		if _, err := repo.Create(
			context.Background(),
			newSeedMessage(id, base.Add(time.Duration(i)*time.Minute), state),
		); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	return ids
}

func TestList_PaginationSortAndStateFilter(t *testing.T) {
	app, repo := newTestApp(t)
	ids := seedListData(t, repo)

	all := doRequest(t, app, http.MethodGet, "/api/v1/messages", "")
	if all.status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", all.status, fiber.StatusOK, all.body)
	}
	if total := all.header.Get("X-Total-Count"); total != "3" {
		t.Errorf("X-Total-Count = %q, want 3", total)
	}

	var list []smsgateway.MessageState
	if err := json.Unmarshal(all.body, &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != testTotalMessages {
		t.Fatalf("list length = %d, want %d", len(list), testTotalMessages)
	}
	// Default sort is -created_at (descending): newest (Sent) first.
	if list[0].ID != ids[2] || list[0].State != smsgateway.ProcessingStateSent {
		t.Errorf("list[0] = %s (%s), want %s (Sent)", list[0].ID, list[0].State, ids[2])
	}

	page := doRequest(t, app, http.MethodGet, "/api/v1/messages?limit=2", "")
	if page.status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", page.status, fiber.StatusOK, page.body)
	}
	if total := page.header.Get("X-Total-Count"); total != "3" {
		t.Errorf("paged X-Total-Count = %q, want 3", total)
	}
	var pageList []smsgateway.MessageState
	if err := json.Unmarshal(page.body, &pageList); err != nil {
		t.Fatalf("unmarshal page: %v", err)
	}
	if len(pageList) != testPageLimit {
		t.Errorf("page length = %d, want %d", len(pageList), testPageLimit)
	}

	offsetPage := doRequest(t, app, http.MethodGet, "/api/v1/messages?limit=2&offset=2", "")
	var offsetList []smsgateway.MessageState
	if err := json.Unmarshal(offsetPage.body, &offsetList); err != nil {
		t.Fatalf("unmarshal offset page: %v", err)
	}
	if len(offsetList) != 1 || offsetList[0].ID != ids[0] {
		t.Errorf("offset page = %+v, want [%s]", offsetList, ids[0])
	}

	pending := doRequest(t, app, http.MethodGet, "/api/v1/messages?state=Pending", "")
	var pendingList []smsgateway.MessageState
	if err := json.Unmarshal(pending.body, &pendingList); err != nil {
		t.Fatalf("unmarshal pending: %v", err)
	}
	if total := pending.header.Get("X-Total-Count"); total != "2" {
		t.Errorf("pending X-Total-Count = %q, want %d", total, testPendingCount)
	}
	if len(pendingList) != testPendingCount {
		t.Errorf("pending length = %d, want %d", len(pendingList), testPendingCount)
	}

	ascending := doRequest(t, app, http.MethodGet, "/api/v1/messages?sort=created_at", "")
	var ascList []smsgateway.MessageState
	if err := json.Unmarshal(ascending.body, &ascList); err != nil {
		t.Fatalf("unmarshal ascending: %v", err)
	}
	if len(ascList) == 0 || ascList[0].ID != ids[0] {
		t.Errorf("ascending first = %+v, want %s", ascList, ids[0])
	}

	descending := doRequest(t, app, http.MethodGet, "/api/v1/messages?sort=-created_at", "")
	var descList []smsgateway.MessageState
	if err := json.Unmarshal(descending.body, &descList); err != nil {
		t.Fatalf("unmarshal descending: %v", err)
	}
	if len(descList) == 0 || descList[0].ID != ids[2] {
		t.Errorf("descending first = %+v, want %s", descList, ids[2])
	}

	// Accepted-but-ignored parameters must be tolerated (client-go compat).
	ignored := doRequest(
		t,
		app,
		http.MethodGet,
		"/api/v1/messages?deviceId=some-device&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z&includeContent=true",
		"",
	)
	if ignored.status != fiber.StatusOK {
		t.Fatalf("ignored-params status = %d, want %d (body %s)", ignored.status, fiber.StatusOK, ignored.body)
	}
	if total := ignored.header.Get("X-Total-Count"); total != "3" {
		t.Errorf("ignored-params X-Total-Count = %q, want 3", total)
	}
}

func TestList_InvalidParams_400(t *testing.T) {
	app, _ := newTestApp(t)

	for _, target := range []string{
		"/api/v1/messages?sort=bogus",
		"/api/v1/messages?limit=101",
		"/api/v1/messages?limit=0",
		"/api/v1/messages?limit=-1",
	} {
		resp := doRequest(t, app, http.MethodGet, target, "")
		if resp.status != fiber.StatusBadRequest {
			t.Errorf("%s status = %d, want %d (body %s)", target, resp.status, fiber.StatusBadRequest, resp.body)
		}
	}
}

func TestDelete_CancelPending_200(t *testing.T) {
	app, _ := newTestApp(t)

	created := doRequest(t, app, http.MethodPost, "/api/v1/messages",
		`{"textMessage":{"text":"Hello world"},"phoneNumbers":["+15550000001"]}`)
	state := unmarshalState(t, created.body)

	resp := doRequest(t, app, http.MethodDelete, "/api/v1/messages/"+state.ID, "")
	if resp.status != fiber.StatusOK {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusOK, resp.body)
	}
	cancelled := unmarshalState(t, resp.body)
	if cancelled.ID != state.ID {
		t.Errorf("ID = %q, want %q", cancelled.ID, state.ID)
	}
	if cancelled.State != smsgateway.ProcessingStateCancelled {
		t.Errorf("State = %q, want %q", cancelled.State, smsgateway.ProcessingStateCancelled)
	}

	// A second cancel must fail: the message is no longer pending.
	again := doRequest(t, app, http.MethodDelete, "/api/v1/messages/"+state.ID, "")
	if again.status != fiber.StatusBadRequest {
		t.Fatalf("second cancel status = %d, want %d (body %s)", again.status, fiber.StatusBadRequest, again.body)
	}
	if msg := errorMessage(t, again.body); !strings.Contains(msg, "not pending") {
		t.Errorf("error message = %q, want it to mention not pending", msg)
	}
}

func TestDelete_Missing_404(t *testing.T) {
	app, _ := newTestApp(t)

	resp := doRequest(t, app, http.MethodDelete, "/api/v1/messages/does-not-exist", "")
	if resp.status != fiber.StatusNotFound {
		t.Fatalf("status = %d, want %d (body %s)", resp.status, fiber.StatusNotFound, resp.body)
	}
	if msg := errorMessage(t, resp.body); !strings.Contains(msg, "not found") {
		t.Errorf("error message = %q, want it to mention not found", msg)
	}
}

func TestUnauthorized_401(t *testing.T) {
	app, _ := newTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

package messages_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	msgapi "github.com/android-sms-gateway/at-gateway/internal/server/api/messages"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/fiberfx/validation"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/go-playground/validator/v10"
	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	handlerText  = "hello from handler"
	handlerPhone = "79000000001"

	handlerRepoStartTimeout = 10 * time.Second
	handlerRepoStopTimeout  = 5 * time.Second
	handlerRepoSingleConn   = 1
)

// handlerEnv bundles the fiber app (wired like the server module: validation
// middleware + the central error handler) with the service it calls.
type handlerEnv struct {
	app *fiber.App
	svc *messages.Service
}

// newHandlerEnv boots the persistence graph (sqlfx + goosefx + bunfx +
// db.Module over in-memory SQLite, embedded migrations applied), a real
// devices service over a temp storage file and the messages service, then
// registers the handler exactly like internal/server/module.go does (minus
// auth and the other handlers).
func newHandlerEnv(t *testing.T) handlerEnv {
	t.Helper()

	_, repo := newHandlerRepo(t)

	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())
	svc := messages.NewService(messages.Config{}, repo, devicesSvc, nil, nil, zap.NewNop())

	app := fiber.New(fiber.Config{})
	v1 := app.Group("/api/v1")
	v1.Use(validation.Middleware)
	msgapi.NewHandler(svc, zap.NewNop(), validator.New()).Register(v1)

	return handlerEnv{app: app, svc: svc}
}

// newHandlerRepo boots the full persistence graph against an in-memory SQLite
// database so the embedded migrations are applied, enables foreign keys and
// returns the bun handle plus the repository.
func newHandlerRepo(t *testing.T) (*bun.DB, *messages.Repository) {
	t.Helper()

	var (
		sqldb *sql.DB
		bundb *bun.DB
	)

	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Supply(sqlfx.Config{
			URL:             "sqlite://:memory:",
			ConnMaxIdleTime: 0,
			ConnMaxLifetime: 0,
			MaxOpenConns:    handlerRepoSingleConn,
			MaxIdleConns:    handlerRepoSingleConn,
		}),
		db.Module(),
		sqlfx.Module(),
		goosefx.Module(),
		bunfx.Module(),
		fx.Invoke(func(sqlDB *sql.DB, bunDB *bun.DB) {
			sqldb = sqlDB
			bundb = bunDB
		}),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), handlerRepoStartTimeout)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), handlerRepoStopTimeout)
		defer cancelStop()
		if err := app.Stop(stopCtx); err != nil {
			t.Errorf("stop app: %v", err)
		}
	})

	if _, err := sqldb.ExecContext(context.Background(), "PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}

	return bundb, messages.NewRepository(bundb)
}

// handlerEnqueue creates a text message through the service (the same entry
// point the handler uses) and returns its ID.
func handlerEnqueue(t *testing.T, svc *messages.Service, extID string, phones ...string) string {
	t.Helper()
	msg, err := svc.Enqueue(context.Background(), messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: handlerText},
		},
		ExtID:        extID,
		PhoneNumbers: phones,
	})
	if err != nil {
		t.Fatalf("Enqueue(%q): %v", extID, err)
	}

	return msg.ID
}

// doRequest runs one request through the fiber app and returns the response.
func doRequest(t *testing.T, app *fiber.App, method, path, body string) *http.Response {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() {
		_ = resp.Body.Close()
	})

	return resp
}

// decodeBody unmarshals the response body into out.
func decodeBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func TestHandler_PostMessage_202Accepted(t *testing.T) {
	env := newHandlerEnv(t)

	resp := doRequest(t, env.app, http.MethodPost, "/api/v1/messages",
		`{"phoneNumbers":["`+handlerPhone+`"],"textMessage":{"text":"`+handlerText+`"}}`)

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	var got smsgateway.MessageState
	decodeBody(t, resp, &got)
	if got.ID == "" {
		t.Fatal("body ID is empty, want the generated ext_id")
	}
	if got.State != smsgateway.ProcessingStatePending {
		t.Fatalf("state = %q, want Pending", got.State)
	}
	if got.DeviceID == "" {
		t.Fatal("deviceId is empty, want the local device ID")
	}
	if len(got.Recipients) != 1 || got.Recipients[0].PhoneNumber != handlerPhone {
		t.Fatalf("recipients = %+v, want [%s]", got.Recipients, handlerPhone)
	}
	if got.Recipients[0].State != smsgateway.ProcessingStatePending {
		t.Fatalf("recipient state = %q, want Pending", got.Recipients[0].State)
	}
	if len(got.States) != 1 {
		t.Fatalf("states = %v, want a single Pending entry", got.States)
	}

	wantLocation := "/api/v1/messages/" + got.ID
	if location := resp.Header.Get("Location"); location != wantLocation {
		t.Fatalf("Location = %q, want %q", location, wantLocation)
	}
}

func TestHandler_PostMessage_Validation400(t *testing.T) {
	env := newHandlerEnv(t)

	resp := doRequest(t, env.app, http.MethodPost, "/api/v1/messages", `{}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}

	var body struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	decodeBody(t, resp, &body)
	if body.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", body.Code)
	}
	if !strings.Contains(body.Message, "PhoneNumbers is required") {
		t.Fatalf("message = %q, want a phoneNumbers validation failure", body.Message)
	}
}

func TestHandler_ListMessages_200WithCount(t *testing.T) {
	env := newHandlerEnv(t)
	handlerEnqueue(t, env.svc, "list-1", handlerPhone)
	handlerEnqueue(t, env.svc, "list-2", handlerPhone)
	handlerEnqueue(t, env.svc, "list-3", handlerPhone)

	resp := doRequest(t, env.app, http.MethodGet, "/api/v1/messages", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if total := resp.Header.Get("X-Total-Count"); total != "3" {
		t.Fatalf("X-Total-Count = %q, want 3", total)
	}

	var items []json.RawMessage
	decodeBody(t, resp, &items)
	if len(items) != 3 {
		t.Fatalf("list length = %d, want 3", len(items))
	}

	// The pagination limit is honored while the count stays global.
	page := doRequest(t, env.app, http.MethodGet, "/api/v1/messages?limit=2", "")
	if total := page.Header.Get("X-Total-Count"); total != "3" {
		t.Fatalf("limited X-Total-Count = %q, want 3", total)
	}
	var pageItems []json.RawMessage
	decodeBody(t, page, &pageItems)
	if len(pageItems) != 2 {
		t.Fatalf("limited list length = %d, want 2", len(pageItems))
	}
}

func TestHandler_ListMessages_Empty200(t *testing.T) {
	env := newHandlerEnv(t)

	resp := doRequest(t, env.app, http.MethodGet, "/api/v1/messages", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if total := resp.Header.Get("X-Total-Count"); total != "0" {
		t.Fatalf("X-Total-Count = %q, want 0", total)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got smsgateway.GetMessagesResponse
	if err = json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("list = %#v, want an empty array", got)
	}
}

// TestHandler_ListMessages_WireShape pins the list response to the client-go
// wire shape (GetMessagesResponse / MessageState keys) instead of the raw
// domain model Go field names.
func TestHandler_ListMessages_WireShape(t *testing.T) {
	env := newHandlerEnv(t)
	id := handlerEnqueue(t, env.svc, "wire-1", handlerPhone)

	resp := doRequest(t, env.app, http.MethodGet, "/api/v1/messages", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var got smsgateway.GetMessagesResponse
	if err = json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal into GetMessagesResponse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list length = %d, want 1", len(got))
	}
	item := got[0]
	if item.ID != id {
		t.Fatalf("id = %q, want %q", item.ID, id)
	}
	if item.State != smsgateway.ProcessingStatePending {
		t.Fatalf("state = %q, want Pending", item.State)
	}
	if item.DeviceID == "" {
		t.Fatal("deviceId is empty, want the local device ID")
	}
	if len(item.Recipients) != 1 || item.Recipients[0].PhoneNumber != handlerPhone {
		t.Fatalf("recipients = %+v, want [%s]", item.Recipients, handlerPhone)
	}
	if item.TextMessage == nil || item.TextMessage.Text != handlerText {
		t.Fatalf("textMessage = %+v, want %q", item.TextMessage, handlerText)
	}
	if _, ok := item.States[string(smsgateway.ProcessingStatePending)]; !ok {
		t.Fatalf("states = %v, want a Pending entry", item.States)
	}

	// The raw JSON must carry only wire keys: no domain Go field names leak.
	var raw []map[string]json.RawMessage
	if err = json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal raw body: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("raw list length = %d, want 1", len(raw))
	}
	for _, leak := range []string{
		"ID", "State", "DeviceID", "Recipients", "PhoneNumber", "States",
		"TextContent", "SimNumber", "WithDeliveryReport", "TTL", "Priority",
	} {
		if _, found := raw[0][leak]; found {
			t.Fatalf("wire key %q leaked from the raw domain model", leak)
		}
	}
	for _, key := range []string{
		"id", "deviceId", "state", "isHashed", "isEncrypted",
		"recipients", "states", "textMessage",
	} {
		if _, found := raw[0][key]; !found {
			t.Fatalf("missing wire key %q", key)
		}
	}
	var recipients []map[string]json.RawMessage
	if err = json.Unmarshal(raw[0]["recipients"], &recipients); err != nil {
		t.Fatalf("unmarshal recipients: %v", err)
	}
	for _, leak := range []string{"PhoneNumber", "State", "States", "RefID"} {
		if _, present := recipients[0][leak]; present {
			t.Fatalf("recipient wire key %q leaked from the raw domain model", leak)
		}
	}
	for _, key := range []string{"phoneNumber", "state"} {
		if _, present := recipients[0][key]; !present {
			t.Fatalf("missing recipient wire key %q", key)
		}
	}
}

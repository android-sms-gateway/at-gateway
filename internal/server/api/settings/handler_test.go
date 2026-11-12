package settings_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/ping"
	apisettings "github.com/android-sms-gateway/at-gateway/internal/server/api/settings"
	"github.com/android-sms-gateway/at-gateway/internal/settings"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
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

	testSingleConn = 1

	// handlerRetryCount and handlerInterval are the fixture defaults the
	// handler test pins in GET responses.
	handlerRetryCount = 15
	handlerInterval   = 60

	// settingsPath is the only route this handler suite exercises.
	settingsPath = "/api/v1/settings"
)

// newPersistence builds the persistence graph against an in-memory SQLite
// database so the embedded migrations (incl. the settings table) apply before
// the handler under test is wired.
func newPersistence(t *testing.T) *settings.Repository {
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

	return settings.NewRepository(bunDB)
}

// newHandlerApp boots the settings handler the way module.go wires it: the
// fiberfx validation middleware wraps the handler group, the global error
// handler formats wire bodies, and the service runs over the full persistence
// graph. Basic auth is NOT installed (same harness choice as the webhooks
// handler_test - the v1 group auth is exercised by module wiring, not here).
func newHandlerApp(t *testing.T) (*fiber.App, *settings.Repository) {
	t.Helper()

	repo := newPersistence(t)
	pingSettings := ping.NewSettings(
		settings.Scoped(repo, "ping"),
		ping.Config{Interval: handlerInterval * time.Second},
	)
	webhooksSettings := webhooks.NewSettings(
		settings.Scoped(repo, "webhooks"),
		handlerWebhooksConfig(),
	)
	handler := apisettings.NewHandler(
		settings.NewService([]settings.Section{pingSettings, webhooksSettings}),
		validator.New(),
	)

	app := fiber.New(fiber.Config{
		ErrorHandler: fiberfx.NewJSONErrorHandler(zap.NewNop()),
	})
	app.Use(fiberval.Middleware)
	handler.Register(app.Group("/api/v1"))

	return app, repo
}

// handlerWebhooksConfig is the fixture default backing the facade: retry 15,
// no configured signing key, and an inert queue (this suite never runs the
// worker).
func handlerWebhooksConfig() webhooks.Config {
	return webhooks.Config{
		RetryCount: handlerRetryCount,
		SigningKey: "",
		Queue: webhooks.QueueConfig{
			BatchSize:              0,
			RequestTimeout:         0,
			DialTimeout:            0,
			RetryBaseDelay:         0,
			IdleDelay:              0,
			StuckProcessingTimeout: 0,
			CleanupRetention:       0,
		},
	}
}

// doRequest performs a request against the in-memory app and returns the
// status plus the raw response body.
func doRequest(t *testing.T, app *fiber.App, method string, body []byte) (int, []byte) {
	t.Helper()

	req, err := http.NewRequest(method, settingsPath, bytes.NewReader(body))
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

// decodeSettings unmarshals a settings response body.
func decodeSettings(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return parsed
}

// TestGetSettings_ReturnsConfiguredDefaults pins the Wave-1 observable: GET
// answers 200 with the effective settings built from the configured defaults,
// only the honored sections present.
func TestGetSettings_ReturnsConfiguredDefaults(t *testing.T) {
	t.Parallel()

	app, _ := newHandlerApp(t)

	status, body := doRequest(t, app, http.MethodGet, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}

	parsed := decodeSettings(t, body)
	ping, ok := parsed["ping"].(map[string]any)
	if !ok {
		t.Fatalf("ping section missing in %s", body)
	}
	if got := ping["interval_seconds"]; got != float64(handlerInterval) {
		t.Errorf("interval_seconds = %v, want %d", got, handlerInterval)
	}
	webhooks, ok := parsed["webhooks"].(map[string]any)
	if !ok {
		t.Fatalf("webhooks section missing in %s", body)
	}
	if got := webhooks["retry_count"]; got != float64(handlerRetryCount) {
		t.Errorf("retry_count = %v, want %d", got, handlerRetryCount)
	}
	if _, present := webhooks["signing_key"]; present {
		t.Errorf("signing_key present in %s, want hidden", body)
	}
	for _, absent := range []string{"encryption", "messages", "logs", "gateway", "receiver"} {
		if _, present := parsed[absent]; present {
			t.Errorf("section %q present in %s, want absent", absent, body)
		}
	}
}

// TestGetSettings_ReflectsStoredRows pins the read path: rows the
// repository holds surface on GET while untouched keys keep their defaults,
// and a stored signing key stays hidden.
func TestGetSettings_ReflectsStoredRows(t *testing.T) {
	t.Parallel()

	app, repo := newHandlerApp(t)

	if err := repo.Set(context.Background(), "ping.interval_seconds", "30"); err != nil {
		t.Fatalf("seed interval: %v", err)
	}
	if err := repo.Set(context.Background(), "webhooks.signing_key", "stored-secret"); err != nil {
		t.Fatalf("seed signing key: %v", err)
	}

	status, body := doRequest(t, app, http.MethodGet, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}

	parsed := decodeSettings(t, body)
	ping := parsed["ping"].(map[string]any)
	if got := ping["interval_seconds"]; got != float64(30) {
		t.Errorf("interval_seconds = %v, want 30 (stored wins)", got)
	}
	webhooks := parsed["webhooks"].(map[string]any)
	if got := webhooks["retry_count"]; got != float64(handlerRetryCount) {
		t.Errorf("retry_count = %v, want %d (default)", got, handlerRetryCount)
	}
	if _, present := webhooks["signing_key"]; present {
		t.Errorf("signing_key present in %s, want hidden", body)
	}
}

// TestPatch_MergesProvidedFields pins PATCH semantics: only the provided key
// changes, its sibling keeps the configured default, the response IS the
// effective view, and the write survives on GET.
func TestPatch_MergesProvidedFields(t *testing.T) {
	t.Parallel()

	app, _ := newHandlerApp(t)

	status, body := doRequest(t, app, http.MethodPatch,
		[]byte(`{"webhooks":{"retry_count":7}}`))
	if status != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200; body %s", status, body)
	}

	parsed := decodeSettings(t, body)
	if got := parsed["ping"].(map[string]any)["interval_seconds"]; got != float64(handlerInterval) {
		t.Errorf("response interval_seconds = %v, want %d (default kept)", got, handlerInterval)
	}
	if got := parsed["webhooks"].(map[string]any)["retry_count"]; got != float64(7) {
		t.Errorf("response retry_count = %v, want 7", got)
	}

	status, body = doRequest(t, app, http.MethodGet, nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body %s", status, body)
	}
	parsed = decodeSettings(t, body)
	if got := parsed["webhooks"].(map[string]any)["retry_count"]; got != float64(7) {
		t.Errorf("GET retry_count = %v, want 7 (persisted)", got)
	}
}

// TestPut_ReplacesAndResetsUnprovided pins PUT semantics: a previous PATCH's
// sibling value is un-set by the replace, so the configured default resumes,
// while the provided key takes the new value. An empty body resets everything.
func TestPut_ReplacesAndResetsUnprovided(t *testing.T) {
	t.Parallel()

	app, _ := newHandlerApp(t)

	status, body := doRequest(t, app, http.MethodPatch,
		[]byte(`{"ping":{"interval_seconds":30},"webhooks":{"retry_count":7}}`))
	if status != http.StatusOK {
		t.Fatalf("seed PATCH status = %d, want 200; body %s", status, body)
	}

	status, body = doRequest(t, app, http.MethodPut,
		[]byte(`{"ping":{"interval_seconds":25}}`))
	if status != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body %s", status, body)
	}

	parsed := decodeSettings(t, body)
	if got := parsed["ping"].(map[string]any)["interval_seconds"]; got != float64(25) {
		t.Errorf("interval_seconds = %v, want 25 (provided)", got)
	}
	if got := parsed["webhooks"].(map[string]any)["retry_count"]; got != float64(handlerRetryCount) {
		t.Errorf("retry_count = %v, want %d (unprovided -> default resumed)", got, handlerRetryCount)
	}

	status, body = doRequest(t, app, http.MethodPut, []byte(`{}`))
	if status != http.StatusOK {
		t.Fatalf("empty PUT status = %d, want 200; body %s", status, body)
	}
	parsed = decodeSettings(t, body)
	if got := parsed["ping"].(map[string]any)["interval_seconds"]; got != float64(handlerInterval) {
		t.Errorf("empty PUT interval_seconds = %v, want %d (reset)", got, handlerInterval)
	}
}

// TestPatch_SigningKeyRejectedByClientGoTag pins the INTERIM edge behavior:
// the client-go omitempty,isdefault tag rejects every non-empty signing_key
// under DecorateWithBodyEx, so the write answers 400 and stores nothing
// until the client-go tag is fixed (owner decision: validation via
// DecorateWithBodyEx for now). Write-only read semantics are pinned
// separately by the seeded-row GET tests above.
func TestPatch_SigningKeyRejectedByClientGoTag(t *testing.T) {
	t.Parallel()

	app, repo := newHandlerApp(t)
	ctx := context.Background()

	status, body := doRequest(t, app, http.MethodPatch,
		[]byte(`{"webhooks":{"signing_key":"example-key"}}`))
	if status != http.StatusBadRequest {
		t.Fatalf("PATCH status = %d, want 400 (isdefault tag); body %s", status, body)
	}
	if _, ok, err := repo.Get(ctx, "webhooks.signing_key"); err != nil {
		t.Fatalf("repo.Get: %v", err)
	} else if ok {
		t.Error("signing key stored despite the 400, want no write")
	}

	status, body = doRequest(t, app, http.MethodGet, nil)
	if status != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body %s", status, body)
	}
	if _, present := decodeSettings(t, body)["webhooks"].(map[string]any)["signing_key"]; present {
		t.Errorf("signing_key echoed on GET %s, want hidden", body)
	}
}

// TestPatch_UnknownSectionAcceptedAndDropped pins the local-DTO contract: a
// section this gateway does not honor is accepted with 200 and silently
// dropped - it never appears in the response and never 400s.
func TestPatch_UnknownSectionAcceptedAndDropped(t *testing.T) {
	t.Parallel()

	app, _ := newHandlerApp(t)

	status, body := doRequest(t, app, http.MethodPatch,
		[]byte(`{"messages":{"send_interval_min":1},"ping":{"interval_seconds":45}}`))
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}

	parsed := decodeSettings(t, body)
	if _, present := parsed["messages"]; present {
		t.Errorf("messages section present in %s, want dropped", body)
	}
	if got := parsed["ping"].(map[string]any)["interval_seconds"]; got != float64(45) {
		t.Errorf("interval_seconds = %v, want 45 (honored sibling applied)", got)
	}
}

// TestPutPatch_ValidationErrors pins the wire 400s: the local DTO keeps the
// client-go min=1 constraints on the honored fields, and a malformed section
// shape fails binding. signing_key is deliberately NOT in this list - it is a
// free string by design (see dtos.go).
func TestPutPatch_ValidationErrors(t *testing.T) {
	t.Parallel()

	app, _ := newHandlerApp(t)

	for _, tt := range []struct {
		method string
		body   string
		desc   string
	}{
		{method: http.MethodPut, body: `{"ping":{"interval_seconds":0}}`, desc: "interval min=1"},
		{method: http.MethodPatch, body: `{"webhooks":{"retry_count":0}}`, desc: "retry min=1"},
		{method: http.MethodPut, body: `{"ping":"notanobject"}`, desc: "malformed section"},
	} {
		status, body := doRequest(t, app, tt.method, []byte(tt.body))
		if status != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400; body %s", tt.method, tt.desc, status, body)
		}
	}
}

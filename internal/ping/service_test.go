//nolint:paralleltest // every fixture swaps the process-global prometheus.DefaultRegisterer in newPingMetrics, so fixtures must not run concurrently
package ping_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/ping"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	// The intervals under test are 20-25ms, so a 5s budget is generous; it
	// exists only to turn a stuck ticker into a failure instead of a hang.
	pingWaitTimeout = 5 * time.Second

	testStartTimeout = 10 * time.Second
	testStopTimeout  = 5 * time.Second

	testSingleConn  = 1
	testSigningKey  = "ping-test-key"
	testFixtureURL  = "https://example.com/ping"
	postContentType = "application/json"
)

// TestRun_OneEnvelopePerWebhookPerTick pins the per-tick fan-out: one
// envelope per REGISTERED webhook for system:ping, exactly once. The interval
// is long enough that a second tick cannot occur inside the test window, so the
// counts are exact rather than "at least".
func TestRun_OneEnvelopePerWebhookPerTick(t *testing.T) {
	const interval = 2 * time.Second

	svc, bunDB := newPingFixture(t, registryConfig())
	ctx := context.Background()

	for _, id := range []string{"ping-a", "ping-b"} {
		if err := svc.Replace(ctx, newPingWebhook(
			id,
			"https://example.com/"+id,
		)); err != nil {
			t.Fatalf("replace %s: %v", id, err)
		}
	}

	pingSvc := startPingService(t, svc, interval)
	waitForQueueRows(t, bunDB, 2)

	if got := countQueueRows(t, bunDB); got != 2 {
		t.Fatalf("webhook_queue rows = %d, want exactly 2 (one per registered webhook)", got)
	}

	// A stop is prompt and final: Run returns, and no further tick can enqueue.
	pingSvc.stop(t)
	if got := countQueueRows(t, bunDB); got != 2 {
		t.Errorf("webhook_queue rows = %d after stop, want the same 2", got)
	}
}

// TestRun_TicksRepeatedlyAndEmitsOnlyPingEnvelopes pins the steady
// state: the ticker keeps emitting while it runs (one NEW envelope per tick, so
// the queue grows), stops cleanly on context cancellation and leaves no tick
// behind. Every stored envelope is a system:ping one.
func TestRun_TicksRepeatedlyAndEmitsOnlyPingEnvelopes(t *testing.T) {
	const interval = 20 * time.Millisecond

	svc, bunDB := newPingFixture(t, registryConfig())
	ctx := context.Background()

	if err := svc.Replace(ctx, newPingWebhook(
		"ping-repeat",
		testFixtureURL,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	pingSvc := startPingService(t, svc, interval)
	waitForQueueRows(t, bunDB, 3)
	pingSvc.stop(t)

	rows := countQueueRows(t, bunDB)
	if rows < 3 {
		t.Fatalf("webhook_queue rows = %d, want at least 3 ticks", rows)
	}

	stored := storedPayloadList(t, bunDB)
	if len(stored) != rows {
		t.Fatalf("read %d envelopes, want %d rows", len(stored), rows)
	}

	ids := make(map[string]struct{}, len(stored))
	for index, raw := range stored {
		var envelope struct {
			Event   smsgateway.WebhookEvent `json:"event"`
			Payload json.RawMessage         `json:"payload"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatalf("decode stored envelope %d: %v", index, err)
		}
		if envelope.Event != smsgateway.WebhookEventSystemPing {
			t.Errorf("envelope %d event = %q, want system:ping", index, envelope.Event)
		}
		if string(envelope.Payload) != "{}" {
			t.Errorf("envelope %d payload = %s, want {}", index, envelope.Payload)
		}
		if _, duplicate := ids[raw]; duplicate {
			t.Errorf("envelope %d repeats an earlier envelope, want one new envelope per tick", index)
		}
		ids[raw] = struct{}{}
	}

	// No tick survives the stop: five intervals of silence change nothing.
	time.Sleep(5 * interval)
	if got := countQueueRows(t, bunDB); got != rows {
		t.Errorf("webhook_queue rows = %d after stop, want the frozen %d", got, rows)
	}
}

// TestRun_NonPositiveIntervalDisablesEmission pins the opt-out: a zero
// (or negative) interval makes Run return immediately without a single tick,
// which is the zero-interval opt-out contract.
func TestRun_NonPositiveIntervalDisablesEmission(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{name: "zero disables", interval: 0},
		{name: "negative disables", interval: -time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, bunDB := newPingFixture(t, registryConfig())
			ctx := context.Background()

			if err := svc.Replace(ctx, newPingWebhook(
				"ping-off",
				testFixtureURL,
			)); err != nil {
				t.Fatalf("replace webhook: %v", err)
			}

			pingSvc := startPingService(t, svc, tt.interval)
			pingSvc.stop(t)

			if got := countQueueRows(t, bunDB); got != 0 {
				t.Errorf("webhook_queue rows = %d, want 0 with a disabled interval", got)
			}
		})
	}
}

// TestRun_PostsOneSignedEnvelopePerTick is the end-to-end contract of
// the ticker: the signed POST reaches the registered endpoint, its body is the
// stored envelope with the empty payload object, and X-Signature is the HMAC
// over exactly those bytes plus the unix-seconds X-Timestamp.
func TestRun_PostsOneSignedEnvelopePerTick(t *testing.T) {
	const interval = 25 * time.Millisecond

	log := &requestLog{
		mu:       sync.Mutex{},
		requests: []capturedRequest{},
	}
	server := startEndpoint(t, log)

	svc, bunDB := newPingFixture(t, deliveryConfig())
	ctx := context.Background()

	if err := svc.Replace(ctx, newPingWebhook(
		"ping-post",
		server.URL,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	worker := startWorker(t, svc)
	pingSvc := startPingService(t, svc, interval)
	waitForPostCount(t, log, 3)

	// Stop the ticker first, then let the delivery worker finish what it has
	// already enqueued: every heartbeat must end up completed, not merely
	// received.
	pingSvc.stop(t)
	waitForCompletedRows(t, bunDB)
	worker.stop(t)

	requests := log.snapshot()
	if len(requests) < 3 {
		t.Fatalf("POST count = %d, want at least 3 ticks", len(requests))
	}
	mac := hmac.New(sha256.New, []byte(testSigningKey))
	for index, request := range requests {
		if request.contentType != postContentType {
			t.Errorf("POST %d Content-Type = %q, want %s", index+1, request.contentType, postContentType)
		}
		var envelope struct {
			Event   smsgateway.WebhookEvent `json:"event"`
			Payload json.RawMessage         `json:"payload"`
		}
		if err := json.Unmarshal(request.body, &envelope); err != nil {
			t.Fatalf("decode POST %d body: %v", index+1, err)
		}
		if envelope.Event != smsgateway.WebhookEventSystemPing {
			t.Errorf("POST %d event = %q, want system:ping", index+1, envelope.Event)
		}
		if string(envelope.Payload) != "{}" {
			t.Errorf("POST %d payload = %s, want {}", index+1, envelope.Payload)
		}

		mac.Reset()
		_, _ = mac.Write(request.body)
		_, _ = mac.Write([]byte(request.timestamp))
		if want := hex.EncodeToString(mac.Sum(nil)); request.signature != want {
			t.Errorf("POST %d X-Signature = %q, want %q over the posted bytes", index+1, request.signature, want)
		}
	}
}

// newPingFixture builds a real webhooks service over in-memory SQLite - so the
// embedded migrations are applied before the queue exists - and hands back the
// bun.DB, so a ticker test can count the rows it enqueued.
func newPingFixture(t *testing.T, config webhooks.Config) (*webhooks.Service, *bun.DB) {
	t.Helper()

	bunDB := newPersistence(t)
	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}
	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())
	service, err := webhooks.NewService(
		config,
		webhooks.NewRepository(bunDB),
		devicesSvc,
		storageSvc,
		newPingMetrics(),
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create webhooks service: %v", err)
	}

	return service, bunDB
}

// newPersistence builds the persistence graph (db.Module + sqlfx + goosefx +
// bunfx) against an in-memory SQLite database, so the embedded migrations are
// applied before the webhooks service is created.
func newPersistence(t *testing.T) *bun.DB {
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

	return bunDB
}

// newPingMetrics isolates the promauto registration of the webhooks counters:
// two fixtures in the same process must never collide on the default registry.
func newPingMetrics() *webhooks.Metrics {
	registerer := &prometheus.DefaultRegisterer
	previous := *registerer
	*registerer = prometheus.NewRegistry()
	defer func() {
		*registerer = previous
	}()

	return webhooks.NewMetrics()
}

// registryConfig is the emission-only webhooks fixture: these tests enqueue
// heartbeats and read the queue rows themselves, never deliver them.
func registryConfig() webhooks.Config {
	return webhooks.Config{
		RetryCount: 0,
		SigningKey: testSigningKey,
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

// deliveryConfig tunes the queue for the signed-POST test, where the webhooks
// delivery worker performs the requests.
func deliveryConfig() webhooks.Config {
	return webhooks.Config{
		RetryCount: 1,
		SigningKey: testSigningKey,
		Queue: webhooks.QueueConfig{
			BatchSize:              20,
			RequestTimeout:         500 * time.Millisecond,
			DialTimeout:            100 * time.Millisecond,
			RetryBaseDelay:         20 * time.Millisecond,
			IdleDelay:              5 * time.Millisecond,
			StuckProcessingTimeout: 100 * time.Millisecond,
			CleanupRetention:       24 * time.Hour,
		},
	}
}

// pingConfig is the ticker fixture: the cadence under test.
func pingConfig(interval time.Duration) ping.Config {
	return ping.Config{Interval: interval}
}

// newPingWebhook builds the registry fixture every ticker test uses: a
// device-wide webhook for system:ping.
func newPingWebhook(id string, url string) *smsgateway.Webhook {
	return &smsgateway.Webhook{
		ID:       id,
		DeviceID: nil,
		URL:      url,
		Event:    smsgateway.WebhookEventSystemPing,
	}
}

type pingHandle struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

// startPingService runs the ticker the way fx does: in its own goroutine,
// cancelled through the context.
func startPingService(t *testing.T, service *webhooks.Service, interval time.Duration) *pingHandle {
	t.Helper()

	pingSvc := ping.NewService(pingConfig(interval), service, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	handle := &pingHandle{
		cancel: cancel,
		done:   make(chan error, 1),
		once:   sync.Once{},
	}
	go func() {
		handle.done <- pingSvc.Run(ctx)
	}()
	t.Cleanup(func() {
		handle.stop(t)
	})

	return handle
}

func (h *pingHandle) stop(t *testing.T) {
	t.Helper()

	h.once.Do(func() {
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("ping service Run returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("ping service Run did not stop after context cancellation")
		}
	})
}

type workerHandle struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

// startWorker runs the webhooks delivery worker - the ONLY HTTP performer - the
// way fx does.
func startWorker(t *testing.T, service *webhooks.Service) *workerHandle {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	handle := &workerHandle{
		cancel: cancel,
		done:   make(chan error, 1),
		once:   sync.Once{},
	}
	go func() {
		handle.done <- service.Run(ctx)
	}()
	t.Cleanup(func() {
		handle.stop(t)
	})

	return handle
}

func (h *workerHandle) stop(t *testing.T) {
	t.Helper()

	h.once.Do(func() {
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Errorf("worker Run returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("worker Run did not stop after context cancellation")
		}
	})
}

type capturedRequest struct {
	body        []byte
	timestamp   string
	signature   string
	contentType string
}

type requestLog struct {
	mu       sync.Mutex
	requests []capturedRequest
}

func (l *requestLog) add(request capturedRequest) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.requests = append(l.requests, request)
}

func (l *requestLog) snapshot() []capturedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()

	requests := make([]capturedRequest, len(l.requests))
	copy(requests, l.requests)

	return requests
}

// startEndpoint serves the delivery worker: it captures the raw body and the
// signing headers of every request, then answers 200.
func startEndpoint(t *testing.T, log *requestLog) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}

		log.add(capturedRequest{
			body:        body,
			timestamp:   request.Header.Get("X-Timestamp"),
			signature:   request.Header.Get("X-Signature"),
			contentType: request.Header.Get("Content-Type"),
		})
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	return server
}

// countQueueRows counts every webhook_queue row regardless of status.
func countQueueRows(t *testing.T, bunDB *bun.DB) int {
	t.Helper()

	var count int
	if err := bunDB.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM webhook_queue`,
	).Scan(&count); err != nil {
		t.Fatalf("count webhook_queue rows: %v", err)
	}

	return count
}

// storedPayloadList reads the raw stored envelope bytes of every queue row.
func storedPayloadList(t *testing.T, bunDB *bun.DB) []string {
	t.Helper()

	rows, err := bunDB.QueryContext(context.Background(), `SELECT payload FROM webhook_queue`)
	if err != nil {
		t.Fatalf("read queue payloads: %v", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			t.Errorf("close payload rows: %v", closeErr)
		}
	}()

	payloads := make([]string, 0, 8)
	for rows.Next() {
		var payload string
		if scanErr := rows.Scan(&payload); scanErr != nil {
			t.Fatalf("scan queue payload: %v", scanErr)
		}
		payloads = append(payloads, payload)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatalf("iterate queue payloads: %v", rowsErr)
	}

	return payloads
}

// waitForQueueRows blocks until the queue holds at least want rows.
func waitForQueueRows(t *testing.T, bunDB *bun.DB, want int) {
	t.Helper()

	deadline := time.Now().Add(pingWaitTimeout)
	for {
		got := countQueueRows(t, bunDB)
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook_queue rows = %d, want at least %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForCompletedRows blocks until no queue row is left un-delivered, i.e. the
// worker completed every heartbeat the ticker enqueued. Completed rows are kept
// for the configured queue cleanup retention, so the queue does not empty.
func waitForCompletedRows(t *testing.T, bunDB *bun.DB) {
	t.Helper()

	deadline := time.Now().Add(pingWaitTimeout)
	for {
		var outstanding int
		if err := bunDB.QueryRowContext(
			context.Background(),
			`SELECT COUNT(*) FROM webhook_queue WHERE status <> ?`,
			string(webhooks.QueueStatusCompleted),
		).Scan(&outstanding); err != nil {
			t.Fatalf("count undelivered webhook_queue rows: %v", err)
		}
		if outstanding == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("undelivered webhook_queue rows = %d, want every ping delivered", outstanding)
		}
		time.Sleep(time.Millisecond)
	}
}

// waitForPostCount blocks until the endpoint has received at least want POSTs.
func waitForPostCount(t *testing.T, log *requestLog, want int) {
	t.Helper()

	deadline := time.Now().Add(pingWaitTimeout)
	for {
		if got := len(log.snapshot()); got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("POST count = %d, want at least %d", len(log.snapshot()), want)
		}
		time.Sleep(time.Millisecond)
	}
}

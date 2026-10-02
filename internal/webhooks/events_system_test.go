package webhooks_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
	"go.uber.org/zap"
)

// The intervals under test are 20-25ms, so a 5s budget is generous; it exists
// only to turn a stuck ticker into a failure instead of a hang.
const pingWaitTimeout = 5 * time.Second

// TestEmitSystemPing_RawEnvelopeCarriesAnEmptyJSONObject pins the system:ping
// wire contract on the RAW STORED ENVELOPE BYTES: the payload sub-document is
// byte-identical to the marshalled client-go SystemPingPayload, i.e. the empty
// JSON object {} - the accepted parity gap versus the Android gateway, which
// publishes {"health": {...}} there.
func TestEmitSystemPing_RawEnvelopeCarriesAnEmptyJSONObject(t *testing.T) {
	svc, repo, _, devicesSvc := newInboundFixture(t, inboundConfig())
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"ping-raw",
		nil,
		"https://example.com/ping",
		smsgateway.WebhookEventSystemPing,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	svc.EmitSystemPing()

	payloads := storedPayloads(t, repo)
	raw, ok := payloads["ping-raw"]
	if !ok {
		t.Fatalf("stored payloads = %v, want one for the registered ping webhook", payloads)
	}

	var envelope struct {
		ID        string                  `json:"id"`
		WebhookID string                  `json:"webhookId"`
		Event     smsgateway.WebhookEvent `json:"event"`
		DeviceID  string                  `json:"deviceId"`
		Payload   json.RawMessage         `json:"payload"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode stored envelope: %v", err)
	}
	if envelope.Event != smsgateway.WebhookEventSystemPing {
		t.Errorf("envelope event = %q, want %q", envelope.Event, smsgateway.WebhookEventSystemPing)
	}
	if envelope.WebhookID != "ping-raw" {
		t.Errorf("envelope webhookId = %q, want ping-raw", envelope.WebhookID)
	}
	if envelope.DeviceID != devicesSvc.Get().ID {
		t.Errorf("envelope deviceId = %q, want the local device", envelope.DeviceID)
	}
	if envelope.ID == "" {
		t.Error("envelope id is empty")
	}

	want, err := json.Marshal(smsgateway.SystemPingPayload{})
	if err != nil {
		t.Fatalf("marshal expected payload: %v", err)
	}
	if string(envelope.Payload) != string(want) {
		t.Errorf("stored payload = %s, want byte-identical %s", envelope.Payload, want)
	}
	if string(envelope.Payload) != "{}" {
		t.Errorf("stored payload = %s, want the empty JSON object {}", envelope.Payload)
	}
	if !strings.Contains(raw, `"payload":{}`) {
		t.Errorf("stored envelope %s does not carry the empty payload object", raw)
	}
}

// TestEmitSystemPing_NoMatchingWebhookWritesNoQueueRows pins the emit() no-op
// boundary for system:ping: with no webhook registered for the event (here a
// webhook for a different event is registered), the tick performs no queue
// writes at all.
func TestEmitSystemPing_NoMatchingWebhookWritesNoQueueRows(t *testing.T) {
	svc, _, bunDB, _ := newInboundFixture(t, inboundConfig())
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"other-event",
		nil,
		"https://example.com/other",
		smsgateway.WebhookEventSmsSent,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	svc.EmitSystemPing()

	if got := countQueueRows(t, bunDB); got != 0 {
		t.Errorf("webhook_queue rows = %d, want 0 when no webhook matches system:ping", got)
	}
}

// TestPingEmitter_OneEnvelopePerWebhookPerTick pins the per-tick fan-out: one
// envelope per REGISTERED webhook for system:ping, exactly once. The interval
// is long enough that a second tick cannot occur inside the test window, so the
// counts are exact rather than "at least".
func TestPingEmitter_OneEnvelopePerWebhookPerTick(t *testing.T) {
	const interval = 2 * time.Second

	svc, _, bunDB, _ := newInboundFixture(t, pingConfig(interval))
	ctx := context.Background()

	for _, id := range []string{"ping-a", "ping-b"} {
		if err := svc.Replace(ctx, newWebhookDTO(
			id,
			nil,
			"https://example.com/"+id,
			smsgateway.WebhookEventSystemPing,
		)); err != nil {
			t.Fatalf("replace %s: %v", id, err)
		}
	}

	emitter := startPingEmitter(t, svc, interval)
	waitForQueueRows(t, bunDB, 2)

	if got := countQueueRows(t, bunDB); got != 2 {
		t.Fatalf("webhook_queue rows = %d, want exactly 2 (one per registered webhook)", got)
	}

	// A stop is prompt and final: Run returns, and no further tick can enqueue.
	emitter.stop(t)
	if got := countQueueRows(t, bunDB); got != 2 {
		t.Errorf("webhook_queue rows = %d after stop, want the same 2", got)
	}
}

// TestPingEmitter_TicksRepeatedlyAndEmitsOnlyPingEnvelopes pins the steady
// state: the ticker keeps emitting while it runs (one NEW envelope per tick, so
// the queue grows), stops cleanly on context cancellation and leaves no tick
// behind. Every stored envelope is a system:ping one.
func TestPingEmitter_TicksRepeatedlyAndEmitsOnlyPingEnvelopes(t *testing.T) {
	const interval = 20 * time.Millisecond

	svc, _, bunDB, _ := newInboundFixture(t, pingConfig(interval))
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"ping-repeat",
		nil,
		"https://example.com/ping",
		smsgateway.WebhookEventSystemPing,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	emitter := startPingEmitter(t, svc, interval)
	waitForQueueRows(t, bunDB, 3)
	emitter.stop(t)

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

// TestPingEmitter_NonPositiveIntervalDisablesEmission pins the opt-out: a zero
// (or negative) interval makes Run return immediately without a single tick,
// which is the WEBHOOKS__PING_INTERVAL=0 contract.
func TestPingEmitter_NonPositiveIntervalDisablesEmission(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{name: "zero disables", interval: 0},
		{name: "negative disables", interval: -time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, bunDB, _ := newInboundFixture(t, pingConfig(tt.interval))
			ctx := context.Background()

			if err := svc.Replace(ctx, newWebhookDTO(
				"ping-off",
				nil,
				"https://example.com/ping",
				smsgateway.WebhookEventSystemPing,
			)); err != nil {
				t.Fatalf("replace webhook: %v", err)
			}

			emitter := startPingEmitter(t, svc, tt.interval)
			emitter.stop(t)

			if got := countQueueRows(t, bunDB); got != 0 {
				t.Errorf("webhook_queue rows = %d, want 0 with a disabled interval", got)
			}
		})
	}
}

// TestPingEmitter_PostsOneSignedEnvelopePerTick is the end-to-end contract of
// the ticker: the signed POST reaches the registered endpoint, its body is the
// stored envelope with the empty payload object, and X-Signature is the HMAC
// over exactly those bytes plus the unix-seconds X-Timestamp.
func TestPingEmitter_PostsOneSignedEnvelopePerTick(t *testing.T) {
	const interval = 25 * time.Millisecond

	log := &requestLog{}
	server := newRequestServer(t, log, func(int) int { return http.StatusOK }, nil)

	svc, _, bunDB, _ := newInboundFixture(t, runConfig(1))
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"ping-post",
		nil,
		server.URL,
		smsgateway.WebhookEventSystemPing,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	worker := startWorker(t, svc)
	emitter := startPingEmitter(t, svc, interval)
	waitForPostCount(t, log, 3)

	// Stop the ticker first, then let the delivery worker finish what it has
	// already enqueued: every heartbeat must end up completed, not merely
	// received.
	emitter.stop(t)
	waitForCompletedRows(t, bunDB)
	worker.stop(t)

	requests := log.snapshot()
	if len(requests) < 3 {
		t.Fatalf("POST count = %d, want at least 3 ticks", len(requests))
	}
	mac := hmac.New(sha256.New, []byte(testSigningKey))
	for index, request := range requests {
		if request.contentType != "application/json" {
			t.Errorf("POST %d Content-Type = %q, want application/json", index+1, request.contentType)
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

// pingConfig is the ticker fixture config: emission only, so the queue tuning
// stays at the defaults the emitter never reads.
func pingConfig(interval time.Duration) webhooks.Config {
	return webhooks.Config{
		SigningKey:   testFixtureSigningKey,
		PingInterval: interval,
	}
}

type pingHandle struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

// startPingEmitter runs the ticker the way fx does: in its own goroutine,
// cancelled through the context.
func startPingEmitter(t *testing.T, service *webhooks.Service, interval time.Duration) *pingHandle {
	t.Helper()

	emitter := webhooks.NewPingEmitter(pingConfig(interval), service, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	handle := &pingHandle{
		cancel: cancel,
		done:   make(chan error, 1),
	}
	go func() {
		handle.done <- emitter.Run(ctx)
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
				t.Errorf("ping emitter Run returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("ping emitter Run did not stop after context cancellation")
		}
	})
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
// for WEBHOOKS__QUEUE__CLEANUP_RETENTION, so the queue does not empty.
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

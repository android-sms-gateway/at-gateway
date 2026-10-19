package webhooks_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
	"go.uber.org/zap"
)

// newInboundFixture builds the same graph as newServiceFixture but also hands
// back the bun.DB, so tests can inject per-webhook enqueue failures.
func newInboundFixture(
	t *testing.T,
	config webhooks.Config,
) (*webhooks.Service, *webhooks.Repository, *bun.DB, *devices.Service) {
	t.Helper()

	repo, _, bunDB := newPersistence(t)
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
		repo,
		devicesSvc,
		storageSvc,
		newTestMetrics(),
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create webhooks service: %v", err)
	}

	return service, repo, bunDB, devicesSvc
}

// inboundConfig is the registry-only config: emission needs no queue tuning.
func inboundConfig() webhooks.Config {
	return webhooks.Config{
		RetryCount: 0,
		SigningKey: testFixtureSigningKey,
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

// inboundEvent is the fixture every inbound-emitter test emits.
func inboundEvent() webhooks.InboundEvent {
	recipient := "+79991234567"
	simNumber := uint8(1)

	return webhooks.InboundEvent{
		MessageID: "0f1e2d3c4b5a6978",
		Sender:    "+79990001234",
		Recipient: &recipient,
		SimNumber: &simNumber,
		Message:   "hello",
		Data:      "3q2+7w==",
		At:        time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC),
	}
}

// storedPayloads returns the RAW stored envelope bytes, keyed by webhook id.
// They are the exact bytes the delivery worker signs and POSTs, so tests must
// never re-marshal them.
func storedPayloads(t *testing.T, repo *webhooks.Repository) map[string]string {
	t.Helper()

	payloads := make(map[string]string)
	for _, item := range dueItems(t, repo) {
		payloads[item.WebhookID] = item.Payload
	}

	return payloads
}

// dueItems reads every queued item with the stored (raw) envelope bytes.
func dueItems(t *testing.T, repo *webhooks.Repository) []webhooks.QueueItem {
	t.Helper()

	items, err := repo.DueItems(context.Background(), time.Now().Add(time.Hour), 32)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	return items
}

// TestEmitSmsReceived_RawEnvelopeIsByteIdenticalToThePayload pins the
// sms:received wire contract on the RAW STORED ENVELOPE BYTES: the payload
// sub-document is byte-identical to the marshalled client-go
// SmsReceivedPayload (phoneNumber repeats the sender, recipient and simNumber
// are present, receivedAt is the RFC3339 UTC stamp), inside an envelope that
// carries the event, the webhook and the local device.
func TestEmitSmsReceived_RawEnvelopeIsByteIdenticalToThePayload(t *testing.T) {
	t.Parallel()

	svc, repo, _, devicesSvc := newInboundFixture(t, inboundConfig())
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"inbound-received",
		nil,
		"https://example.com/received",
		smsgateway.WebhookEventSmsReceived,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	event := inboundEvent()
	out := svc.EmitSmsReceived(event, nil)
	if len(out.Results) != 1 || out.Results[0].Err != nil {
		t.Fatalf("emission = %+v, want one successful verdict", out)
	}

	payloads := storedPayloads(t, repo)
	raw, ok := payloads["inbound-received"]
	if !ok {
		t.Fatalf("stored payloads = %v, want one for the webhook", payloads)
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
	if envelope.Event != smsgateway.WebhookEventSmsReceived {
		t.Errorf("envelope event = %q, want %q", envelope.Event, smsgateway.WebhookEventSmsReceived)
	}
	if envelope.WebhookID != "inbound-received" {
		t.Errorf("envelope webhookId = %q, want inbound-received", envelope.WebhookID)
	}
	if envelope.DeviceID != devicesSvc.Get().ID {
		t.Errorf("envelope deviceId = %q, want the local device", envelope.DeviceID)
	}
	if envelope.ID == "" {
		t.Error("envelope id is empty")
	}

	want, err := json.Marshal(smsgateway.SmsReceivedPayload{
		SmsEventPayload: smsgateway.SmsEventPayload{
			MessageID:   event.MessageID,
			PhoneNumber: event.Sender,
			Sender:      event.Sender,
			Recipient:   event.Recipient,
			SimNumber:   event.SimNumber,
		},
		Message:    event.Message,
		ReceivedAt: event.At,
	})
	if err != nil {
		t.Fatalf("marshal expected payload: %v", err)
	}
	if string(envelope.Payload) != string(want) {
		t.Errorf("stored payload = %s, want byte-identical %s", envelope.Payload, want)
	}
	if !strings.Contains(string(envelope.Payload), `"receivedAt":"2026-10-02T12:00:00Z"`) {
		t.Errorf("stored payload %s does not carry the RFC3339 UTC stamp", envelope.Payload)
	}
}

// TestEmitSmsDataReceived_RawEnvelopeIsByteIdenticalToThePayload pins the
// sms:data-received wire contract: the base64 NO_WRAP payload of the assembled
// octets and no message field at all.
func TestEmitSmsDataReceived_RawEnvelopeIsByteIdenticalToThePayload(t *testing.T) {
	t.Parallel()

	svc, repo, _, _ := newInboundFixture(t, inboundConfig())
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"inbound-data",
		nil,
		"https://example.com/data",
		smsgateway.WebhookEventSmsDataReceived,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	event := inboundEvent()
	event.Message = ""
	if out := svc.EmitSmsDataReceived(event, nil); len(out.Results) != 1 || out.Results[0].Err != nil {
		t.Fatalf("emission = %+v, want one successful verdict", out)
	}

	payloads := storedPayloads(t, repo)
	raw, ok := payloads["inbound-data"]
	if !ok {
		t.Fatalf("stored payloads = %v, want one for the webhook", payloads)
	}

	var envelope struct {
		Event   smsgateway.WebhookEvent `json:"event"`
		Payload json.RawMessage         `json:"payload"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode stored envelope: %v", err)
	}
	if envelope.Event != smsgateway.WebhookEventSmsDataReceived {
		t.Errorf("envelope event = %q, want sms:data-received", envelope.Event)
	}

	want, err := json.Marshal(smsgateway.SmsDataReceivedPayload{
		SmsEventPayload: smsgateway.SmsEventPayload{
			MessageID:   event.MessageID,
			PhoneNumber: event.Sender,
			Sender:      event.Sender,
			Recipient:   event.Recipient,
			SimNumber:   event.SimNumber,
		},
		Data:       event.Data,
		ReceivedAt: event.At,
	})
	if err != nil {
		t.Fatalf("marshal expected payload: %v", err)
	}
	if string(envelope.Payload) != string(want) {
		t.Errorf("stored payload = %s, want byte-identical %s", envelope.Payload, want)
	}
	if strings.Contains(string(envelope.Payload), `"message"`) {
		t.Errorf("stored payload %s must not carry a message field", envelope.Payload)
	}
}

// TestEmitSmsReceived_PostsTheStoredBytesVerbatim pins the delivery contract
// of an inbound event end to end: the POSTed body IS the raw stored envelope
// byte-for-byte and X-Signature is the HMAC over exactly those bytes plus the
// unix-seconds X-Timestamp.
func TestEmitSmsReceived_PostsTheStoredBytesVerbatim(t *testing.T) {
	t.Parallel()

	config := runConfig(1)
	config.SigningKey = testFixtureSigningKey
	svc, repo, bunDB, _ := newInboundFixture(t, config)
	ctx := context.Background()

	log := &requestLog{
		mu:       sync.Mutex{},
		requests: []capturedRequest{},
	}
	server := newRequestServer(t, log, func(int) int { return http.StatusOK }, nil)

	if err := svc.Replace(ctx, newWebhookDTO(
		"inbound-post",
		nil,
		server.URL,
		smsgateway.WebhookEventSmsReceived,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	event := inboundEvent()
	if out := svc.EmitSmsReceived(event, nil); len(out.Results) != 1 {
		t.Fatalf("emission = %+v, want one verdict", out)
	}

	// Capture the RAW stored bytes (and the queue row) before the worker
	// consumes it.
	items := dueItems(t, repo)
	if len(items) != 1 {
		t.Fatalf("queued items = %d, want 1", len(items))
	}
	raw := items[0].Payload

	worker := startWorker(t, svc)
	waitForStatus(t, runFixture{
		service: nil,
		repo:    nil,
		db:      bunDB,
		metrics: nil,
	}, items[0].ID, webhooks.QueueStatusCompleted)
	worker.stop(t)

	requests := log.snapshot()
	if len(requests) != 1 {
		t.Fatalf("POST count = %d, want 1", len(requests))
	}
	if string(requests[0].body) != raw {
		t.Errorf("POSTed body = %s, want the stored bytes %s", requests[0].body, raw)
	}
	if requests[0].contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", requests[0].contentType)
	}

	mac := hmac.New(sha256.New, []byte(testFixtureSigningKey))
	_, _ = mac.Write(requests[0].body)
	_, _ = mac.Write([]byte(requests[0].timestamp))
	if want := hex.EncodeToString(mac.Sum(nil)); requests[0].signature != want {
		t.Errorf("X-Signature = %q, want %q over the raw stored bytes", requests[0].signature, want)
	}
}

// TestEmitTracked_PerWebhookVerdictsAndSuppression pins the per-webhook verdict
// contract the inbound dedupe depends on: every MATCHED webhook is reported
// separately, a failing enqueue carries its error, and a caller-supplied
// served set suppresses exactly those webhooks (counted separately from the
// enqueue verdicts).
func TestEmitTracked_PerWebhookVerdictsAndSuppression(t *testing.T) {
	t.Parallel()

	svc, repo, bunDB, _ := newInboundFixture(t, inboundConfig())
	ctx := context.Background()

	for _, id := range []string{"hook-ok-1", "hook-ok-2", "hook-broken"} {
		if err := svc.Replace(ctx, newWebhookDTO(
			id,
			nil,
			"https://example.com/"+id,
			smsgateway.WebhookEventSmsReceived,
		)); err != nil {
			t.Fatalf("replace %s: %v", id, err)
		}
	}

	// Inject a per-webhook enqueue failure: the queue insert for hook-broken
	// aborts, the other two store normally.
	if _, err := bunDB.ExecContext(ctx, `
		CREATE TRIGGER fail_hook_broken BEFORE INSERT ON webhook_queue
		WHEN NEW.webhook_id = 'hook-broken'
		BEGIN SELECT RAISE(ABORT, 'injected enqueue failure'); END;`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	event := inboundEvent()

	first := svc.EmitSmsReceived(event, nil)
	assertVerdicts(t, "first emission", first, map[string]bool{
		"hook-ok-1":   true,
		"hook-ok-2":   true,
		"hook-broken": false,
	})
	if first.Suppressed != 0 {
		t.Errorf("Suppressed = %d, want 0 for a first emission", first.Suppressed)
	}
	if got := len(storedPayloads(t, repo)); got != 2 {
		t.Fatalf("stored envelopes = %d, want 2 (the broken hook must not store)", got)
	}

	// A re-notification with the already-served set reaches exactly the hook
	// whose enqueue failed: the two that already hold a queue item are
	// suppressed, so they are never delivered a duplicate.
	second := svc.EmitSmsReceived(event, []string{"hook-ok-1", "hook-ok-2"})
	// Suppressed webhooks are NOT reported as enqueue verdicts: only the hook
	// that never got a queue item is attempted again.
	assertVerdicts(t, "re-notification", second, map[string]bool{
		"hook-broken": false,
	})
	if second.Suppressed != 2 {
		t.Errorf("Suppressed = %d, want the 2 already-served hooks", second.Suppressed)
	}

	payloads := storedPayloads(t, repo)
	if got := len(payloads); got != 2 {
		t.Errorf("stored envelopes = %d, want 2 - the served hooks must not gain a second item", got)
	}
}

// TestEmitTracked_AllMatchedWebhooksSuppressed pins the fully-suppressed case:
// every match is in the served set, so nothing is enqueued, nothing is
// reported as an enqueue verdict and the suppression count equals the number
// of matched webhooks.
func TestEmitTracked_AllMatchedWebhooksSuppressed(t *testing.T) {
	t.Parallel()

	svc, repo, _, _ := newInboundFixture(t, inboundConfig())
	ctx := context.Background()

	for _, id := range []string{"hook-1", "hook-2"} {
		if err := svc.Replace(ctx, newWebhookDTO(
			id,
			nil,
			"https://example.com/"+id,
			smsgateway.WebhookEventSmsReceived,
		)); err != nil {
			t.Fatalf("replace %s: %v", id, err)
		}
	}

	out := svc.EmitSmsReceived(inboundEvent(), []string{"hook-1", "hook-2"})
	if len(out.Results) != 0 {
		t.Errorf("Results = %+v, want none when every match is suppressed", out.Results)
	}
	if out.Suppressed != 2 {
		t.Errorf("Suppressed = %d, want 2", out.Suppressed)
	}
	if got := len(storedPayloads(t, repo)); got != 0 {
		t.Errorf("stored envelopes = %d, want none", got)
	}
}

// TestEmitTracked_ZeroMatchesStaysEmpty pins the empty-registry boundary: a
// notification with no registered webhook reports neither verdicts nor
// suppressions, so the caller has nothing to mark.
func TestEmitTracked_ZeroMatchesStaysEmpty(t *testing.T) {
	t.Parallel()

	svc, repo, _, _ := newInboundFixture(t, inboundConfig())
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"other-event",
		nil,
		"https://example.com/other",
		smsgateway.WebhookEventSmsSent,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	out := svc.EmitSmsReceived(inboundEvent(), nil)
	if len(out.Results) != 0 || out.Suppressed != 0 {
		t.Errorf("emission = %+v, want an empty outcome for a non-matching registry", out)
	}
	if got := len(storedPayloads(t, repo)); got != 0 {
		t.Errorf("stored envelopes = %d, want none", got)
	}
}

// assertVerdicts pins the per-webhook verdicts: every expected webhook must
// appear exactly once, with a nil error when wantOK and a non-nil error
// otherwise.
func assertVerdicts(t *testing.T, what string, out webhooks.TrackedEmission, wantOK map[string]bool) {
	t.Helper()

	if len(out.Results) != len(wantOK) {
		t.Fatalf("%s results = %+v, want %d verdicts", what, out.Results, len(wantOK))
	}

	seen := make(map[string]bool, len(out.Results))
	for _, result := range out.Results {
		ok, expected := wantOK[result.WebhookID]
		if !expected {
			t.Errorf("%s reports unexpected webhook %q", what, result.WebhookID)

			continue
		}
		seen[result.WebhookID] = true
		if ok && result.Err != nil {
			t.Errorf("%s verdict for %s = %v, want no error", what, result.WebhookID, result.Err)
		}
		if !ok && result.Err == nil {
			t.Errorf("%s verdict for %s reports no error, want the enqueue failure", what, result.WebhookID)
		}
	}
	for id := range wantOK {
		if !seen[id] {
			t.Errorf("%s did not report webhook %q", what, id)
		}
	}
}

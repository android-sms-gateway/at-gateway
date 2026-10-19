package webhooks_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
)

// TestEmitSystemPing_RawEnvelopeCarriesAnEmptyJSONObject pins the system:ping
// wire contract on the RAW STORED ENVELOPE BYTES: the payload sub-document is
// byte-identical to the marshalled client-go SystemPingPayload, i.e. the empty
// JSON object {} - the accepted parity gap versus the Android gateway, which
// publishes {"health": {...}} there.
func TestEmitSystemPing_RawEnvelopeCarriesAnEmptyJSONObject(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

package webhooks_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
)

const testFixtureSigningKey = "webhooks-test-key"

// newService builds the T1 persistence graph (see newPersistence) plus the
// storage-backed devices service, then returns a registry-only
// *webhooks.Service together with the devices service so tests can read the
// local device id that Replace validates supplied deviceIds against.
func newService(t *testing.T) (*webhooks.Service, *devices.Service) {
	svc, devicesSvc, _ := newServiceFixture(t, webhooks.Config{SigningKey: testFixtureSigningKey})
	return svc, devicesSvc
}

func newServiceFixture(
	t *testing.T,
	config webhooks.Config,
) (*webhooks.Service, *devices.Service, *webhooks.Repository) {
	t.Helper()

	repo, _, _ := newPersistence(t)

	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}

	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())
	metrics := newTestMetrics()
	service, err := webhooks.NewService(config, repo, devicesSvc, storageSvc, metrics, zap.NewNop())
	if err != nil {
		t.Fatalf("create webhooks service: %v", err)
	}

	return service, devicesSvc, repo
}

func newTestMetrics() *webhooks.Metrics {
	return webhooks.NewTestMetrics(prometheus.NewRegistry())
}

func newWebhookDTO(id string, deviceID *string, url string, event smsgateway.WebhookEvent) *smsgateway.Webhook {
	return &smsgateway.Webhook{
		ID:       id,
		DeviceID: deviceID,
		URL:      url,
		Event:    event,
	}
}

// TestReplace_InvalidEvent pins AC2 event validation: unknown or empty event
// types are rejected with ErrInvalidEvent and nothing is persisted, across
// three distinct invalid inputs.
func TestReplace_InvalidEvent(t *testing.T) {
	tests := []struct {
		name  string
		event smsgateway.WebhookEvent
	}{
		{"empty event", ""},
		{"unknown event", "bogus:event"},
		{"case variant rejected", "SMS:RECEIVED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newService(t)
			ctx := context.Background()

			webhook := newWebhookDTO("", nil, "https://example.com/hook", tt.event)
			err := svc.Replace(ctx, webhook)
			if !errors.Is(err, webhooks.ErrInvalidEvent) {
				t.Fatalf("replace error = %v, want ErrInvalidEvent", err)
			}

			got, selectErr := svc.Select(ctx)
			if selectErr != nil {
				t.Fatalf("select: %v", selectErr)
			}
			if len(got) != 0 {
				t.Fatalf("registry has %d webhooks after rejected replace, want 0", len(got))
			}
		})
	}
}

// TestReplace_ForeignDeviceID pins owner rule 2 device ownership: a non-empty
// deviceId that differs from the local device is rejected with
// ErrDeviceNotFound across three distinct foreign ids, and nothing is
// persisted.
func TestReplace_ForeignDeviceID(t *testing.T) {
	tests := []struct {
		name    string
		foreign string
	}{
		{"short foreign id", "other-device"},
		{"nanoid-shaped foreign id", "PyDmBQZZXYmyxMwED8Fzy"},
		{"another foreign id", "dev-999"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newService(t)
			ctx := context.Background()

			foreign := tt.foreign
			webhook := newWebhookDTO("", &foreign, "https://example.com/hook", smsgateway.WebhookEventSmsReceived)
			err := svc.Replace(ctx, webhook)
			if !errors.Is(err, webhooks.ErrDeviceNotFound) {
				t.Fatalf("replace error = %v, want ErrDeviceNotFound", err)
			}

			got, selectErr := svc.Select(ctx)
			if selectErr != nil {
				t.Fatalf("select: %v", selectErr)
			}
			if len(got) != 0 {
				t.Fatalf("registry has %d webhooks after rejected replace, want 0", len(got))
			}
		})
	}
}

// TestReplace_NilOrEmptyDeviceIDStoresNull pins owner rule 1: a nil or
// empty-string deviceId registers for ALL devices - stored as nil (SQL
// NULL), NEVER the local device id - and the dto is normalized to nil so the
// 201 echo carries deviceId null.
func TestReplace_NilOrEmptyDeviceIDStoresNull(t *testing.T) {
	tests := []struct {
		name     string
		deviceID *string
	}{
		{"nil device id", nil},
		{"empty device id", new(string)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, devicesSvc := newService(t)
			ctx := context.Background()
			localID := devicesSvc.Get().ID

			webhook := newWebhookDTO("", tt.deviceID, "https://example.com/hook", smsgateway.WebhookEventSmsReceived)
			if err := svc.Replace(ctx, webhook); err != nil {
				t.Fatalf("replace: %v", err)
			}
			if webhook.ID == "" {
				t.Fatal("replace did not mutate webhook.ID, want generated nanoid")
			}
			if webhook.DeviceID != nil {
				t.Fatalf("echo DeviceID = %q, want nil (never filled with local %q)", *webhook.DeviceID, localID)
			}

			got, err := svc.Select(ctx)
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("select returned %d webhooks, want 1", len(got))
			}
			if got[0].DeviceID != nil {
				t.Fatalf(
					"selected DeviceID = %q, want nil (registered for all devices; local was %q)",
					*got[0].DeviceID,
					localID,
				)
			}
			if got[0].ID != webhook.ID {
				t.Fatalf("selected ID = %q, want %q", got[0].ID, webhook.ID)
			}
		})
	}
}

// TestReplace_MatchingLocalDeviceIDAccepted verifies a non-empty deviceId equal
// to the local device id is accepted, stored, and echoed back on the dto.
func TestReplace_MatchingLocalDeviceIDAccepted(t *testing.T) {
	svc, devicesSvc := newService(t)
	ctx := context.Background()
	localID := devicesSvc.Get().ID

	webhook := newWebhookDTO("w-local", &localID, "https://example.com/hook", smsgateway.WebhookEventSmsSent)
	if err := svc.Replace(ctx, webhook); err != nil {
		t.Fatalf("replace with local device id: %v", err)
	}
	if webhook.DeviceID == nil || *webhook.DeviceID != localID {
		t.Fatalf("echo DeviceID = %v, want %q", webhook.DeviceID, localID)
	}

	got, err := svc.Select(ctx)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("select returned %d webhooks, want 1", len(got))
	}
	if got[0].DeviceID == nil || *got[0].DeviceID != localID {
		t.Fatalf("selected DeviceID = %v, want %q", got[0].DeviceID, localID)
	}
}

// TestReplace_IDGeneration pins AC2 sole-generator behaviour: an empty ID is
// replaced with a generated nanoid (mutating the input for server 201-echo
// parity) while a supplied ID is preserved exactly.
func TestReplace_IDGeneration(t *testing.T) {
	tests := []struct {
		name       string
		suppliedID string
		wantMutate bool
	}{
		{"empty id generates nanoid", "", true},
		{"supplied id preserved", "supplied-id-1", false},
		{"second supplied id preserved", "w-explicit-2", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newService(t)
			ctx := context.Background()

			webhook := newWebhookDTO(tt.suppliedID, nil, "https://example.com/hook", smsgateway.WebhookEventSmsReceived)
			if err := svc.Replace(ctx, webhook); err != nil {
				t.Fatalf("replace: %v", err)
			}

			if tt.wantMutate {
				if webhook.ID == "" {
					t.Fatal("webhook.ID still empty, want generated nanoid")
				}
				if len(webhook.ID) > 36 {
					t.Fatalf("generated ID length = %d, want <= 36 (wire max)", len(webhook.ID))
				}
			} else if webhook.ID != tt.suppliedID {
				t.Fatalf("webhook.ID = %q, want supplied %q", webhook.ID, tt.suppliedID)
			}

			got, err := svc.Select(ctx)
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("select returned %d webhooks, want 1", len(got))
			}
			if got[0].ID != webhook.ID {
				t.Fatalf("selected ID = %q, want %q", got[0].ID, webhook.ID)
			}
		})
	}
}

// TestReplace_DoesNotEnforceHTTPS pins AC3: the service accepts plain http and
// even an empty URL - the http_url gate belongs to the edge validator, and
// client-go Webhook.Validate() is never called here.
func TestReplace_DoesNotEnforceHTTPS(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"https accepted", "https://example.com/hook"},
		{"plain http accepted", "http://127.0.0.1:9099/hook"},
		{"empty url deferred to edge", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newService(t)
			ctx := context.Background()

			webhook := newWebhookDTO("", nil, tt.url, smsgateway.WebhookEventSystemPing)
			if err := svc.Replace(ctx, webhook); err != nil {
				t.Fatalf("replace with url %q: %v", tt.url, err)
			}

			got, err := svc.Select(ctx)
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("select returned %d webhooks, want 1", len(got))
			}
			if got[0].URL != tt.url {
				t.Fatalf("selected URL = %q, want %q", got[0].URL, tt.url)
			}
		})
	}
}

// TestReplace_UpsertByGeneratedID proves repeated Replace calls with the same
// (generated or supplied) id keep a single registry row and refresh its fields.
func TestReplace_UpsertByGeneratedID(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	first := newWebhookDTO("", nil, "https://example.com/first", smsgateway.WebhookEventSmsReceived)
	if err := svc.Replace(ctx, first); err != nil {
		t.Fatalf("first replace: %v", err)
	}

	second := newWebhookDTO(first.ID, nil, "https://example.com/second", smsgateway.WebhookEventSmsSent)
	if err := svc.Replace(ctx, second); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	got, err := svc.Select(ctx)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("select returned %d webhooks after upsert, want 1", len(got))
	}
	if got[0].ID != first.ID {
		t.Fatalf("upserted ID = %q, want preserved %q", got[0].ID, first.ID)
	}
	if got[0].URL != "https://example.com/second" {
		t.Fatalf("upserted URL = %q, want refreshed second url", got[0].URL)
	}
	if got[0].Event != smsgateway.WebhookEventSmsSent {
		t.Fatalf("upserted Event = %q, want %q", got[0].Event, smsgateway.WebhookEventSmsSent)
	}
}

// TestSelect_EmptyRegistryReturnsEmptySlice pins AC4: an idle registry yields
// an empty non-nil slice, never nil.
func TestSelect_EmptyRegistryReturnsEmptySlice(t *testing.T) {
	svc, _ := newService(t)

	got, err := svc.Select(context.Background())
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if got == nil {
		t.Fatal("select returned nil, want empty non-nil slice")
	}
	if len(got) != 0 {
		t.Fatalf("select returned %d webhooks, want 0", len(got))
	}
}

// TestSelect_DeviceIDPresent pins owner rule 2 at the Select layer: after a
// Replace that supplied the local device id, Select returns DeviceID present
// (equal to the local id) plus ID/URL/Event from the registry, across three
// distinct events.
func TestSelect_DeviceIDPresent(t *testing.T) {
	svc, devicesSvc := newService(t)
	ctx := context.Background()
	localID := devicesSvc.Get().ID

	cases := []struct {
		name  string
		url   string
		event smsgateway.WebhookEvent
	}{
		{"sms received", "https://example.com/a", smsgateway.WebhookEventSmsReceived},
		{"sms sent", "http://127.0.0.1:9099/b", smsgateway.WebhookEventSmsSent},
		{"mms batch downloaded", "https://example.com/c", smsgateway.WebhookEventMmsBatchDownloaded},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			webhook := newWebhookDTO("", &localID, tc.url, tc.event)
			if err := svc.Replace(ctx, webhook); err != nil {
				t.Fatalf("replace: %v", err)
			}

			got, err := svc.Select(ctx)
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if len(got) != i+1 {
				t.Fatalf("select returned %d webhooks, want %d", len(got), i+1)
			}

			last := got[len(got)-1]
			if last.DeviceID == nil {
				t.Fatal("DeviceID is nil, want supplied local device id")
			}
			if *last.DeviceID != localID {
				t.Fatalf("DeviceID = %q, want local %q", *last.DeviceID, localID)
			}
			if last.ID != webhook.ID {
				t.Fatalf("ID = %q, want %q", last.ID, webhook.ID)
			}
			if last.URL != tc.url {
				t.Fatalf("URL = %q, want %q", last.URL, tc.url)
			}
			if last.Event != tc.event {
				t.Fatalf("Event = %q, want %q", last.Event, tc.event)
			}
		})
	}
}

// TestSelect_DeviceIDNullWhenUnscoped pins owner rule 1 at the Select layer:
// webhooks registered without a deviceId return DeviceID nil (JSON null), never
// the local device id, across three distinct events.
func TestSelect_DeviceIDNullWhenUnscoped(t *testing.T) {
	svc, devicesSvc := newService(t)
	ctx := context.Background()
	localID := devicesSvc.Get().ID

	cases := []struct {
		name  string
		url   string
		event smsgateway.WebhookEvent
	}{
		{"sms received", "https://example.com/a", smsgateway.WebhookEventSmsReceived},
		{"sms sent", "http://127.0.0.1:9099/b", smsgateway.WebhookEventSmsSent},
		{"system ping", "https://example.com/ping", smsgateway.WebhookEventSystemPing},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			webhook := newWebhookDTO("", nil, tc.url, tc.event)
			if err := svc.Replace(ctx, webhook); err != nil {
				t.Fatalf("replace: %v", err)
			}

			got, err := svc.Select(ctx)
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if len(got) != i+1 {
				t.Fatalf("select returned %d webhooks, want %d", len(got), i+1)
			}

			last := got[len(got)-1]
			if last.DeviceID != nil {
				t.Fatalf("DeviceID = %q, want nil (registered for all devices; local was %q)", *last.DeviceID, localID)
			}
			if last.ID != webhook.ID {
				t.Fatalf("ID = %q, want %q", last.ID, webhook.ID)
			}
		})
	}
}

// TestDelete_Idempotent pins AC5: deleting an unknown id, an empty id or an
// already-deleted id all succeed without ErrNotFound and without touching
// unrelated rows.
func TestDelete_Idempotent(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()

	kept := newWebhookDTO("w-kept", nil, "https://example.com/kept", smsgateway.WebhookEventSmsSent)
	if err := svc.Replace(ctx, kept); err != nil {
		t.Fatalf("replace kept: %v", err)
	}
	removed := newWebhookDTO("w-gone", nil, "https://example.com/gone", smsgateway.WebhookEventSmsFailed)
	if err := svc.Replace(ctx, removed); err != nil {
		t.Fatalf("replace removed: %v", err)
	}

	for _, id := range []string{"does-not-exist", "", "w-gone", "w-gone"} {
		if err := svc.Delete(ctx, id); err != nil {
			t.Fatalf("delete %q: %v", id, err)
		}
	}
	if err := svc.Delete(ctx, kept.ID); err != nil {
		t.Fatalf("delete kept: %v", err)
	}
	if err := svc.Delete(ctx, kept.ID); err != nil {
		t.Fatalf("repeat delete kept: %v", err)
	}

	got, err := svc.Select(ctx)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("select after deletes returned %d webhooks, want 0", len(got))
	}
}

// TestSentinels_MatchWithErrorsIs verifies AC6: both sentinels are distinct
// static errors matched only via [errors.Is].
func TestSentinels_MatchWithErrorsIs(t *testing.T) {
	if errors.Is(webhooks.ErrInvalidEvent, webhooks.ErrDeviceNotFound) {
		t.Fatal("ErrInvalidEvent matches ErrDeviceNotFound, want distinct sentinels")
	}
	if errors.Is(webhooks.ErrDeviceNotFound, webhooks.ErrInvalidEvent) {
		t.Fatal("ErrDeviceNotFound matches ErrInvalidEvent, want distinct sentinels")
	}
	if !errors.Is(webhooks.ErrInvalidEvent, webhooks.ErrInvalidEvent) {
		t.Fatal("ErrInvalidEvent does not match itself")
	}
	if !errors.Is(webhooks.ErrDeviceNotFound, webhooks.ErrDeviceNotFound) {
		t.Fatal("ErrDeviceNotFound does not match itself")
	}
}

// TestEmitSmsSent_BuildsOneEnvelopePerMatchingWebhook pins the queue payload shape and
// URL snapshot: only matching registry rows produce queue items, and the
// envelope device id always comes from the local device service.
func TestEmitSmsSent_BuildsOneEnvelopePerMatchingWebhook(t *testing.T) {
	svc, devicesSvc, repo := newServiceFixture(t, webhooks.Config{SigningKey: testFixtureSigningKey})
	ctx := context.Background()
	localID := devicesSvc.Get().ID

	registered := []struct {
		id  string
		url string
	}{
		{id: "emit-one", url: "https://example.com/one"},
		{id: "emit-two", url: "https://example.com/two"},
	}
	for _, item := range registered {
		webhook := newWebhookDTO(item.id, nil, item.url, smsgateway.WebhookEventSmsSent)
		if err := svc.Replace(ctx, webhook); err != nil {
			t.Fatalf("replace %s: %v", item.id, err)
		}
	}
	if err := svc.Replace(ctx, newWebhookDTO(
		"emit-mismatch",
		nil,
		"https://example.com/mismatch",
		smsgateway.WebhookEventSmsReceived,
	)); err != nil {
		t.Fatalf("replace mismatch: %v", err)
	}

	at := time.Date(2026, time.September, 25, 12, 0, 0, 0, time.UTC)
	recipient := "+79990009999"
	simNumber := uint8(2)
	svc.EmitSmsSent(webhooks.MessageEvent{
		MessageID:   "message-1",
		PhoneNumber: "+79990001234",
		Sender:      "+79990009999",
		Recipient:   &recipient,
		SimNumber:   &simNumber,
		At:          at,
	})

	items, err := repo.DueItems(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}
	if len(items) != len(registered) {
		t.Fatalf("queued items = %d, want %d", len(items), len(registered))
	}

	seen := make(map[string]webhooks.QueueItem, len(items))
	for _, item := range items {
		seen[item.WebhookID] = item
	}
	for _, want := range registered {
		item, ok := seen[want.id]
		if !ok {
			t.Fatalf("queue items = %+v, missing webhook %q", items, want.id)
		}
		if item.URL != want.url {
			t.Errorf("queue URL = %q, want %q", item.URL, want.url)
		}
		if item.Status != webhooks.QueueStatusPending {
			t.Errorf("queue status = %q, want pending", item.Status)
		}
		if item.RetryCount != 0 || item.LastError != nil {
			t.Errorf("queue initial state = %+v, want zero retry/error", item)
		}

		var envelope map[string]any
		unmarshalErr := json.Unmarshal([]byte(item.Payload), &envelope)
		if unmarshalErr != nil {
			t.Fatalf("decode envelope for %s: %v", want.id, unmarshalErr)
		}
		if len(envelope) != 5 {
			t.Errorf("envelope keys = %v, want exactly 5", envelope)
		}
		if id, _ := envelope["id"].(string); id == "" {
			t.Error("envelope id is empty")
		}
		if got, _ := envelope["webhookId"].(string); got != want.id {
			t.Errorf("envelope webhookId = %q, want %q", got, want.id)
		}
		if got, _ := envelope["event"].(string); got != smsgateway.WebhookEventSmsSent {
			t.Errorf("envelope event = %q, want %q", got, smsgateway.WebhookEventSmsSent)
		}
		if got, _ := envelope["deviceId"].(string); got != localID {
			t.Errorf("envelope deviceId = %q, want local device %q", got, localID)
		}
		payload, ok := envelope["payload"].(map[string]any)
		if !ok || payload["phoneNumber"] != "+79990001234" {
			t.Errorf("envelope payload = %#v, want phoneNumber +79990001234", envelope["payload"])
		}
	}
}

func TestMessageEventMethods_MapToEventAndTimestamp(t *testing.T) {
	svc, _, repo := newServiceFixture(t, webhooks.Config{SigningKey: testFixtureSigningKey})
	ctx := context.Background()
	at := time.Date(2026, time.September, 25, 12, 34, 56, 123456789, time.UTC)
	recipient := "+79990009999"
	simNumber := uint8(2)
	event := webhooks.MessageEvent{
		MessageID:   "message-1",
		PhoneNumber: "+79990001234",
		Sender:      "+79990008888",
		Recipient:   &recipient,
		SimNumber:   &simNumber,
		At:          at,
	}

	tests := []struct {
		name           string
		event          smsgateway.WebhookEvent
		timestampField string
		reason         string
		emit           func(*webhooks.Service, webhooks.MessageEvent)
	}{
		{
			name:           "sent",
			event:          smsgateway.WebhookEventSmsSent,
			timestampField: "sentAt",
			emit: func(svc *webhooks.Service, event webhooks.MessageEvent) {
				svc.EmitSmsSent(event)
			},
		},
		{
			name:           "delivered",
			event:          smsgateway.WebhookEventSmsDelivered,
			timestampField: "deliveredAt",
			emit: func(svc *webhooks.Service, event webhooks.MessageEvent) {
				svc.EmitSmsDelivered(event)
			},
		},
		{
			name:           "failed",
			event:          smsgateway.WebhookEventSmsFailed,
			timestampField: "failedAt",
			reason:         "modem unavailable",
			emit: func(svc *webhooks.Service, event webhooks.MessageEvent) {
				svc.EmitSmsFailed(event, "modem unavailable")
			},
		},
		{
			name:           "cancelled",
			event:          smsgateway.WebhookEventSmsCancelled,
			timestampField: "cancelledAt",
			emit: func(svc *webhooks.Service, event webhooks.MessageEvent) {
				svc.EmitSmsCancelled(event)
			},
		},
	}

	for _, tt := range tests {
		id := "event-" + tt.name
		if err := svc.Replace(ctx, newWebhookDTO(
			id,
			nil,
			"https://example.com/"+tt.name,
			tt.event,
		)); err != nil {
			t.Fatalf("replace %s: %v", tt.name, err)
		}
		tt.emit(svc, event)
	}

	items, err := repo.DueItems(ctx, time.Now().Add(time.Hour), len(tests))
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}
	if len(items) != len(tests) {
		t.Fatalf("queued items = %d, want %d", len(items), len(tests))
	}

	byWebhookID := make(map[string]webhooks.QueueItem, len(items))
	for _, item := range items {
		byWebhookID[item.WebhookID] = item
	}
	for _, tt := range tests {
		item, ok := byWebhookID["event-"+tt.name]
		if !ok {
			t.Fatalf("queue items = %+v, missing webhook %q", items, tt.name)
		}

		var envelope struct {
			Event   smsgateway.WebhookEvent `json:"event"`
			Payload json.RawMessage         `json:"payload"`
		}
		if decodeErr := json.Unmarshal([]byte(item.Payload), &envelope); decodeErr != nil {
			t.Fatalf("decode envelope for %s: %v", tt.name, decodeErr)
		}
		if envelope.Event != tt.event {
			t.Errorf("%s event = %q, want %q", tt.name, envelope.Event, tt.event)
		}

		var payload map[string]json.RawMessage
		if decodeErr := json.Unmarshal(envelope.Payload, &payload); decodeErr != nil {
			t.Fatalf("decode payload for %s: %v", tt.name, decodeErr)
		}
		var fields struct {
			MessageID   string  `json:"messageId"`
			PhoneNumber string  `json:"phoneNumber"`
			Sender      string  `json:"sender"`
			Recipient   *string `json:"recipient"`
			SimNumber   *uint8  `json:"simNumber"`
		}
		if decodeErr := json.Unmarshal(envelope.Payload, &fields); decodeErr != nil {
			t.Fatalf("decode SMS fields for %s: %v", tt.name, decodeErr)
		}
		if fields.MessageID != event.MessageID {
			t.Errorf("%s messageId = %q, want %q", tt.name, fields.MessageID, event.MessageID)
		}
		if fields.PhoneNumber != event.PhoneNumber {
			t.Errorf("%s phoneNumber = %q, want %q", tt.name, fields.PhoneNumber, event.PhoneNumber)
		}
		if fields.Sender != event.Sender {
			t.Errorf("%s sender = %q, want %q", tt.name, fields.Sender, event.Sender)
		}
		if fields.Recipient == nil || *fields.Recipient != *event.Recipient {
			t.Errorf("%s recipient = %v, want %q", tt.name, fields.Recipient, *event.Recipient)
		}
		if fields.SimNumber == nil || *fields.SimNumber != *event.SimNumber {
			t.Errorf("%s simNumber = %v, want %d", tt.name, fields.SimNumber, *event.SimNumber)
		}
		for _, field := range []string{"sentAt", "deliveredAt", "failedAt", "cancelledAt"} {
			if field == tt.timestampField {
				continue
			}
			if _, exists := payload[field]; exists {
				t.Errorf("%s payload contains %q, want only %q", tt.name, field, tt.timestampField)
			}
		}
		rawAt, ok := payload[tt.timestampField]
		if !ok {
			t.Fatalf("%s payload missing %q", tt.name, tt.timestampField)
		}
		var gotAt time.Time
		if decodeErr := json.Unmarshal(rawAt, &gotAt); decodeErr != nil {
			t.Fatalf("decode %s for %s: %v", tt.timestampField, tt.name, decodeErr)
		}
		if !gotAt.Equal(at) {
			t.Errorf("%s = %s, want %s", tt.timestampField, gotAt, at)
		}
		if tt.reason != "" {
			var gotReason string
			if decodeErr := json.Unmarshal(payload["reason"], &gotReason); decodeErr != nil {
				t.Fatalf("decode reason for %s: %v", tt.name, decodeErr)
			}
			if gotReason != tt.reason {
				t.Errorf("reason = %q, want %q", gotReason, tt.reason)
			}
		}
	}
}

// TestEmitSmsSent_NoMatchesPerformsNoQueueWrites pins the empty-registry and event
// mismatch boundary: emission performs a registry read but creates no queue rows.
func TestEmitSmsSent_NoMatchesPerformsNoQueueWrites(t *testing.T) {
	svc, _, repo := newServiceFixture(t, webhooks.Config{SigningKey: testFixtureSigningKey})
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"other-event",
		nil,
		"https://example.com/other",
		smsgateway.WebhookEventSmsReceived,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	svc.EmitSmsSent(webhooks.MessageEvent{At: time.Now().UTC()})

	items, err := repo.DueItems(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("queued items = %+v, want none", items)
	}
}

// TestEmit_SwallowsPayloadMarshalError pins the fire-and-forget error path:
// an unsupported payload is logged and ignored without creating a queue row.
func TestEmit_SwallowsPayloadMarshalError(t *testing.T) {
	svc, _, repo := newServiceFixture(t, webhooks.Config{SigningKey: testFixtureSigningKey})
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"marshal-error",
		nil,
		"https://example.com/hook",
		smsgateway.WebhookEventSmsSent,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	svc.EmitForTest(smsgateway.WebhookEventSmsSent, func() {})

	items, err := repo.DueItems(ctx, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("queued items = %+v, want none after marshal error", items)
	}
}

// TestMetrics_Mutators pins that each public mutator increments its own counter.
func TestMetrics_Mutators(t *testing.T) {
	metrics := newTestMetrics()
	metrics.IncEnqueued()
	metrics.IncDelivered()
	metrics.IncFailed()
	metrics.IncPermanentlyFailed()

	tests := []struct {
		name string
		want float64
	}{
		{"at_gateway_webhooks_enqueued_total", 1},
		{"at_gateway_webhooks_delivered_total", 1},
		{"at_gateway_webhooks_failed_total", 1},
		{"at_gateway_webhooks_permanently_failed_total", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webhooks.CounterValue(metrics, tt.name); got != tt.want {
				t.Fatalf("counter value = %v, want %v", got, tt.want)
			}
		})
	}
}

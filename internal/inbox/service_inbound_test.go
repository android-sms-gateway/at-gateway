package inbox_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/uptrace/bun"
	"go.uber.org/zap"
)

// inboundSender is the originating address of every fixture part.
const inboundSender = "+79990001234"

// inboundFixture wires the inbound consumer: the inbox service, the webhook
// registry/queue behind it, isolated metrics and the modem service whose
// inbound channel the tests push parts into.
type inboundFixture struct {
	service  *inbox.Service
	webhooks *webhooks.Service
	repo     *webhooks.Repository
	metrics  *inbox.Metrics
	modem    *modem.Service
	inbox    *inbox.Repository
	devices  *devices.Service
	db       *bun.DB
}

func newInboundFixture(t *testing.T, config inbox.Config) inboundFixture {
	t.Helper()

	bunDB := newInboundDB(t)

	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}
	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())

	metrics := inbox.NewTestMetrics(prometheus.NewRegistry())
	webhooksSvc, err := webhooks.NewService(
		webhooks.Config{
			SigningKey: "messages-inbound-test-key",
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
	// state, which is empty here - exactly the "SIM reports no MSISDN" case the
	// payload contract allows (recipient omitted).
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

	service := inbox.NewService(
		config,
		inbox.NewRepository(bunDB),
		devicesSvc,
		modemSvc,
		webhooksSvc,
		metrics,
		zap.NewNop(),
	)

	return inboundFixture{
		service:  service,
		webhooks: webhooksSvc,
		repo:     webhooks.NewRepository(bunDB),
		metrics:  metrics,
		modem:    modemSvc,
		inbox:    inbox.NewRepository(bunDB),
		devices:  devicesSvc,
		db:       bunDB,
	}
}

// registerWebhook registers a webhook for an event and returns its id.
func (f inboundFixture) registerWebhook(t *testing.T, id string, event smsgateway.WebhookEvent) {
	t.Helper()

	err := f.webhooks.Replace(context.Background(), &smsgateway.Webhook{
		ID:    id,
		URL:   "https://example.com/" + id,
		Event: event,
	})
	if err != nil {
		t.Fatalf("register webhook %s: %v", id, err)
	}
}

// storedEnvelopes returns the RAW stored envelope bytes keyed by webhook id.
func (f inboundFixture) storedEnvelopes(t *testing.T) map[string]string {
	t.Helper()

	items, err := f.repo.DueItems(context.Background(), time.Now().Add(time.Hour), 32)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	envelopes := make(map[string]string, len(items))
	for _, item := range items {
		envelopes[item.WebhookID] = item.Payload
	}

	return envelopes
}

// storedCountsByWebhook counts the queue items currently stored per webhook.
func (f inboundFixture) storedCountsByWebhook(t *testing.T) map[string]int {
	t.Helper()

	items, err := f.repo.DueItems(context.Background(), time.Now().Add(time.Hour), 64)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	counts := make(map[string]int, len(items))
	for _, item := range items {
		counts[item.WebhookID]++
	}

	return counts
}

// storedCount is the number of queue items currently stored (one per
// webhook-and-message pair).
func (f inboundFixture) storedCount(t *testing.T) int {
	t.Helper()

	items, err := f.repo.DueItems(context.Background(), time.Now().Add(time.Hour), 64)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	return len(items)
}

// payloadsOf extracts the payload sub-documents of the stored envelopes, keyed
// by webhook id, WITHOUT re-marshaling them.
func (f inboundFixture) payloadsOf(t *testing.T) map[string]json.RawMessage {
	t.Helper()

	payloads := make(map[string]json.RawMessage)
	for id, raw := range f.storedEnvelopes(t) {
		var envelope struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
			t.Fatalf("decode envelope for %s: %v", id, err)
		}
		payloads[id] = envelope.Payload
	}

	return payloads
}

// TestMain warms the GSM charsets on the main goroutine before any fixture
// builds a modem service: warthog618/sms initialises them lazily, and the
// parallel tests below would otherwise race on that first construction.
func TestMain(m *testing.M) {
	modem.WarmGSMCharsets()
	os.Exit(m.Run())
}

// TestInbound_SinglePartEmitsOneSignedPayload pins the sms:received contract on
// the RAW stored bytes: phoneNumber repeats the sender, the recipient is
// omitted while the SIM reports no MSISDN, simNumber is 1 and receivedAt is the
// service-centre stamp in UTC.
func TestInbound_SinglePartEmitsOneSignedPayload(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	f.registerWebhook(t, "other", smsgateway.WebhookEventSmsSent)

	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase())

	payloads := f.payloadsOf(t)
	raw, ok := payloads["text"]
	if !ok {
		t.Fatalf("stored payloads = %v, want one for the sms:received hook", payloads)
	}

	var got struct {
		MessageID   string  `json:"messageId"`
		PhoneNumber string  `json:"phoneNumber"`
		Sender      string  `json:"sender"`
		Recipient   *string `json:"recipient"`
		SimNumber   *uint8  `json:"simNumber"`
		Message     string  `json:"message"`
		ReceivedAt  string  `json:"receivedAt"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	if got.Message != "hello" {
		t.Errorf("message = %q, want %q", got.Message, "hello")
	}
	if got.PhoneNumber != inboundSender || got.Sender != inboundSender {
		t.Errorf("phoneNumber/sender = %q/%q, want %q", got.PhoneNumber, got.Sender, inboundSender)
	}
	if got.Recipient != nil {
		t.Errorf("recipient = %q, want omitted while the SIM reports no MSISDN", *got.Recipient)
	}
	if got.SimNumber == nil || *got.SimNumber != 1 {
		t.Errorf("simNumber = %v, want 1", got.SimNumber)
	}
	if got.ReceivedAt != "2026-10-02T12:00:00Z" {
		t.Errorf("receivedAt = %q, want the SCTS in UTC RFC3339", got.ReceivedAt)
	}
	if len(got.MessageID) != 16 {
		t.Errorf("messageId = %q, want 16 hex chars", got.MessageID)
	}

	// The sms:sent hook must stay untouched.
	if _, exists := payloads["other"]; exists {
		t.Error("a non-matching webhook received an inbound event")
	}
	if got := inbox.InboundCounterValue(f.metrics, "at_gateway_inbox_events_total"); got != 1 {
		t.Errorf("inbound events = %v, want 1", got)
	}
}

// TestInbound_PortAddressedEmitsDataReceived pins the sms:data-received
// contract: the assembled octets base64 NO_WRAP encoded, and no message field.
func TestInbound_PortAddressedEmitsDataReceived(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "data", smsgateway.WebhookEventSmsDataReceived)
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)

	f.service.HandleInboundPartForTest(dataPart("\xca\xfe\xba\xbe", 80), inboundBase())

	payloads := f.payloadsOf(t)
	if _, exists := payloads["text"]; exists {
		t.Fatal("a port-addressed SMS must not be emitted as sms:received")
	}

	var got struct {
		MessageID  string  `json:"messageId"`
		Data       string  `json:"data"`
		Message    *string `json:"message"`
		ReceivedAt string  `json:"receivedAt"`
	}
	if err := json.Unmarshal(payloads["data"], &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	if got.Data != "yv66vg==" { // base64 of cafe babe
		t.Errorf("data = %q, want %q", got.Data, "yv66vg==")
	}
	if strings.ContainsAny(got.Data, "\n\r") {
		t.Errorf("data = %q, want a single base64 line (NO_WRAP)", got.Data)
	}
	if got.Message != nil {
		t.Errorf("message = %q, want absent for a data SMS", *got.Message)
	}
	if got.MessageID == "" {
		t.Error("messageId is empty")
	}
	if got.ReceivedAt != "2026-10-02T12:00:00Z" {
		t.Errorf("receivedAt = %q, want the SCTS in UTC", got.ReceivedAt)
	}
	if value := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_events_total:data",
	); value != 1 {
		t.Errorf("inbound data events = %v, want 1", value)
	}
}

// TestInbound_ConcatReassemblesIntoOneEvent pins the reassembly for both
// reference widths and both arrival orders: exactly ONE event, with the parts
// concatenated in SEQUENCE order regardless of the order they arrived in.
func TestInbound_ConcatReassemblesIntoOneEvent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		refWidth   int
		sequences  []int
		wantEvents float64
	}{
		{name: "8-bit in order", refWidth: 8, sequences: []int{1, 2}, wantEvents: 1},
		{name: "8-bit out of order", refWidth: 8, sequences: []int{2, 1}, wantEvents: 1},
		{name: "16-bit in order", refWidth: 16, sequences: []int{1, 2}, wantEvents: 1},
		{name: "16-bit out of order", refWidth: 16, sequences: []int{2, 1}, wantEvents: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
			f.registerWebhook(t, "concat", smsgateway.WebhookEventSmsReceived)

			for _, sequence := range tt.sequences {
				// The content is bound to the SEQUENCE number, so an
				// out-of-order arrival must still assemble in order.
				part := concatPart(partText(sequence-1), 2, sequence, 0x42, tt.refWidth)
				f.service.HandleInboundPartForTest(part, inboundBase())
			}

			payloads := f.payloadsOf(t)
			raw, exists := payloads["concat"]
			if !exists {
				t.Fatalf("stored payloads = %v, want exactly one event", payloads)
			}

			var got struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if got.Message != partText(0)+partText(1) {
				t.Errorf("message = %q, want the parts in sequence order %q",
					got.Message, partText(0)+partText(1))
			}
			if groups := f.service.InboundGroupsForTest(); len(groups) != 0 {
				t.Errorf("reassembly groups = %+v, want none after completion", groups)
			}
			if value := inbox.InboundCounterValue(
				f.metrics,
				"at_gateway_inbox_events_total",
			); value != tt.wantEvents {
				t.Errorf("inbound events = %v, want %v", value, tt.wantEvents)
			}
		})
	}
}

// TestInbound_ConcatRefCollisionDoesNotMerge pins the (sender, refWidth, ref)
// key: a 0x42 8-bit reference and a 0x0042 16-bit reference from the SAME
// sender are two independent messages, interleaved part by part.
func TestInbound_ConcatRefCollisionDoesNotMerge(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "narrow", smsgateway.WebhookEventSmsReceived)

	// Interleave the two messages: 8-bit seq 1, 16-bit seq 1, 8-bit seq 2,
	// 16-bit seq 2. A width-agnostic key would merge them into one.
	f.service.HandleInboundPartForTest(concatPart("1111", 2, 1, 0x42, 8), inboundBase())
	f.service.HandleInboundPartForTest(concatPart("2222", 2, 1, 0x42, 16), inboundBase())
	f.service.HandleInboundPartForTest(concatPart("3333", 2, 2, 0x42, 8), inboundBase())
	f.service.HandleInboundPartForTest(concatPart("4444", 2, 2, 0x42, 16), inboundBase())

	items, readErr := f.repo.DueItems(context.Background(), time.Now().Add(time.Hour), 8)
	if readErr != nil {
		t.Fatalf("read queued items: %v", readErr)
	}
	if len(items) != 2 {
		t.Fatalf("queued items = %d, want 2 independent events", len(items))
	}

	messagesSeen := make(map[string]bool, 2)
	for _, item := range items {
		var got struct {
			Message string `json:"message"`
		}
		var envelope struct {
			Payload json.RawMessage `json:"payload"`
		}
		if decodeErr := json.Unmarshal([]byte(item.Payload), &envelope); decodeErr != nil {
			t.Fatalf("decode envelope: %v", decodeErr)
		}
		if decodeErr := json.Unmarshal(envelope.Payload, &got); decodeErr != nil {
			t.Fatalf("decode payload: %v", decodeErr)
		}
		messagesSeen[got.Message] = true
	}
	for _, want := range []string{"11113333", "22224444"} {
		if !messagesSeen[want] {
			t.Errorf("assembled messages = %v, want %q among them", messagesSeen, want)
		}
	}
}

// TestInbound_SlowMultipartWithinWindowEmitsOneEvent pins that a legitimate but
// SLOW concatenation (parts minutes apart, inside the configured window) still
// yields exactly one event instead of two partial ones.
func TestInbound_SlowMultipartWithinWindowEmitsOneEvent(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{
		ReassemblyTimeout: 5 * time.Minute,
		SweepInterval:     0,
	})
	f.registerWebhook(t, "slow", smsgateway.WebhookEventSmsReceived)

	f.service.HandleInboundPartForTest(concatPart("aaaa", 2, 1, 0x07, 8), inboundBase())
	f.service.HandleInboundPartForTest(
		concatPart("bbbb", 2, 2, 0x07, 8),
		inboundBase().Add(4*time.Minute),
	)

	items, readErr := f.repo.DueItems(context.Background(), time.Now().Add(time.Hour), 8)
	if readErr != nil {
		t.Fatalf("read queued items: %v", readErr)
	}
	if len(items) != 1 {
		t.Fatalf("queued items = %d, want exactly 1 event for a slow multipart message", len(items))
	}
	var envelope struct {
		Payload struct {
			Message string `json:"message"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(items[0].Payload), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Payload.Message != "aaaabbbb" {
		t.Errorf("message = %q, want %q", envelope.Payload.Message, "aaaabbbb")
	}
}

// TestInbound_ReassemblyTimeoutEvictsAndCounts pins the eviction: a part whose
// siblings never arrive is dropped (never emitted as a partial message) once
// the configured window elapsed, and the timeout counter moves.
func TestInbound_ReassemblyTimeoutEvictsAndCounts(t *testing.T) {
	t.Parallel()

	const timeout = 5 * time.Minute
	f := newInboundFixture(t, inbox.Config{
		ReassemblyTimeout: timeout,
		SweepInterval:     0,
	})
	f.registerWebhook(t, "timeout", smsgateway.WebhookEventSmsReceived)

	f.service.HandleInboundPartForTest(concatPart("aaaa", 2, 1, 0x07, 8), inboundBase())

	// Just inside the window the group survives...
	f.service.SweepInboundForTest(inboundBase().Add(timeout - time.Second))
	if groups := f.service.InboundGroupsForTest(); len(groups) != 1 {
		t.Fatalf("reassembly groups = %+v, want the group kept inside the window", groups)
	}
	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_reassembly_timeouts_total",
	); got != 0 {
		t.Errorf("reassembly timeouts = %v, want 0 inside the window", got)
	}

	// ...and at the window it is evicted, counted and never emitted.
	f.service.SweepInboundForTest(inboundBase().Add(timeout))
	if groups := f.service.InboundGroupsForTest(); len(groups) != 0 {
		t.Errorf("reassembly groups = %+v, want none after the window", groups)
	}
	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_reassembly_timeouts_total",
	); got != 1 {
		t.Errorf("reassembly timeouts = %v, want 1", got)
	}
	if payloads := f.payloadsOf(t); len(payloads) != 0 {
		t.Errorf("stored payloads = %v, want none for an incomplete message", payloads)
	}

	// The late sibling starts a NEW group instead of completing the evicted
	// one.
	f.service.HandleInboundPartForTest(concatPart("bbbb", 2, 2, 0x07, 8), inboundBase().Add(timeout))
	if groups := f.service.InboundGroupsForTest(); len(groups) != 1 {
		t.Errorf("reassembly groups = %+v, want the late sibling buffered", groups)
	}
	if payloads := f.payloadsOf(t); len(payloads) != 0 {
		t.Errorf("stored payloads = %v, want none for a half message", payloads)
	}
}

// TestInbound_GroupCapacityEvictionCounts pins the SECOND cause of the same
// eviction counter: once the concurrent-group bound is reached the oldest
// incomplete group is evicted, counted exactly like a window timeout, and never
// emitted as a partial message.
func TestInbound_GroupCapacityEvictionCounts(t *testing.T) {
	t.Parallel()

	const capacity = 512
	f := newInboundFixture(t, inbox.Config{
		ReassemblyTimeout: time.Hour,
		SweepInterval:     0,
	})
	f.registerWebhook(t, "capacity", smsgateway.WebhookEventSmsReceived)

	// Fill the group bound with parts of half-delivered concatenations. Each
	// part carries its own instant, so "oldest" is deterministic and nothing
	// expires inside the sweep.
	for ref := range capacity {
		f.service.HandleInboundPartForTest(
			concatPart("aaaa", 2, 1, ref, 16),
			inboundBase().Add(time.Duration(ref)*time.Second),
		)
	}
	groups := f.service.InboundGroupsForTest()
	if len(groups) != capacity {
		t.Fatalf("reassembly groups = %d, want the pinned capacity %d", len(groups), capacity)
	}
	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_reassembly_timeouts_total",
	); got != 0 {
		t.Fatalf("reassembly evictions = %v, want 0 below the capacity bound", got)
	}

	// The group that exceeds the bound evicts the OLDEST one and counts it.
	f.service.HandleInboundPartForTest(
		concatPart("bbbb", 2, 1, capacity, 16),
		inboundBase().Add(time.Duration(capacity)*time.Second),
	)

	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_reassembly_timeouts_total",
	); got != 1 {
		t.Errorf("reassembly evictions = %v, want 1 for the capacity eviction", got)
	}
	groups = f.service.InboundGroupsForTest()
	if len(groups) != capacity {
		t.Errorf("reassembly groups = %d, want the capacity bound %d kept", len(groups), capacity)
	}
	oldest := inbox.NewInboundKey(inboundSender, 16, 0)
	if _, kept := groups[oldest]; kept {
		t.Errorf("reassembly groups keep the oldest key, want it evicted")
	}
	newest := inbox.NewInboundKey(inboundSender, 16, capacity)
	if _, buffered := groups[newest]; !buffered {
		t.Errorf("reassembly groups miss the newest key, want it buffered")
	}
	if payloads := f.payloadsOf(t); len(payloads) != 0 {
		t.Errorf("stored payloads = %v, want none for the evicted group", payloads)
	}
}

// TestInbound_ReassemblyEvictionMetricHelpNamesBothCauses pins that the single
// eviction counter describes BOTH of its causes (an elapsed window and the
// concurrent-group capacity). An operator alerting on it must not be told it
// only counts timeouts, which is why the Help text is read off the PRODUCTION
// registration instead of a test-local copy.
func TestInbound_ReassemblyEvictionMetricHelpNamesBothCauses(t *testing.T) {
	t.Parallel()

	help := productionMetricHelp(t, "at_gateway_inbox_reassembly_timeouts_total")

	for _, want := range []string{"window", "capacity"} {
		if !strings.Contains(help, want) {
			t.Errorf("help %q does not name the %q cause", help, want)
		}
	}
	if strings.Contains(help, "timeout") {
		t.Errorf("help %q describes only the timeout cause", help)
	}
}

// TestInbound_ShutdownAbortsIncompleteGroups pins the shutdown accounting: the
// groups that never completed are counted when the worker stops.
func TestInbound_ShutdownAbortsIncompleteGroups(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "abort", smsgateway.WebhookEventSmsReceived)

	f.service.HandleInboundPartForTest(concatPart("aaaa", 2, 1, 0x07, 8), inboundBase())
	f.service.HandleInboundPartForTest(concatPart("bbbb", 3, 1, 0x08, 8), inboundBase())

	f.service.AbortInboundForTest()

	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_reassembly_aborted_total",
	); got != 2 {
		t.Errorf("reassembly aborted = %v, want 2", got)
	}
	if groups := f.service.InboundGroupsForTest(); len(groups) != 0 {
		t.Errorf("reassembly groups = %+v, want none after shutdown", groups)
	}
}

// TestInbound_ShutdownAbortRunsFromTheRunLoop pins that stopping Run really
// performs the abort accounting (the production call site, not just the
// exported helper).
func TestInbound_ShutdownAbortRunsFromTheRunLoop(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: time.Hour})
	f.registerWebhook(t, "abort", smsgateway.WebhookEventSmsReceived)

	f.service.HandleInboundPartForTest(concatPart("aaaa", 2, 1, 0x07, 8), inboundBase())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- f.service.Run(ctx)
	}()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}

	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_reassembly_aborted_total",
	); got != 1 {
		t.Errorf("reassembly aborted = %v, want 1 after the worker stopped", got)
	}
}

// TestInbound_DedupeSuppressesWithinTheWindowAndReEmitsAfterIt pins the
// damper: a modem re-notification of an UNDELETED index inside the 10-minute
// window produces no second queue item, and after the window it does.
func TestInbound_DedupeSuppressesWithinTheWindowAndReEmitsAfterIt(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "dedupe", smsgateway.WebhookEventSmsReceived)

	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase())
	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase().Add(time.Minute))

	if got := f.storedCount(t); got != 1 {
		t.Fatalf("stored envelopes = %d, want 1 - the in-window re-notification must be suppressed", got)
	}
	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_dedupe_blocked_total",
	); got != 1 {
		t.Errorf("dedupe blocked = %v, want 1", got)
	}

	// After the window the dedupe has forgotten the id, so the re-notification
	// is emitted again (webhook delivery is at-least-once by contract).
	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase().Add(11*time.Minute))
	if got := f.storedCount(t); got != 2 {
		t.Errorf("stored envelopes = %d, want 2 after the dedupe window", got)
	}
}

// TestInbound_DedupeMarksPerWebhook pins the round-4 marking rule: only the
// webhooks whose enqueue SUCCEEDED are marked. The failing hook is therefore
// retried on the next notification, while the served ones are not - the
// all-or-nothing mark would suppress the failed hook too and silently lose the
// event (a) and would blast duplicates at every other hook (c).
func TestInbound_DedupeMarksPerWebhook(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	for _, id := range []string{"ok-1", "ok-2", "broken"} {
		f.registerWebhook(t, id, smsgateway.WebhookEventSmsReceived)
	}

	// Inject a per-webhook enqueue failure for the third hook: the queue insert
	// aborts for it while the other two store normally.
	if _, err := f.db.ExecContext(context.Background(), `
		CREATE TRIGGER fail_hook_broken BEFORE INSERT ON webhook_queue
		WHEN NEW.webhook_id = 'broken'
		BEGIN SELECT RAISE(ABORT, 'injected enqueue failure'); END;`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase())
	if got := f.storedCountsByWebhook(t); got["ok-1"] != 1 || got["ok-2"] != 1 || got["broken"] != 0 {
		t.Fatalf("stored items = %+v, want ok-1/ok-2 stored and broken failed", got)
	}

	// The modem keeps re-notifying the undeleted index; the failure is gone.
	if _, err := f.db.ExecContext(context.Background(), `DROP TRIGGER fail_hook_broken`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase().Add(time.Minute))

	// (a)+(c): EXACTLY the failed hook is retried. (b): the two served hooks
	// gain no second queue item.
	got := f.storedCountsByWebhook(t)
	if got["broken"] != 1 {
		t.Errorf(
			"failed hook stored %d items, want 1 from the retry - a failed enqueue must never be marked",
			got["broken"],
		)
	}
	if got["ok-1"] != 1 || got["ok-2"] != 1 {
		t.Errorf(
			"served hooks stored %d/%d items, want 1 each - a served webhook must never get a duplicate",
			got["ok-1"],
			got["ok-2"],
		)
	}
	if value := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_dedupe_blocked_total",
	); value != 1 {
		t.Errorf("dedupe blocked = %v, want 1 (the emission for the two served hooks)", value)
	}
}

// TestInbound_DedupeDropsTheMarksOfAnExpiredEntry pins the counterpart of
// served: an elapsed entry is forgotten by mark, not only by sweep. Keeping the
// marks would hide a webhook whose re-enqueue just failed for a whole FRESH
// TTL instead of the leftover one, which breaks the rule mark documents.
func TestInbound_DedupeDropsTheMarksOfAnExpiredEntry(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	dedupe := f.service.DedupeForTest()

	dedupe.MarkServedForTest("message", "hook-a", inboundBase())
	dedupe.MarkServedForTest("message", "hook-b", inboundBase())

	// The window elapses. The sweep has NOT run, so the entry - carrying both
	// marks - is still on the map: served already reports nothing for it.
	expired := inboundBase().Add(dedupe.TTLForTest() + time.Second)
	if served := dedupe.ServedForTest("message", expired); len(served) != 0 {
		t.Fatalf("expired entry served %v, want nothing", served)
	}

	// The modem re-notifies the undeleted index and only hook-a's enqueue
	// succeeds, so only hook-a may be marked.
	dedupe.MarkServedForTest("message", "hook-a", expired)

	served := dedupe.ServedForTest("message", expired)
	if len(served) != 1 || served[0] != "hook-a" {
		t.Errorf(
			"served %v, want [hook-a] - the stale hook-b mark must not outlive the expiry",
			served,
		)
	}
}

// TestInbound_MessageIDIsDeterministicAndStampFree pins the identity: the same
// content yields the same 16-hex-char id, different content or port yields a
// different one, and a message without a usable TP-SCTS gets a STABLE id that
// omits the stamp (so a re-notification still maps to the same id).
func TestInbound_MessageIDIsDeterministicAndStampFree(t *testing.T) {
	t.Parallel()

	base := inbox.NewInboundMessage(inboundSender, "hello", false, 0, inboundBase(), true, nil)
	want := inbox.InboundMessageID(base)

	if len(want) != 16 {
		t.Fatalf("messageId = %q, want 16 hex chars", want)
	}
	if got := inbox.InboundMessageID(base); got != want {
		t.Errorf("messageId = %q, want the stable %q", got, want)
	}

	otherContent := inbox.NewInboundMessage(inboundSender, "hello!", false, 0, inboundBase(), true, nil)
	if got := inbox.InboundMessageID(otherContent); got == want {
		t.Error("a different body produced the same messageId")
	}

	otherPort := inbox.NewInboundMessage(inboundSender, "", true, 80, inboundBase(), true, nil)
	if got := inbox.InboundMessageID(otherPort); got == want {
		t.Error("a different destination port produced the same messageId")
	}

	otherSender := inbox.NewInboundMessage("+79990009999", "hello", false, 0, inboundBase(), true, nil)
	if got := inbox.InboundMessageID(otherSender); got == want {
		t.Error("a different sender produced the same messageId")
	}

	otherConcat := inbox.NewInboundMessage(
		inboundSender,
		"hello",
		false,
		0,
		inboundBase(),
		true,
		&modem.ConcatRef{Segments: 2, Sequence: 1, Ref: 1, RefWidth: 8},
	)
	if got := inbox.InboundMessageID(otherConcat); got == want {
		t.Error("a concatenated message produced the same messageId as a single-part one")
	}

	// A malformed/absent SCTS drops the stamp component: the same content must
	// still map to one id, and the identity must NOT depend on the observation
	// time (which the consumer substitutes into receivedAt).
	stampFree := inbox.NewInboundMessage(inboundSender, "hello", false, 0, time.Time{}, false, nil)
	stampFreeID := inbox.InboundMessageID(stampFree)

	stampFreeLater := inbox.NewInboundMessage(
		inboundSender,
		"hello",
		false,
		0,
		inboundBase().Add(time.Hour),
		false,
		nil,
	)
	if got := inbox.InboundMessageID(stampFreeLater); got != stampFreeID {
		t.Errorf("stamp-free messageId = %q, want the stable %q", got, stampFreeID)
	}
	if got := inbox.InboundMessageID(stampFree); got != stampFreeID {
		t.Errorf("messageId = %q, want the stable %q", got, stampFreeID)
	}
	if strings.EqualFold(stampFreeID, want) {
		t.Error("the stamp-free identity must differ from the stamped one")
	}
}

// TestInbound_MalformedSCTSUsesTheStampFreeIdentityAndObservationTime pins the
// stable fallback end to end: a part with an unusable TP-SCTS still produces an
// event, its receivedAt falls back to the observation time and two deliveries
// of the same content collapse onto ONE id (and therefore one emission).
func TestInbound_MalformedSCTSUsesTheStampFreeIdentityAndObservationTime(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "nostamp", smsgateway.WebhookEventSmsReceived)

	part := textPart("hello")
	part.TimestampValid = false
	part.ReceivedAt = time.Time{}

	observed := inboundBase()
	f.service.HandleInboundPartForTest(part, observed)
	f.service.HandleInboundPartForTest(part, observed.Add(30*time.Second))

	items, readErr := f.repo.DueItems(context.Background(), time.Now().Add(time.Hour), 8)
	if readErr != nil {
		t.Fatalf("read queued items: %v", readErr)
	}
	if len(items) != 1 {
		t.Fatalf("queued items = %d, want 1 - the second delivery shares the id", len(items))
	}

	var envelope struct {
		Payload struct {
			MessageID  string `json:"messageId"`
			ReceivedAt string `json:"receivedAt"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(items[0].Payload), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Payload.ReceivedAt != "2026-10-02T12:00:00Z" {
		t.Errorf("receivedAt = %q, want the observation time", envelope.Payload.ReceivedAt)
	}
	if len(envelope.Payload.MessageID) != 16 {
		t.Errorf("messageId = %q, want 16 hex chars", envelope.Payload.MessageID)
	}
}

// TestInbound_DedupeCapacityEvictsTheOldest pins the pinned capacity of 1024
// remembered message ids.
func TestInbound_DedupeCapacityEvictsTheOldest(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	dedupe := f.service.DedupeForTest()

	if got := dedupe.CapacityForTest(); got != 1024 {
		t.Fatalf("dedupe capacity = %d, want the pinned 1024", got)
	}

	for i := range dedupe.CapacityForTest() {
		dedupe.MarkServedForTest(idForIndex(i), "hook", inboundBase())
	}
	if got := dedupe.LenForTest(); got != 1024 {
		t.Fatalf("dedupe size = %d, want 1024", got)
	}

	dedupe.MarkServedForTest("message-overflow", "hook", inboundBase())
	if got := dedupe.LenForTest(); got != 1024 {
		t.Errorf("dedupe size = %d, want the capacity to stay pinned at 1024", got)
	}
	if served := dedupe.ServedForTest("message-0", inboundBase()); len(served) != 0 {
		t.Errorf("oldest entry served %v, want it evicted", served)
	}
	if served := dedupe.ServedForTest("message-overflow", inboundBase()); len(served) != 1 {
		t.Errorf("newest entry served %v, want it remembered", served)
	}
}

// TestInbound_PostRestartWindowCountsAcceptedEmissions pins the post-restart
// observability: inside the observation window an accepted emission is counted
// (the dedupe has no memory of a previous process), and the counting stops once
// the window elapsed.
func TestInbound_PostRestartWindowCountsAcceptedEmissions(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{
		ReassemblyTimeout: 5 * time.Minute,
		SweepInterval:     0,
	})
	f.registerWebhook(t, "restart", smsgateway.WebhookEventSmsReceived)

	started := inboundBase()
	f.service.SetStartedAtForTest(started)

	f.service.HandleInboundPartForTest(textPart("first"), started.Add(time.Minute))
	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_dedupe_skipped_post_restart_total",
	); got != 1 {
		t.Errorf("post-restart emissions = %v, want 1", got)
	}

	// After the window the counter stops: from then on the dedupe has its own
	// memory and suppression is the normal path.
	f.service.HandleInboundPartForTest(textPart("second"), started.Add(6*time.Minute))
	f.service.HandleInboundPartForTest(textPart("third"), started.Add(6*time.Minute+time.Second))
	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_dedupe_skipped_post_restart_total",
	); got != 1 {
		t.Errorf("post-restart emissions = %v, want the counting to stop after the window", got)
	}
}

// inboundBase is the service-centre stamp every inbound fixture carries; it is
// the value the emitted receivedAt must reproduce in UTC. It is a function
// because the linters forbid package-level variables in tests.
func inboundBase() time.Time {
	return time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
}

// textPart is a single-part text SMS-DELIVER fixture.
func textPart(content string) modem.InboundPart {
	return modem.InboundPart{
		Sender:         inboundSender,
		ReceivedAt:     inboundBase(),
		TimestampValid: true,
		Content:        content,
		Port:           0,
		Ported:         false,
		Concat:         nil,
	}
}

// concatPart is part `sequence` of a `segments`-part message.
func concatPart(
	content string,
	segments, sequence, ref, refWidth int,
) modem.InboundPart {
	return modem.InboundPart{
		Sender:         inboundSender,
		ReceivedAt:     inboundBase(),
		TimestampValid: true,
		Content:        content,
		Port:           0,
		Ported:         false,
		Concat: &modem.ConcatRef{
			Segments: segments,
			Sequence: sequence,
			Ref:      ref,
			RefWidth: refWidth,
		},
	}
}

// dataPart is a port-addressed data SMS-DELIVER fixture.
func dataPart(content string, port uint16) modem.InboundPart {
	return modem.InboundPart{
		Sender:         inboundSender,
		ReceivedAt:     inboundBase(),
		TimestampValid: true,
		Content:        content,
		Port:           port,
		Ported:         true,
		Concat:         nil,
	}
}

// partText is the content of part index i (0-based) in the concat fixtures.
func partText(index int) string {
	return "abcdefgh"[index*4 : index*4+4]
}

// productionMetricHelp reads the Help string of a metric straight off the
// default registry. NewMetrics registers there and panics on a duplicate, so
// the family is built only while it is still absent: a repeated -count run
// reuses the already-registered one.
func productionMetricHelp(t *testing.T, name string) string {
	t.Helper()

	if help, found := gatheredMetricHelp(name); found {
		return help
	}

	inbox.NewMetrics()

	help, found := gatheredMetricHelp(name)
	if !found {
		t.Fatalf("metric %s was not registered by NewMetrics", name)
	}

	return help
}

func gatheredMetricHelp(name string) (string, bool) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return "", false
	}

	for _, family := range families {
		if family.GetName() == name {
			return family.GetHelp(), true
		}
	}

	return "", false
}

// idForIndex is the deterministic message id of the capacity fixture.
func idForIndex(index int) string {
	return "message-" + strconv.Itoa(index)
}

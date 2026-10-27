package inbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// replayQueueLimit is the read bound of the UN-CAPPED replay fixtures; the
// shared storedCount helper is capped at 64, which the 501-row fixture exceeds.
const replayQueueLimit = 1024

// queuedCountAll counts the stored queue items with a bound large enough for
// the no-cap replay fixtures.
func (f inboundFixture) queuedCountAll(t *testing.T) int {
	t.Helper()

	items, err := f.repo.DueItems(context.Background(), time.Now().Add(time.Hour), replayQueueLimit)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	return len(items)
}

// queuedPayloadsByWebhook groups the stored payload sub-documents by webhook id
// WITHOUT re-marshaling them. Unlike payloadsOf it keeps every item, so one
// webhook with many replayed events stays observable.
func (f inboundFixture) queuedPayloadsByWebhook(t *testing.T) map[string][]json.RawMessage {
	t.Helper()

	grouped := make(map[string][]json.RawMessage)
	for _, item := range f.allQueuedItems(t) {
		var envelope struct {
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(item.Payload), &envelope); err != nil {
			t.Fatalf("decode envelope for %s: %v", item.WebhookID, err)
		}
		grouped[item.WebhookID] = append(grouped[item.WebhookID], envelope.Payload)
	}

	return grouped
}

// allQueuedItems returns every pending/failed queue item within the replay
// read bound.
func (f inboundFixture) allQueuedItems(t *testing.T) []webhooks.QueueItem {
	t.Helper()

	items, err := f.repo.DueItems(context.Background(), time.Now().Add(time.Hour), replayQueueLimit)
	if err != nil {
		t.Fatalf("read queued items: %v", err)
	}

	return items
}

// TestService_PersistsTheAssembledMessage pins the persistence contract: every
// assembled message becomes a stored row owned by the LOCAL device, and the
// stored content is the wire-ready body - the assembled text for an SMS, the
// base64 NO_WRAP encoding of the raw octets for a DATA_SMS.
func TestService_PersistsTheAssembledMessage(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})

	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase())
	f.service.HandleInboundPartForTest(dataPart("\xca\xfe\xba\xbe", 80), inboundBase())

	deviceID := f.devices.Get().ID
	rows, total, err := f.service.List(context.Background(), inbox.ListOptions{
		Filter: &inbox.ListFilter{Types: allTypes()},
	})
	if err != nil {
		t.Fatalf("list the stored messages: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("rows = %d, total = %d, want 2 stored messages", len(rows), total)
	}

	for _, row := range rows {
		if row.DeviceID != deviceID {
			t.Errorf("device id = %q, want the local device %q", row.DeviceID, deviceID)
		}
	}

	if rows[0].Type != smsgateway.IncomingMessageTypeDataSMS {
		t.Errorf("type = %q, want %q", rows[0].Type, smsgateway.IncomingMessageTypeDataSMS)
	}
	if rows[0].Content != "yv66vg==" {
		t.Errorf("data content = %q, want the base64 NO_WRAP payload %q", rows[0].Content, "yv66vg==")
	}
	if rows[1].Type != smsgateway.IncomingMessageTypeSMS {
		t.Errorf("type = %q, want %q", rows[1].Type, smsgateway.IncomingMessageTypeSMS)
	}
	if rows[1].Content != "hello" {
		t.Errorf("text content = %q, want %q", rows[1].Content, "hello")
	}

	if got := inbox.InboundCounterValue(f.metrics, "at_gateway_inbox_stored_total"); got != 2 {
		t.Errorf("stored messages = %v, want 2", got)
	}
	if got := inbox.InboundCounterValue(f.metrics, "at_gateway_inbox_store_errors_total"); got != 0 {
		t.Errorf("store errors = %v, want 0", got)
	}
}

// TestService_ListReturnsOnlyTheLocalDeviceRows pins the ownership filter: the
// listing always belongs to the local device, so a row stored under another
// device id is never listed.
func TestService_ListReturnsOnlyTheLocalDeviceRows(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})

	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase())

	foreignDevice := "000000000000000000000"
	if err := f.inbox.Insert(context.Background(), inbox.IncomingMessage{
		ID:         "ffffffffffffffff",
		DeviceID:   foreignDevice,
		Type:       smsgateway.IncomingMessageTypeSMS,
		Sender:     "+79990009999",
		Recipient:  nil,
		SimNumber:  nil,
		Content:    "foreign",
		ReceivedAt: inboundBase(),
	}); err != nil {
		t.Fatalf("insert the foreign row: %v", err)
	}

	rows, total, err := f.service.List(context.Background(), inbox.ListOptions{
		Filter: &inbox.ListFilter{Types: allTypes()},
	})
	if err != nil {
		t.Fatalf("list the stored messages: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("rows = %d, total = %d, want only the local device row", len(rows), total)
	}
	if rows[0].Content != "hello" {
		t.Errorf("content = %q, want the local row %q", rows[0].Content, "hello")
	}
}

// TestService_ListRejectsAForeignDevice pins the device id VALIDATION: an
// explicitly supplied device id that is not the local one is rejected instead
// of filtering the rows.
func TestService_ListRejectsAForeignDevice(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})

	foreignDevice := "000000000000000000000"
	_, _, err := f.service.List(context.Background(), inbox.ListOptions{
		Filter: &inbox.ListFilter{DeviceID: &foreignDevice, Types: allTypes()},
	})
	if !errors.Is(err, inbox.ErrDeviceNotFound) {
		t.Fatalf("error = %v, want %v", err, inbox.ErrDeviceNotFound)
	}
}

// TestService_ListAcceptsTheLocalDevice pins that the local device id passes
// the validation.
func TestService_ListAcceptsTheLocalDevice(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})

	deviceID := f.devices.Get().ID
	_, _, err := f.service.List(context.Background(), inbox.ListOptions{
		Filter: &inbox.ListFilter{DeviceID: &deviceID, Types: allTypes()},
	})
	if err != nil {
		t.Fatalf("list the stored messages: %v", err)
	}
}

// TestService_ListRejectsAnInvertedDateRange pins the date range validation:
// a lower bound after the upper bound is rejected.
func TestService_ListRejectsAnInvertedDateRange(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})

	from := inboundBase()
	to := from.Add(-time.Minute)

	_, _, err := f.service.List(context.Background(), inbox.ListOptions{
		Filter: &inbox.ListFilter{Types: allTypes(), From: &from, To: &to},
	})
	if !errors.Is(err, inbox.ErrInvalidDateRange) {
		t.Fatalf("error = %v, want %v", err, inbox.ErrInvalidDateRange)
	}
}

// TestService_StoreFailureKeepsTheEmission pins the persist-before-emit
// contract under a store failure: the row cannot be written, the failure is
// counted, and the webhook emission STILL happens - the emission is the
// notification, the row is the durable record.
func TestService_StoreFailureKeepsTheEmission(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "stored", smsgateway.WebhookEventSmsReceived)

	// The store is broken: the insert cannot succeed any more.
	if _, err := f.db.ExecContext(context.Background(), "DROP TABLE inbox"); err != nil {
		t.Fatalf("drop the inbox table: %v", err)
	}

	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase())

	if got := inbox.InboundCounterValue(f.metrics, "at_gateway_inbox_store_errors_total"); got != 1 {
		t.Errorf("store errors = %v, want 1", got)
	}
	if got := inbox.InboundCounterValue(f.metrics, "at_gateway_inbox_stored_total"); got != 0 {
		t.Errorf("stored messages = %v, want 0", got)
	}

	// The emission is NOT aborted by the store failure.
	if got := f.storedCount(t); got != 1 {
		t.Errorf("queued envelopes = %d, want 1 - a store failure must not abort the emission", got)
	}
	if got := inbox.InboundCounterValue(
		f.metrics,
		"at_gateway_inbox_events_total",
	); got != 1 {
		t.Errorf("inbound events = %v, want 1", got)
	}
}

// TestService_StoreTimeoutIsCountedAndTheEmissionSurvives pins the BOUNDED
// insert: a store write that cannot complete within the bound is counted as a
// store failure instead of stalling the consumer, and the emission still
// happens.
func TestService_StoreTimeoutIsCountedAndTheEmissionSurvives(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "stored", smsgateway.WebhookEventSmsReceived)

	// The pool holds a single connection: a transaction keeps it busy, so the
	// store write cannot get one until the bound elapses.
	tx, err := f.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin the blocking transaction: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(inbox.InboundStoreTimeout() + 500*time.Millisecond)
		_ = tx.Rollback()
	}()
	defer func() {
		<-released
	}()

	start := time.Now()
	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase())
	elapsed := time.Since(start)

	if elapsed < inbox.InboundStoreTimeout() {
		t.Errorf("the store write gave up after %s, want at least the %s bound",
			elapsed, inbox.InboundStoreTimeout())
	}
	if got := inbox.InboundCounterValue(f.metrics, "at_gateway_inbox_store_errors_total"); got != 1 {
		t.Errorf("store errors = %v, want 1", got)
	}
	if got := inbox.InboundCounterValue(f.metrics, "at_gateway_inbox_stored_total"); got != 0 {
		t.Errorf("stored messages = %v, want 0", got)
	}

	// The emission happens AFTER the bounded store write gave up.
	if got := f.storedCount(t); got != 1 {
		t.Errorf("queued envelopes = %d, want 1 - a store timeout must not abort the emission", got)
	}
}

// TestService_StoreFailureDoesNotStallTheConsumer pins that a broken store
// does not stop the consumer: the next message is still processed and still
// emitted.
func TestService_StoreFailureDoesNotStallTheConsumer(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "stored", smsgateway.WebhookEventSmsReceived)

	if _, err := f.db.ExecContext(context.Background(), "DROP TABLE inbox"); err != nil {
		t.Fatalf("drop the inbox table: %v", err)
	}

	f.service.HandleInboundPartForTest(textPart("first"), inboundBase())
	f.service.HandleInboundPartForTest(textPart("second"), inboundBase().Add(time.Minute))

	if got := inbox.InboundCounterValue(f.metrics, "at_gateway_inbox_store_errors_total"); got != 2 {
		t.Errorf("store errors = %v, want 2", got)
	}
	if got := f.storedCount(t); got != 2 {
		t.Errorf("queued envelopes = %d, want 2 - the consumer must keep consuming", got)
	}
}

// TestService_RefreshDeliveryModeMatrix pins the D-3 delivery modes: Disabled
// returns nil with NO emission, Individual enqueues exactly one inbound event
// per stored row of the window, and Batch is rejected with
// ErrBatchNotSupported instead of silently degrading.
func TestService_RefreshDeliveryModeMatrix(t *testing.T) {
	t.Parallel()

	t.Run("Disabled emits nothing", func(t *testing.T) {
		t.Parallel()

		f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
		f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
		seedInbound(t, f, "sms-disabled", smsgateway.IncomingMessageTypeSMS, "hello", inboundBase())

		since, until := refreshWindow()
		err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
			Since:    since,
			Until:    until,
			Delivery: smsgateway.WebhookDeliveryDisabled,
		})
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if got := f.queuedCountAll(t); got != 0 {
			t.Errorf("queued items = %d, want 0 for the Disabled mode", got)
		}
	})

	t.Run("Individual emits one event per stored row", func(t *testing.T) {
		t.Parallel()

		f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
		f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
		seedInbound(t, f, "sms-one", smsgateway.IncomingMessageTypeSMS, "hello", inboundBase())
		seedInbound(t, f, "sms-two", smsgateway.IncomingMessageTypeSMS, "world", inboundBase().Add(time.Minute))

		since, until := refreshWindow()
		err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
			Since:    since,
			Until:    until,
			Delivery: smsgateway.WebhookDeliveryIndividual,
		})
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if got := f.queuedCountAll(t); got != 2 {
			t.Errorf("queued items = %d, want exactly one event per stored row (2)", got)
		}
	})

	t.Run("Batch is rejected", func(t *testing.T) {
		t.Parallel()

		f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
		f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
		seedInbound(t, f, "sms-batch", smsgateway.IncomingMessageTypeSMS, "hello", inboundBase())

		since, until := refreshWindow()
		err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
			Since:    since,
			Until:    until,
			Delivery: smsgateway.WebhookDeliveryBatch,
		})
		if !errors.Is(err, inbox.ErrBatchNotSupported) {
			t.Fatalf("error = %v, want %v", err, inbox.ErrBatchNotSupported)
		}
		if got := f.queuedCountAll(t); got != 0 {
			t.Errorf("queued items = %d, want 0 - a rejected Batch must not emit", got)
		}
	})
}

// TestService_RefreshEmitsForAnAlreadyServedWebhook pins the served=nil
// contract of an explicit refresh (D-3): the dedupe of a modem emission marks
// the webhook as served, yet the replay still enqueues for it - a second
// refresh emits again, and Refresh never adds a dedupe mark of its own.
func TestService_RefreshEmitsForAnAlreadyServedWebhook(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)

	// The modem emission serves the webhook and marks the dedupe.
	f.service.HandleInboundPartForTest(textPart("hello"), inboundBase())
	if got := f.storedCount(t); got != 1 {
		t.Fatalf("queued items = %d, want 1 from the modem emission", got)
	}
	dedupe := f.service.DedupeForTest()
	if got := dedupe.LenForTest(); got != 1 {
		t.Fatalf("dedupe entries = %d, want 1 from the modem emission", got)
	}

	since, until := refreshWindow()
	refresh := inbox.RefreshRequest{
		Since:    since,
		Until:    until,
		Delivery: smsgateway.WebhookDeliveryIndividual,
	}

	if err := f.service.Refresh(context.Background(), refresh); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	if got := f.storedCount(t); got != 2 {
		t.Errorf(
			"queued items after the first refresh = %d, want 2 - an ALREADY SERVED webhook must receive the replay",
			got,
		)
	}

	if err := f.service.Refresh(context.Background(), refresh); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if got := f.storedCount(t); got != 3 {
		t.Errorf("queued items after the second refresh = %d, want 3 - a refresh must emit again", got)
	}

	// Refresh does NOT mark the dedupe: only the one entry from the modem
	// emission exists, and its served set is untouched.
	if got := dedupe.LenForTest(); got != 1 {
		t.Errorf("dedupe entries = %d, want 1 - the refresh must not mark the dedupe", got)
	}
}

// TestService_RefreshDoesNotMarkTheDedupe pins the marking half of AC2 with a
// row that was NEVER emitted through the pipeline: after the replay the dedupe
// stays empty, so a later genuine modem re-notification is damped only by its
// own emission - never by the refresh.
func TestService_RefreshDoesNotMarkTheDedupe(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	seedInbound(t, f, "sms-unemitted", smsgateway.IncomingMessageTypeSMS, "hello", inboundBase())

	since, until := refreshWindow()
	if err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		Since:    since,
		Until:    until,
		Delivery: smsgateway.WebhookDeliveryIndividual,
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := f.queuedCountAll(t); got != 1 {
		t.Fatalf("queued items = %d, want 1", got)
	}
	if got := f.service.DedupeForTest().LenForTest(); got != 0 {
		t.Errorf("dedupe entries = %d, want 0 - Refresh must not mark the dedupe", got)
	}
}

// TestService_RefreshReplaysByteIdenticalFields pins AC3: the replayed event
// carries the STORED bytes - the base64 content as data for a DATA_SMS, the
// stored text as message for an SMS - with event.MessageID = row id and
// event.At = the stored received_at. Both types are named EXPLICITLY: exact
// matching replays exactly the requested types, and only an in-process caller
// can name DATA_SMS (the wire DTO validates oneof=SMS MMS).
func TestService_RefreshReplaysByteIdenticalFields(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	f.registerWebhook(t, "data", smsgateway.WebhookEventSmsDataReceived)

	seedInbound(t, f, "0123456789abcdef", smsgateway.IncomingMessageTypeSMS, "hello", inboundBase())
	seedInbound(
		t,
		f,
		"fedcba9876543210",
		smsgateway.IncomingMessageTypeDataSMS,
		"yv66vg==",
		inboundBase().Add(time.Minute),
	)

	since, until := refreshWindow()
	if err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		Since: since,
		Until: until,
		Types: []smsgateway.IncomingMessageType{
			smsgateway.IncomingMessageTypeSMS,
			smsgateway.IncomingMessageTypeDataSMS,
		},
		Delivery: smsgateway.WebhookDeliveryIndividual,
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	payloads := f.queuedPayloadsByWebhook(t)

	var textPayload struct {
		MessageID   string `json:"messageId"`
		PhoneNumber string `json:"phoneNumber"`
		Sender      string `json:"sender"`
		Message     string `json:"message"`
		ReceivedAt  string `json:"receivedAt"`
	}
	if got := payloads["text"]; len(got) != 1 {
		t.Fatalf("sms:received payloads = %d, want 1", len(got))
	}
	if err := json.Unmarshal(payloads["text"][0], &textPayload); err != nil {
		t.Fatalf("decode sms:received payload: %v", err)
	}
	if textPayload.MessageID != "0123456789abcdef" {
		t.Errorf("messageId = %q, want the row id %q", textPayload.MessageID, "0123456789abcdef")
	}
	if textPayload.Message != "hello" {
		t.Errorf("message = %q, want the stored text %q", textPayload.Message, "hello")
	}
	if textPayload.PhoneNumber != inboundSender || textPayload.Sender != inboundSender {
		t.Errorf("phoneNumber/sender = %q/%q, want %q", textPayload.PhoneNumber, textPayload.Sender, inboundSender)
	}
	if textPayload.ReceivedAt != "2026-10-02T12:00:00Z" {
		t.Errorf("receivedAt = %q, want the stored received_at in UTC", textPayload.ReceivedAt)
	}

	var dataPayload struct {
		MessageID  string `json:"messageId"`
		Data       string `json:"data"`
		ReceivedAt string `json:"receivedAt"`
	}
	if got := payloads["data"]; len(got) != 1 {
		t.Fatalf("sms:data-received payloads = %d, want 1", len(got))
	}
	if err := json.Unmarshal(payloads["data"][0], &dataPayload); err != nil {
		t.Fatalf("decode sms:data-received payload: %v", err)
	}
	if dataPayload.MessageID != "fedcba9876543210" {
		t.Errorf("messageId = %q, want the row id %q", dataPayload.MessageID, "fedcba9876543210")
	}
	if dataPayload.Data != "yv66vg==" {
		t.Errorf("data = %q, want the STORED base64 %q", dataPayload.Data, "yv66vg==")
	}
	if dataPayload.ReceivedAt != "2026-10-02T12:01:00Z" {
		t.Errorf("receivedAt = %q, want the stored received_at in UTC", dataPayload.ReceivedAt)
	}
}

// TestService_RefreshMatchesMessageTypesExactly pins D-10 (amended to EXACT
// matching) at the observable boundary: the requested types are matched
// EXACTLY against the stored type - no grouping, no expansion. The empty
// request defaults to [SMS] and replays the SMS row ONLY (the stored
// DATA_SMS row stays silent), an explicit [SMS] replays exactly one row again,
// and [MMS] matches zero stored rows. Because an empty type set DISABLES the
// repository's type filter (pinned in repository_test.go), emissions under an
// empty request prove the service resolved the DEFAULT into [SMS] before the
// select - a disabled filter would have replayed the DATA_SMS row too.
func TestService_RefreshMatchesMessageTypesExactly(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	f.registerWebhook(t, "data", smsgateway.WebhookEventSmsDataReceived)

	seedInbound(t, f, "sms-row", smsgateway.IncomingMessageTypeSMS, "hello", inboundBase())
	seedInbound(
		t,
		f,
		"data-row",
		smsgateway.IncomingMessageTypeDataSMS,
		"yv66vg==",
		inboundBase().Add(time.Minute),
	)

	since, until := refreshWindow()
	refresh := func(types ...smsgateway.IncomingMessageType) error {
		return f.service.Refresh(context.Background(), inbox.RefreshRequest{
			Since:    since,
			Until:    until,
			Types:    types,
			Delivery: smsgateway.WebhookDeliveryIndividual,
		})
	}

	// Empty/omitted -> [SMS]: ONLY the SMS row replays - DATA_SMS is NOT
	// pulled in by the default (exact matching).
	if err := refresh(); err != nil {
		t.Fatalf("refresh with the default type: %v", err)
	}
	if got := f.queuedCountAll(t); got != 1 {
		t.Fatalf("queued items after the default refresh = %d, want 1 (SMS only)", got)
	}

	// The explicit value SMS matches ONLY the SMS row - no {SMS, DATA_SMS}
	// expansion.
	if err := refresh(smsgateway.IncomingMessageTypeSMS); err != nil {
		t.Fatalf("refresh with SMS: %v", err)
	}
	if got := f.queuedCountAll(t); got != 2 {
		t.Errorf("queued items after the SMS refresh = %d, want 2 (1 SMS row per refresh)", got)
	}

	// The value MMS matches zero stored rows (this gateway stores SMS and
	// DATA_SMS only).
	if err := refresh(smsgateway.IncomingMessageTypeMMS); err != nil {
		t.Fatalf("refresh with MMS: %v", err)
	}
	if got := f.queuedCountAll(t); got != 2 {
		t.Errorf("queued items after the MMS refresh = %d, want 2 - MMS rows are never stored here", got)
	}

	// In-process callers may name DATA_SMS directly: exact matching replays
	// exactly that stored row (the wire DTO cannot name it).
	if err := refresh(smsgateway.IncomingMessageTypeDataSMS); err != nil {
		t.Fatalf("refresh with DATA_SMS: %v", err)
	}
	if got := f.queuedCountAll(t); got != 3 {
		t.Errorf("queued items after the DATA_SMS refresh = %d, want 3 (the data row)", got)
	}
}

// TestService_RefreshHasNoRowCap pins the owner-ratified D-3 residual design:
// a window with 501 matching rows triggers exactly 501 emissions - there is NO
// row cap on the replay.
func TestService_RefreshHasNoRowCap(t *testing.T) {
	t.Parallel()

	const seededRows = 501

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)

	since, until := refreshWindow()
	for i := range seededRows {
		seedInbound(
			t,
			f,
			fmt.Sprintf("msg-%03d", i),
			smsgateway.IncomingMessageTypeSMS,
			"hello",
			inboundBase().Add(time.Duration(i)*time.Second),
		)
	}

	if err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		Since:    since,
		Until:    until,
		Delivery: smsgateway.WebhookDeliveryIndividual,
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := f.queuedCountAll(t); got != seededRows {
		t.Errorf("emissions = %d, want exactly %d - the replay must not be capped", got, seededRows)
	}
}

// TestService_RefreshRejectsAForeignDevice pins AC5: an explicitly named
// device that is not the local one is a VALIDATION (ErrDeviceNotFound), never
// a row filter.
func TestService_RefreshRejectsAForeignDevice(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})

	foreignDevice := "000000000000000000000"
	since, until := refreshWindow()
	err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		DeviceID: &foreignDevice,
		Since:    since,
		Until:    until,
		Delivery: smsgateway.WebhookDeliveryIndividual,
	})
	if !errors.Is(err, inbox.ErrDeviceNotFound) {
		t.Fatalf("error = %v, want %v", err, inbox.ErrDeviceNotFound)
	}
}

// TestService_RefreshReplaysOnlyLocalDeviceRows pins the owner-decided device
// scoping of the replay: Refresh scopes the window by the LOCAL device exactly
// as List does, so a row stored under a DIFFERENT device id is never
// re-emitted while the local row of the same window still replays.
func TestService_RefreshReplaysOnlyLocalDeviceRows(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)

	seedInbound(t, f, "sms-local", smsgateway.IncomingMessageTypeSMS, "local", inboundBase())

	foreignDevice := "000000000000000000000"
	if err := f.inbox.Insert(context.Background(), inbox.IncomingMessage{
		ID:         "sms-foreign",
		DeviceID:   foreignDevice,
		Type:       smsgateway.IncomingMessageTypeSMS,
		Sender:     "+79990009999",
		Recipient:  nil,
		SimNumber:  nil,
		Content:    "foreign",
		ReceivedAt: inboundBase().Add(time.Minute),
	}); err != nil {
		t.Fatalf("insert the foreign row: %v", err)
	}

	since, until := refreshWindow()
	if err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		Since:    since,
		Until:    until,
		Delivery: smsgateway.WebhookDeliveryIndividual,
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := f.queuedCountAll(t); got != 1 {
		t.Fatalf("queued items = %d, want only the local row", got)
	}

	payloads := f.queuedPayloadsByWebhook(t)
	got := payloads["text"]
	if len(got) != 1 {
		t.Fatalf("sms:received payloads = %d, want 1", len(got))
	}
	var payload struct {
		MessageID string `json:"messageId"`
	}
	if err := json.Unmarshal(got[0], &payload); err != nil {
		t.Fatalf("decode sms:received payload: %v", err)
	}
	if payload.MessageID != "sms-local" {
		t.Errorf("messageId = %q, want the local row %q - a foreign-device row must never replay",
			payload.MessageID, "sms-local")
	}
}

// TestService_RefreshWithoutDeviceIDReplaysLocalRows pins the positive half of
// the device scoping: a request that carries NO deviceId still replays the
// LOCAL rows of the window - the local scope is unconditional, not conditional
// on the request naming the device.
func TestService_RefreshWithoutDeviceIDReplaysLocalRows(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)

	seedInbound(t, f, "sms-local-only", smsgateway.IncomingMessageTypeSMS, "hello", inboundBase())

	since, until := refreshWindow()
	if err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		Since:    since,
		Until:    until,
		Delivery: smsgateway.WebhookDeliveryIndividual,
	}); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if got := f.queuedCountAll(t); got != 1 {
		t.Errorf("queued items = %d, want the local row replayed by a deviceId-less request", got)
	}
}

// TestService_RefreshValidatesTheDeviceBeforeTheRange pins the rule order of
// RefreshRequest.validate: a request violating both rules reports the device
// error first (the same order as ListFilter.validate).
func TestService_RefreshValidatesTheDeviceBeforeTheRange(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})

	foreignDevice := "000000000000000000000"
	since := inboundBase()
	err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		DeviceID: &foreignDevice,
		Since:    since,
		Until:    since.Add(-time.Hour),
		Delivery: smsgateway.WebhookDeliveryIndividual,
	})
	if !errors.Is(err, inbox.ErrDeviceNotFound) {
		t.Fatalf("error = %v, want %v checked before the date range", err, inbox.ErrDeviceNotFound)
	}
}

// TestService_RefreshRejectsAnInvertedWindow pins the window validation for
// in-process callers (the HTTP edge rejects it earlier via the DTO validator).
func TestService_RefreshRejectsAnInvertedWindow(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})

	since := inboundBase()
	err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		Since:    since,
		Until:    since.Add(-time.Hour),
		Delivery: smsgateway.WebhookDeliveryIndividual,
	})
	if !errors.Is(err, inbox.ErrInvalidDateRange) {
		t.Fatalf("error = %v, want %v", err, inbox.ErrInvalidDateRange)
	}
}

// TestService_RefreshEmptyWindowEmitsNothing pins AC6's zero case: a valid
// request whose window matches no row succeeds with ZERO emissions.
func TestService_RefreshEmptyWindowEmitsNothing(t *testing.T) {
	t.Parallel()

	f := newInboundFixture(t, inbox.Config{ReassemblyTimeout: 0, SweepInterval: 0})
	f.registerWebhook(t, "text", smsgateway.WebhookEventSmsReceived)
	seedInbound(t, f, "sms-outside", smsgateway.IncomingMessageTypeSMS, "hello", inboundBase())

	err := f.service.Refresh(context.Background(), inbox.RefreshRequest{
		Since:    inboundBase().Add(48 * time.Hour),
		Until:    inboundBase().Add(72 * time.Hour),
		Delivery: smsgateway.WebhookDeliveryIndividual,
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := f.queuedCountAll(t); got != 0 {
		t.Errorf("queued items = %d, want 0 for an empty window", got)
	}
}

// seedInbound stores one inbound row of the local device with an explicit
// identity and receive stamp, bypassing the modem pipeline: the replay reads
// stored rows, so the fixtures seed them directly.
func seedInbound(
	t *testing.T,
	f inboundFixture,
	id string,
	messageType smsgateway.IncomingMessageType,
	content string,
	receivedAt time.Time,
) {
	t.Helper()

	if err := f.inbox.Insert(context.Background(), inbox.IncomingMessage{
		ID:         id,
		DeviceID:   f.devices.Get().ID,
		Type:       messageType,
		Sender:     inboundSender,
		Recipient:  nil,
		SimNumber:  nil,
		Content:    content,
		ReceivedAt: receivedAt,
	}); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
}

// refreshWindow covers every fixture stamp the inbound pipeline and the seed
// helpers produce.
func refreshWindow() (time.Time, time.Time) {
	return inboundBase().Add(-time.Hour), inboundBase().Add(24 * time.Hour)
}

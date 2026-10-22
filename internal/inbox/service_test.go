package inbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

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
	rows, total, err := f.service.List(context.Background(), inbox.ListOptions{})
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

	rows, total, err := f.service.List(context.Background(), inbox.ListOptions{})
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
		Filter: &inbox.ListFilter{DeviceID: &foreignDevice},
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
		Filter: &inbox.ListFilter{DeviceID: &deviceID},
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
		Filter: &inbox.ListFilter{From: &from, To: &to},
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

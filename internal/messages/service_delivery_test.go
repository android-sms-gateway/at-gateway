//nolint:testpackage // white-box tests reach the unexported delivery-report handler and TP-ST classifier
package messages

import (
	"context"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// newWhiteboxService builds the persistence graph (same stack as
// newRepository in repository_test.go, which lives in the external test
// package) plus plain-constructor metrics and returns a Service whose
// delivery-report handler can be exercised directly.
func newWhiteboxService(t *testing.T) (*Service, *Repository) {
	t.Helper()

	var bunDB *bun.DB

	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Supply(sqlfx.Config{
			URL:             "sqlite://:memory:",
			ConnMaxIdleTime: 0,
			ConnMaxLifetime: 0,
			MaxOpenConns:    1,
			MaxIdleConns:    1,
		}),
		db.Module(),
		sqlfx.Module(),
		goosefx.Module(),
		bunfx.Module(),
		fx.Invoke(func(b *bun.DB) {
			bunDB = b
		}),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelStop()
		if stopErr := app.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop app: %v", stopErr)
		}
	})

	repo := NewRepository(bunDB)

	metrics := &Metrics{
		EnqueuedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_enqueued_total", Help: "Test counter"},
		),
		SentTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_sent_total", Help: "Test counter"},
		),
		DeliveredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_delivered_total", Help: "Test counter"},
		),
		FailedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_failed_total", Help: "Test counter"},
		),
		CancelledTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_cancelled_total", Help: "Test counter"},
		),
	}

	return NewService(Config{}, repo, nil, nil, metrics, zap.NewNop()), repo
}

// whiteboxEnqueueAndSend creates a message with the given option and moves
// every recipient to Sent with a distinct reference, then sets the message
// state to Sent - the exact snapshot the send batch leaves behind.
func whiteboxEnqueueAndSend(
	t *testing.T,
	repo *Repository,
	extID string,
	withDeliveryReport bool,
	phones ...string,
) {
	t.Helper()
	ctx := context.Background()

	deviceID := "device-1"
	input := &MessageInput{
		MessageContent: MessageContent{
			TextContent: &smsgateway.TextMessage{Text: "hello"},
		},
		MessageOptions: MessageOptions{
			WithDeliveryReport: &withDeliveryReport,
		},
		ExtID:        extID,
		DeviceID:     &deviceID,
		PhoneNumbers: phones,
	}
	if err := repo.Create(ctx, input); err != nil {
		t.Fatalf("create message: %v", err)
	}

	for i, phone := range phones {
		if err := repo.SetRecipientProcessed(ctx, extID, phone); err != nil {
			t.Fatalf("set recipient processed: %v", err)
		}
		if err := repo.SetRecipientSent(ctx, extID, phone, i+1); err != nil {
			t.Fatalf("set recipient sent: %v", err)
		}
	}
	if err := repo.SetState(ctx, extID, smsgateway.ProcessingStateSent); err != nil {
		t.Fatalf("set message sent: %v", err)
	}
}

// TestDeliveryReportState pins the TP-ST classification: received-by-SME
// statuses deliver, temporary errors are ignored (the SC retries) and
// permanent errors fail with the status as the reason.
func TestDeliveryReportState(t *testing.T) {
	tests := []struct {
		name    string
		status  byte
		want    smsgateway.ProcessingState
		wantErr string
		handled bool
	}{
		{"delivered", 0x00, smsgateway.ProcessingStateDelivered, "", true},
		{"delivered forwarded without confirmation", 0x01, smsgateway.ProcessingStateDelivered, "", true},
		{"delivered SC specific", 0x1F, smsgateway.ProcessingStateDelivered, "", true},
		{"temporary retrying", 0x20, "", "", false},
		{"temporary giving up", 0x3F, "", "", false},
		{"permanent error", 0x41, smsgateway.ProcessingStateFailed, "delivery report: SC status 0x41", true},
		{"permanent reserved", 0x7F, smsgateway.ProcessingStateFailed, "delivery report: SC status 0x7F", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, reason, handled := deliveryReportState(tt.status)
			if handled != tt.handled {
				t.Fatalf("handled = %v, want %v", handled, tt.handled)
			}
			if state != tt.want {
				t.Errorf("state = %q, want %q", state, tt.want)
			}
			if reason != tt.wantErr {
				t.Errorf("reason = %q, want %q", reason, tt.wantErr)
			}
		})
	}
}

// TestHandleDeliveryReport_Delivered pins the happy path end to end: a
// delivered report flips the matching recipient, promotes the message to
// Delivered and bumps the delivered counter.
func TestHandleDeliveryReport_Delivered(t *testing.T) {
	svc, repo := newWhiteboxService(t)
	ctx := context.Background()

	const extID = "dr-delivered"
	const phone = "+79990001234"

	whiteboxEnqueueAndSend(t, repo, extID, true, phone)

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 1, Phone: "79990001234", Status: 0x00})

	message, err := repo.GetByID(ctx, extID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if state := message.Recipients[0].State; state != smsgateway.ProcessingStateDelivered {
		t.Errorf("recipient state = %q, want Delivered", state)
	}
	if message.State != smsgateway.ProcessingStateDelivered {
		t.Errorf("message state = %q, want Delivered", message.State)
	}
	if got := testutil.ToFloat64(svc.metrics.DeliveredTotal); got != 1 {
		t.Errorf("delivered total = %v, want 1", got)
	}
}

// TestHandleDeliveryReport_PermanentFailure pins the failure path: a
// permanent status fails the recipient with the SC status as the reason and
// promotes the all-terminal message to Failed.
func TestHandleDeliveryReport_PermanentFailure(t *testing.T) {
	svc, repo := newWhiteboxService(t)
	ctx := context.Background()

	const extID = "dr-failed"
	const phone = "+79990001234"

	whiteboxEnqueueAndSend(t, repo, extID, true, phone)

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 1, Phone: "79990001234", Status: 0x41})

	message, err := repo.GetByID(ctx, extID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	recipient := message.Recipients[0]
	if recipient.State != smsgateway.ProcessingStateFailed {
		t.Errorf("recipient state = %q, want Failed", recipient.State)
	}
	if recipient.Error == nil || *recipient.Error != "delivery report: SC status 0x41" {
		t.Errorf("recipient error = %v, want the SC status reason", recipient.Error)
	}
	if message.State != smsgateway.ProcessingStateFailed {
		t.Errorf("message state = %q, want Failed", message.State)
	}
	if got := testutil.ToFloat64(svc.metrics.FailedTotal); got != 1 {
		t.Errorf("failed total = %v, want 1", got)
	}
}

// TestHandleDeliveryReport_TemporaryIgnored pins the temporary-error rule: a
// 0x30 status leaves the recipient Sent, the message Sent and the counters
// untouched - the SC reports progress on its own retries.
func TestHandleDeliveryReport_TemporaryIgnored(t *testing.T) {
	svc, repo := newWhiteboxService(t)
	ctx := context.Background()

	const extID = "dr-temporary"
	const phone = "+79990001234"

	whiteboxEnqueueAndSend(t, repo, extID, true, phone)

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 1, Phone: "79990001234", Status: 0x30})

	message, err := repo.GetByID(ctx, extID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if state := message.Recipients[0].State; state != smsgateway.ProcessingStateSent {
		t.Errorf("recipient state = %q, want Sent", state)
	}
	if message.State != smsgateway.ProcessingStateSent {
		t.Errorf("message state = %q, want Sent", message.State)
	}
	if got := testutil.ToFloat64(svc.metrics.DeliveredTotal); got != 0 {
		t.Errorf("delivered total = %v, want 0", got)
	}
	if got := testutil.ToFloat64(svc.metrics.FailedTotal); got != 0 {
		t.Errorf("failed total = %v, want 0", got)
	}
}

// TestHandleDeliveryReport_UnknownReport pins the no-match rule: a report
// for a reference no Sent recipient owns (opt-out message) is ignored.
func TestHandleDeliveryReport_UnknownReport(t *testing.T) {
	svc, repo := newWhiteboxService(t)
	ctx := context.Background()

	const extID = "dr-unknown"
	const phone = "+79990001234"

	whiteboxEnqueueAndSend(t, repo, extID, false, phone)

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 1, Phone: "79990001234", Status: 0x00})

	message, err := repo.GetByID(ctx, extID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if state := message.Recipients[0].State; state != smsgateway.ProcessingStateSent {
		t.Errorf("recipient state = %q, want Sent", state)
	}
	if message.State != smsgateway.ProcessingStateSent {
		t.Errorf("message state = %q, want Sent", message.State)
	}
}

// TestHandleDeliveryReport_AllRecipients pins the message promotion across
// recipients: the message only becomes Delivered after the LAST outstanding
// recipient delivers; a partial report keeps it Sent.
func TestHandleDeliveryReport_AllRecipients(t *testing.T) {
	svc, repo := newWhiteboxService(t)
	ctx := context.Background()

	const extID = "dr-two"
	const phoneA = "+79990001234"
	const phoneB = "+79990004321"

	whiteboxEnqueueAndSend(t, repo, extID, true, phoneA, phoneB)

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 1, Phone: "79990001234", Status: 0x00})

	message, err := repo.GetByID(ctx, extID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if message.State != smsgateway.ProcessingStateSent {
		t.Errorf("message state after first report = %q, want Sent", message.State)
	}

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 2, Phone: "79990004321", Status: 0x00})

	message, err = repo.GetByID(ctx, extID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if message.State != smsgateway.ProcessingStateDelivered {
		t.Errorf("message state after last report = %q, want Delivered", message.State)
	}
	for _, recipient := range message.Recipients {
		if recipient.State != smsgateway.ProcessingStateDelivered {
			t.Errorf("recipient %q state = %q, want Delivered", recipient.PhoneNumber, recipient.State)
		}
	}
}

// TestHandleDeliveryReport_MixedOutcome pins the mixed resolution parity with
// the send batch: one recipient already failed at send time, the other
// delivers later - the message stays Sent, exactly what resolveFinalState
// yields for a mixed outcome.
func TestHandleDeliveryReport_MixedOutcome(t *testing.T) {
	svc, repo := newWhiteboxService(t)
	ctx := context.Background()

	const extID = "dr-mixed"
	const phoneA = "+79990001234"
	const phoneB = "+79990004321"

	whiteboxEnqueueAndSend(t, repo, extID, true, phoneA, phoneB)

	if err := repo.SetRecipientFailed(ctx, extID, phoneA, "send failure"); err != nil {
		t.Fatalf("fail recipient: %v", err)
	}

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 2, Phone: "79990004321", Status: 0x00})

	message, err := repo.GetByID(ctx, extID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if message.State != smsgateway.ProcessingStateSent {
		t.Errorf("message state = %q, want Sent for a mixed outcome", message.State)
	}
}

// TestDeliveryReportDefaults pins the option defaulting rule: the report
// matching treats an absent option as true (see SetRecipientDeliveredByRef),
// and the send path forwards the resolved default. The latter is pinned by
// the modem layer; here the report path is exercised through the service.
func TestDeliveryReportDefaults(t *testing.T) {
	svc, repo := newWhiteboxService(t)
	ctx := context.Background()

	const extID = "dr-defaults"
	const phone = "+79990001234"

	deviceID := "device-1"
	input := &MessageInput{
		MessageContent: MessageContent{
			TextContent: &smsgateway.TextMessage{Text: "hello"},
		},
		ExtID:        extID,
		DeviceID:     &deviceID,
		PhoneNumbers: []string{phone},
	}
	if err := repo.Create(ctx, input); err != nil {
		t.Fatalf("create message: %v", err)
	}
	if err := repo.SetRecipientProcessed(ctx, extID, phone); err != nil {
		t.Fatalf("set recipient processed: %v", err)
	}
	if err := repo.SetRecipientSent(ctx, extID, phone, 1); err != nil {
		t.Fatalf("set recipient sent: %v", err)
	}
	if err := repo.SetState(ctx, extID, smsgateway.ProcessingStateSent); err != nil {
		t.Fatalf("set message sent: %v", err)
	}

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 1, Phone: "79990001234", Status: 0x00})

	message, err := repo.GetByID(ctx, extID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if message.State != smsgateway.ProcessingStateDelivered {
		t.Errorf("message state = %q, want Delivered", message.State)
	}
	if message.WithDeliveryReport != nil {
		t.Error("stored option was rewritten, want nil preserved (nil defaulting is a decision-site concern)")
	}
}

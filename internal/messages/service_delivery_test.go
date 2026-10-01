//nolint:testpackage // white-box tests reach the unexported delivery-report handler and TP-ST classifier
package messages

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/at-gateway/internal/storage"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
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
func newWhiteboxService(t *testing.T) (*Service, *Repository, *bun.DB, *webhooks.Service) {
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

	storageSvc, err := storage.NewService(
		storage.Config{Path: filepath.Join(t.TempDir(), "storage.json")},
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create storage service: %v", err)
	}
	devicesSvc := devices.NewService(devices.Config{Name: "test-device"}, storageSvc, zap.NewNop())
	webhooksSvc, err := webhooks.NewService(
		webhooks.Config{SigningKey: "messages-delivery-test-key"},
		webhooks.NewRepository(bunDB),
		devicesSvc,
		storageSvc,
		nil,
		zap.NewNop(),
	)
	if err != nil {
		t.Fatalf("create webhooks service: %v", err)
	}

	metrics := &Metrics{
		enqueuedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_enqueued_total", Help: "Test counter"},
		),
		sentTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_sent_total", Help: "Test counter"},
		),
		deliveredTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_delivered_total", Help: "Test counter"},
		),
		failedTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_failed_total", Help: "Test counter"},
		),
		cancelledTotal: prometheus.NewCounter(
			prometheus.CounterOpts{Name: "whitebox_cancelled_total", Help: "Test counter"},
		),
	}

	// The modem service is never run by the fixture: a disconnected service
	// still serves cached-SIM reads (empty phone number), which is exactly the
	// degraded Sender the webhook contract permits. Nil metrics is safe here -
	// no modem code path runs.
	modemSvc := modem.NewService(modem.Config{}, zap.NewNop(), nil)

	return NewService(
		Config{},
		repo,
		devicesSvc,
		modemSvc,
		webhooksSvc,
		metrics,
		zap.NewNop(),
	), repo, bunDB, webhooksSvc
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

func whiteboxEnqueue(
	t *testing.T,
	svc *Service,
	extID string,
	phones ...string,
) *Message {
	t.Helper()

	message, err := svc.Enqueue(
		context.Background(),
		MessageInput{
			MessageContent: MessageContent{
				TextContent: &smsgateway.TextMessage{Text: "hello"},
			},
			ExtID:        extID,
			PhoneNumbers: phones,
		},
		EnqueueOptions{},
	)
	if err != nil {
		t.Fatalf("enqueue message: %v", err)
	}

	return message
}

func registerWhiteboxWebhook(
	t *testing.T,
	svc *webhooks.Service,
	event smsgateway.WebhookEvent,
) {
	t.Helper()

	webhook := &smsgateway.Webhook{
		ID:       "",
		DeviceID: nil,
		URL:      "https://example.com/webhook",
		Event:    event,
	}
	if err := svc.Replace(context.Background(), webhook); err != nil {
		t.Fatalf("register webhook: %v", err)
	}
}

type queuedWebhookEnvelope struct {
	Event   smsgateway.WebhookEvent `json:"event"`
	Payload json.RawMessage         `json:"payload"`
}

type queuedSmsPayload struct {
	MessageID   string    `json:"messageId"`
	PhoneNumber string    `json:"phoneNumber"`
	Sender      string    `json:"sender"`
	Recipient   *string   `json:"recipient,omitempty"`
	SimNumber   *uint8    `json:"simNumber,omitempty"`
	SentAt      time.Time `json:"sentAt"`
	DeliveredAt time.Time `json:"deliveredAt"`
	FailedAt    time.Time `json:"failedAt"`
	CancelledAt time.Time `json:"cancelledAt"`
	Reason      string    `json:"reason,omitempty"`
}

// requireOutgoingIdentity pins the identity fields shared by every outgoing
// webhook event: phoneNumber is the recipient, recipient repeats it (the
// client-go contract for the outgoing direction) and sender is the device's
// own number. The fixture's modem never connected, so no +CNUM number was
// cached and sender stays empty - the degraded case the contract allows.
func requireOutgoingIdentity(t *testing.T, payload queuedSmsPayload, phone string) {
	t.Helper()

	if payload.PhoneNumber != phone {
		t.Errorf("phoneNumber = %q, want %q", payload.PhoneNumber, phone)
	}
	if payload.Sender != "" {
		t.Errorf("sender = %q, want empty for an unqueried SIM", payload.Sender)
	}
	if payload.Recipient == nil {
		t.Error("recipient is nil, want the recipient number")
	} else if *payload.Recipient != phone {
		t.Errorf("recipient = %q, want %q", *payload.Recipient, phone)
	}
}

func requireQueueCount(t *testing.T, bunDB *bun.DB, want int) {
	t.Helper()

	var got int
	if err := bunDB.QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM webhook_queue",
	).Scan(&got); err != nil {
		t.Fatalf("count webhook queue rows: %v", err)
	}
	if got != want {
		t.Fatalf("webhook queue rows = %d, want %d", got, want)
	}
}

func requireOnlyQueuePayload(t *testing.T, bunDB *bun.DB, target any) queuedWebhookEnvelope {
	t.Helper()
	requireQueueCount(t, bunDB, 1)

	var raw string
	if err := bunDB.QueryRowContext(
		context.Background(),
		"SELECT payload FROM webhook_queue LIMIT 1",
	).Scan(&raw); err != nil {
		t.Fatalf("select webhook queue payload: %v", err)
	}

	var envelope queuedWebhookEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("unmarshal webhook envelope %q: %v", raw, err)
	}
	if err := json.Unmarshal(envelope.Payload, target); err != nil {
		t.Fatalf("unmarshal webhook payload %s: %v", envelope.Payload, err)
	}

	return envelope
}

func TestProcessPending_FailedEmitsWebhookForEveryFailurePath(t *testing.T) {
	tests := []struct {
		name           string
		input          MessageInput
		maxSegments    int
		wantReasonText string
	}{
		{
			name: "unsupported content",
			input: MessageInput{MessageContent: MessageContent{
				DataContent: &smsgateway.DataMessage{Data: "aGVsbG8=", Port: 53739},
			}},
			wantReasonText: "only text messages are supported",
		},
		{
			name: "segment cap exceeded",
			input: MessageInput{MessageContent: MessageContent{
				TextContent: &smsgateway.TextMessage{Text: strings.Repeat("a", 161)},
			}},
			maxSegments:    1,
			wantReasonText: "maximum is 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, bunDB, webhooksSvc := newWhiteboxService(t)
			registerWhiteboxWebhook(t, webhooksSvc, smsgateway.WebhookEventSmsFailed)
			svc.config.MaxSegments = tt.maxSegments

			input := tt.input
			input.ExtID = "failed-" + strings.ReplaceAll(tt.name, " ", "-")
			input.PhoneNumbers = []string{"+79990001234"}
			message, err := svc.Enqueue(context.Background(), input, EnqueueOptions{})
			if err != nil {
				t.Fatalf("enqueue message: %v", err)
			}
			if !svc.processPending(context.Background()) {
				t.Fatal("process pending message = false, want true")
			}

			var payload queuedSmsPayload
			envelope := requireOnlyQueuePayload(t, bunDB, &payload)
			if envelope.Event != smsgateway.WebhookEventSmsFailed {
				t.Errorf("event = %q, want sms:failed", envelope.Event)
			}
			if payload.MessageID != message.ID {
				t.Errorf("messageId = %q, want %q", payload.MessageID, message.ID)
			}
			requireOutgoingIdentity(t, payload, "+79990001234")
			if !strings.Contains(payload.Reason, tt.wantReasonText) {
				t.Errorf("reason = %q, want substring %q", payload.Reason, tt.wantReasonText)
			}
			if payload.FailedAt.IsZero() {
				t.Error("failedAt is zero")
			}
		})
	}
}

func TestCancel_EmitsWebhookForEachCancelledRecipient(t *testing.T) {
	svc, repo, bunDB, webhooksSvc := newWhiteboxService(t)
	registerWhiteboxWebhook(t, webhooksSvc, smsgateway.WebhookEventSmsCancelled)

	whiteboxEnqueue(t, svc, "cancel-webhook", "+79990001234", "+79990004321")
	if err := repo.SetRecipientFailed(
		context.Background(),
		"cancel-webhook",
		"+79990001234",
		"send failure",
	); err != nil {
		t.Fatalf("set recipient failed: %v", err)
	}

	message, err := svc.Cancel(context.Background(), "cancel-webhook")
	if err != nil {
		t.Fatalf("cancel message: %v", err)
	}

	var payload queuedSmsPayload
	envelope := requireOnlyQueuePayload(t, bunDB, &payload)
	if envelope.Event != smsgateway.WebhookEventSmsCancelled {
		t.Errorf("event = %q, want sms:cancelled", envelope.Event)
	}
	if payload.MessageID != message.ID {
		t.Errorf("messageId = %q, want %q", payload.MessageID, message.ID)
	}
	requireOutgoingIdentity(t, payload, "+79990004321")
	if payload.CancelledAt.IsZero() {
		t.Error("cancelledAt is zero")
	}
}

// TestCancel_SecondCancelIsRejectedAndDoesNotReEmit pins that only the
// cancelling call emits sms:cancelled: a repeat cancel of the same message
// answers ErrNotPending (mapped to 409 by the handler) and leaves the
// webhook queue untouched.
func TestCancel_SecondCancelIsRejectedAndDoesNotReEmit(t *testing.T) {
	svc, _, bunDB, webhooksSvc := newWhiteboxService(t)
	registerWhiteboxWebhook(t, webhooksSvc, smsgateway.WebhookEventSmsCancelled)

	whiteboxEnqueue(t, svc, "cancel-once", "+79990001234")
	if _, err := svc.Cancel(context.Background(), "cancel-once"); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	requireQueueCount(t, bunDB, 1)

	message, err := svc.Cancel(context.Background(), "cancel-once")
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("second cancel = %v, %v; want ErrNotPending", message, err)
	}
	if message != nil {
		t.Fatalf("second cancel message = %+v, want nil", message)
	}
	requireQueueCount(t, bunDB, 1)
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
	svc, repo, bunDB, webhooksSvc := newWhiteboxService(t)
	ctx := context.Background()
	registerWhiteboxWebhook(t, webhooksSvc, smsgateway.WebhookEventSmsDelivered)

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
	if got := testutil.ToFloat64(svc.metrics.deliveredTotal); got != 1 {
		t.Errorf("delivered total = %v, want 1", got)
	}

	var payload queuedSmsPayload
	envelope := requireOnlyQueuePayload(t, bunDB, &payload)
	if envelope.Event != smsgateway.WebhookEventSmsDelivered {
		t.Errorf("event = %q, want sms:delivered", envelope.Event)
	}
	if payload.MessageID != extID {
		t.Errorf("messageId = %q, want %q", payload.MessageID, extID)
	}
	requireOutgoingIdentity(t, payload, phone)
	if payload.DeliveredAt.IsZero() {
		t.Error("deliveredAt is zero")
	}
	requireOutgoingIdentity(t, payload, phone)
}

// TestHandleDeliveryReport_PermanentFailure pins the failure path: a
// permanent status fails the recipient with the SC status as the reason and
// promotes the all-terminal message to Failed.
func TestHandleDeliveryReport_PermanentFailure(t *testing.T) {
	svc, repo, bunDB, webhooksSvc := newWhiteboxService(t)
	ctx := context.Background()
	registerWhiteboxWebhook(t, webhooksSvc, smsgateway.WebhookEventSmsFailed)

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
	if got := testutil.ToFloat64(svc.metrics.failedTotal); got != 1 {
		t.Errorf("failed total = %v, want 1", got)
	}

	var payload queuedSmsPayload
	envelope := requireOnlyQueuePayload(t, bunDB, &payload)
	if envelope.Event != smsgateway.WebhookEventSmsFailed {
		t.Errorf("event = %q, want sms:failed", envelope.Event)
	}
	if payload.MessageID != extID {
		t.Errorf("messageId = %q, want %q", payload.MessageID, extID)
	}
	if payload.Reason != "delivery report: SC status 0x41" {
		t.Errorf("reason = %q, want SC status reason", payload.Reason)
	}
	if payload.FailedAt.IsZero() {
		t.Error("failedAt is zero")
	}
	requireOutgoingIdentity(t, payload, phone)
}

// TestHandleDeliveryReport_TemporaryIgnored pins the temporary-error rule: a
// 0x30 status leaves the recipient Sent, the message Sent and the counters
// untouched - the SC reports progress on its own retries.
func TestHandleDeliveryReport_TemporaryIgnored(t *testing.T) {
	svc, repo, bunDB, webhooksSvc := newWhiteboxService(t)
	ctx := context.Background()
	registerWhiteboxWebhook(t, webhooksSvc, smsgateway.WebhookEventSmsDelivered)

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
	if got := testutil.ToFloat64(svc.metrics.deliveredTotal); got != 0 {
		t.Errorf("delivered total = %v, want 0", got)
	}
	if got := testutil.ToFloat64(svc.metrics.failedTotal); got != 0 {
		t.Errorf("failed total = %v, want 0", got)
	}
	requireQueueCount(t, bunDB, 0)
}

// TestHandleDeliveryReport_UnknownReport pins the no-match rule: a report
// for a reference no Sent recipient owns (opt-out message) is ignored.
func TestHandleDeliveryReport_UnknownReport(t *testing.T) {
	svc, repo, bunDB, webhooksSvc := newWhiteboxService(t)
	ctx := context.Background()
	registerWhiteboxWebhook(t, webhooksSvc, smsgateway.WebhookEventSmsDelivered)

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
	requireQueueCount(t, bunDB, 0)
}

func TestHandleDeliveryReport_PersistenceFailureDoesNotEmitWebhook(t *testing.T) {
	svc, repo, bunDB, webhooksSvc := newWhiteboxService(t)
	ctx := context.Background()
	registerWhiteboxWebhook(t, webhooksSvc, smsgateway.WebhookEventSmsDelivered)

	whiteboxEnqueueAndSend(t, repo, "dr-persistence-failure", true, "+79990001234")
	if _, err := bunDB.ExecContext(ctx, "DROP TABLE message_recipients"); err != nil {
		t.Fatalf("drop message recipients: %v", err)
	}

	svc.handleDeliveryReport(ctx, modem.DeliveryReport{Reference: 1, Phone: "79990001234", Status: 0x00})

	requireQueueCount(t, bunDB, 0)
}

// TestHandleDeliveryReport_AllRecipients pins the message promotion across
// recipients: the message only becomes Delivered after the LAST outstanding
// recipient delivers; a partial report keeps it Sent.
func TestHandleDeliveryReport_AllRecipients(t *testing.T) {
	svc, repo, _, _ := newWhiteboxService(t)
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
	svc, repo, _, _ := newWhiteboxService(t)
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
	svc, repo, _, _ := newWhiteboxService(t)
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

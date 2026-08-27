package messages_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// newRepository builds the full persistence graph (sqlfx + goosefx + bunfx +
// db.Module) against an in-memory SQLite database, so the embedded migrations
// are applied before the repository under test is created.
func newRepository(t *testing.T) (*messages.Repository, *bun.DB) {
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

	return messages.NewRepository(bunDB), bunDB
}

func newInput(extID string, phones ...string) *messages.MessageInput {
	deviceID := "device-1"
	return &messages.MessageInput{
		MessageContent: messages.MessageContent{
			TextContent: &smsgateway.TextMessage{Text: "hello"},
		},
		ExtID:        extID,
		DeviceID:     &deviceID,
		PhoneNumbers: phones,
	}
}

// TestDequeueNextPending_EmptyQueue verifies that an idle queue yields
// ErrNotFound instead of a phantom message.
func TestDequeueNextPending_EmptyQueue(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	message, err := repo.DequeueNextPending(ctx)
	if !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("dequeue = %v, %v; want ErrNotFound", message, err)
	}
}

// TestDequeueNextPending_FIFO verifies claims happen in insertion order, the
// claim records the Processed transition (state, updated_at and states
// history) and the returned message carries its recipients in insertion
// order.
func TestDequeueNextPending_FIFO(t *testing.T) {
	repo, bunDB := newRepository(t)
	ctx := context.Background()

	first := newInput("m1", "+11111111111", "+22222222222")
	if err := repo.Create(ctx, first); err != nil {
		t.Fatalf("create first: %v", err)
	}
	second := newInput("m2", "+33333333333")
	if err := repo.Create(ctx, second); err != nil {
		t.Fatalf("create second: %v", err)
	}

	claimed, err := repo.DequeueNextPending(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if claimed.ID != "m1" {
		t.Fatalf("dequeued ext_id = %q, want m1", claimed.ID)
	}
	if claimed.State != smsgateway.ProcessingStateProcessed {
		t.Fatalf("dequeued state = %q, want %q", claimed.State, smsgateway.ProcessingStateProcessed)
	}
	if len(claimed.Recipients) != 2 {
		t.Fatalf("dequeued recipients = %d, want 2", len(claimed.Recipients))
	}
	for i, want := range []string{"+11111111111", "+22222222222"} {
		if got := claimed.Recipients[i].PhoneNumber; got != want {
			t.Fatalf("recipient %d = %q, want %q", i, got, want)
		}
		if claimed.Recipients[i].State != smsgateway.ProcessingStatePending {
			t.Fatalf("recipient %d state = %q, want Pending", i, claimed.Recipients[i].State)
		}
	}
	if _, recorded := claimed.States[string(smsgateway.ProcessingStateProcessed)]; !recorded {
		t.Fatal("claimed states history has no Processed entry")
	}

	var bumped bool
	err = bunDB.QueryRowContext(
		ctx,
		"SELECT updated_at > created_at FROM messages WHERE ext_id = ?",
		"m1",
	).Scan(&bumped)
	if err != nil {
		t.Fatalf("check updated_at bump: %v", err)
	}
	if !bumped {
		t.Fatal("updated_at not bumped past created_at for m1")
	}

	err = repo.SetState(ctx, "m1", smsgateway.ProcessingStateSent)
	if err != nil {
		t.Fatalf("finalize m1: %v", err)
	}

	next, err := repo.DequeueNextPending(ctx)
	if err != nil {
		t.Fatalf("second dequeue: %v", err)
	}
	if next.ID != "m2" {
		t.Fatalf("second dequeue ext_id = %q, want m2", next.ID)
	}

	err = repo.SetState(ctx, "m2", smsgateway.ProcessingStateSent)
	if err != nil {
		t.Fatalf("finalize m2: %v", err)
	}

	message, err := repo.DequeueNextPending(ctx)
	if !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("third dequeue = %v, %v; want ErrNotFound", message, err)
	}
}

// TestDequeueNextPending_ReclaimsInterrupted verifies that a message left in
// Processed (claim without completion, e.g. after a restart) is reclaimed
// before newer Pending messages, so interrupted processing resumes in FIFO
// order.
func TestDequeueNextPending_ReclaimsInterrupted(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	if err := repo.Create(ctx, newInput("m1", "+11111111111")); err != nil {
		t.Fatalf("create first: %v", err)
	}

	if _, err := repo.DequeueNextPending(ctx); err != nil {
		t.Fatalf("first dequeue: %v", err)
	}

	if err := repo.Create(ctx, newInput("m2", "+22222222222")); err != nil {
		t.Fatalf("create second: %v", err)
	}

	claimed, err := repo.DequeueNextPending(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if claimed.ID != "m1" {
		t.Fatalf("reclaimed ext_id = %q, want m1", claimed.ID)
	}
}

// TestRecipientStateTransitions verifies the JSON-history recipient flow used
// by the processing loop: Pending -> Processed (once) -> Sent with ref_id,
// with later transitions guarded out.
func TestRecipientStateTransitions(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	if err := repo.Create(ctx, newInput("m1", "+11111111111")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.DequeueNextPending(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}

	load := func() messages.Recipient {
		t.Helper()
		message, err := repo.GetByID(ctx, "m1")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(message.Recipients) != 1 {
			t.Fatalf("recipients = %d, want 1", len(message.Recipients))
		}
		return message.Recipients[0]
	}

	if state := load().State; state != smsgateway.ProcessingStatePending {
		t.Fatalf("initial recipient state = %q, want Pending", state)
	}

	if err := repo.SetRecipientProcessed(ctx, "m1", "+11111111111"); err != nil {
		t.Fatalf("mark processed: %v", err)
	}
	if err := repo.SetRecipientProcessed(ctx, "m1", "+11111111111"); err != nil {
		t.Fatalf("duplicate processed: %v", err)
	}
	if got := load(); got.State != smsgateway.ProcessingStateProcessed || len(got.States) != 2 {
		t.Fatalf("after processed = %q with %d entries, want Processed with 2", got.State, len(got.States))
	}

	refID := 42
	if err := repo.SetRecipientSent(ctx, "m1", "+11111111111", refID); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if got := load(); got.State != smsgateway.ProcessingStateSent ||
		got.RefID == nil || *got.RefID != refID || len(got.States) != 3 {
		t.Fatalf(
			"after sent = %q (ref %v) with %d entries, want Sent (42) with 3",
			got.State,
			got.RefID,
			len(got.States),
		)
	}

	if err := repo.SetRecipientFailed(ctx, "m1", "+11111111111", "boom"); err != nil {
		t.Fatalf("fail sent recipient: %v", err)
	}
	if got := load(); got.State != smsgateway.ProcessingStateFailed {
		t.Fatalf("state after guarded failure = %q, want Failed", got.State)
	}
}

// TestDequeueNextPending_SkipsFinalized verifies that terminal messages are
// never claimed and that Cancel cascades the Failed state onto recipients.
func TestDequeueNextPending_SkipsFinalized(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	if err := repo.Create(ctx, newInput("m1", "+11111111111")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Cancel(ctx, "m1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	cancelled, err := repo.GetByID(ctx, "m1")
	if err != nil {
		t.Fatalf("load cancelled: %v", err)
	}
	if cancelled.State != smsgateway.ProcessingStateFailed {
		t.Fatalf("cancelled state = %q, want Failed", cancelled.State)
	}
	if len(cancelled.Recipients) != 1 || cancelled.Recipients[0].State != smsgateway.ProcessingStateFailed {
		t.Fatalf("cancelled recipients not cascaded: %+v", cancelled.Recipients)
	}

	message, err := repo.DequeueNextPending(ctx)
	if !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("dequeue = %v, %v; want ErrNotFound", message, err)
	}
}

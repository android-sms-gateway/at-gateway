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
// never claimed and that Cancel cascades the Cancelled state onto recipients.
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
	if cancelled.State != smsgateway.ProcessingStateCancelled {
		t.Fatalf("cancelled state = %q, want Cancelled", cancelled.State)
	}
	if len(cancelled.Recipients) != 1 || cancelled.Recipients[0].State != smsgateway.ProcessingStateCancelled {
		t.Fatalf("cancelled recipients not cascaded: %+v", cancelled.Recipients)
	}

	message, err := repo.DequeueNextPending(ctx)
	if !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("dequeue = %v, %v; want ErrNotFound", message, err)
	}
}

// TestCancel_AlreadyCancelledIsNotPending pins the atomic cancel contract:
// only the call that actually transitions the row succeeds, so a repeat
// cancel of an already-cancelled message reports ErrNotPending instead of
// returning success a second time.
func TestCancel_AlreadyCancelledIsNotPending(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	if err := repo.Create(ctx, newInput("m1", "+11111111111")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := repo.Cancel(ctx, "m1"); err != nil {
		t.Fatalf("first cancel: %v", err)
	}

	message, err := repo.Cancel(ctx, "m1")
	if !errors.Is(err, messages.ErrNotPending) {
		t.Fatalf("second cancel = %v, %v; want ErrNotPending", message, err)
	}
	if message != nil {
		t.Fatalf("second cancel message = %+v, want nil", message)
	}
}

// TestDequeueNextPending_ExpiresValidUntil verifies that a message whose
// validity window has closed is expired to Failed (with the recipient
// histories cascaded) instead of being claimed.
func TestDequeueNextPending_ExpiresValidUntil(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	expired := newInput("m1", "+11111111111")
	past := time.Now().UTC().Add(-time.Minute)
	expired.ValidUntil = &past
	if err := repo.Create(ctx, expired); err != nil {
		t.Fatalf("create: %v", err)
	}

	message, err := repo.DequeueNextPending(ctx)
	if !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("dequeue = %v, %v; want ErrNotFound", message, err)
	}

	got, err := repo.GetByID(ctx, "m1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.State != smsgateway.ProcessingStateFailed {
		t.Fatalf("state = %q, want Failed", got.State)
	}
	if got.Recipients[0].State != smsgateway.ProcessingStateFailed {
		t.Fatalf("recipient state = %q, want Failed", got.Recipients[0].State)
	}
}

// TestDequeueNextPending_ExpiryLeavesPendingClaimable verifies that expiry
// removes only expired messages: a healthy Pending message is still claimed
// in the same pass.
func TestDequeueNextPending_ExpiryLeavesPendingClaimable(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	expired := newInput("m1", "+11111111111")
	past := time.Now().UTC().Add(-time.Minute)
	expired.ValidUntil = &past
	if err := repo.Create(ctx, expired); err != nil {
		t.Fatalf("create expired: %v", err)
	}

	if err := repo.Create(ctx, newInput("m2", "+22222222222")); err != nil {
		t.Fatalf("create healthy: %v", err)
	}

	claimed, err := repo.DequeueNextPending(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if claimed.ID != "m2" {
		t.Fatalf("dequeued = %q, want m2 (expired m1 skipped)", claimed.ID)
	}

	if got, loadErr := repo.GetByID(ctx, "m1"); loadErr != nil {
		t.Fatalf("load expired: %v", loadErr)
	} else if got.State != smsgateway.ProcessingStateFailed {
		t.Fatalf("expired state = %q, want Failed", got.State)
	}
}

// TestDequeueNextPending_SkipsFutureSchedule verifies that a message scheduled
// in the future stays Pending and is not claimed.
func TestDequeueNextPending_SkipsFutureSchedule(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	scheduled := newInput("m1", "+11111111111")
	future := time.Now().UTC().Add(time.Hour)
	scheduled.ScheduleAt = &future
	if err := repo.Create(ctx, scheduled); err != nil {
		t.Fatalf("create: %v", err)
	}

	message, err := repo.DequeueNextPending(ctx)
	if !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("dequeue = %v, %v; want ErrNotFound", message, err)
	}

	got, err := repo.GetByID(ctx, "m1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.State != smsgateway.ProcessingStatePending {
		t.Fatalf("state = %q, want Pending", got.State)
	}
}

// TestDequeueNextPending_ClaimsDueSchedule verifies that a scheduled message
// whose time has arrived is claimed like any other.
func TestDequeueNextPending_ClaimsDueSchedule(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	scheduled := newInput("m1", "+11111111111")
	due := time.Now().UTC().Add(-time.Minute)
	scheduled.ScheduleAt = &due
	if err := repo.Create(ctx, scheduled); err != nil {
		t.Fatalf("create: %v", err)
	}

	claimed, err := repo.DequeueNextPending(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if claimed.ID != "m1" || claimed.State != smsgateway.ProcessingStateProcessed {
		t.Fatalf("dequeued = %q state %q, want m1 Processed", claimed.ID, claimed.State)
	}
}

// countMessageTimeComparison runs a raw DATETIME comparison through Bun's
// parameter formatter.
func countMessageTimeComparison(t *testing.T, bunDB *bun.DB, predicate string, value time.Time) int {
	t.Helper()

	var count int
	if err := bunDB.NewRaw(
		"SELECT COUNT(*) FROM messages WHERE "+predicate,
		value,
	).Scan(context.Background(), &count); err != nil {
		t.Fatalf("count %s: %v", predicate, err)
	}

	return count
}

func messageMatchesTimeComparison(
	t *testing.T,
	bunDB *bun.DB,
	extID string,
	predicate string,
	value time.Time,
) bool {
	t.Helper()

	var matched bool
	if err := bunDB.NewRaw(
		"SELECT "+predicate+" FROM messages WHERE ext_id = ?",
		value,
		extID,
	).Scan(context.Background(), &matched); err != nil {
		t.Fatalf("match %s for %s: %v", predicate, extID, err)
	}

	return matched
}

// TestDequeueNextPending_ValidUntilBoundary pins the inclusive valid_until
// comparison used by both expiry updates. The exact value is included, while
// a value one hour earlier is the only strict-less-than match.
func TestDequeueNextPending_ValidUntilBoundary(t *testing.T) {
	repo, bunDB := newRepository(t)
	ctx := context.Background()
	boundary := time.Now().UTC().Truncate(time.Microsecond)
	futureSchedule := time.Now().UTC().Add(2 * time.Hour)

	for _, seed := range []struct {
		extID string
		at    time.Time
	}{
		{extID: "valid-before", at: boundary.Add(-time.Hour)},
		{extID: "valid-at", at: boundary},
		{extID: "valid-after", at: boundary.Add(time.Hour)},
	} {
		input := newInput(seed.extID, "+11111111111")
		at := seed.at
		input.ValidUntil = &at
		if seed.extID == "valid-after" {
			input.ScheduleAt = &futureSchedule
		}
		if err := repo.Create(ctx, input); err != nil {
			t.Fatalf("create %s: %v", seed.extID, err)
		}
	}

	if got := countMessageTimeComparison(t, bunDB, "valid_until <= ?", boundary); got != 2 {
		t.Fatalf("valid_until <= boundary matched %d rows, want 2", got)
	}
	if got := countMessageTimeComparison(t, bunDB, "valid_until < ?", boundary); got != 1 {
		t.Fatalf("valid_until < boundary matched %d rows, want 1", got)
	}
	if !messageMatchesTimeComparison(t, bunDB, "valid-at", "valid_until <= ?", boundary) {
		t.Fatal("valid_until == boundary was not matched by <=")
	}
	if messageMatchesTimeComparison(t, bunDB, "valid-at", "valid_until < ?", boundary) {
		t.Fatal("valid_until == boundary was matched by <")
	}

	if _, err := repo.DequeueNextPending(ctx); !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("dequeue after expiry = %v, want ErrNotFound", err)
	}
	for _, extID := range []string{"valid-before", "valid-at"} {
		got, err := repo.GetByID(ctx, extID)
		if err != nil {
			t.Fatalf("load %s: %v", extID, err)
		}
		if got.State != smsgateway.ProcessingStateFailed {
			t.Fatalf("state for %s = %q, want Failed", extID, got.State)
		}
	}
}

// TestDequeueNextPending_ScheduleAtBoundary pins the inclusive schedule_at
// comparison used by both the outer claim predicate and its nested selector.
func TestDequeueNextPending_ScheduleAtBoundary(t *testing.T) {
	repo, bunDB := newRepository(t)
	ctx := context.Background()
	boundary := time.Now().UTC().Truncate(time.Microsecond)

	for _, seed := range []struct {
		extID string
		at    time.Time
	}{
		{extID: "schedule-before", at: boundary.Add(-time.Hour)},
		{extID: "schedule-at", at: boundary},
		{extID: "schedule-after", at: boundary.Add(time.Hour)},
	} {
		input := newInput(seed.extID, "+11111111111")
		at := seed.at
		input.ScheduleAt = &at
		if err := repo.Create(ctx, input); err != nil {
			t.Fatalf("create %s: %v", seed.extID, err)
		}
	}

	if got := countMessageTimeComparison(t, bunDB, "schedule_at <= ?", boundary); got != 2 {
		t.Fatalf("schedule_at <= boundary matched %d rows, want 2", got)
	}
	if got := countMessageTimeComparison(t, bunDB, "schedule_at < ?", boundary); got != 1 {
		t.Fatalf("schedule_at < boundary matched %d rows, want 1", got)
	}
	if !messageMatchesTimeComparison(t, bunDB, "schedule-at", "schedule_at <= ?", boundary) {
		t.Fatal("schedule_at == boundary was not matched by <=")
	}
	if messageMatchesTimeComparison(t, bunDB, "schedule-at", "schedule_at < ?", boundary) {
		t.Fatal("schedule_at == boundary was matched by <")
	}

	for _, wantID := range []string{"schedule-before", "schedule-at"} {
		claimed, err := repo.DequeueNextPending(ctx)
		if err != nil {
			t.Fatalf("dequeue %s: %v", wantID, err)
		}
		if claimed.ID != wantID {
			t.Fatalf("dequeued = %q, want %q", claimed.ID, wantID)
		}
		if setErr := repo.SetState(ctx, claimed.ID, smsgateway.ProcessingStateSent); setErr != nil {
			t.Fatalf("finalize %s: %v", claimed.ID, setErr)
		}
	}
	if _, err := repo.DequeueNextPending(ctx); !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("dequeue future schedule = %v, want ErrNotFound", err)
	}
}

// TestMessagesSQLiteDefaultTimestamp_RawTimeComparison compares SQLite's
// CURRENT_TIMESTAMP text with a raw [time.Time] parameter and records both values
// on failure because their textual forms intentionally differ.
func TestMessagesSQLiteDefaultTimestamp_RawTimeComparison(t *testing.T) {
	_, bunDB := newRepository(t)
	ctx := context.Background()
	const extID = "sqlite-default-timestamp"

	if _, err := bunDB.ExecContext(
		ctx,
		`INSERT INTO messages (ext_id, device_id, content, state)
		 VALUES (?, ?, ?, ?)`,
		extID,
		"device-1",
		"{}",
		string(smsgateway.ProcessingStatePending),
	); err != nil {
		t.Fatalf("insert default timestamp row: %v", err)
	}

	var storedType, stored string
	if err := bunDB.QueryRowContext(
		ctx,
		"SELECT typeof(created_at), CAST(created_at AS TEXT) FROM messages WHERE ext_id = ?",
		extID,
	).Scan(&storedType, &stored); err != nil {
		t.Fatalf("read default created_at: %v", err)
	}
	if storedType != "text" {
		t.Fatalf("created_at type = %q, want text", storedType)
	}
	parameter, err := time.ParseInLocation("2006-01-02 15:04:05", stored, time.UTC)
	if err != nil {
		t.Fatalf("parse default created_at %q: %v", stored, err)
	}
	parameterText := bunDB.QueryGen().FormatQuery("?", parameter)

	var inclusive, exclusive bool
	if compareErr := bunDB.NewRaw(
		`SELECT created_at <= ?, created_at < ? FROM messages WHERE ext_id = ?`,
		parameter,
		parameter,
		extID,
	).Scan(ctx, &inclusive, &exclusive); compareErr != nil {
		t.Fatalf(
			"compare default created_at %q with parameter %q (%s): %v",
			stored,
			parameter,
			parameterText,
			compareErr,
		)
	}
	if !inclusive {
		t.Fatalf("created_at <= raw time failed: stored=%q parameter=%q bun=%q", stored, parameter, parameterText)
	}
	// The default is a shorter lexical prefix; Bun's raw time has a fractional
	// and offset suffix, so SQLite currently reports the default as < too.
	if !exclusive {
		t.Fatalf("created_at < raw time changed: stored=%q parameter=%q bun=%q", stored, parameter, parameterText)
	}
}

type messageSeed struct {
	extID string
	at    time.Time
}

// seedListMessages creates messages and pins their created_at to exact
// timestamps so filter boundaries are asserted at millisecond precision.
func seedListMessages(t *testing.T, repo *messages.Repository, bunDB *bun.DB, seeds ...messageSeed) {
	t.Helper()
	ctx := context.Background()
	for _, seed := range seeds {
		if err := repo.Create(ctx, newInput(seed.extID, "+79990001111")); err != nil {
			t.Fatalf("create %s: %v", seed.extID, err)
		}
		if _, err := bunDB.ExecContext(
			ctx,
			"UPDATE messages SET created_at = ? WHERE ext_id = ?",
			seed.at,
			seed.extID,
		); err != nil {
			t.Fatalf("set created_at for %s: %v", seed.extID, err)
		}
	}
}

func hasMessage(result []messages.Message, extID string) bool {
	for _, m := range result {
		if m.ID == extID {
			return true
		}
	}
	return false
}

func messageIDs(result []messages.Message) []string {
	ids := make([]string, 0, len(result))
	for _, m := range result {
		ids = append(ids, m.ID)
	}
	return ids
}

// TestList_FilterFromInclusive verifies the start boundary follows the
// date-range convention: a message whose created_at equals the from timestamp
// IS returned (created_at >= from).
func TestList_FilterFromInclusive(t *testing.T) {
	repo, bunDB := newRepository(t)
	ctx := context.Background()

	from := time.Date(2026, 9, 9, 10, 0, 0, 123_000_000, time.UTC)
	to := from.Add(2 * time.Minute)
	seedListMessages(t, repo, bunDB,
		messageSeed{extID: "m-at-from", at: from},
		messageSeed{extID: "m-after-from", at: from.Add(time.Minute)},
	)

	result, _, err := repo.List(ctx, messages.ListOptions{
		Filter: &messages.ListFilter{From: &from, To: &to},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hasMessage(result, "m-at-from") {
		t.Fatalf("list = %v, want m-at-from (created_at == from) included", messageIDs(result))
	}
}

// TestList_FilterToExclusive verifies the end boundary follows the date-range
// convention: a message whose created_at equals the to timestamp is NOT
// returned (created_at < to).
func TestList_FilterToExclusive(t *testing.T) {
	repo, bunDB := newRepository(t)
	ctx := context.Background()

	from := time.Date(2026, 9, 9, 10, 0, 0, 123_000_000, time.UTC)
	to := from.Add(2 * time.Minute)
	seedListMessages(t, repo, bunDB,
		messageSeed{extID: "m-before-to", at: from.Add(time.Minute)},
		messageSeed{extID: "m-at-to", at: to},
	)

	result, _, err := repo.List(ctx, messages.ListOptions{
		Filter: &messages.ListFilter{From: &from, To: &to},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !hasMessage(result, "m-before-to") {
		t.Fatalf("list = %v, want m-before-to included", messageIDs(result))
	}
	if hasMessage(result, "m-at-to") {
		t.Fatalf("list = %v, want m-at-to (created_at == to) excluded", messageIDs(result))
	}
}

// TestList_FilterBoundaryMembership pins the full date-range convention at the
// storage layer: rows seeded at from, between and to (millisecond precision)
// yield exactly [from, between] in the result set.
func TestList_FilterBoundaryMembership(t *testing.T) {
	repo, bunDB := newRepository(t)
	ctx := context.Background()

	from := time.Date(2026, 9, 9, 10, 0, 0, 123_000_000, time.UTC)
	between := from.Add(time.Minute)
	to := from.Add(2 * time.Minute)
	seedListMessages(t, repo, bunDB,
		messageSeed{extID: "m-at-from", at: from},
		messageSeed{extID: "m-between", at: between},
		messageSeed{extID: "m-at-to", at: to},
	)

	result, total, err := repo.List(ctx, messages.ListOptions{
		Filter: &messages.ListFilter{From: &from, To: &to},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}
	for _, want := range []string{"m-at-from", "m-between"} {
		if !hasMessage(result, want) {
			t.Fatalf("list = %v, want %s included", messageIDs(result), want)
		}
	}
	if hasMessage(result, "m-at-to") {
		t.Fatalf("list = %v, want m-at-to (created_at == to) excluded", messageIDs(result))
	}
}

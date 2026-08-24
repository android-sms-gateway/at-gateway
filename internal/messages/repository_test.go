package messages_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db/migrations"
	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.uber.org/zap"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

const (
	testMaxConns = 1

	testListLimitPage  = 2
	testOffsetSecond   = 2
	testOffsetBeyond   = 10
	testSeedTotalCount = 5
	testPendingCount   = 3
)

// newTestRepository builds a Repository over a fresh in-memory SQLite
// database with the embedded goose migration applied.
func newTestRepository(t *testing.T) *messages.Repository {
	t.Helper()

	sqldb, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqldb.SetMaxOpenConns(testMaxConns)
	sqldb.SetMaxIdleConns(testMaxConns)
	t.Cleanup(func() {
		if closeErr := sqldb.Close(); closeErr != nil {
			t.Errorf("close sqlite: %v", closeErr)
		}
	})

	provider, providerErr := goose.NewProvider(database.DialectSQLite3, sqldb, goosefx.Storage(migrations.FS))
	if providerErr != nil {
		t.Fatalf("init goose provider: %v", providerErr)
	}
	if _, upErr := provider.Up(context.Background()); upErr != nil {
		t.Fatalf("apply migrations: %v", upErr)
	}

	return messages.NewRepository(bunfx.New(sqldb, sqlitedialect.New(), zap.NewNop()))
}

// testTime returns a fixed UTC instant offset by duration; [time.Date] is
// mnd-exempt and second precision survives SQLite DATETIME roundtrips.
func testTime(offset time.Duration) time.Time {
	base := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	return base.Add(offset)
}

func newTestMessage(id string, createdAt time.Time) *messages.Message {
	return &messages.Message{
		ID:                 id,
		DeviceID:           "device-under-test",
		State:              messages.StatePending,
		IsHashed:           false,
		IsEncrypted:        false,
		TextMessage:        "hello world",
		SimNumber:          nil,
		WithDeliveryReport: false,
		Priority:           0,
		Recipients:         []string{"+15550000001"},
		States:             []messages.StateChange{{State: messages.StatePending, At: createdAt}},
		ErrorMessage:       nil,
		CreatedAt:          createdAt,
		UpdatedAt:          createdAt,
		ProcessedAt:        nil,
		SentAt:             nil,
		FailedAt:           nil,
	}
}

func assertMessageEqual(t *testing.T, want, got *messages.Message) {
	t.Helper()

	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
	if got.DeviceID != want.DeviceID {
		t.Errorf("DeviceID = %q, want %q", got.DeviceID, want.DeviceID)
	}
	if got.State != want.State {
		t.Errorf("State = %q, want %q", got.State, want.State)
	}
	if got.TextMessage != want.TextMessage {
		t.Errorf("TextMessage = %q, want %q", got.TextMessage, want.TextMessage)
	}
	if got.IsHashed != want.IsHashed || got.IsEncrypted != want.IsEncrypted {
		t.Errorf("IsHashed/IsEncrypted = %t/%t, want %t/%t",
			got.IsHashed, got.IsEncrypted, want.IsHashed, want.IsEncrypted)
	}
	if got.WithDeliveryReport != want.WithDeliveryReport || got.Priority != want.Priority {
		t.Errorf("WithDeliveryReport/Priority = %t/%d, want %t/%d",
			got.WithDeliveryReport, got.Priority, want.WithDeliveryReport, want.Priority)
	}
	if (got.SimNumber == nil) != (want.SimNumber == nil) {
		t.Fatalf("SimNumber presence mismatch: got %v, want %v", got.SimNumber, want.SimNumber)
	}
	if got.SimNumber != nil && *got.SimNumber != *want.SimNumber {
		t.Errorf("SimNumber = %d, want %d", *got.SimNumber, *want.SimNumber)
	}
	if len(got.Recipients) != len(want.Recipients) {
		t.Fatalf("Recipients = %v, want %v", got.Recipients, want.Recipients)
	}
	for i := range want.Recipients {
		if got.Recipients[i] != want.Recipients[i] {
			t.Errorf("Recipients[%d] = %q, want %q", i, got.Recipients[i], want.Recipients[i])
		}
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("CreatedAt/UpdatedAt = %v/%v, want %v/%v",
			got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
}

func assertStateCount(t *testing.T, msg *messages.Message, want int) messages.StateChange {
	t.Helper()

	if len(msg.States) != want {
		t.Fatalf("States entries = %d, want %d (%+v)", len(msg.States), want, msg.States)
	}

	return msg.States[len(msg.States)-1]
}

// TestCreateAndGetByID_Roundtrip covers the happy path: what is persisted is
// byte-equivalent to what is read back, including JSON columns.
func TestCreateAndGetByID_Roundtrip(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	sim := 2
	original := newTestMessage("11111111-1111-4111-8111-111111111111", testTime(time.Minute))
	original.SimNumber = &sim

	if _, createErr := repo.Create(ctx, original); createErr != nil {
		t.Fatalf("Create: %v", createErr)
	}

	fetched, getErr := repo.GetByID(ctx, original.ID)
	if getErr != nil {
		t.Fatalf("GetByID: %v", getErr)
	}

	assertMessageEqual(t, original, fetched)

	last := assertStateCount(t, fetched, 1)
	if last.State != messages.StatePending || !last.At.Equal(original.CreatedAt) {
		t.Errorf("last state = %+v, want Pending at %v", last, original.CreatedAt)
	}
	if fetched.SimNumber == nil || *fetched.SimNumber != sim {
		t.Errorf("SimNumber roundtrip = %v, want %d", fetched.SimNumber, sim)
	}
}

// TestCreate_DuplicateIDReturnsAlreadyExists pins the duplicate-key path.
func TestCreate_DuplicateIDReturnsAlreadyExists(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	first := newTestMessage("22222222-2222-4222-8222-222222222222", testTime(0))
	if _, createErr := repo.Create(ctx, first); createErr != nil {
		t.Fatalf("first Create: %v", createErr)
	}

	_, dupErr := repo.Create(ctx, newTestMessage(first.ID, testTime(time.Minute)))
	if !errors.Is(dupErr, messages.ErrAlreadyExists) {
		t.Fatalf("duplicate Create error = %v, want ErrAlreadyExists", dupErr)
	}
}

// TestGetByID_MissingIDReturnsNotFound covers the empty-table boundary.
func TestGetByID_MissingIDReturnsNotFound(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	_, getErr := repo.GetByID(ctx, "33333333-3333-4333-8333-333333333333")
	if !errors.Is(getErr, messages.ErrNotFound) {
		t.Fatalf("GetByID error = %v, want ErrNotFound", getErr)
	}

	_, nextErr := repo.GetNextPending(ctx)
	if !errors.Is(nextErr, messages.ErrNotFound) {
		t.Fatalf("GetNextPending on empty table error = %v, want ErrNotFound", nextErr)
	}
}

// TestList_EmptyOrderDefaultsAscending pins the zero-value SortOrder contract
// (empty string behaves as ascending).
func TestList_EmptyOrderDefaultsAscending(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	first := newTestMessage("aaaaaaaa-bbbb-4ccc-8ddd-000000000001", testTime(0))
	second := newTestMessage("aaaaaaaa-bbbb-4ccc-8ddd-000000000002", testTime(time.Minute))
	for _, message := range []*messages.Message{first, second} {
		if _, createErr := repo.Create(ctx, message); createErr != nil {
			t.Fatalf("Create %s: %v", message.ID, createErr)
		}
	}

	rows, total, listErr := repo.List(ctx, messages.ListFilter{Limit: 0, Offset: 0, State: nil, Order: ""})
	if listErr != nil {
		t.Fatalf("List with empty order: %v", listErr)
	}
	if total != 2 || len(rows) != 2 || rows[0].ID != first.ID || rows[1].ID != second.ID {
		t.Errorf("rows = %+v (total %d), want [%s %s]", rows, total, first.ID, second.ID)
	}
}

// TestUpdateState_MissingIDReturnsNotFound covers the error path of updates.
func TestUpdateState_MissingIDReturnsNotFound(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	err := repo.UpdateState(ctx, "44444444-4444-4444-8444-444444444444", messages.StateSent, nil)
	if !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("UpdateState error = %v, want ErrNotFound", err)
	}
}

// TestGetNextPending_FIFOOrderAndPeekSemantics pins oldest-first ordering
// across three pending messages with shuffled insertion order, plus peek
// semantics (reading does not mutate the queue).
func TestGetNextPending_FIFOOrderAndPeekSemantics(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	ids := []string{
		"aaaaaaaa-0000-4000-8000-aaaaaaaaaaa1",
		"aaaaaaaa-0000-4000-8000-aaaaaaaaaaa2",
		"aaaaaaaa-0000-4000-8000-aaaaaaaaaaa3",
	}
	offsets := []time.Duration{2 * time.Minute, 0, time.Minute} // shuffled on purpose

	for i, id := range ids {
		if _, createErr := repo.Create(ctx, newTestMessage(id, testTime(offsets[i]))); createErr != nil {
			t.Fatalf("Create %s: %v", id, createErr)
		}
	}

	first, nextErr := repo.GetNextPending(ctx)
	if nextErr != nil {
		t.Fatalf("first GetNextPending: %v", nextErr)
	}
	if first.ID != ids[1] {
		t.Fatalf("oldest pending = %s, want %s", first.ID, ids[1])
	}

	again, peekErr := repo.GetNextPending(ctx)
	if peekErr != nil {
		t.Fatalf("peek GetNextPending: %v", peekErr)
	}
	if again.ID != first.ID {
		t.Fatalf("peek changed queue head: %s then %s", first.ID, again.ID)
	}

	if updateErr := repo.UpdateState(ctx, first.ID, messages.StateSent, nil); updateErr != nil {
		t.Fatalf("advance head to Sent: %v", updateErr)
	}

	second, nextErr := repo.GetNextPending(ctx)
	if nextErr != nil {
		t.Fatalf("second GetNextPending: %v", nextErr)
	}
	if second.ID != ids[2] {
		t.Fatalf("second oldest pending = %s, want %s", second.ID, ids[2])
	}
}

// TestUpdateState_TransitionsAndTimestamps verifies that each transition is
// appended to states_json and stamps its terminal timestamp column.
func TestUpdateState_TransitionsAndTimestamps(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	msg := newTestMessage("55555555-5555-4555-8555-555555555555", testTime(0))
	if _, createErr := repo.Create(ctx, msg); createErr != nil {
		t.Fatalf("Create: %v", createErr)
	}

	if updateErr := repo.UpdateState(ctx, msg.ID, messages.StateSent, nil); updateErr != nil {
		t.Fatalf("UpdateState Sent: %v", updateErr)
	}

	sent, getErr := repo.GetByID(ctx, msg.ID)
	if getErr != nil {
		t.Fatalf("GetByID after Sent: %v", getErr)
	}
	if sent.State != messages.StateSent {
		t.Errorf("State = %q, want Sent", sent.State)
	}
	if sent.SentAt == nil {
		t.Error("SentAt not stamped on Sent transition")
	}
	if sent.FailedAt != nil || sent.ProcessedAt != nil {
		t.Errorf("FailedAt/ProcessedAt = %v/%v, want both nil", sent.FailedAt, sent.ProcessedAt)
	}
	if !sent.UpdatedAt.After(sent.CreatedAt) {
		t.Errorf("UpdatedAt %v not after CreatedAt %v", sent.UpdatedAt, sent.CreatedAt)
	}
	if last := assertStateCount(t, sent, 2); last.State != messages.StateSent {
		t.Errorf("appended state = %q, want Sent", last.State)
	}

	failure := "modem timeout"
	if updateErr := repo.UpdateState(ctx, msg.ID, messages.StateFailed, &failure); updateErr != nil {
		t.Fatalf("UpdateState Failed: %v", updateErr)
	}

	failed, getErr := repo.GetByID(ctx, msg.ID)
	if getErr != nil {
		t.Fatalf("GetByID after Failed: %v", getErr)
	}
	if failed.FailedAt == nil {
		t.Error("FailedAt not stamped on Failed transition")
	}
	if failed.ErrorMessage == nil || *failed.ErrorMessage != failure {
		t.Errorf("ErrorMessage = %v, want %q", failed.ErrorMessage, failure)
	}
	if last := assertStateCount(t, failed, 3); last.State != messages.StateFailed {
		t.Errorf("appended state = %q, want Failed", last.State)
	}
}

// TestCancel_OnlyFromPending pins the atomic guard: cancellation succeeds iff
// the message is still Pending, and repeated or late cancels fail.
func TestCancel_OnlyFromPending(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	pending := newTestMessage("66666666-6666-4666-8666-666666666666", testTime(0))
	if _, createErr := repo.Create(ctx, pending); createErr != nil {
		t.Fatalf("Create pending: %v", createErr)
	}

	cancelled, cancelErr := repo.Cancel(ctx, pending.ID)
	if cancelErr != nil {
		t.Fatalf("Cancel pending: %v", cancelErr)
	}
	if cancelled.State != messages.StateCancelled {
		t.Errorf("State after Cancel = %q, want Cancelled", cancelled.State)
	}
	if cancelled.ProcessedAt == nil {
		t.Error("ProcessedAt not stamped on Cancel")
	}
	if last := assertStateCount(t, cancelled, 2); last.State != messages.StateCancelled {
		t.Errorf("appended state = %q, want Cancelled", last.State)
	}

	_, repeatErr := repo.Cancel(ctx, pending.ID)
	if !errors.Is(repeatErr, messages.ErrNotPending) {
		t.Fatalf("second Cancel error = %v, want ErrNotPending", repeatErr)
	}

	sent := newTestMessage("77777777-7777-4777-8777-777777777777", testTime(0))
	if _, createErr := repo.Create(ctx, sent); createErr != nil {
		t.Fatalf("Create sent: %v", createErr)
	}
	if updateErr := repo.UpdateState(ctx, sent.ID, messages.StateSent, nil); updateErr != nil {
		t.Fatalf("advance to Sent: %v", updateErr)
	}

	_, lateErr := repo.Cancel(ctx, sent.ID)
	if !errors.Is(lateErr, messages.ErrNotPending) {
		t.Fatalf("Cancel on Sent error = %v, want ErrNotPending", lateErr)
	}

	stillSent, getErr := repo.GetByID(ctx, sent.ID)
	if getErr != nil {
		t.Fatalf("GetByID after rejected Cancel: %v", getErr)
	}
	if stillSent.State != messages.StateSent {
		t.Errorf("State mutated by rejected Cancel: %q", stillSent.State)
	}

	_, missingErr := repo.Cancel(ctx, "88888888-8888-4888-8888-888888888888")
	if !errors.Is(missingErr, messages.ErrNotFound) {
		t.Fatalf("Cancel missing error = %v, want ErrNotFound", missingErr)
	}
}

// TestList_FiltersPaginationAndCount covers state filtering, ascending and
// descending order, limit/offset pagination and the total count.
func TestList_FiltersPaginationAndCount(t *testing.T) {
	repo := newTestRepository(t)
	ctx := context.Background()

	seed := []struct {
		id     string
		state  messages.State
		offset time.Duration
	}{
		{id: "99999999-0000-4000-8000-000000000001", state: messages.StatePending, offset: 0},
		{id: "99999999-0000-4000-8000-000000000002", state: messages.StatePending, offset: time.Minute},
		{id: "99999999-0000-4000-8000-000000000003", state: messages.StatePending, offset: 2 * time.Minute},
		{id: "99999999-0000-4000-8000-000000000004", state: messages.StateSent, offset: 3 * time.Minute},
		{id: "99999999-0000-4000-8000-000000000005", state: messages.StateSent, offset: 4 * time.Minute},
	}
	for _, item := range seed {
		message := newTestMessage(item.id, testTime(item.offset))
		if _, createErr := repo.Create(ctx, message); createErr != nil {
			t.Fatalf("Create %s: %v", item.id, createErr)
		}
		if item.state == messages.StateSent {
			if updateErr := repo.UpdateState(ctx, item.id, messages.StateSent, nil); updateErr != nil {
				t.Fatalf("advance %s to Sent: %v", item.id, updateErr)
			}
		}
	}

	pendingState := messages.StatePending

	pageOne, total, listErr := repo.List(ctx, messages.ListFilter{
		Limit:  testListLimitPage,
		Offset: 0,
		State:  &pendingState,
		Order:  messages.SortAsc,
	})
	if listErr != nil {
		t.Fatalf("List page one: %v", listErr)
	}
	if total != testPendingCount {
		t.Errorf("total = %d, want %d", total, testPendingCount)
	}
	if len(pageOne) != testListLimitPage ||
		pageOne[0].ID != seed[0].id || pageOne[1].ID != seed[1].id {
		t.Errorf("page one = %+v, want [%s %s]", pageOne, seed[0].id, seed[1].id)
	}

	pageTwo, total, listErr := repo.List(ctx, messages.ListFilter{
		Limit:  testListLimitPage,
		Offset: testOffsetSecond,
		State:  &pendingState,
		Order:  messages.SortAsc,
	})
	if listErr != nil {
		t.Fatalf("List page two: %v", listErr)
	}
	if total != testPendingCount {
		t.Errorf("page two total = %d, want %d", total, testPendingCount)
	}
	if len(pageTwo) != 1 || pageTwo[0].ID != seed[2].id {
		t.Errorf("page two = %+v, want [%s]", pageTwo, seed[2].id)
	}

	allDesc, total, listErr := repo.List(ctx, messages.ListFilter{
		Limit:  0,
		Offset: 0,
		State:  &pendingState,
		Order:  messages.SortDesc,
	})
	if listErr != nil {
		t.Fatalf("List all desc: %v", listErr)
	}
	if total != testPendingCount || len(allDesc) != testPendingCount {
		t.Fatalf("all desc size = %d (total %d), want %d", len(allDesc), total, testPendingCount)
	}
	for i, want := range []string{seed[2].id, seed[1].id, seed[0].id} {
		if allDesc[i].ID != want {
			t.Errorf("allDesc[%d] = %s, want %s", i, allDesc[i].ID, want)
		}
	}

	everything, total, listErr := repo.List(ctx, messages.ListFilter{
		Limit:  0,
		Offset: 0,
		State:  nil,
		Order:  messages.SortAsc,
	})
	if listErr != nil {
		t.Fatalf("List unfiltered: %v", listErr)
	}
	if total != testSeedTotalCount || len(everything) != testSeedTotalCount {
		t.Errorf("unfiltered = %d rows (total %d), want %d", len(everything), total, testSeedTotalCount)
	}

	failedState := messages.StateFailed
	noMatches, total, listErr := repo.List(ctx, messages.ListFilter{
		Limit:  0,
		Offset: 0,
		State:  &failedState,
		Order:  messages.SortAsc,
	})
	if listErr != nil {
		t.Fatalf("List no-match filter: %v", listErr)
	}
	if total != 0 || len(noMatches) != 0 {
		t.Errorf("no-match filter returned %d rows (total %d), want empty", len(noMatches), total)
	}

	beyond, total, listErr := repo.List(ctx, messages.ListFilter{
		Limit:  testListLimitPage,
		Offset: testOffsetBeyond,
		State:  &pendingState,
		Order:  messages.SortAsc,
	})
	if listErr != nil {
		t.Fatalf("List beyond range: %v", listErr)
	}
	if total != testPendingCount || len(beyond) != 0 {
		t.Errorf("beyond-range page = %+v (total %d), want empty (total %d)",
			beyond, total, testPendingCount)
	}
}

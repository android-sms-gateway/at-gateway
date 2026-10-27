package inbox_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db/migrations"
	"github.com/android-sms-gateway/at-gateway/internal/inbox"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/goosefx"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
)

// TestRepository_MigrationAppliesAndRollsBack pins the schema contract: the
// migration creates the inbox table with the UNIQUE(device_id, message_id)
// index and the two listing indexes, and the rollback removes the indexes and
// the table in reverse order.
func TestRepository_MigrationAppliesAndRollsBack(t *testing.T) {
	t.Parallel()

	bunDB := newInboundDB(t)
	sqldb := bunDB.DB

	if got := countSchemaObjects(t, sqldb, "table", "inbox"); got != 1 {
		t.Fatalf("inbox table count = %d, want 1", got)
	}

	for _, index := range []string{
		"idx_inbox_device_message",
		"idx_inbox_received_at",
		"idx_inbox_type_received_at",
	} {
		if got := countSchemaObjects(t, sqldb, "index", index); got != 1 {
			t.Fatalf("index %s count = %d, want 1", index, got)
		}
	}

	unique := schemaObjectSQL(t, sqldb, "index", "idx_inbox_device_message")
	for _, want := range []string{"UNIQUE", "device_id", "message_id"} {
		if !strings.Contains(unique, want) {
			t.Errorf("idx_inbox_device_message sql = %q, want %q in it", unique, want)
		}
	}

	provider, err := goose.NewProvider(database.DialectSQLite3, sqldb, goosefx.Storage(migrations.FS))
	if err != nil {
		t.Fatalf("init goose provider: %v", err)
	}

	if _, downErr := provider.DownTo(context.Background(), 0); downErr != nil {
		t.Fatalf("DownTo(0): %v", downErr)
	}

	if got := countSchemaObjects(t, sqldb, "table", "inbox"); got != 0 {
		t.Fatalf("inbox table count after rollback = %d, want 0", got)
	}
	for _, index := range []string{
		"idx_inbox_device_message",
		"idx_inbox_received_at",
		"idx_inbox_type_received_at",
	} {
		if got := countSchemaObjects(t, sqldb, "index", index); got != 0 {
			t.Fatalf("index %s count after rollback = %d, want 0", index, got)
		}
	}
}

// TestRepository_InsertIgnoresTheSameIdentity pins the insert-or-ignore
// contract: the same message identity under one device is stored once, and the
// SAME message id under another device is a second row (a rotated device id
// must not collide with an old row's content hash).
func TestRepository_InsertIgnoresTheSameIdentity(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceA := "device-a"
	deviceB := "device-b"

	message := newStoredMessage(
		"0123456789abcdef",
		deviceA,
		smsgateway.IncomingMessageTypeSMS,
		"hello",
		inboxBase(),
	)

	if err := repo.Insert(ctx, message); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if err := repo.Insert(ctx, message); err != nil {
		t.Fatalf("insert the same identity again: %v", err)
	}

	rows, total, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{DeviceID: &deviceA, Types: allTypes()},
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("rows = %d, total = %d, want 1 row and total 1", len(rows), total)
	}

	// The same message id under a DIFFERENT device id is a second row.
	other := newStoredMessage(
		"0123456789abcdef",
		deviceB,
		smsgateway.IncomingMessageTypeSMS,
		"hello",
		inboxBase(),
	)
	if insertErr := repo.Insert(ctx, other); insertErr != nil {
		t.Fatalf("insert the same identity under another device: %v", insertErr)
	}

	_, totalA, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{DeviceID: &deviceA, Types: allTypes()},
	})
	if err != nil {
		t.Fatalf("list messages of the first device: %v", err)
	}
	if totalA != 1 {
		t.Errorf("rows of the first device = %d, want 1", totalA)
	}

	rowsB, totalB, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{DeviceID: &deviceB, Types: allTypes()},
	})
	if err != nil {
		t.Fatalf("list messages of the second device: %v", err)
	}
	if totalB != 1 || len(rowsB) != 1 {
		t.Fatalf("rows of the second device = %d, total = %d, want 1 row and total 1", len(rowsB), totalB)
	}
}

// TestRepository_ListCountsBeforePagination pins the listing contract: the
// page holds at most `limit` rows, the total is the count of ALL matching
// rows, and the order is received_at DESC with the rowid breaking the tie.
func TestRepository_ListCountsBeforePagination(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceID := "device-a"
	base := inboxBase()

	// Five rows, one per minute, newest last.
	for i := range 5 {
		message := newStoredMessage(
			"id-"+string(rune('a'+i)),
			deviceID,
			smsgateway.IncomingMessageTypeSMS,
			"hello",
			base.Add(time.Duration(i)*time.Minute),
		)
		if err := repo.Insert(ctx, message); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	limit := 2
	rows, total, err := repo.List(ctx, inbox.ListOptions{
		Filter:     &inbox.ListFilter{DeviceID: &deviceID, Types: allTypes()},
		Pagination: &inbox.ListPagination{Limit: &limit},
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(rows) != limit {
		t.Fatalf("rows = %d, want the page limit %d", len(rows), limit)
	}
	if total != 5 {
		t.Fatalf("total = %d, want the full count 5", total)
	}
	if rows[0].ID != "id-e" || rows[1].ID != "id-d" {
		t.Errorf("order = %s, %s, want the newest rows first (id-e, id-d)", rows[0].ID, rows[1].ID)
	}

	// Rows sharing a stamp are ordered by the rowid, newest first.
	sameStamp := base.Add(time.Hour)
	for _, id := range []string{"tie-1", "tie-2"} {
		if insertErr := repo.Insert(ctx, newStoredMessage(
			id,
			deviceID,
			smsgateway.IncomingMessageTypeSMS,
			"hello",
			sameStamp,
		)); insertErr != nil {
			t.Fatalf("insert %s: %v", id, insertErr)
		}
	}

	rows, _, err = repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{DeviceID: &deviceID, Types: allTypes()},
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if rows[0].ID != "tie-2" || rows[1].ID != "tie-1" {
		t.Errorf("tie order = %s, %s, want the newer rowid first (tie-2, tie-1)", rows[0].ID, rows[1].ID)
	}
}

// TestRepository_ListDateBounds pins the half-open date range: From is
// INCLUSIVE, To is EXCLUSIVE, and an omitted To is unbounded.
func TestRepository_ListDateBounds(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceID := "device-a"
	from := inboxBase()
	between := from.Add(time.Minute)
	to := from.Add(2 * time.Minute)

	for _, row := range []struct {
		id string
		at time.Time
	}{
		{"at-from", from},
		{"between", between},
		{"at-to", to},
		{"after-to", to.Add(time.Minute)},
	} {
		if err := repo.Insert(ctx, newStoredMessage(
			row.id,
			deviceID,
			smsgateway.IncomingMessageTypeSMS,
			"hello",
			row.at,
		)); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	rows, total, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{
			DeviceID: &deviceID,
			Types:    allTypes(),
			From:     &from,
			To:       &to,
		},
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("rows = %d, total = %d, want 2 rows in [from, to)", len(rows), total)
	}
	if rows[0].ID != "between" || rows[1].ID != "at-from" {
		t.Errorf("rows = %s, %s, want the newest row of the window first", rows[0].ID, rows[1].ID)
	}

	// An omitted To is unbounded.
	_, unbounded, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{DeviceID: &deviceID, Types: allTypes(), From: &from},
	})
	if err != nil {
		t.Fatalf("list messages without an upper bound: %v", err)
	}
	if unbounded != 4 {
		t.Errorf("rows without an upper bound = %d, want 4", unbounded)
	}
}

// TestRepository_RoundTripsTheWireReadyContent pins the content contract: an
// SMS keeps its assembled text verbatim and a DATA_SMS keeps the base64
// NO_WRAP encoding of the FULL raw octets, non-UTF8 bytes included.
func TestRepository_RoundTripsTheWireReadyContent(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceID := "device-a"

	raw := []byte{0xca, 0xfe, 0xba, 0xbe, 0x00, 0xff, 0x80}
	encoded := base64.StdEncoding.EncodeToString(raw)

	if err := repo.Insert(ctx, newStoredMessage(
		"sms-1",
		deviceID,
		smsgateway.IncomingMessageTypeSMS,
		"hello world",
		inboxBase(),
	)); err != nil {
		t.Fatalf("insert the text message: %v", err)
	}
	if err := repo.Insert(ctx, newStoredMessage(
		"data-1",
		deviceID,
		smsgateway.IncomingMessageTypeDataSMS,
		encoded,
		inboxBase().Add(time.Minute),
	)); err != nil {
		t.Fatalf("insert the data message: %v", err)
	}

	rows, _, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{DeviceID: &deviceID, Types: allTypes()},
	})
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}

	if rows[0].Type != smsgateway.IncomingMessageTypeDataSMS {
		t.Errorf("type = %q, want %q", rows[0].Type, smsgateway.IncomingMessageTypeDataSMS)
	}
	if rows[0].Content != encoded {
		t.Errorf("data content = %q, want %q", rows[0].Content, encoded)
	}

	decoded, err := base64.StdEncoding.DecodeString(rows[0].Content)
	if err != nil {
		t.Fatalf("decode the stored data content: %v", err)
	}
	if string(decoded) != string(raw) {
		t.Errorf("decoded octets = %v, want %v", decoded, raw)
	}

	if rows[1].Type != smsgateway.IncomingMessageTypeSMS {
		t.Errorf("type = %q, want %q", rows[1].Type, smsgateway.IncomingMessageTypeSMS)
	}
	if rows[1].Content != "hello world" {
		t.Errorf("text content = %q, want %q", rows[1].Content, "hello world")
	}
}

// TestRepository_FiltersByType pins the type filter: the whole shared enum is
// accepted and the types this gateway never stores match no rows.
func TestRepository_FiltersByType(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceID := "device-a"

	if err := repo.Insert(ctx, newStoredMessage(
		"sms-1",
		deviceID,
		smsgateway.IncomingMessageTypeSMS,
		"hello",
		inboxBase(),
	)); err != nil {
		t.Fatalf("insert the text message: %v", err)
	}

	rows, total, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{
			DeviceID: &deviceID,
			Types:    []smsgateway.IncomingMessageType{smsgateway.IncomingMessageTypeSMS},
		},
	})
	if err != nil {
		t.Fatalf("list the text messages: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("rows = %d, total = %d, want 1 row and total 1", len(rows), total)
	}

	rows, total, err = repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{
			DeviceID: &deviceID,
			Types:    []smsgateway.IncomingMessageType{smsgateway.IncomingMessageTypeMMS},
		},
	})
	if err != nil {
		t.Fatalf("list the MMS messages: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Fatalf("rows = %d, total = %d, want no rows for a type this gateway never stores", len(rows), total)
	}
}

// TestRepository_ListBoundsAndAscendingOrder pins the replay range contract
// through the shared List path: the half-open [since, until) window (a row at
// exactly until is NOT selected, a row at until-1ns IS), the explicit
// non-empty type predicate, and the explicit SortAsc order - CHRONOLOGICAL,
// the opposite of the listing order.
func TestRepository_ListBoundsAndAscendingOrder(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceID := "device-a"
	since := inboxBase()
	until := since.Add(10 * time.Minute)

	for _, row := range []struct {
		id string
		at time.Time
	}{
		{"before-since", since.Add(-time.Nanosecond)},
		{"at-since", since},
		{"between", since.Add(time.Minute)},
		{"until-minus-1ns", until.Add(-time.Nanosecond)},
		{"at-until", until},
		{"after-until", until.Add(time.Minute)},
	} {
		if err := repo.Insert(ctx, newStoredMessage(
			row.id,
			deviceID,
			smsgateway.IncomingMessageTypeSMS,
			"hello",
			row.at,
		)); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}

	ascending := inbox.SortAsc
	rows, _, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{
			Types: []smsgateway.IncomingMessageType{smsgateway.IncomingMessageTypeSMS},
			From:  &since,
			To:    &until,
		},
		Order: &inbox.ListOrder{Order: &ascending},
	})
	if err != nil {
		t.Fatalf("list the replay window: %v", err)
	}

	want := []string{"at-since", "between", "until-minus-1ns"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d (%s), want %d in chronological order",
			len(rows), rowIDs(rows), len(want))
	}
	for i, id := range want {
		if rows[i].ID != id {
			t.Errorf("row[%d] = %s, want %s (received_at ASC, id ASC)", i, rows[i].ID, id)
		}
	}
}

// TestRepository_ListEmptyTypesDisablesTheTypeFilter pins the owner decision
// that an EMPTY or NIL type list DISABLES the type filter: no type predicate
// is added at all, so every row of the window matches. Both a stored SMS row
// AND a stored DATA_SMS row come back for nil and for an empty non-nil list -
// two distinct stored types prove the predicate is genuinely ABSENT rather
// than "matched everything by accident".
func TestRepository_ListEmptyTypesDisablesTheTypeFilter(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceID := "device-a"
	since := inboxBase()
	until := since.Add(time.Hour)

	if err := repo.Insert(ctx, newStoredMessage(
		"sms-1",
		deviceID,
		smsgateway.IncomingMessageTypeSMS,
		"hello",
		since.Add(time.Minute),
	)); err != nil {
		t.Fatalf("insert the text message: %v", err)
	}
	if err := repo.Insert(ctx, newStoredMessage(
		"data-1",
		deviceID,
		smsgateway.IncomingMessageTypeDataSMS,
		"yv66vg==",
		since.Add(2*time.Minute),
	)); err != nil {
		t.Fatalf("insert the data message: %v", err)
	}

	for name, types := range map[string][]smsgateway.IncomingMessageType{
		"nil":   nil,
		"empty": {},
	} {
		rows, total, err := repo.List(ctx, inbox.ListOptions{
			Filter: &inbox.ListFilter{Types: types, From: &since, To: &until},
		})
		if err != nil {
			t.Fatalf("list (%s types): %v", name, err)
		}
		if len(rows) != 2 || total != 2 {
			t.Errorf(
				"list (%s types) returned %d rows (total %d), want both stored rows - the type filter must be DISABLED",
				name,
				len(rows),
				total,
			)

			continue
		}
		seen := make(map[string]bool, len(rows))
		for _, row := range rows {
			seen[row.ID] = true
		}
		if !seen["sms-1"] || !seen["data-1"] {
			t.Errorf("list (%s types) rows = %s, want both sms-1 and data-1", name, rowIDs(rows))
		}
	}
}

// TestRepository_ListFiltersByConcreteTypes pins the type filter of the replay
// through the shared List path: only the requested types are returned, and the
// values MMS + MMS_DOWNLOADED match zero rows because this gateway stores SMS
// and DATA_SMS only.
func TestRepository_ListFiltersByConcreteTypes(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceID := "device-a"
	since := inboxBase()
	until := since.Add(time.Hour)

	if err := repo.Insert(ctx, newStoredMessage(
		"sms-1",
		deviceID,
		smsgateway.IncomingMessageTypeSMS,
		"hello",
		since.Add(time.Minute),
	)); err != nil {
		t.Fatalf("insert the text message: %v", err)
	}
	if err := repo.Insert(ctx, newStoredMessage(
		"data-1",
		deviceID,
		smsgateway.IncomingMessageTypeDataSMS,
		"yv66vg==",
		since.Add(2*time.Minute),
	)); err != nil {
		t.Fatalf("insert the data message: %v", err)
	}

	rows, _, err := repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{
			Types: []smsgateway.IncomingMessageType{smsgateway.IncomingMessageTypeDataSMS},
			From:  &since,
			To:    &until,
		},
	})
	if err != nil {
		t.Fatalf("list the data messages: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "data-1" {
		t.Fatalf("rows = %s, want only the concrete DATA_SMS row", rowIDs(rows))
	}

	// MMS and MMS_DOWNLOADED are never stored at this gateway.
	rows, _, err = repo.List(ctx, inbox.ListOptions{
		Filter: &inbox.ListFilter{
			Types: []smsgateway.IncomingMessageType{
				smsgateway.IncomingMessageTypeMMS,
				smsgateway.IncomingMessageTypeMmsDownloaded,
			},
			From: &since,
			To:   &until,
		},
	})
	if err != nil {
		t.Fatalf("list the MMS rows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %s, want none for MMS/MMS_DOWNLOADED", rowIDs(rows))
	}
}

// TestRepository_ListAppliesTheExplicitOrder pins the order options through
// ListOptions.Order: SortAsc returns the oldest row first, SortDesc the
// newest, and an explicit-but-empty ListOrder keeps the newest-first listing
// order. The unset-Order default is pinned by
// TestRepository_ListCountsBeforePagination.
func TestRepository_ListAppliesTheExplicitOrder(t *testing.T) {
	t.Parallel()

	repo := inbox.NewRepository(newInboundDB(t))
	ctx := context.Background()

	deviceID := "device-a"
	base := inboxBase()

	for i := range 2 {
		if err := repo.Insert(ctx, newStoredMessage(
			"id-"+string(rune('a'+i)),
			deviceID,
			smsgateway.IncomingMessageTypeSMS,
			"hello",
			base.Add(time.Duration(i)*time.Minute),
		)); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}

	ascending := inbox.SortAsc
	descending := inbox.SortDesc

	tests := []struct {
		name  string
		order *inbox.ListOrder
		want  []string
	}{
		{
			name:  "SortAsc lists the oldest row first",
			order: &inbox.ListOrder{Order: &ascending},
			want:  []string{"id-a", "id-b"},
		},
		{
			name:  "SortDesc lists the newest row first",
			order: &inbox.ListOrder{Order: &descending},
			want:  []string{"id-b", "id-a"},
		},
		{
			name:  "an explicit empty ListOrder keeps the listing order",
			order: &inbox.ListOrder{},
			want:  []string{"id-b", "id-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rows, _, err := repo.List(ctx, inbox.ListOptions{
				Filter: &inbox.ListFilter{DeviceID: &deviceID, Types: allTypes()},
				Order:  tt.order,
			})
			if err != nil {
				t.Fatalf("list messages: %v", err)
			}
			if len(rows) != 2 {
				t.Fatalf("rows = %d (%s), want 2", len(rows), rowIDs(rows))
			}
			for i, id := range tt.want {
				if rows[i].ID != id {
					t.Errorf("row[%d] = %s, want %s", i, rows[i].ID, id)
				}
			}
		})
	}
}

// inboxBase is the receive stamp of the repository fixtures.
func inboxBase() time.Time {
	return time.Date(2026, time.October, 22, 9, 0, 0, 0, time.UTC)
}

// countSchemaObjects counts the sqlite_master entries of one type and name.
func countSchemaObjects(t *testing.T, sqldb *sql.DB, objectType, name string) int {
	t.Helper()

	var count int
	row := sqldb.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`,
		objectType,
		name,
	)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query sqlite_master (%s %s): %v", objectType, name, err)
	}

	return count
}

// schemaObjectSQL returns the sqlite_master definition of one object.
func schemaObjectSQL(t *testing.T, sqldb *sql.DB, objectType, name string) string {
	t.Helper()

	var definition string
	row := sqldb.QueryRowContext(
		context.Background(),
		`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = ? AND name = ?`,
		objectType,
		name,
	)
	if err := row.Scan(&definition); err != nil {
		t.Fatalf("query sqlite_master (%s %s): %v", objectType, name, err)
	}

	return definition
}

// newStoredMessage builds a stored-message fixture. Every field is set, so a
// change of the domain struct breaks the fixture loudly.
func newStoredMessage(
	id, deviceID string,
	messageType smsgateway.IncomingMessageType,
	content string,
	receivedAt time.Time,
) inbox.IncomingMessage {
	return inbox.IncomingMessage{
		ID:         id,
		DeviceID:   deviceID,
		Type:       messageType,
		Sender:     "+79990001234",
		Recipient:  nil,
		SimNumber:  nil,
		Content:    content,
		ReceivedAt: receivedAt,
	}
}

// rowIDs lists the message identities of the given rows, for failure messages.
func rowIDs(rows []inbox.IncomingMessage) []string {
	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}

	return ids
}

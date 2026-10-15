package webhooks_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/db/migrations"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/go-core-fx/bunfx"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/sqlfx"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/uptrace/bun"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

const (
	testStartTimeout = 10 * time.Second
	testStopTimeout  = 5 * time.Second

	testSingleConn = 1

	// messagesMigrationVersion is the version of the webhooks migration's
	// predecessor; rolling back "to" it removes the webhooks schema.
	messagesMigrationVersion = int64(20260821000001)
)

// newPersistence builds the full persistence graph (sqlfx + goosefx + bunfx +
// db.Module) against an in-memory SQLite database, so the embedded migrations
// are applied before the repository under test is created.
func newPersistence(t *testing.T) (*webhooks.Repository, *sql.DB, *bun.DB) {
	t.Helper()

	var (
		sqldb *sql.DB
		bunDB *bun.DB
	)

	app := fx.New(
		fx.NopLogger,
		fx.Supply(zap.NewNop()),
		fx.Supply(sqlfx.Config{
			URL:             "sqlite://:memory:",
			ConnMaxIdleTime: 0,
			ConnMaxLifetime: 0,
			MaxOpenConns:    testSingleConn,
			MaxIdleConns:    testSingleConn,
		}),
		db.Module(),
		sqlfx.Module(),
		goosefx.Module(),
		bunfx.Module(),
		fx.Invoke(func(s *sql.DB, b *bun.DB) {
			sqldb = s
			bunDB = b
		}),
	)

	startCtx, cancelStart := context.WithTimeout(context.Background(), testStartTimeout)
	defer cancelStart()

	if err := app.Start(startCtx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancelStop := context.WithTimeout(context.Background(), testStopTimeout)
		defer cancelStop()
		if stopErr := app.Stop(stopCtx); stopErr != nil {
			t.Errorf("stop app: %v", stopErr)
		}
	})

	return webhooks.NewRepository(bunDB), sqldb, bunDB
}

// newGooseProvider builds a goose provider over the same migrations storage
// the fx graph uses, bound to the live test database.
func newGooseProvider(t *testing.T, sqldb *sql.DB) *goose.Provider {
	t.Helper()

	provider, err := goose.NewProvider(database.DialectSQLite3, sqldb, goosefx.Storage(migrations.FS))
	if err != nil {
		t.Fatalf("init goose provider: %v", err)
	}

	return provider
}

// tableColumn is one PRAGMA table_info row used to pin the AC2 schema.
type tableColumn struct {
	name    string
	typ     string
	notnull int
	pk      int
}

func webhooksColumns(t *testing.T, bunDB *bun.DB) []tableColumn {
	t.Helper()

	rows, err := bunDB.QueryContext(
		context.Background(),
		`SELECT name, type, "notnull", pk FROM pragma_table_info('webhooks') ORDER BY cid`,
	)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			t.Errorf("close rows: %v", closeErr)
		}
	}()

	columns := make([]tableColumn, 0, 7)
	for rows.Next() {
		var col tableColumn
		if scanErr := rows.Scan(&col.name, &col.typ, &col.notnull, &col.pk); scanErr != nil {
			t.Fatalf("scan table_info: %v", scanErr)
		}
		columns = append(columns, col)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatalf("table_info rows: %v", rowsErr)
	}

	return columns
}

func countSchemaObjects(t *testing.T, bunDB *bun.DB, objectType, name string) int {
	t.Helper()

	var count int
	if err := bunDB.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`,
		objectType,
		name,
	).Scan(&count); err != nil {
		t.Fatalf("query sqlite_master (%s %s): %v", objectType, name, err)
	}

	return count
}

func schemaObjectSQL(t *testing.T, bunDB *bun.DB, objectType, name string) string {
	t.Helper()

	var definition string
	if err := bunDB.QueryRowContext(
		context.Background(),
		`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = ? AND name = ?`,
		objectType,
		name,
	).Scan(&definition); err != nil {
		t.Fatalf("query schema object (%s %s): %v", objectType, name, err)
	}

	return definition
}

func webhookQueueColumns(t *testing.T, bunDB *bun.DB) []tableColumn {
	t.Helper()

	rows, err := bunDB.QueryContext(
		context.Background(),
		`SELECT name, type, "notnull", pk FROM pragma_table_info('webhook_queue') ORDER BY cid`,
	)
	if err != nil {
		t.Fatalf("queue pragma table_info: %v", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			t.Errorf("close queue rows: %v", closeErr)
		}
	}()

	columns := make([]tableColumn, 0, 9)
	for rows.Next() {
		var col tableColumn
		if scanErr := rows.Scan(&col.name, &col.typ, &col.notnull, &col.pk); scanErr != nil {
			t.Fatalf("scan queue table_info: %v", scanErr)
		}
		columns = append(columns, col)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatalf("queue table_info rows: %v", rowsErr)
	}

	return columns
}

// TestMigration_CreatesWebhooksSchema pins the registry schema: the webhooks
// table and its unique ext_id index exist with the exact column layout
// (device_id directly after ext_id).
func TestMigration_CreatesWebhooksSchema(t *testing.T) {
	_, _, bunDB := newPersistence(t)

	if got := countSchemaObjects(t, bunDB, "table", "webhooks"); got != 1 {
		t.Fatalf("webhooks table count = %d, want 1", got)
	}
	if got := countSchemaObjects(t, bunDB, "index", "idx_webhooks_ext_id"); got != 1 {
		t.Fatalf("idx_webhooks_ext_id count = %d, want 1", got)
	}

	var indexSQL string
	if err := bunDB.QueryRowContext(
		context.Background(),
		`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND name = 'idx_webhooks_ext_id'`,
	).Scan(&indexSQL); err != nil {
		t.Fatalf("index sql: %v", err)
	}
	if !strings.Contains(strings.ToUpper(indexSQL), "UNIQUE") {
		t.Fatalf("idx_webhooks_ext_id sql = %q, want UNIQUE index", indexSQL)
	}

	want := []tableColumn{
		{name: "id", typ: "INTEGER", notnull: 0, pk: 1},
		{name: "ext_id", typ: "VARCHAR(36)", notnull: 1, pk: 0},
		{name: "device_id", typ: "VARCHAR(36)", notnull: 0, pk: 0},
		{name: "url", typ: "VARCHAR(256)", notnull: 1, pk: 0},
		{name: "event", typ: "VARCHAR(32)", notnull: 1, pk: 0},
		{name: "created_at", typ: "DATETIME", notnull: 1, pk: 0},
		{name: "updated_at", typ: "DATETIME", notnull: 1, pk: 0},
	}
	got := webhooksColumns(t, bunDB)
	if len(got) != len(want) {
		t.Fatalf("columns = %d (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("column %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestMigration_CreatesWebhookQueueSchema pins AC1/AC2: the queue table has
// the Android-compatible column layout, defaults, and both composite indexes.
func TestMigration_CreatesWebhookQueueSchema(t *testing.T) {
	_, _, bunDB := newPersistence(t)

	if got := countSchemaObjects(t, bunDB, "table", "webhook_queue"); got != 1 {
		t.Fatalf("webhook_queue table count = %d, want 1", got)
	}

	for _, index := range []string{
		"idx_webhook_queue_status_next_attempt",
		"idx_webhook_queue_status_created_at",
	} {
		if got := countSchemaObjects(t, bunDB, "index", index); got != 1 {
			t.Fatalf("%s count = %d, want 1", index, got)
		}
	}

	nextAttemptIndex := schemaObjectSQL(t, bunDB, "index", "idx_webhook_queue_status_next_attempt")
	if !strings.Contains(strings.ToUpper(nextAttemptIndex), "STATUS") ||
		!strings.Contains(strings.ToUpper(nextAttemptIndex), "NEXT_ATTEMPT") {
		t.Fatalf("next-attempt index sql = %q, want status and next_attempt", nextAttemptIndex)
	}
	createdAtIndex := schemaObjectSQL(t, bunDB, "index", "idx_webhook_queue_status_created_at")
	if !strings.Contains(strings.ToUpper(createdAtIndex), "STATUS") ||
		!strings.Contains(strings.ToUpper(createdAtIndex), "CREATED_AT") {
		t.Fatalf("created-at index sql = %q, want status and created_at", createdAtIndex)
	}

	want := []tableColumn{
		{name: "id", typ: "VARCHAR(36)", notnull: 0, pk: 1},
		{name: "webhook_id", typ: "VARCHAR(36)", notnull: 0, pk: 0},
		{name: "url", typ: "TEXT", notnull: 1, pk: 0},
		{name: "payload", typ: "TEXT", notnull: 1, pk: 0},
		{name: "retry_count", typ: "INTEGER", notnull: 1, pk: 0},
		{name: "status", typ: "VARCHAR(32)", notnull: 1, pk: 0},
		{name: "created_at", typ: "DATETIME", notnull: 0, pk: 0},
		{name: "next_attempt", typ: "DATETIME", notnull: 0, pk: 0},
		{name: "last_error", typ: "TEXT", notnull: 0, pk: 0},
	}
	got := webhookQueueColumns(t, bunDB)
	if len(got) != len(want) {
		t.Fatalf("queue columns = %d (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queue column %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	for column, wantDefault := range map[string]string{
		"retry_count": "0",
		"status":      "'pending'",
	} {
		var defaultValue sql.NullString
		if err := bunDB.QueryRowContext(
			context.Background(),
			`SELECT dflt_value FROM pragma_table_info('webhook_queue') WHERE name = ?`,
			column,
		).Scan(&defaultValue); err != nil {
			t.Fatalf("read %s default: %v", column, err)
		}
		if !defaultValue.Valid || defaultValue.String != wantDefault {
			t.Fatalf("%s default = %+v, want %q", column, defaultValue, wantDefault)
		}
	}
}

// TestMigration_DownRemovesWebhooksSchema proves the Down of the merged
// webhooks migration removes the webhooks and queue schema while the messages
// migration stays applied.
func TestMigration_DownRemovesWebhooksSchema(t *testing.T) {
	_, sqldb, bunDB := newPersistence(t)
	provider := newGooseProvider(t, sqldb)
	ctx := context.Background()

	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}

	if got := countSchemaObjects(t, bunDB, "table", "webhook_queue"); got != 0 {
		t.Fatalf("webhook_queue table count after Down = %d, want 0", got)
	}
	for _, index := range []string{
		"idx_webhook_queue_status_next_attempt",
		"idx_webhook_queue_status_created_at",
	} {
		if got := countSchemaObjects(t, bunDB, "index", index); got != 0 {
			t.Fatalf("%s count after Down = %d, want 0", index, got)
		}
	}
	if got := countSchemaObjects(t, bunDB, "table", "webhooks"); got != 0 {
		t.Fatalf("webhooks table count after Down = %d, want 0", got)
	}
	if got := countSchemaObjects(t, bunDB, "table", "messages"); got != 1 {
		t.Fatalf("messages table count after Down = %d, want 1", got)
	}
}

// TestMigration_DownToMessagesVersion rolls back the webhooks migration,
// leaving the messages schema intact.
func TestMigration_DownToMessagesVersion(t *testing.T) {
	_, sqldb, bunDB := newPersistence(t)
	provider := newGooseProvider(t, sqldb)
	ctx := context.Background()

	results, err := provider.DownTo(ctx, messagesMigrationVersion)
	if err != nil {
		t.Fatalf("DownTo(%d): %v", messagesMigrationVersion, err)
	}
	if len(results) != 1 {
		t.Fatalf("DownTo applied %d migrations, want 1", len(results))
	}
	if got := countSchemaObjects(t, bunDB, "table", "webhook_queue"); got != 0 {
		t.Fatalf("webhook_queue table count after DownTo = %d, want 0", got)
	}
	if got := countSchemaObjects(t, bunDB, "table", "webhooks"); got != 0 {
		t.Fatalf("webhooks table count after DownTo = %d, want 0", got)
	}
	if got := countSchemaObjects(t, bunDB, "table", "messages"); got != 1 {
		t.Fatalf("messages table count after DownTo = %d, want 1", got)
	}
}

// newWebhook builds a domain webhook for tests; an empty deviceID means the
// unscoped case persisted as SQL NULL (DeviceID nil).
func newWebhook(extID, deviceID, url string, event smsgateway.WebhookEvent) webhooks.Webhook {
	w := webhooks.Webhook{
		ID:    extID,
		URL:   url,
		Event: event,
	}
	if deviceID != "" {
		w.DeviceID = &deviceID
	}
	return w
}

// equalDeviceID compares nullable device ids by value (nil == nil only).
func equalDeviceID(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// equalWebhook compares domain webhooks field-wise so *string DeviceID
// round-trips compare by value, not pointer identity.
func equalWebhook(a, b webhooks.Webhook) bool {
	return a.ID == b.ID &&
		equalDeviceID(a.DeviceID, b.DeviceID) &&
		a.URL == b.URL &&
		a.Event == b.Event
}

func equalQueueString(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalQueueTime(a, b time.Time) bool {
	return a.UTC().Truncate(time.Microsecond).Equal(b.UTC().Truncate(time.Microsecond))
}

func equalQueueItem(a, b webhooks.QueueItem) bool {
	return a.ID == b.ID &&
		a.WebhookID == b.WebhookID &&
		a.URL == b.URL &&
		a.Payload == b.Payload &&
		a.RetryCount == b.RetryCount &&
		a.Status == b.Status &&
		equalQueueTime(a.CreatedAt, b.CreatedAt) &&
		equalQueueTime(a.NextAttempt, b.NextAttempt) &&
		equalQueueString(a.LastError, b.LastError)
}

func newQueueItem(id, webhookID, url, payload string, createdAt, nextAttempt time.Time) webhooks.QueueItem {
	return webhooks.QueueItem{
		ID:          id,
		WebhookID:   webhookID,
		URL:         url,
		Payload:     payload,
		RetryCount:  0,
		Status:      webhooks.QueueStatusPending,
		CreatedAt:   createdAt,
		NextAttempt: nextAttempt,
		LastError:   nil,
	}
}

func loadQueueItem(t *testing.T, bunDB *bun.DB, id string) webhooks.QueueItem {
	t.Helper()

	var model webhooks.QueueItemModel
	if err := bunDB.NewSelect().
		Model(&model).
		Where("id = ?", id).
		Scan(context.Background()); err != nil {
		t.Fatalf("load queue item %s: %v", id, err)
	}

	return model.ToDomain()
}

func countQueueItems(t *testing.T, bunDB *bun.DB, id string) int {
	t.Helper()

	var count int
	if err := bunDB.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM webhook_queue WHERE id = ?`,
		id,
	).Scan(&count); err != nil {
		t.Fatalf("count queue item %s: %v", id, err)
	}

	return count
}

// TestSelect_EmptyRegistry verifies an idle registry yields an empty (non-nil)
// slice instead of an error or nil.
func TestSelect_EmptyRegistry(t *testing.T) {
	repo, _, _ := newPersistence(t)

	got, err := repo.Select(context.Background())
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

// TestReplaceThenSelect_RoundTrip covers AC3/AC4: a replaced webhook is
// selected back with every field - including device_id - round-tripped
// exactly, in insertion order, with non-zero timestamps.
func TestReplaceThenSelect_RoundTrip(t *testing.T) {
	repo, _, _ := newPersistence(t)
	ctx := context.Background()

	first := newWebhook("w1", "dev-1", "https://example.com/hook", smsgateway.WebhookEventSmsSent)
	second := newWebhook(
		"w2",
		"PyDmBQZZXYmyxMwED8Fzy",
		"http://127.0.0.1:9099/hook",
		smsgateway.WebhookEventSmsReceived,
	)
	third := newWebhook("w3", "dev-3", "https://example.com/other", smsgateway.WebhookEventMmsDownloaded)

	for _, w := range []webhooks.Webhook{first, second, third} {
		if err := repo.Replace(ctx, w); err != nil {
			t.Fatalf("replace %s: %v", w.ID, err)
		}
	}

	got, err := repo.Select(ctx)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("select returned %d webhooks, want 3", len(got))
	}
	for i, want := range []webhooks.Webhook{first, second, third} {
		if !equalWebhook(got[i], want) {
			t.Fatalf("webhook %d = %+v, want %+v", i, got[i], want)
		}
	}
}

// TestReplaceThenSelect_NullDeviceIDRoundTrip pins owner rule 1: an unscoped
// webhook (nil DeviceID) persists as SQL NULL and selects back with
// DeviceID nil; upserts clear and restore device_id across NULL/valued
// transitions on the same ext_id.
func TestReplaceThenSelect_NullDeviceIDRoundTrip(t *testing.T) {
	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()

	scoped := newWebhook("w1", "dev-1", "https://example.com/scoped", smsgateway.WebhookEventSmsSent)
	unscoped := newWebhook("w1", "", "https://example.com/all", smsgateway.WebhookEventSmsReceived)

	if err := repo.Replace(ctx, scoped); err != nil {
		t.Fatalf("replace scoped: %v", err)
	}
	if err := repo.Replace(ctx, unscoped); err != nil {
		t.Fatalf("replace unscoped: %v", err)
	}

	var stored sql.NullString
	if err := bunDB.QueryRowContext(
		ctx,
		`SELECT device_id FROM webhooks WHERE ext_id = ?`,
		"w1",
	).Scan(&stored); err != nil {
		t.Fatalf("load device_id: %v", err)
	}
	if stored.Valid {
		t.Fatalf("device_id = %q, want SQL NULL after unscoped upsert", stored.String)
	}

	got, err := repo.Select(ctx)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("select returned %d webhooks, want 1", len(got))
	}
	if !equalWebhook(got[0], unscoped) {
		t.Fatalf("unscoped webhook = %+v, want %+v", got[0], unscoped)
	}
	if got[0].DeviceID != nil {
		t.Fatalf("DeviceID = %q, want nil for unscoped row", *got[0].DeviceID)
	}

	// Transition back to scoped: device_id becomes the supplied value again.
	if err = repo.Replace(ctx, scoped); err != nil {
		t.Fatalf("replace scoped again: %v", err)
	}
	got, err = repo.Select(ctx)
	if err != nil {
		t.Fatalf("select after rescope: %v", err)
	}
	if len(got) != 1 || !equalWebhook(got[0], scoped) {
		t.Fatalf("rescoped webhook = %+v, want %+v", got, scoped)
	}
	if !equalDeviceID(got[0].DeviceID, scoped.DeviceID) {
		t.Fatalf("DeviceID = %v, want %v", got[0].DeviceID, scoped.DeviceID)
	}
}

// TestReplace_UpsertByExtID proves AC3 upsert semantics: replacing the same
// ext_id repeatedly keeps a single row (no duplicates), refreshes the
// mutable fields including device_id, and preserves the original primary key
// and created_at.
func TestReplace_UpsertByExtID(t *testing.T) {
	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()

	original := newWebhook("w1", "dev-original", "https://example.com/first", smsgateway.WebhookEventSmsSent)
	if err := repo.Replace(ctx, original); err != nil {
		t.Fatalf("first replace: %v", err)
	}

	var (
		idBefore        int64
		createdAtBefore string
	)
	if err := bunDB.QueryRowContext(
		ctx,
		`SELECT id, created_at FROM webhooks WHERE ext_id = ?`,
		"w1",
	).Scan(&idBefore, &createdAtBefore); err != nil {
		t.Fatalf("load first row: %v", err)
	}
	if idBefore == 0 {
		t.Fatal("first row id = 0, want generated primary key")
	}
	if createdAtBefore == "" {
		t.Fatal("first row created_at empty")
	}

	updated := newWebhook("w1", "dev-updated", "https://example.com/second", smsgateway.WebhookEventSmsDelivered)
	for i := range 3 {
		if err := repo.Replace(ctx, updated); err != nil {
			t.Fatalf("repeat replace %d: %v", i, err)
		}
	}

	var rowCount int
	if err := bunDB.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM webhooks WHERE ext_id = ?`,
		"w1",
	).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("row count after repeated replace = %d, want 1", rowCount)
	}

	var (
		idAfter        int64
		createdAtAfter string
	)
	if err := bunDB.QueryRowContext(
		ctx,
		`SELECT id, created_at FROM webhooks WHERE ext_id = ?`,
		"w1",
	).Scan(&idAfter, &createdAtAfter); err != nil {
		t.Fatalf("load upserted row: %v", err)
	}
	if idAfter != idBefore {
		t.Fatalf("id after upsert = %d, want preserved %d", idAfter, idBefore)
	}
	if createdAtAfter != createdAtBefore {
		t.Fatalf("created_at after upsert = %q, want preserved %q", createdAtAfter, createdAtBefore)
	}

	got, err := repo.Select(ctx)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("select returned %d webhooks, want 1", len(got))
	}
	if !equalWebhook(got[0], updated) {
		t.Fatalf("upserted webhook = %+v, want %+v", got[0], updated)
	}
}

// TestSelectByEvent_Filters verifies event filtering across three distinct
// events and that an event with no rows yields an empty result.
func TestSelectByEvent_Filters(t *testing.T) {
	repo, _, _ := newPersistence(t)
	ctx := context.Background()

	sent := newWebhook("w-sent", "dev-a", "https://example.com/sent", smsgateway.WebhookEventSmsSent)
	received := newWebhook("w-rcv", "dev-b", "https://example.com/received", smsgateway.WebhookEventSmsReceived)
	batch := newWebhook("w-batch", "dev-c", "https://example.com/batch", smsgateway.WebhookEventMmsBatchDownloaded)
	for _, w := range []webhooks.Webhook{sent, received, batch} {
		if err := repo.Replace(ctx, w); err != nil {
			t.Fatalf("replace %s: %v", w.ID, err)
		}
	}

	got, err := repo.SelectByEvent(ctx, smsgateway.WebhookEventSmsSent)
	if err != nil {
		t.Fatalf("select by sms:sent: %v", err)
	}
	if len(got) != 1 || !equalWebhook(got[0], sent) {
		t.Fatalf("select by sms:sent = %+v, want [%+v]", got, sent)
	}

	got, err = repo.SelectByEvent(ctx, smsgateway.WebhookEventMmsBatchDownloaded)
	if err != nil {
		t.Fatalf("select by mms:batch:downloaded: %v", err)
	}
	if len(got) != 1 || !equalWebhook(got[0], batch) {
		t.Fatalf("select by mms:batch:downloaded = %+v, want [%+v]", got, batch)
	}

	got, err = repo.SelectByEvent(ctx, "bogus:event")
	if err != nil {
		t.Fatalf("select by bogus event: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("select by bogus event = %+v, want empty", got)
	}
}

// TestDelete_UnknownIDIsNoop verifies AC3 idempotent delete: an unknown ext_id
// succeeds without touching existing rows.
func TestDelete_UnknownIDIsNoop(t *testing.T) {
	repo, _, _ := newPersistence(t)
	ctx := context.Background()

	kept := newWebhook("w-kept", "dev-kept", "https://example.com/kept", smsgateway.WebhookEventSmsSent)
	if err := repo.Replace(ctx, kept); err != nil {
		t.Fatalf("replace: %v", err)
	}

	if err := repo.Delete(ctx, "does-not-exist"); err != nil {
		t.Fatalf("delete unknown id: %v", err)
	}
	if err := repo.Delete(ctx, ""); err != nil {
		t.Fatalf("delete empty id: %v", err)
	}

	got, err := repo.Select(ctx)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 || !equalWebhook(got[0], kept) {
		t.Fatalf("select after noop deletes = %+v, want [%+v]", got, kept)
	}
}

// TestDelete_RemovesKnownID verifies the happy-path delete removes exactly the
// targeted row and is repeatable.
func TestDelete_RemovesKnownID(t *testing.T) {
	repo, _, _ := newPersistence(t)
	ctx := context.Background()

	removed := newWebhook("w-gone", "dev-1", "https://example.com/gone", smsgateway.WebhookEventSmsFailed)
	kept := newWebhook("w-kept", "dev-2", "https://example.com/kept", smsgateway.WebhookEventSmsSent)
	for _, w := range []webhooks.Webhook{removed, kept} {
		if err := repo.Replace(ctx, w); err != nil {
			t.Fatalf("replace %s: %v", w.ID, err)
		}
	}

	if err := repo.Delete(ctx, removed.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := repo.Delete(ctx, removed.ID); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}

	got, err := repo.Select(ctx)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 || !equalWebhook(got[0], kept) {
		t.Fatalf("select after delete = %+v, want [%+v]", got, kept)
	}
}

// TestWebhookModelRoundTrip exercises the exported model converters across
// three distinct device ids/events plus the unscoped (nil) case:
// newWebhookModel -> toDomain preserves every field (AC4) without touching
// the database (no timestamp equality assertions - conversion is exact for
// the non-time fields).
func TestWebhookModelRoundTrip(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 123456789, time.UTC)

	cases := []webhooks.Webhook{
		newWebhook("w1", "dev-1", "https://example.com/a", smsgateway.WebhookEventSmsSent),
		newWebhook("w2", "PyDmBQZZXYmyxMwED8Fzy", "http://127.0.0.1:9099/hook", smsgateway.WebhookEventSmsReceived),
		newWebhook("w3", "dev-3", "https://example.com/b", smsgateway.WebhookEventSmsBatchDataReceived),
		newWebhook("w4", "", "https://example.com/all", smsgateway.WebhookEventSystemPing),
	}

	for _, want := range cases {
		model := webhooks.NewWebhookModel(want, now)
		if model.ExtID != want.ID {
			t.Fatalf("model ext_id = %q, want %q", model.ExtID, want.ID)
		}
		if !equalDeviceID(model.DeviceID, want.DeviceID) {
			t.Fatalf("model device_id = %v, want %v", model.DeviceID, want.DeviceID)
		}
		got := model.ToDomain()
		if !equalWebhook(got, want) {
			t.Fatalf("toDomain = %+v, want %+v", got, want)
		}
		if !equalDeviceID(got.DeviceID, want.DeviceID) {
			t.Fatalf("round-tripped device_id = %v, want %v", got.DeviceID, want.DeviceID)
		}
	}
}

// TestEnqueue_RoundTripAndDuplicateID covers persistence of all queue fields
// and the boundary error path for a duplicate primary key.
func TestEnqueue_RoundTripAndDuplicateID(t *testing.T) {
	repo, _, bunDB := newPersistence(t)
	createdAt := time.Date(2026, 9, 22, 10, 0, 0, 123456789, time.UTC)
	item := newQueueItem(
		"queue-1",
		"webhook-1",
		"https://example.com/hook",
		`{"event":"sms:received"}`,
		createdAt,
		createdAt.Add(time.Second),
	)

	if err := repo.Enqueue(context.Background(), item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got := loadQueueItem(t, bunDB, item.ID)
	if !equalQueueItem(got, item) {
		t.Fatalf("stored queue item = %+v, want %+v", got, item)
	}
	if count := countQueueItems(t, bunDB, item.ID); count != 1 {
		t.Fatalf("queue item count = %d, want 1", count)
	}

	err := repo.Enqueue(context.Background(), item)
	if err == nil {
		t.Fatal("duplicate enqueue returned nil error")
	}
	if !strings.Contains(err.Error(), "enqueue webhook queue item") {
		t.Fatalf("duplicate enqueue error = %q, want operation context", err)
	}
}

// TestDueItems_OrdersFiltersAndLimits covers FIFO ordering, both due statuses,
// the inclusive next_attempt boundary, a non-due row, and batch limiting.
func TestDueItems_OrdersFiltersAndLimits(t *testing.T) {
	repo, _, _ := newPersistence(t)
	now := time.Date(2026, 9, 22, 11, 0, 0, 0, time.UTC)
	createdAt := now.Add(-time.Hour)

	pending := newQueueItem(
		"queue-pending",
		"",
		"https://example.com/pending",
		`{"event":"pending"}`,
		createdAt,
		now.Add(-2*time.Minute),
	)
	failedError := "connection reset"
	failed := newQueueItem(
		"queue-failed",
		"webhook-failed",
		"https://example.com/failed",
		`{"event":"failed"}`,
		createdAt,
		now,
	)
	failed.Status = webhooks.QueueStatusFailed
	failed.RetryCount = 2
	failed.LastError = &failedError
	future := newQueueItem(
		"queue-future",
		"webhook-future",
		"https://example.com/future",
		`{"event":"future"}`,
		createdAt,
		now.Add(time.Minute),
	)
	completed := newQueueItem(
		"queue-completed",
		"webhook-completed",
		"https://example.com/completed",
		`{"event":"completed"}`,
		createdAt,
		now.Add(-time.Minute),
	)
	completed.Status = webhooks.QueueStatusCompleted

	for _, item := range []webhooks.QueueItem{completed, future, failed, pending} {
		if err := repo.Enqueue(context.Background(), item); err != nil {
			t.Fatalf("enqueue %s: %v", item.ID, err)
		}
	}

	got, err := repo.DueItems(context.Background(), now, 1)
	if err != nil {
		t.Fatalf("due items with limit 1: %v", err)
	}
	if len(got) != 1 || got[0].ID != pending.ID || !equalQueueItem(got[0], pending) {
		t.Fatalf("limited due items = %+v, want first %+v", got, pending)
	}

	got, err = repo.DueItems(context.Background(), now, 10)
	if err != nil {
		t.Fatalf("due items with limit 10: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("due items = %d, want 2", len(got))
	}
	if !equalQueueItem(got[0], pending) || !equalQueueItem(got[1], failed) {
		t.Fatalf("due items = [%+v, %+v], want [%+v, %+v]", got[0], got[1], pending, failed)
	}
}

// TestDueItems_ExactNowIsDue pins the inclusive next_attempt boundary.
func TestDueItems_ExactNowIsDue(t *testing.T) {
	repo, _, _ := newPersistence(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 11, 30, 0, 0, time.UTC)
	item := newQueueItem(
		"queue-exact-now",
		"webhook-exact-now",
		"https://example.com/exact-now",
		`{"event":"exact-now"}`,
		now.Add(-time.Hour),
		now,
	)

	if err := repo.Enqueue(ctx, item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := repo.DueItems(ctx, now, 1)
	if err != nil {
		t.Fatalf("due items at exact now: %v", err)
	}
	if len(got) != 1 || got[0].ID != item.ID {
		t.Fatalf("due items at exact now = %+v, want %s", got, item.ID)
	}
}

// TestCleanup_ExactCutoffIsRetained pins the exclusive created_at cutoff.
func TestCleanup_ExactCutoffIsRetained(t *testing.T) {
	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()
	cutoff := time.Date(2026, 9, 22, 15, 30, 0, 0, time.UTC)
	item := newQueueItem(
		"queue-exact-cutoff",
		"webhook-exact-cutoff",
		"https://example.com/exact-cutoff",
		`{"event":"exact-cutoff"}`,
		cutoff,
		cutoff,
	)
	item.Status = webhooks.QueueStatusCompleted

	if err := repo.Enqueue(ctx, item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := repo.Cleanup(ctx, cutoff); err != nil {
		t.Fatalf("cleanup at exact cutoff: %v", err)
	}
	if count := countQueueItems(t, bunDB, item.ID); count != 1 {
		t.Fatalf("queue item count = %d, want 1", count)
	}
}

// TestRecoverStuck_ExactThresholdIsRetained pins the strict next_attempt cutoff.
func TestRecoverStuck_ExactThresholdIsRetained(t *testing.T) {
	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()
	threshold := time.Date(2026, 9, 22, 14, 30, 0, 0, time.UTC)
	item := newQueueItem(
		"queue-exact-threshold",
		"webhook-exact-threshold",
		"https://example.com/exact-threshold",
		`{"event":"exact-threshold"}`,
		threshold.Add(-time.Hour),
		threshold,
	)

	if err := repo.Enqueue(ctx, item); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := repo.MarkProcessing(ctx, item.ID); err != nil {
		t.Fatalf("mark processing: %v", err)
	}
	if err := repo.RecoverStuck(ctx, threshold); err != nil {
		t.Fatalf("recover at exact threshold: %v", err)
	}
	if got := loadQueueItem(t, bunDB, item.ID); got.Status != webhooks.QueueStatusProcessing {
		t.Fatalf("status = %q, want processing", got.Status)
	}
}

// TestDueItems_EmptyAndNonPositiveLimits verifies the empty-result invariant
// and the batch-size boundary without relying on SQLite LIMIT interpretation.
func TestDueItems_EmptyAndNonPositiveLimits(t *testing.T) {
	repo, _, _ := newPersistence(t)
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

	for _, limit := range []int{0, -1} {
		got, err := repo.DueItems(context.Background(), now, limit)
		if err != nil {
			t.Fatalf("due items with limit %d: %v", limit, err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("due items with limit %d = %+v, want empty non-nil slice", limit, got)
		}
	}
}

// TestQueueLifecycle_TransitionsAndRetry covers pending -> processing ->
// completed, processing -> failed -> processing -> permanently_failed, retry
// increments, nullable errors, and idempotent terminal transitions.
func TestQueueLifecycle_TransitionsAndRetry(t *testing.T) {
	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()
	createdAt := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)

	completedItem := newQueueItem(
		"queue-completed",
		"webhook-completed",
		"https://example.com/completed",
		`{"event":"completed"}`,
		createdAt,
		createdAt,
	)
	if err := repo.Enqueue(ctx, completedItem); err != nil {
		t.Fatalf("enqueue completed item: %v", err)
	}
	due, err := repo.DueItems(ctx, createdAt, 10)
	if err != nil {
		t.Fatalf("due completed item: %v", err)
	}
	if len(due) != 1 || due[0].ID != completedItem.ID {
		t.Fatalf("due items = %+v, want completed item", due)
	}
	if err = repo.MarkProcessing(ctx, completedItem.ID); err != nil {
		t.Fatalf("mark processing: %v", err)
	}
	if err = repo.MarkProcessing(ctx, completedItem.ID); err != nil {
		t.Fatalf("repeat mark processing: %v", err)
	}
	got := loadQueueItem(t, bunDB, completedItem.ID)
	if got.Status != webhooks.QueueStatusProcessing || got.RetryCount != 0 {
		t.Fatalf("processing item = %+v, want processing with zero retries", got)
	}
	if err = repo.MarkCompleted(ctx, completedItem.ID); err != nil {
		t.Fatalf("mark completed: %v", err)
	}
	if err = repo.MarkCompleted(ctx, completedItem.ID); err != nil {
		t.Fatalf("repeat mark completed: %v", err)
	}
	got = loadQueueItem(t, bunDB, completedItem.ID)
	if got.Status != webhooks.QueueStatusCompleted || got.RetryCount != 0 {
		t.Fatalf("completed item = %+v, want completed with zero retries", got)
	}

	retryItem := newQueueItem(
		"queue-retry",
		"webhook-retry",
		"https://example.com/retry",
		`{"event":"retry"}`,
		createdAt,
		createdAt,
	)
	if err = repo.Enqueue(ctx, retryItem); err != nil {
		t.Fatalf("enqueue retry item: %v", err)
	}
	if err = repo.MarkProcessing(ctx, retryItem.ID); err != nil {
		t.Fatalf("mark retry item processing: %v", err)
	}
	nextAttempt := createdAt.Add(5 * time.Minute)
	retryError := "timeout"
	if err = repo.MarkFailed(ctx, retryItem.ID, nextAttempt, retryError); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	got = loadQueueItem(t, bunDB, retryItem.ID)
	if got.Status != webhooks.QueueStatusFailed || got.RetryCount != 1 ||
		!equalQueueTime(got.NextAttempt, nextAttempt) ||
		!equalQueueString(got.LastError, &retryError) {
		t.Fatalf("failed item = %+v, want failed with one retry and error", got)
	}

	if err = repo.MarkProcessing(ctx, retryItem.ID); err != nil {
		t.Fatalf("mark failed item processing: %v", err)
	}
	permanentError := "maximum retries"
	if err = repo.MarkPermanentlyFailed(ctx, retryItem.ID, permanentError); err != nil {
		t.Fatalf("mark permanently failed: %v", err)
	}
	if err = repo.MarkPermanentlyFailed(ctx, retryItem.ID, permanentError); err != nil {
		t.Fatalf("repeat mark permanently failed: %v", err)
	}
	got = loadQueueItem(t, bunDB, retryItem.ID)
	if got.Status != webhooks.QueueStatusPermanentlyFailed || got.RetryCount != 1 ||
		!equalQueueString(got.LastError, &permanentError) {
		t.Fatalf("permanent item = %+v, want permanent with one retry", got)
	}
}

// TestRecoverStuck_OnlyMovesStaleProcessing proves the stale-threshold boundary
// and leaves fresh processing plus pending rows untouched.
func TestRecoverStuck_OnlyMovesStaleProcessing(t *testing.T) {
	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)
	createdAt := now.Add(-time.Hour)

	stale := newQueueItem(
		"queue-stale",
		"webhook-stale",
		"https://example.com/stale",
		`{"event":"stale"}`,
		createdAt,
		now.Add(-6*time.Minute),
	)
	fresh := newQueueItem(
		"queue-fresh",
		"webhook-fresh",
		"https://example.com/fresh",
		`{"event":"fresh"}`,
		createdAt,
		now.Add(-4*time.Minute),
	)
	pending := newQueueItem(
		"queue-pending",
		"webhook-pending",
		"https://example.com/pending",
		`{"event":"pending"}`,
		createdAt,
		now.Add(-10*time.Minute),
	)
	for _, item := range []webhooks.QueueItem{stale, fresh, pending} {
		if err := repo.Enqueue(ctx, item); err != nil {
			t.Fatalf("enqueue %s: %v", item.ID, err)
		}
	}
	if err := repo.MarkProcessing(ctx, stale.ID); err != nil {
		t.Fatalf("mark stale processing: %v", err)
	}
	if err := repo.MarkProcessing(ctx, fresh.ID); err != nil {
		t.Fatalf("mark fresh processing: %v", err)
	}

	threshold := now.Add(-5 * time.Minute)
	if err := repo.RecoverStuck(ctx, threshold); err != nil {
		t.Fatalf("recover stuck: %v", err)
	}
	if err := repo.RecoverStuck(ctx, threshold); err != nil {
		t.Fatalf("repeat recover stuck: %v", err)
	}

	if got := loadQueueItem(t, bunDB, stale.ID); got.Status != webhooks.QueueStatusPending {
		t.Fatalf("stale status = %q, want pending", got.Status)
	}
	if got := loadQueueItem(t, bunDB, fresh.ID); got.Status != webhooks.QueueStatusProcessing {
		t.Fatalf("fresh status = %q, want processing", got.Status)
	}
	if got := loadQueueItem(t, bunDB, pending.ID); got.Status != webhooks.QueueStatusPending {
		t.Fatalf("pending status = %q, want pending", got.Status)
	}
	due, err := repo.DueItems(ctx, now, 10)
	if err != nil {
		t.Fatalf("due after recovery: %v", err)
	}
	if len(due) != 2 || due[0].ID != pending.ID || due[1].ID != stale.ID {
		t.Fatalf("due after recovery = %+v, want pending then stale", due)
	}
}

// TestCleanup_RemovesOnlyOldTerminalRows covers completed/permanently_failed
// deletion, the strict cutoff boundary, preservation of retryable rows, and
// idempotent cleanup.
func TestCleanup_RemovesOnlyOldTerminalRows(t *testing.T) {
	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()
	cutoff := time.Date(2026, 9, 22, 15, 0, 0, 0, time.UTC)

	oldCompleted := newQueueItem(
		"old-completed",
		"webhook-1",
		"https://example.com/1",
		`{"event":"old-completed"}`,
		cutoff.Add(-2*time.Hour),
		cutoff.Add(-2*time.Hour),
	)
	oldPermanent := newQueueItem(
		"old-permanent",
		"webhook-2",
		"https://example.com/2",
		`{"event":"old-permanent"}`,
		cutoff.Add(-time.Hour),
		cutoff.Add(-time.Hour),
	)
	oldFailed := newQueueItem(
		"old-failed",
		"webhook-3",
		"https://example.com/3",
		`{"event":"old-failed"}`,
		cutoff.Add(-30*time.Minute),
		cutoff.Add(-30*time.Minute),
	)
	oldPending := newQueueItem(
		"old-pending",
		"webhook-4",
		"https://example.com/4",
		`{"event":"old-pending"}`,
		cutoff.Add(-20*time.Minute),
		cutoff.Add(-20*time.Minute),
	)
	atCutoff := newQueueItem("at-cutoff", "webhook-5", "https://example.com/5", `{"event":"at-cutoff"}`, cutoff, cutoff)
	newCompleted := newQueueItem(
		"new-completed",
		"webhook-6",
		"https://example.com/6",
		`{"event":"new-completed"}`,
		cutoff.Add(time.Hour),
		cutoff.Add(time.Hour),
	)
	for _, item := range []webhooks.QueueItem{
		oldCompleted,
		oldPermanent,
		oldFailed,
		oldPending,
		atCutoff,
		newCompleted,
	} {
		if err := repo.Enqueue(ctx, item); err != nil {
			t.Fatalf("enqueue %s: %v", item.ID, err)
		}
	}

	for _, id := range []string{oldCompleted.ID, oldPermanent.ID, oldFailed.ID, atCutoff.ID, newCompleted.ID} {
		if err := repo.MarkProcessing(ctx, id); err != nil {
			t.Fatalf("mark %s processing: %v", id, err)
		}
	}
	if err := repo.MarkCompleted(ctx, oldCompleted.ID); err != nil {
		t.Fatalf("mark old completed: %v", err)
	}
	if err := repo.MarkPermanentlyFailed(ctx, oldPermanent.ID, "terminal"); err != nil {
		t.Fatalf("mark old permanent: %v", err)
	}
	if err := repo.MarkFailed(ctx, oldFailed.ID, cutoff.Add(time.Hour), "retryable"); err != nil {
		t.Fatalf("mark old failed: %v", err)
	}
	if err := repo.MarkCompleted(ctx, atCutoff.ID); err != nil {
		t.Fatalf("mark cutoff completed: %v", err)
	}
	if err := repo.MarkCompleted(ctx, newCompleted.ID); err != nil {
		t.Fatalf("mark new completed: %v", err)
	}

	if err := repo.Cleanup(ctx, cutoff); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := repo.Cleanup(ctx, cutoff); err != nil {
		t.Fatalf("repeat cleanup: %v", err)
	}

	for _, id := range []string{oldCompleted.ID, oldPermanent.ID} {
		if count := countQueueItems(t, bunDB, id); count != 0 {
			t.Fatalf("old terminal item %s count = %d, want 0", id, count)
		}
	}
	for _, id := range []string{oldFailed.ID, oldPending.ID, atCutoff.ID, newCompleted.ID} {
		if count := countQueueItems(t, bunDB, id); count != 1 {
			t.Fatalf("retained item %s count = %d, want 1", id, count)
		}
	}
}

// TestQueueItemModelRoundTrip exercises all queue statuses and both nullable
// error forms without database timestamp round-tripping.
func TestQueueItemModelRoundTrip(t *testing.T) {
	createdAt := time.Date(2026, 9, 22, 16, 0, 0, 123456789, time.UTC)
	firstError := "first"
	secondError := "second"
	cases := []webhooks.QueueItem{
		newQueueItem("model-pending", "", "https://example.com/pending", `{"event":"pending"}`, createdAt, createdAt),
		newQueueItem(
			"model-failed",
			"webhook-1",
			"https://example.com/failed",
			`{"event":"failed"}`,
			createdAt,
			createdAt.Add(time.Second),
		),
		newQueueItem(
			"model-permanent",
			"webhook-2",
			"https://example.com/permanent",
			`{"event":"permanent"}`,
			createdAt,
			createdAt.Add(time.Minute),
		),
	}
	cases[1].Status = webhooks.QueueStatusFailed
	cases[1].RetryCount = 1
	cases[1].LastError = &firstError
	cases[2].Status = webhooks.QueueStatusPermanentlyFailed
	cases[2].RetryCount = 3
	cases[2].LastError = &secondError

	for _, want := range cases {
		model := webhooks.NewQueueItemModel(want)
		got := model.ToDomain()
		if !equalQueueItem(got, want) {
			t.Fatalf("queue model round-trip = %+v, want %+v", got, want)
		}
	}
}

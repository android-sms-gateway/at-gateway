package settings_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/db/migrations"
	"github.com/android-sms-gateway/at-gateway/internal/settings"
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

	// inboxMigrationVersion is the version of the settings migration's
	// predecessor; rolling back "to" it removes the settings schema.
	inboxMigrationVersion = int64(20261022000003)
)

// newPersistence builds the full persistence graph (sqlfx + goosefx + bunfx +
// db.Module) against an in-memory SQLite database, so the embedded migrations
// are applied before the repository under test is created.
func newPersistence(t *testing.T) (*settings.Repository, *sql.DB, *bun.DB) {
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

	return settings.NewRepository(bunDB), sqldb, bunDB
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

func countSchemaObjects(t *testing.T, bunDB *bun.DB, objectType, name string) int {
	t.Helper()

	var count int
	if err := bunDB.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`,
		objectType, name,
	).Scan(&count); err != nil {
		t.Fatalf("query sqlite_master (%s %s): %v", objectType, name, err)
	}

	return count
}

// TestMigration_CreatesSettingsSchema pins the settings table layout: a
// nullable TEXT value (NULL = unset) under a primary-key key column.
func TestMigration_CreatesSettingsSchema(t *testing.T) {
	t.Parallel()

	_, _, bunDB := newPersistence(t)

	if got := countSchemaObjects(t, bunDB, "table", "settings"); got != 1 {
		t.Fatalf("settings table count = %d, want 1", got)
	}

	rows, err := bunDB.QueryContext(
		context.Background(),
		`SELECT name, type, "notnull", pk FROM pragma_table_info('settings') ORDER BY cid`,
	)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			t.Errorf("close rows: %v", closeErr)
		}
	}()

	type tableColumn struct {
		name    string
		typ     string
		notnull int
		pk      int
	}
	want := []tableColumn{
		{name: "key", typ: "VARCHAR(64)", notnull: 1, pk: 1},
		{name: "value", typ: "TEXT", notnull: 0, pk: 0},
		{name: "created_at", typ: "DATETIME", notnull: 1, pk: 0},
		{name: "updated_at", typ: "DATETIME", notnull: 1, pk: 0},
	}
	got := make([]tableColumn, 0, len(want))
	for rows.Next() {
		var col tableColumn
		if scanErr := rows.Scan(&col.name, &col.typ, &col.notnull, &col.pk); scanErr != nil {
			t.Fatalf("scan table_info: %v", scanErr)
		}
		got = append(got, col)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatalf("table_info rows: %v", rowsErr)
	}
	if len(got) != len(want) {
		t.Fatalf("columns = %d (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("column %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestMigration_DownRemovesSettingsSchema rolls back only the settings
// migration (Down pops the newest) and asserts the table is gone while the
// inbox schema below survives.
func TestMigration_DownRemovesSettingsSchema(t *testing.T) {
	t.Parallel()

	_, sqldb, bunDB := newPersistence(t)
	provider := newGooseProvider(t, sqldb)
	ctx := context.Background()

	if _, err := provider.DownTo(ctx, inboxMigrationVersion); err != nil {
		t.Fatalf("DownTo(%d): %v", inboxMigrationVersion, err)
	}

	if got := countSchemaObjects(t, bunDB, "table", "settings"); got != 0 {
		t.Fatalf("settings table count after Down = %d, want 0", got)
	}
	if got := countSchemaObjects(t, bunDB, "table", "inbox"); got != 1 {
		t.Fatalf("inbox table count after Down = %d, want 1", got)
	}
}

// TestGet_UnsetKeyReturnsAbsent pins the unset baseline: a key that was
// never written reports ok=false and an empty value, never an error.
func TestGet_UnsetKeyReturnsAbsent(t *testing.T) {
	t.Parallel()

	repo, _, _ := newPersistence(t)

	value, ok, err := repo.Get(context.Background(), "ping.interval_seconds")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false (never written)")
	}
	if value != "" {
		t.Errorf("value = %q, want empty", value)
	}
}

// TestSet_RoundTrip pins insert and overwrite: a second Set replaces the
// value in place (one row, created_at preserved, updated_at refreshed).
func TestSet_RoundTrip(t *testing.T) {
	t.Parallel()

	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "ping.interval_seconds", "60"); err != nil {
		t.Fatalf("first Set: %v", err)
	}

	var createdAt string
	if err := bunDB.QueryRowContext(ctx, `SELECT created_at FROM settings WHERE key = ?`, "ping.interval_seconds").
		Scan(&createdAt); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	if err := repo.Set(ctx, "ping.interval_seconds", "30"); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	value, ok, err := repo.Get(ctx, "ping.interval_seconds")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || value != "30" {
		t.Fatalf("value = %q (ok=%v), want 30", value, ok)
	}

	var rowCount int
	if scanErr := bunDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings`).Scan(&rowCount); scanErr != nil {
		t.Fatalf("count rows: %v", scanErr)
	}
	if rowCount != 1 {
		t.Fatalf("row count = %d, want 1 (upsert, not insert)", rowCount)
	}

	var createdAtAfter string
	if scanErr := bunDB.QueryRowContext(ctx, `SELECT created_at FROM settings WHERE key = ?`, "ping.interval_seconds").
		Scan(&createdAtAfter); scanErr != nil {
		t.Fatalf("read created_at after: %v", scanErr)
	}
	if createdAtAfter != createdAt {
		t.Fatalf("created_at changed on update: %q -> %q", createdAt, createdAtAfter)
	}
}

// TestDelete_RemovesRowAndIsIdempotent pins unset-by-deletion: the row is
// gone (not NULL), a repeat delete of the same key is a no-op, and other keys
// are untouched.
func TestDelete_RemovesRowAndIsIdempotent(t *testing.T) {
	t.Parallel()

	repo, _, bunDB := newPersistence(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "webhooks.retry_count", "7"); err != nil {
		t.Fatalf("seed retry: %v", err)
	}
	if err := repo.Set(ctx, "ping.interval_seconds", "30"); err != nil {
		t.Fatalf("seed interval: %v", err)
	}

	if err := repo.Delete(ctx, "webhooks.retry_count"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, getErr := repo.Get(ctx, "webhooks.retry_count"); getErr != nil {
		t.Fatalf("Get after delete: %v", getErr)
	} else if ok {
		t.Error("deleted key still present")
	}

	if err := repo.Delete(ctx, "webhooks.retry_count"); err != nil {
		t.Fatalf("repeat Delete: %v", err)
	}

	var rowCount int
	if err := bunDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings`).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("row count = %d, want 1 (interval untouched)", rowCount)
	}
}

// TestGet_NullValueReadsAsUnset pins tolerance for a schema-legal NULL row
// (the column stays nullable): it reads as an unset key, never as "".
func TestGet_NullValueReadsAsUnset(t *testing.T) {
	t.Parallel()

	_, _, bunDB := newPersistence(t)
	ctx := context.Background()

	if _, err := bunDB.ExecContext(ctx,
		`INSERT INTO settings (key, value, created_at, updated_at) VALUES (?, NULL, datetime('now'), datetime('now'))`,
		"legacy.key",
	); err != nil {
		t.Fatalf("insert NULL row: %v", err)
	}

	repo := settings.NewRepository(bunDB)
	value, ok, err := repo.Get(ctx, "legacy.key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false (NULL = unset)")
	}
	if value != "" {
		t.Errorf("value = %q, want empty", value)
	}
}

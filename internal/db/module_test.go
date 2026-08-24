package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/at-gateway/internal/db/migrations"
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
)

// newStartedApp builds the full persistence graph (sqlfx + goosefx + bunfx +
// db.Module) against an in-memory SQLite database and starts it, so the
// embedded migrations are applied before assertions run.
func newStartedApp(t *testing.T) (*sql.DB, *bun.DB) {
	t.Helper()

	var (
		sqldb *sql.DB
		bundb *bun.DB
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
		fx.Invoke(func(sqlDB *sql.DB, bunDB *bun.DB) {
			sqldb = sqlDB
			bundb = bunDB
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

	return sqldb, bundb
}

func countObjects(t *testing.T, sqldb *sql.DB, objectType, name string) int {
	t.Helper()

	row := sqldb.QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`,
		objectType,
		name,
	)

	var count int
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query sqlite_master (%s %s): %v", objectType, name, err)
	}

	return count
}

func indexSQL(t *testing.T, sqldb *sql.DB, name string) string {
	t.Helper()

	row := sqldb.QueryRowContext(
		context.Background(),
		`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND name = ?`,
		name,
	)

	var definition string
	if err := row.Scan(&definition); err != nil {
		t.Fatalf("query index definition (%s): %v", name, err)
	}

	return definition
}

// TestModule_BindingsAndMigrationsApplied verifies that Module() supplies all
// three bindings (goose dialect, bun schema dialect, migration storage), the
// composition starts, and the embedded migration created the messages table
// plus both indexes.
func TestModule_BindingsAndMigrationsApplied(t *testing.T) {
	sqldb, bundb := newStartedApp(t)

	if bundb == nil {
		t.Fatal("*bun.DB binding was not resolved")
	}

	if got := countObjects(t, sqldb, "table", "messages"); got != 1 {
		t.Fatalf("messages table count = %d, want 1", got)
	}

	for _, index := range []string{"idx_messages_state_created", "idx_messages_created_at"} {
		if got := countObjects(t, sqldb, "index", index); got != 1 {
			t.Fatalf("index %s count = %d, want 1", index, got)
		}
	}

	if def := indexSQL(t, sqldb, "idx_messages_state_created"); !strings.Contains(def, "state") ||
		!strings.Contains(def, "created_at") {
		t.Fatalf("idx_messages_state_created sql = %q, want state + created_at columns", def)
	}
	if def := indexSQL(t, sqldb, "idx_messages_created_at"); !strings.Contains(def, "created_at") {
		t.Fatalf("idx_messages_created_at sql = %q, want created_at column", def)
	}
}

// TestModule_UpIdempotent pins goose as schema authority: a second Up over an
// already-migrated database applies zero results.
func TestModule_UpIdempotent(t *testing.T) {
	sqldb, _ := newStartedApp(t)

	provider, err := goose.NewProvider(database.DialectSQLite3, sqldb, goosefx.Storage(migrations.FS))
	if err != nil {
		t.Fatalf("init provider: %v", err)
	}

	results, upErr := provider.Up(context.Background())
	if upErr != nil {
		t.Fatalf("second Up: %v", upErr)
	}
	if len(results) != 0 {
		t.Fatalf("second Up applied %d migrations, want 0", len(results))
	}
}

// TestModule_DownRemovesSchema rolls the schema back and asserts the table and
// both indexes are gone.
func TestModule_DownRemovesSchema(t *testing.T) {
	sqldb, _ := newStartedApp(t)

	provider, err := goose.NewProvider(database.DialectSQLite3, sqldb, goosefx.Storage(migrations.FS))
	if err != nil {
		t.Fatalf("init provider: %v", err)
	}

	if _, downErr := provider.Down(context.Background()); downErr != nil {
		t.Fatalf("Down: %v", downErr)
	}

	if got := countObjects(t, sqldb, "table", "messages"); got != 0 {
		t.Fatalf("messages table count after Down = %d, want 0", got)
	}
	for _, index := range []string{"idx_messages_state_created", "idx_messages_created_at"} {
		if got := countObjects(t, sqldb, "index", index); got != 0 {
			t.Fatalf("index %s count after Down = %d, want 0", index, got)
		}
	}
}

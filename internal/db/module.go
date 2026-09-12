// Package db provides the fx wiring for SQLite persistence: the goose and bun
// dialect bindings plus the embedded migration storage.
package db

import (
	"github.com/android-sms-gateway/at-gateway/internal/db/migrations"
	"github.com/go-core-fx/goosefx"
	"github.com/go-core-fx/logger"
	"github.com/pressly/goose/v3/database"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/schema"
	"go.uber.org/fx"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" database/sql driver
)

// Module returns the fx module providing storage bindings consumed by
// sqlfx.Module, goosefx.Module and bunfx.Module.
func Module() fx.Option {
	return fx.Module(
		"db",
		logger.WithNamedLogger("db"),
		fx.Provide(func() database.Dialect {
			return database.DialectSQLite3
		}),
		fx.Provide(func() schema.Dialect {
			return sqlitedialect.New()
		}),
		fx.Provide(func() goosefx.Storage {
			return goosefx.Storage(migrations.FS)
		}),
	)
}

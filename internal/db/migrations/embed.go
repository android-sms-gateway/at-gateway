// Package migrations embeds the goose SQL schema migrations.
package migrations

import "embed"

// FS contains *sql schema migration files applied by goosefx.
//
//go:embed *.sql
var FS embed.FS

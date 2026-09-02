package db

import "time"

type TimedModel struct {
	CreatedAt time.Time `bun:"created_at,nullzero,notnull"`
	UpdatedAt time.Time `bun:"updated_at,nullzero,notnull"`
}

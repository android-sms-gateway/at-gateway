package settings

import (
	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/uptrace/bun"
)

// settingsModel is the persisted settings row: one key/value pair per row.
// Writers always store a concrete string; a NULL value (legacy row) reads as
// an unset key.
type settingsModel struct {
	bun.BaseModel `bun:"table:settings,alias:s"`
	db.TimedModel

	Key   string  `bun:"key,pk"`
	Value *string `bun:"value"`
}

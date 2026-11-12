package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/uptrace/bun"
)

// Repository is the bun-backed key-value store over the settings table. It
// stores keys as-is: no prefixing, no value interpretation - callers see the
// exact strings that were written.
type Repository struct {
	db *bun.DB
}

// NewRepository returns a Repository backed by the given bun database.
func NewRepository(db *bun.DB) *Repository {
	return &Repository{db: db}
}

// Get returns the stored value for key; ok is false when the row is absent
// or its value is NULL.
func (r *Repository) Get(ctx context.Context, key string) (string, bool, error) {
	model := settingsModel{
		BaseModel: bun.BaseModel{},
		TimedModel: db.TimedModel{
			CreatedAt: time.Time{},
			UpdatedAt: time.Time{},
		},
		Key:   "",
		Value: nil,
	}
	if err := r.db.NewSelect().
		Model(&model).
		Column("value").
		Where("key = ?", key).
		Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("select setting %s: %w", key, err)
	}

	if model.Value == nil {
		return "", false, nil
	}

	return *model.Value, true, nil
}

// Set stores value under key, refreshing updated_at and keeping the
// created_at of an existing row.
func (r *Repository) Set(ctx context.Context, key string, value string) error {
	now := time.Now().UTC()
	model := settingsModel{
		BaseModel: bun.BaseModel{},
		TimedModel: db.TimedModel{
			CreatedAt: now,
			UpdatedAt: now,
		},
		Key:   key,
		Value: &value,
	}

	if _, err := r.db.NewInsert().
		Model(&model).
		On("CONFLICT (key) DO UPDATE").
		Set("value = EXCLUDED.value").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx); err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}

	return nil
}

// Delete removes the row for key; an absent key is a no-op.
func (r *Repository) Delete(ctx context.Context, key string) error {
	if _, err := r.db.NewDelete().
		Table("settings").
		Where("key = ?", key).
		Exec(ctx); err != nil {
		return fmt.Errorf("delete setting %s: %w", key, err)
	}

	return nil
}

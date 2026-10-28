// Package inbox holds the inbound message persistence: the domain records, the
// bun models and the repository over *bun.DB.
package inbox

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

const (
	// orderIncomingDescending is the listing order: the newest received
	// message first, the rowid breaks the tie between messages received
	// within the same stamp.
	orderIncomingDescending = "received_at DESC, id DESC"
	// orderIncomingAscending is the replay order: the oldest received message
	// first, the rowid breaks the tie - deliberately the OPPOSITE of the
	// listing order, so a refresh window replays chronologically.
	orderIncomingAscending = "received_at ASC, id ASC"
)

// Repository is the bun-backed data access layer for the persisted inbound
// messages.
type Repository struct {
	db *bun.DB
}

// NewRepository returns a Repository backed by the given bun database.
func NewRepository(db *bun.DB) *Repository {
	return &Repository{db: db}
}

// Insert stores one inbound message and reports whether a row was ACTUALLY
// written. The insert IGNORES a row that already exists for the same
// (device_id, message_id) pair: the modem re-notifies a message it did not
// delete yet and the retained-entry sweep re-feeds a stored entry after a
// restart, so the same message must never become a second row. The uniqueness
// is per DEVICE, so a rotated device id cannot collide with an old row's
// content hash.
//
// inserted is true when the row was written and false when the conflict was
// ignored because the row already existed - that outcome is NOT an error. If
// reading the affected-row count itself fails, the error is wrapped and
// returned: the outcome is unknown, so the caller must count a store failure
// instead of claiming success.
func (r *Repository) Insert(ctx context.Context, message IncomingMessage) (bool, error) {
	model := newInboundModel(message, time.Now().UTC())

	// bun's On() prepends " ON " itself, so the conflict target is bare.
	res, err := r.db.NewInsert().
		Model(model).
		On("CONFLICT (device_id, message_id) DO NOTHING").
		Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("insert inbox message: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read inbox insert rows affected: %w", err)
	}

	return rows > 0, nil
}

// List returns one page of stored inbound messages plus the total number of
// rows matching the filter. The total is taken BEFORE the pagination, so it
// is the count of ALL matching rows and not the count of the page.
func (r *Repository) List(ctx context.Context, options ListOptions) ([]IncomingMessage, int, error) {
	models := make([]inboxModel, 0)
	query := r.db.NewSelect().Model(&models)
	query = options.apply(query)

	total, scanErr := query.ScanAndCount(ctx)
	if scanErr != nil {
		return nil, 0, fmt.Errorf("list inbox messages: %w", scanErr)
	}

	result := make([]IncomingMessage, 0, len(models))
	for i := range models {
		result = append(result, models[i].toDomain())
	}

	return result, total, nil
}

// Package messages holds the message persistence domain: state constants,
// domain models and the repository over *bun.DB.
package messages

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"modernc.org/sqlite"
)

const (
	condByID    = "id = ?"
	condByState = "state = ?"

	orderAscending  = "created_at ASC, id ASC"
	orderDescending = "created_at DESC, id DESC"

	cancelStatesAppendExpr = "states_json = json_insert(states_json, '$[#]', json(?))"
	nextPendingLimit       = 1
	sqliteConstraintMask   = 0xff
	sqliteConstraintCode   = 19
)

// Repository is the bun-backed data access layer for persisted messages.
type Repository struct {
	db *bun.DB
}

// NewRepository returns a Repository backed by the given bun database.
func NewRepository(db *bun.DB) *Repository {
	return &Repository{db: db}
}

// Create persists msg as-is; the caller owns ID, CreatedAt, UpdatedAt and the
// initial States entry. Returns ErrAlreadyExists when ID collides.
func (r *Repository) Create(ctx context.Context, msg *Message) (*Message, error) {
	model, err := newModel(msg)
	if err != nil {
		return nil, fmt.Errorf("map message model: %w", err)
	}

	if _, insertErr := r.db.NewInsert().Model(model).Exec(ctx); insertErr != nil {
		if isDuplicateConstraint(insertErr) {
			return nil, ErrAlreadyExists
		}
		return nil, fmt.Errorf("insert message: %w", insertErr)
	}

	return msg, nil
}

// GetByID returns the message with the given ID or ErrNotFound.
func (r *Repository) GetByID(ctx context.Context, id string) (*Message, error) {
	model := new(messageModel)

	err := r.db.NewSelect().Model(model).Where(condByID, id).Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("select message: %w", err)
	}

	return model.toDomain()
}

// List returns one page of messages ordered by created_at plus the total
// number of messages matching the state filter.
func (r *Repository) List(ctx context.Context, filter ListFilter) ([]Message, int, error) {
	total, err := r.count(ctx, filter.State)
	if err != nil {
		return nil, 0, err
	}

	models := make([]messageModel, 0)
	query := r.db.NewSelect().Model(&models)
	if filter.State != nil {
		query = query.Where(condByState, string(*filter.State))
	}
	if filter.Order == SortDesc {
		query = query.OrderExpr(orderDescending)
	} else {
		query = query.OrderExpr(orderAscending)
	}
	if filter.Limit > 0 {
		query = query.Limit(filter.Limit)
	}
	query = query.Offset(filter.Offset)

	if scanErr := query.Scan(ctx); scanErr != nil {
		return nil, 0, fmt.Errorf("list messages: %w", scanErr)
	}

	result := make([]Message, 0, len(models))
	for i := range models {
		item, convErr := models[i].toDomain()
		if convErr != nil {
			return nil, 0, fmt.Errorf("map listed message: %w", convErr)
		}
		result = append(result, *item)
	}

	return result, total, nil
}

// GetNextPending returns the oldest Pending message without mutating it;
// ErrNotFound signals an empty queue.
func (r *Repository) GetNextPending(ctx context.Context) (*Message, error) {
	model := new(messageModel)

	err := r.db.NewSelect().
		Model(model).
		Where(condByState, string(StatePending)).
		OrderExpr(orderAscending).
		Limit(nextPendingLimit).
		Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("select next pending message: %w", err)
	}

	return model.toDomain()
}

// UpdateState appends a StateChange entry and rewrites state, error_message,
// updated_at and the terminal timestamp column matching the new state.
func (r *Repository) UpdateState(ctx context.Context, id string, state State, errorMessage *string) error {
	current, err := r.GetByID(ctx, id)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	statesJSON, marshalErr := json.Marshal(append(current.States, StateChange{State: state, At: now}))
	if marshalErr != nil {
		return fmt.Errorf("marshal states: %w", marshalErr)
	}

	update := r.db.NewUpdate().
		Model((*messageModel)(nil)).
		Set("state = ?", string(state)).
		Set("updated_at = ?", now).
		Set("states_json = ?", string(statesJSON)).
		Set("error_message = ?", errorMessage).
		Where(condByID, id)
	switch state {
	case StatePending:
	case StateSent:
		update = update.Set("sent_at = ?", now)
	case StateFailed:
		update = update.Set("failed_at = ?", now)
	case StateCancelled:
		update = update.Set("processed_at = ?", now)
	}

	if _, execErr := update.Exec(ctx); execErr != nil {
		return fmt.Errorf("update message state: %w", execErr)
	}

	return nil
}

// Cancel atomically moves a Pending message to Cancelled in a single guarded
// UPDATE; it returns ErrNotPending when the message left Pending first.
func (r *Repository) Cancel(ctx context.Context, id string) (*Message, error) {
	now := time.Now().UTC()
	entry, marshalErr := json.Marshal(StateChange{State: StateCancelled, At: now})
	if marshalErr != nil {
		return nil, fmt.Errorf("marshal cancel entry: %w", marshalErr)
	}

	res, execErr := r.db.NewUpdate().
		Model((*messageModel)(nil)).
		Set("state = ?", string(StateCancelled)).
		Set("processed_at = ?", now).
		Set("updated_at = ?", now).
		Set(cancelStatesAppendExpr, string(entry)).
		Where(condByID, id).
		Where(condByState, string(StatePending)).
		Exec(ctx)
	if execErr != nil {
		return nil, fmt.Errorf("cancel message: %w", execErr)
	}

	affected, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return nil, fmt.Errorf("cancel message rows affected: %w", rowsErr)
	}
	if affected == 0 {
		// The guarded UPDATE matched no rows: either the ID is unknown or the
		// message already left Pending; disambiguate for the caller.
		_, getErr := r.GetByID(ctx, id)
		switch {
		case errors.Is(getErr, ErrNotFound):
			return nil, ErrNotFound
		case getErr != nil:
			return nil, fmt.Errorf("inspect cancelled message: %w", getErr)
		default:
			return nil, ErrNotPending
		}
	}

	return r.GetByID(ctx, id)
}

func (r *Repository) count(ctx context.Context, state *State) (int, error) {
	query := r.db.NewSelect().Model((*messageModel)(nil))
	if state != nil {
		query = query.Where(condByState, string(*state))
	}

	total, err := query.Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("count messages: %w", err)
	}

	return total, nil
}

// isDuplicateConstraint reports SQLITE_CONSTRAINT-family errors (any extended
// code sharing the base number), which covers unique/PK violations.
func isDuplicateConstraint(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}

	return (sqliteErr.Code() & sqliteConstraintMask) == sqliteConstraintCode
}

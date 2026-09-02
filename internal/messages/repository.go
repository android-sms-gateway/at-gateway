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

	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
	"modernc.org/sqlite"
)

const (
	// condByExtID addresses messages by their public ext_id (the wire ID);
	// the integer PK stays internal.
	condByExtID = "ext_id = ?"
	condByState = "state = ?"
	// condByMessage addresses recipients of the message with the given
	// ext_id through a subquery on the messages table.
	condByMessage = "message_id = (SELECT id FROM messages WHERE ext_id = ?)"

	orderAscending  = "created_at ASC, id ASC"
	orderDescending = "created_at DESC, id DESC"

	statesAppendExpr     = "states = json_insert(states, '$[#]', json(?))"
	nextPendingLimit     = 1
	sqliteConstraintMask = 0xff
	sqliteConstraintCode = 19
)

// Repository is the bun-backed data access layer for persisted messages.
type Repository struct {
	db *bun.DB
}

// NewRepository returns a Repository backed by the given bun database.
func NewRepository(db *bun.DB) *Repository {
	return &Repository{db: db}
}

// Create persists a message and all its recipients in one transaction. An
// already-known ext_id yields ErrAlreadyExists; a duplicated recipient pair
// rolls the whole transaction back and yields ErrDuplicateRecipient. The
// service is the sole ext_id generator, so an empty ExtID is rejected with
// ErrMissingExtID before any database work.
func (r *Repository) Create(ctx context.Context, msg *MessageInput) error {
	now := time.Now().UTC()
	model, err := newMessageModel(msg, now)
	if err != nil {
		return fmt.Errorf("map message model: %w", err)
	}

	err = r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, insertErr := tx.NewInsert().Model(model).Returning("id").Exec(ctx); insertErr != nil {
			if isDuplicateConstraint(insertErr) {
				return ErrAlreadyExists
			}

			return fmt.Errorf("insert message: %w", insertErr)
		}

		recipientModels := newRecipientModels(msg.PhoneNumbers, model.ID, now)
		if len(recipientModels) == 0 {
			return nil
		}

		if _, insertErr := tx.NewInsert().Model(&recipientModels).Exec(ctx); insertErr != nil {
			if isDuplicateConstraint(insertErr) {
				return ErrDuplicateRecipient
			}

			return fmt.Errorf("insert recipient: %w", insertErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("create message: %w", err)
	}

	return nil
}

// GetByID returns the message with the given ext_id and its recipients, or
// ErrNotFound.
func (r *Repository) GetByID(ctx context.Context, id string) (*Message, error) {
	model := new(messageModel)

	err := r.db.NewSelect().
		Model(model).
		Relation("Recipients", orderRecipients).
		Where(condByExtID, id).
		Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("select message: %w", err)
	}

	message, err := model.toDomain()
	if err != nil {
		return nil, fmt.Errorf("map message: %w", err)
	}

	return message, nil
}

// List returns one page of messages (with recipients) ordered by created_at
// plus the total number of messages matching the state filter.
func (r *Repository) List(ctx context.Context, filter ListFilter) ([]Message, int, error) {
	total, err := r.count(ctx, filter.State)
	if err != nil {
		return nil, 0, err
	}

	models := make([]messageModel, 0)
	query := r.db.NewSelect().
		Model(&models).
		Relation("Recipients", orderRecipients)
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

// GetNextPending returns the oldest Pending message and its recipients
// without mutating it; ErrNotFound signals an empty queue.
func (r *Repository) GetNextPending(ctx context.Context) (*Message, error) {
	model := new(messageModel)

	err := r.db.NewSelect().
		Model(model).
		Relation("Recipients", orderRecipients).
		Where(condByState, string(smsgateway.ProcessingStatePending)).
		OrderExpr(orderAscending).
		Limit(nextPendingLimit).
		Scan(ctx)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("select next pending message: %w", err)
	}

	message, err := model.toDomain()
	if err != nil {
		return nil, fmt.Errorf("map message: %w", err)
	}

	return message, nil
}

// SetRecipientRef stores the send reference of one recipient; ErrNotFound
// when the recipient does not exist.
func (r *Repository) SetRecipientRef(ctx context.Context, messageID, phone string, refID int) error {
	res, err := r.db.NewUpdate().
		Model((*recipientModel)(nil)).
		Set("ref_id = ?", refID).
		Where(condByMessage, messageID).
		Where("phone = ?", phone).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("set recipient ref: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set recipient ref rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}

	return nil
}

// UpdateRecipientState appends a state entry to the recipient history and
// sets the error (and optionally the send reference). A recipient that is
// already Failed is never modified (terminal state); an unknown recipient
// yields ErrNotFound.
func (r *Repository) UpdateRecipientState(
	ctx context.Context,
	messageID, phone string,
	state smsgateway.ProcessingState,
	refID *int,
	errStr *string,
) error {
	now := time.Now().UTC()
	entry, err := json.Marshal(stateModel{State: state, At: now})
	if err != nil {
		return fmt.Errorf("marshal recipient state entry: %w", err)
	}

	query := r.db.NewUpdate().
		Model((*recipientModel)(nil)).
		Set("states = json_insert(states, '$[#]', json(?))", string(entry)).
		Set("error = ?", errStr).
		Where(condByMessage, messageID).
		Where("phone = ?", phone).
		Where("(json_extract(states, '$[#-1].state') IS NULL OR json_extract(states, '$[#-1].state') <> ?)",
			string(smsgateway.ProcessingStateFailed))
	if refID != nil {
		query = query.Set("ref_id = ?", *refID)
	}

	res, err := query.Exec(ctx)
	if err != nil {
		return fmt.Errorf("update recipient state: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update recipient state rows affected: %w", err)
	}
	if affected == 0 {
		exists, existsErr := r.recipientExists(ctx, messageID, phone)
		if existsErr != nil {
			return existsErr
		}
		if !exists {
			return ErrNotFound
		}
	}

	return nil
}

// AppendMessageState moves the message to state in one atomic UPDATE that
// also appends the matching history entry (state column and last states JSON
// entry stay in sync). An already-Failed message is never modified (terminal
// state); an unknown ID yields ErrNotFound.
func (r *Repository) AppendMessageState(ctx context.Context, id string, state smsgateway.ProcessingState) error {
	now := time.Now().UTC()
	entry, err := json.Marshal(stateModel{State: state, At: now})
	if err != nil {
		return fmt.Errorf("marshal state entry: %w", err)
	}

	res, err := r.db.NewUpdate().
		Model((*messageModel)(nil)).
		Set("state = ?", string(state)).
		Set("updated_at = ?", now).
		Set(statesAppendExpr, string(entry)).
		Where(condByExtID, id).
		Where("state <> ?", string(smsgateway.ProcessingStateFailed)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("append message state: %w", err)
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("append message state rows affected: %w", err)
	}
	if affected == 0 {
		exists, existsErr := r.messageExists(ctx, id)
		if existsErr != nil {
			return existsErr
		}
		if !exists {
			return ErrNotFound
		}
	}

	return nil
}

// Cancel atomically moves a Pending message to Cancelled in a single guarded
// UPDATE, marks all its recipients Cancelled in the same transaction and
// returns the updated message. ErrNotPending when the message left Pending
// first; ErrNotFound for an unknown ID.
func (r *Repository) Cancel(ctx context.Context, id string) (*Message, error) {
	now := time.Now().UTC()
	entry, marshalErr := json.Marshal(stateModel{State: smsgateway.ProcessingStateCancelled, At: now})
	if marshalErr != nil {
		return nil, fmt.Errorf("marshal cancel entry: %w", marshalErr)
	}

	var result *Message

	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		affected, updateErr := r.cancelMessage(ctx, tx, id, string(entry))
		if updateErr != nil {
			return updateErr
		}
		if affected == 0 {
			return r.cancelGuardError(ctx, tx, id)
		}

		if _, recipientErr := tx.NewUpdate().
			Model((*recipientModel)(nil)).
			Set(statesAppendExpr, string(entry)).
			Where(condByMessage, id).
			Exec(ctx); recipientErr != nil {
			return fmt.Errorf("cancel recipients: %w", recipientErr)
		}

		message, loadErr := r.loadMessageTx(ctx, tx, id)
		if loadErr != nil {
			return loadErr
		}
		result = message

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cancel message: %w", err)
	}

	return result, nil
}

// orderRecipients makes relation loading deterministic: recipients are
// returned in insertion order.
func orderRecipients(q *bun.SelectQuery) *bun.SelectQuery {
	return q.OrderExpr("id ASC")
}

// cancelMessage runs the guarded Pending->Cancelled UPDATE and returns the
// number of affected rows.
func (r *Repository) cancelMessage(ctx context.Context, tx bun.Tx, id, entry string) (int64, error) {
	res, execErr := tx.NewUpdate().
		Model((*messageModel)(nil)).
		Set("state = ?", string(smsgateway.ProcessingStateCancelled)).
		Set("updated_at = ?", time.Now().UTC()).
		Set(statesAppendExpr, entry).
		Where(condByExtID, id).
		Where(condByState, string(smsgateway.ProcessingStatePending)).
		Exec(ctx)
	if execErr != nil {
		return 0, fmt.Errorf("cancel message: %w", execErr)
	}

	affected, rowsErr := res.RowsAffected()
	if rowsErr != nil {
		return 0, fmt.Errorf("cancel message rows affected: %w", rowsErr)
	}

	return affected, nil
}

// cancelGuardError disambiguates a no-op Cancel: unknown ID vs already left
// Pending.
func (r *Repository) cancelGuardError(ctx context.Context, tx bun.Tx, id string) error {
	exists, existsErr := tx.NewSelect().
		Model((*messageModel)(nil)).
		Where(condByExtID, id).
		Exists(ctx)
	if existsErr != nil {
		return fmt.Errorf("inspect cancelled message: %w", existsErr)
	}
	if !exists {
		return ErrNotFound
	}

	return ErrNotPending
}

// loadMessageTx selects a message with its recipients inside the transaction
// and maps it to the domain.
func (r *Repository) loadMessageTx(ctx context.Context, tx bun.Tx, id string) (*Message, error) {
	model := new(messageModel)
	if selectErr := tx.NewSelect().
		Model(model).
		Relation("Recipients", orderRecipients).
		Where(condByExtID, id).
		Scan(ctx); selectErr != nil {
		return nil, fmt.Errorf("select cancelled message: %w", selectErr)
	}

	message, mapErr := model.toDomain()
	if mapErr != nil {
		return nil, fmt.Errorf("map cancelled message: %w", mapErr)
	}

	return message, nil
}

func (r *Repository) count(ctx context.Context, state *smsgateway.ProcessingState) (int, error) {
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

func (r *Repository) messageExists(ctx context.Context, id string) (bool, error) {
	exists, err := r.db.NewSelect().
		Model((*messageModel)(nil)).
		Where(condByExtID, id).
		Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("check message existence: %w", err)
	}

	return exists, nil
}

func (r *Repository) recipientExists(ctx context.Context, messageID, phone string) (bool, error) {
	exists, err := r.db.NewSelect().
		Model((*recipientModel)(nil)).
		Where(condByMessage, messageID).
		Where("phone = ?", phone).
		Exists(ctx)
	if err != nil {
		return false, fmt.Errorf("check recipient existence: %w", err)
	}

	return exists, nil
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

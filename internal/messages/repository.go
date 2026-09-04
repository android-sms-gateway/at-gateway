// Package messages holds the message persistence domain: state constants,
// domain models and the repository over *bun.DB.
package messages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/db"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
)

const (
	orderAscending  = "id ASC"
	orderDescending = "id DESC"
	orderFiFo       = orderAscending
	orderLiFo       = orderDescending

	statesAppendExpr = "states = json_insert(states, '$[#]', json(?))"

	// statesLastStateExpr yields the current recipient state, which is derived
	// from the last entry of the states history rather than a dedicated column.
	statesLastStateExpr = "json_extract(states, '$[#-1].state')"
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
			if db.IsDuplicateConstraint(insertErr) {
				return ErrAlreadyExists
			}

			return fmt.Errorf("insert message: %w", insertErr)
		}

		recipientModels := newRecipientModels(msg.PhoneNumbers, model.ID, now)
		if len(recipientModels) == 0 {
			return nil
		}

		if _, insertErr := tx.NewInsert().Model(&recipientModels).Exec(ctx); insertErr != nil {
			if db.IsDuplicateConstraint(insertErr) {
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
		Where("ext_id = ?", id).
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
func (r *Repository) List(ctx context.Context, options ListOptions) ([]Message, int, error) {
	models := make([]messageModel, 0)
	query := r.db.NewSelect().
		Model(&models).
		Relation("Recipients", orderRecipients)
	query = options.apply(query)

	total, scanErr := query.ScanAndCount(ctx)
	if scanErr != nil {
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

func (r *Repository) Cancel(ctx context.Context, id string) (*Message, error) {
	if err := r.updateState(ctx, id, smsgateway.ProcessingStateFailed, nil); err != nil {
		return nil, err
	}

	message, err := r.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	return message, nil
}

func (r *Repository) SetState(ctx context.Context, id string, state smsgateway.ProcessingState) error {
	now := time.Now().UTC()
	entry := stateModel{State: state, At: now}

	_, err := r.db.NewUpdate().
		Model((*messageModel)(nil)).
		Set("state = ?", state).
		Set("updated_at = ?", now).
		Set(statesAppendExpr, entry).
		Where("ext_id = ?", id).
		Where("state <> ?", state).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("update state: %w", err)
	}

	return nil
}

func (r *Repository) SetRecipientProcessed(ctx context.Context, messageID, phoneNumber string) error {
	return r.updateRecipientState(
		ctx,
		messageID,
		phoneNumber,
		smsgateway.ProcessingStateProcessed,
		func(uq *bun.UpdateQuery) *bun.UpdateQuery {
			return uq.Where(statesLastStateExpr+" = ?", smsgateway.ProcessingStatePending)
		},
	)
}

func (r *Repository) SetRecipientSent(ctx context.Context, messageID, phoneNumber string, refID int) error {
	return r.updateRecipientState(
		ctx,
		messageID,
		phoneNumber,
		smsgateway.ProcessingStateSent,
		func(uq *bun.UpdateQuery) *bun.UpdateQuery {
			return uq.Set("ref_id = ?", refID)
		},
	)
}

func (r *Repository) SetRecipientFailed(ctx context.Context, messageID, phoneNumber, reason string) error {
	return r.updateRecipientState(
		ctx,
		messageID,
		phoneNumber,
		smsgateway.ProcessingStateFailed,
		func(uq *bun.UpdateQuery) *bun.UpdateQuery {
			return uq.Set("error = ?", reason)
		},
	)
}

// SetRecipientDeliveredByRef flips the recipient whose stored message
// reference matches refID to Delivered, and returns the ext_id of the
// message that owns the recipient. The update is guarded so it only touches
// a recipient that is CURRENTLY Sent, belongs to a delivery-report-enabled
// message (options.with_delivery_report != false; an absent option is the
// default-true case) and whose stored phone matches phone when phone is
// non-empty - a status report can only transition the exact send that
// requested it. phone carries the bare TP-RA digits (no leading '+'), so the
// stored E.164 form is compared without its plus. When several open
// recipients share a reference (8-bit MR wrap) the OLDEST is picked. No
// match yields ErrNotFound; a duplicate report is a no-op match failure
// because the recipient already left the Sent state.
//
// The reverse lookup runs as one raw statement: recipients have no ext_id,
// so the row is selected through a nested SELECT joined on the message
// options, and the message ext_id is returned with a correlated RETURNING
// subquery.
func (r *Repository) SetRecipientDeliveredByRef(ctx context.Context, refID int, phone string) (string, error) {
	now := time.Now().UTC()
	entry := stateModel{State: smsgateway.ProcessingStateDelivered, At: now}

	var extID string

	err := r.db.NewRaw(`
		UPDATE message_recipients
		SET states = json_insert(states, '$[#]', json(?))
		WHERE id = (
			SELECT mr.id
			FROM message_recipients mr
			JOIN messages m ON m.id = mr.message_id
			WHERE mr.ref_id = ?
				AND json_extract(mr.states, '$[#-1].state') = ?
				AND json_extract(m.options, '$.with_delivery_report') IS NOT 0
				AND (? = '' OR replace(mr.phone, '+', '') = ?)
			ORDER BY mr.id ASC
			LIMIT 1
		)
		RETURNING (SELECT m.ext_id FROM messages m WHERE m.id = message_recipients.message_id)`,
		entry.String(),
		refID,
		smsgateway.ProcessingStateSent,
		phone,
		phone,
	).Scan(ctx, &extID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("set recipient state %s: %w", smsgateway.ProcessingStateDelivered, err)
	}

	return extID, nil
}

// SetRecipientFailedByRef flips the recipient whose stored message reference
// matches refID to Failed with the given reason, mirroring
// SetRecipientDeliveredByRef guards and semantics; it returns the ext_id of
// the owning message.
func (r *Repository) SetRecipientFailedByRef(ctx context.Context, refID int, phone, reason string) (string, error) {
	now := time.Now().UTC()
	entry := stateModel{State: smsgateway.ProcessingStateFailed, At: now}

	var extID string

	err := r.db.NewRaw(`
		UPDATE message_recipients
		SET states = json_insert(states, '$[#]', json(?)),
			error = ?
		WHERE id = (
			SELECT mr.id
			FROM message_recipients mr
			JOIN messages m ON m.id = mr.message_id
			WHERE mr.ref_id = ?
				AND json_extract(mr.states, '$[#-1].state') = ?
				AND json_extract(m.options, '$.with_delivery_report') IS NOT 0
				AND (? = '' OR replace(mr.phone, '+', '') = ?)
			ORDER BY mr.id ASC
			LIMIT 1
		)
		RETURNING (SELECT m.ext_id FROM messages m WHERE m.id = message_recipients.message_id)`,
		entry.String(),
		reason,
		refID,
		smsgateway.ProcessingStateSent,
		phone,
		phone,
	).Scan(ctx, &extID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrNotFound
	case err != nil:
		return "", fmt.Errorf("set recipient state %s: %w", smsgateway.ProcessingStateFailed, err)
	}

	return extID, nil
}

func (r *Repository) updateRecipientState(
	ctx context.Context,
	messageID, phone string,
	state smsgateway.ProcessingState,
	additional func(*bun.UpdateQuery) *bun.UpdateQuery,
) error {
	now := time.Now().UTC()
	entry := stateModel{State: state, At: now}

	query := r.db.NewUpdate().
		Model((*recipientModel)(nil)).
		Set(statesAppendExpr, entry).
		Where(statesLastStateExpr+" <> ?", state).
		Where("message_id = (?)", r.db.NewSelect().Model((*messageModel)(nil)).Column("id").Where("ext_id = ?", messageID)).
		Where("phone = ?", phone)

	if additional != nil {
		query = additional(query)
	}

	_, err := query.Exec(ctx)
	if err != nil {
		return fmt.Errorf("set recipient state %s: %w", state, err)
	}

	return nil
}

func (r *Repository) updateState(
	ctx context.Context,
	id string,
	state smsgateway.ProcessingState,
	where func(*bun.UpdateQuery) *bun.UpdateQuery,
) error {
	now := time.Now().UTC()
	entry := stateModel{State: state, At: now}

	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		query := tx.NewUpdate().
			Model((*messageModel)(nil)).
			Set("state = ?", string(state)).
			Set("updated_at = ?", now).
			Set(statesAppendExpr, entry).
			Where("ext_id = ?", id).
			Where("state not in (?)", bun.List([]string{string(smsgateway.ProcessingStateFailed), string(state)}))
		if where != nil {
			query = where(query)
		}

		upd, queryErr := query.Exec(ctx)
		if queryErr != nil {
			return fmt.Errorf("update message state: %w", queryErr)
		}
		rows, rowsErr := upd.RowsAffected()
		if rowsErr != nil {
			return fmt.Errorf("update message state rows affected: %w", rowsErr)
		}
		if rows == 0 {
			return nil
		}

		_, queryErr = tx.NewUpdate().
			Model((*recipientModel)(nil)).
			Set(statesAppendExpr, entry).
			Where(statesLastStateExpr+" not in (?)", bun.List([]string{string(smsgateway.ProcessingStateFailed), string(state)})).
			Where("message_id = (?)", tx.NewSelect().Model((*messageModel)(nil)).Column("id").Where("ext_id = ?", id)).
			Exec(ctx)
		if queryErr != nil {
			return fmt.Errorf("update recipient state: %w", queryErr)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("update message state: %w", err)
	}

	return nil
}

// DequeueNextPending atomically claims the oldest claimable message (FIFO by
// id) and returns it with its recipients loaded. A message is claimable while
// Pending, or while Processed (an interrupted claim is resumed, so at-least-once
// processing survives restarts). The claim records a Processed state entry and
// bumps updated_at. An empty queue yields ErrNotFound.
//
// The claim runs as one raw statement: the target row is picked by a nested
// SELECT ... ORDER BY ... LIMIT, because UPDATE-level ORDER BY/LIMIT is not
// available in the runtime SQLite build (modernc.org/sqlite compiles without
// SQLITE_ENABLE_UPDATE_DELETE_LIMIT) and bun's UpdateQuery cannot express it
// either.
func (r *Repository) DequeueNextPending(ctx context.Context) (*Message, error) {
	now := time.Now().UTC()
	entry := stateModel{State: smsgateway.ProcessingStateProcessed, At: now}

	var extID string

	err := r.db.NewRaw(`
		UPDATE messages
		SET state = ?, updated_at = ?, states = json_insert(states, '$[#]', json(?))
		WHERE state IN (?, ?)
			AND id = (
				SELECT id FROM messages
				WHERE state IN (?, ?)
				ORDER BY id ASC
				LIMIT 1
			)
		RETURNING ext_id`,
		smsgateway.ProcessingStateProcessed,
		now,
		entry.String(),
		smsgateway.ProcessingStatePending,
		smsgateway.ProcessingStateProcessed,
		smsgateway.ProcessingStatePending,
		smsgateway.ProcessingStateProcessed,
	).Scan(ctx, &extID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("dequeue next pending message: %w", err)
	}

	message, err := r.GetByID(ctx, extID)
	if err != nil {
		return nil, fmt.Errorf("load dequeued message: %w", err)
	}

	return message, nil
}

// orderRecipients makes relation loading deterministic: recipients are
// returned in insertion order.
func orderRecipients(q *bun.SelectQuery) *bun.SelectQuery {
	return q.OrderExpr("id ASC")
}

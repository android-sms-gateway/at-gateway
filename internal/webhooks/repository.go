// Package webhooks holds the webhook persistence domain: registry and queue
// models plus the repository over *bun.DB.
package webhooks

import (
	"context"
	"fmt"
	"time"

	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/uptrace/bun"
)

const (
	orderAscending = "id ASC"
	queueDueOrder  = "next_attempt ASC"
)

// Repository is the bun-backed data access layer for webhook registry and
// queue persistence. It trusts its inputs: validation and ID generation belong
// to the service.
type Repository struct {
	db *bun.DB
}

// NewRepository returns a Repository backed by the given bun database.
func NewRepository(db *bun.DB) *Repository {
	return &Repository{db: db}
}

// Select returns all registry webhooks ordered by insertion order (id ASC);
// an empty registry yields an empty non-nil slice.
func (r *Repository) Select(ctx context.Context) ([]Webhook, error) {
	return r.selectWebhooks(ctx, nil)
}

// SelectByEvent returns the webhooks registered for the given event,
// ordered by insertion order; no matches yield an empty non-nil slice.
func (r *Repository) SelectByEvent(ctx context.Context, event smsgateway.WebhookEvent) ([]Webhook, error) {
	return r.selectWebhooks(ctx, &event)
}

// Replace upserts the webhook by ext_id: a new row is inserted, while an
// existing row keeps its id and created_at and takes the new device_id,
// url and event with a refreshed updated_at. Repeated calls with the same
// ext_id never create a duplicate row.
func (r *Repository) Replace(ctx context.Context, w Webhook) error {
	now := time.Now().UTC()
	model := newWebhookModel(w, now)

	_, err := r.db.NewInsert().
		Model(&model).
		On("CONFLICT (ext_id) DO UPDATE").
		Set("device_id = EXCLUDED.device_id").
		Set("url = EXCLUDED.url").
		Set("event = EXCLUDED.event").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("replace webhook: %w", err)
	}

	return nil
}

// Delete removes the webhook with the given ext_id. Unknown ids are a no-op,
// so delete is idempotent (server parity: no not-found error).
func (r *Repository) Delete(ctx context.Context, extID string) error {
	if _, err := r.db.NewDelete().
		Model((*webhookModel)(nil)).
		Where("ext_id = ?", extID).
		Exec(ctx); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}

	return nil
}

// Enqueue persists a queue item without generating or validating its ID. The
// caller owns the complete item, including its initial status and timestamps.
func (r *Repository) Enqueue(ctx context.Context, item QueueItem) error {
	model := newQueueItemModel(item)
	if _, err := r.db.NewInsert().Model(&model).Exec(ctx); err != nil {
		return fmt.Errorf("enqueue webhook queue item: %w", err)
	}

	return nil
}

// DueItems returns pending and failed items whose next attempt is due at now.
// Items are ordered by next_attempt ASC and limited to the requested batch size;
// an empty queue or non-positive limit returns an empty non-nil slice.
func (r *Repository) DueItems(ctx context.Context, now time.Time, limit int) ([]QueueItem, error) {
	if limit <= 0 {
		return []QueueItem{}, nil
	}

	models := make([]queueItemModel, 0)
	query := r.db.NewSelect().
		Model(&models).
		Where(
			"status IN (?)",
			bun.List([]string{
				string(QueueStatusPending),
				string(QueueStatusFailed),
			}),
		).
		// The parameter relies on Bun's time serialization; TestDueItems_ExactNowIsDue pins this boundary.
		Where("next_attempt <= ?", now).
		OrderExpr(queueDueOrder).
		Limit(limit)

	if err := query.Scan(ctx); err != nil {
		return nil, fmt.Errorf("select due webhook queue items: %w", err)
	}

	items := make([]QueueItem, 0, len(models))
	for i := range models {
		items = append(items, models[i].toDomain())
	}

	return items, nil
}

// MarkProcessing marks a queue item as processing. Unknown IDs are a no-op.
func (r *Repository) MarkProcessing(ctx context.Context, id string) error {
	if _, err := r.db.NewUpdate().
		Model((*queueItemModel)(nil)).
		Set("status = ?", string(QueueStatusProcessing)).
		Where("id = ?", id).
		Exec(ctx); err != nil {
		return fmt.Errorf("mark webhook queue item processing: %w", err)
	}

	return nil
}

// MarkCompleted marks a queue item as completed. Unknown IDs are a no-op.
func (r *Repository) MarkCompleted(ctx context.Context, id string) error {
	if _, err := r.db.NewUpdate().
		Model((*queueItemModel)(nil)).
		Set("status = ?", string(QueueStatusCompleted)).
		Where("id = ?", id).
		Exec(ctx); err != nil {
		return fmt.Errorf("mark webhook queue item completed: %w", err)
	}

	return nil
}

// MarkFailed records a delivery failure, increments the retry count, and sets
// the next attempt and last error on the queue item.
func (r *Repository) MarkFailed(ctx context.Context, id string, nextAttempt time.Time, lastErr string) error {
	if _, err := r.db.NewUpdate().
		Model((*queueItemModel)(nil)).
		Set("status = ?", string(QueueStatusFailed)).
		Set("retry_count = retry_count + 1").
		// The parameter relies on Bun's time serialization; TestQueueLifecycle_TransitionsAndRetry pins this write.
		Set("next_attempt = ?", nextAttempt).
		Set("last_error = ?", lastErr).
		Where("id = ?", id).
		Exec(ctx); err != nil {
		return fmt.Errorf("mark webhook queue item failed: %w", err)
	}

	return nil
}

// MarkPermanentlyFailed marks a queue item as terminal and records its error.
func (r *Repository) MarkPermanentlyFailed(ctx context.Context, id string, lastErr string) error {
	if _, err := r.db.NewUpdate().
		Model((*queueItemModel)(nil)).
		Set("status = ?", string(QueueStatusPermanentlyFailed)).
		Set("last_error = ?", lastErr).
		Where("id = ?", id).
		Exec(ctx); err != nil {
		return fmt.Errorf("mark webhook queue item permanently failed: %w", err)
	}

	return nil
}

// RecoverStuck returns stale processing items to pending. An item is stale when
// its next_attempt is strictly older than threshold; fresh processing items are
// left unchanged.
func (r *Repository) RecoverStuck(ctx context.Context, threshold time.Time) error {
	if _, err := r.db.NewUpdate().
		Model((*queueItemModel)(nil)).
		Set("status = ?", string(QueueStatusPending)).
		Where("status = ?", string(QueueStatusProcessing)).
		// The parameter relies on Bun's time serialization; TestRecoverStuck_ExactThresholdIsRetained pins this boundary.
		Where("next_attempt < ?", threshold).
		Exec(ctx); err != nil {
		return fmt.Errorf("recover stuck webhook queue items: %w", err)
	}

	return nil
}

// Cleanup removes completed and permanently failed items created before cutoff.
// The cutoff is exclusive, matching the Android queue cleanup query.
func (r *Repository) Cleanup(ctx context.Context, cutoff time.Time) error {
	if _, err := r.db.NewDelete().
		Model((*queueItemModel)(nil)).
		Where(
			"status IN (?)",
			bun.List([]string{
				string(QueueStatusCompleted),
				string(QueueStatusPermanentlyFailed),
			}),
		).
		// The parameter relies on Bun's time serialization; TestCleanup_ExactCutoffIsRetained pins this boundary.
		Where("created_at < ?", cutoff).
		Exec(ctx); err != nil {
		return fmt.Errorf("cleanup webhook queue items: %w", err)
	}

	return nil
}

// selectWebhooks loads registry rows ordered by id, optionally filtered by
// event, and maps them onto the domain via toDomain.
func (r *Repository) selectWebhooks(ctx context.Context, event *smsgateway.WebhookEvent) ([]Webhook, error) {
	models := make([]webhookModel, 0)
	query := r.db.NewSelect().
		Model(&models).
		OrderExpr(orderAscending)
	if event != nil {
		query = query.Where("event = ?", *event)
	}

	if err := query.Scan(ctx); err != nil {
		return nil, fmt.Errorf("select webhooks: %w", err)
	}

	result := make([]Webhook, 0, len(models))
	for i := range models {
		result = append(result, models[i].toDomain())
	}

	return result, nil
}

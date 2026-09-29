-- +goose Up
-- +goose StatementBegin
CREATE TABLE webhooks (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ext_id VARCHAR(36) NOT NULL,
    device_id VARCHAR(36) NULL,
    url VARCHAR(256) NOT NULL,
    event VARCHAR(32) NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_webhooks_ext_id ON webhooks (ext_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE webhook_queue (
    id VARCHAR(36) PRIMARY KEY,
    webhook_id VARCHAR(36),
    url TEXT NOT NULL,
    payload TEXT NOT NULL,
    retry_count INTEGER NOT NULL DEFAULT 0,
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    created_at DATETIME,
    next_attempt DATETIME,
    last_error TEXT NULL
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_webhook_queue_status_next_attempt ON webhook_queue (status, next_attempt);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_webhook_queue_status_created_at ON webhook_queue (status, created_at);
-- +goose StatementEnd
---
-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_webhook_queue_status_created_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_webhook_queue_status_next_attempt;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS webhook_queue;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_webhooks_ext_id;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS webhooks;
-- +goose StatementEnd
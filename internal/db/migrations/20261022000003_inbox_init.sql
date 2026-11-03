-- +goose Up
-- +goose StatementBegin
CREATE TABLE inbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id VARCHAR(64) NOT NULL,
    device_id VARCHAR(36) NOT NULL,
    type VARCHAR(32) NOT NULL,
    sender TEXT NOT NULL,
    recipient VARCHAR(64) NULL,
    sim_number INTEGER NULL,
    content TEXT NOT NULL,
    received_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_inbox_device_message ON inbox (device_id, message_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_inbox_received_at ON inbox (received_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_inbox_type_received_at ON inbox (type, received_at DESC);
-- +goose StatementEnd
---
-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_inbox_type_received_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_inbox_received_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_inbox_device_message;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS inbox;
-- +goose StatementEnd
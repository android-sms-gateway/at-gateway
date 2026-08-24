-- +goose Up
-- +goose StatementBegin
CREATE TABLE messages (
    id VARCHAR(36) PRIMARY KEY,
    device_id VARCHAR(21) NOT NULL,
    state VARCHAR(20) NOT NULL,
    is_hashed BOOLEAN NOT NULL DEFAULT 0,
    is_encrypted BOOLEAN NOT NULL DEFAULT 0,
    text_message TEXT NOT NULL,
    sim_number INTEGER NULL,
    with_delivery_report BOOLEAN NOT NULL DEFAULT 0,
    priority INTEGER NOT NULL DEFAULT 0,
    recipients_json TEXT NOT NULL,
    states_json TEXT NOT NULL,
    error_message TEXT NULL,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    processed_at DATETIME NULL,
    sent_at DATETIME NULL,
    failed_at DATETIME NULL
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_messages_state_created ON messages (state, created_at ASC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_messages_created_at ON messages (created_at DESC);
-- +goose StatementEnd
---
-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_messages_created_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_messages_state_created;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS messages;
-- +goose StatementEnd
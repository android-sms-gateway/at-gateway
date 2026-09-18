-- +goose Up
-- +goose StatementBegin
CREATE TABLE messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ext_id VARCHAR(36) NOT NULL,
    device_id VARCHAR(36) NOT NULL,
    type VARCHAR(32) NOT NULL DEFAULT 'Text',
    content TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    is_encrypted BOOLEAN NOT NULL DEFAULT 0,
    is_hashed BOOLEAN NOT NULL DEFAULT 0,
    state VARCHAR(32) NOT NULL,
    options TEXT NOT NULL DEFAULT '{}',
    states TEXT NOT NULL DEFAULT '[]',
    valid_until DATETIME NULL,
    schedule_at DATETIME NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE message_recipients (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ref_id INTEGER NULL,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    phone TEXT NOT NULL,
    states TEXT NOT NULL DEFAULT '[]',
    error TEXT NULL
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_messages_ext_id ON messages (ext_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_messages_state_created ON messages (state, created_at ASC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_messages_created_at ON messages (created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_message_recipients_message_id ON message_recipients (message_id);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_message_recipients_message_phone ON message_recipients (message_id, phone);
-- +goose StatementEnd
---
-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_message_recipients_message_phone;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_message_recipients_message_id;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_messages_created_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_messages_state_created;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_messages_ext_id;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS message_recipients;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS messages;
-- +goose StatementEnd
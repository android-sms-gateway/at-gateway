# Changelog

All notable changes to the AT Gateway are documented in this file.

## [Unreleased]

## [0.3.0] - 2026-11-12

### New Features

#### Settings API

- **Runtime settings** — `GET`, `PUT` and `PATCH` at `/api/v1/settings` read and change gateway settings that persist in SQLite and apply without a restart
- **Ping section** — the `system:ping` heartbeat interval (`ping.interval_seconds`); the stored value overrides `PING__INTERVAL`
- **Webhooks section** — the delivery retry count and the HMAC signing key (`webhooks.signing_key`, write-only); stored values override `WEBHOOKS__RETRY_COUNT` and `WEBHOOKS__SIGNING_KEY`

## [0.2.0] - 2026-11-04

### New Features

#### Inbox API

- **Persistent inbox** — incoming messages are stored in SQLite with an age-based retention window of 30 days, cleaned up once a day (`INBOX__RETENTION`, `INBOX__CLEANUP_INTERVAL`)
- **Inbox listing** — `GET /api/v1/inbox` returns the stored incoming messages with type, date-range and device filters plus pagination (previously answered `501 Not Implemented`)
- **Inbox refresh** — `POST /api/v1/inbox/refresh` replays stored messages as webhook events and answers `202 Accepted` (previously answered `501 Not Implemented`)

## [0.1.0] - 2026-10-20

### New Features

#### Modem Management

- **AT-command bring-up** — boot init sequence `AT`, `ATE0`, `AT+CMEE=1`, `AT+CMGF=0` (PDU) and `AT+CNMI=2,1,0,1,0`, gated on the `AT+CPIN?` SIM READY state, with configurable init and per-command timeouts and automatic reconnect handling
- **Signal quality** — signal quality is polled from the modem and exported through Prometheus metrics (`/metrics`)
- **Retained-entry sweep** — a periodic `AT+CMGL=4` sweep re-processes messages that were stored in the modem but never re-notified, so the modem message store cannot fill up
- **Local device identity** — a stable device ID is minted on the first boot and persisted in `data/storage.json`

#### Outgoing SMS

- **Message queue API** — `POST /api/v1/messages` enqueues a message (`202 Accepted` with a `Location` header), `GET /api/v1/messages` and `GET /api/v1/messages/{id}` list and fetch messages, `DELETE /api/v1/messages/{id}` cancels a pending one

  ```bash
  curl -u sms:change-me -X POST http://127.0.0.1:3000/api/v1/messages \
    -H 'Content-Type: application/json' \
    -d '{"phoneNumbers":["+15551234567"],"textMessage":{"text":"Hello from at-gateway"}}'
  ```

- **Concatenation and validation** — texts are split into GSM-7 / UCS-2 segments up to a configurable cap, phone numbers are validated as E.164 against a default region, and `scheduleAt` / `validUntil` schedule delivery
- **Delivery reports** — modem status reports (`+CDS`) move recipients to `delivered` / `failed` and emit webhook events

#### Webhooks

- **Registry API** — `POST`, `GET` and `DELETE /api/v1/webhooks` register and remove webhook destinations
- **Signed delivery** — payloads are signed with HMAC-SHA256 (`X-Signature`, `X-Timestamp`) and delivered from a persistent queue with exponential retry (`WEBHOOKS__RETRY_COUNT`, `WEBHOOKS__SIGNING_KEY`)
- **Events** — `sms:sent`, `sms:delivered`, `sms:failed`, `sms:cancelled`, `sms:received`, `sms:data-received`, `app:started` and `system:ping`

#### Inbound SMS

- **Inbound pipeline** — `+CMTI` notifications are read with `AT+CMGR`, multi-part messages are reassembled with deduplication, deleted from the modem store with `AT+CMGD`, and each assembled message is emitted as an `sms:received` (or `sms:data-received`) webhook event

#### HTTP API

- **Basic auth** — every `/api/v1` route requires HTTP Basic credentials; the password is generated on the first boot when unset
- **Health and docs** — `/health`, `/health/live`, `/health/ready`, `/health/startup` and `/metrics` need no auth; Swagger UI is served at `/api/v1/docs`

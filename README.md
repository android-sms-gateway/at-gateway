# 📱 AT Gateway

[![Contributors][contributors-shield]][contributors-url]
[![Forks][forks-shield]][forks-url]
[![Stars][stars-shield]][stars-url]
[![Issues][issues-shield]][issues-url]
[![License][license-shield]][license-url]

A daemon that turns a serial AT-command modem (SIM800L class and similar) into an SMS gateway device with an HTTP API, health checks, and Prometheus telemetry. Part of the [SMSGate](https://sms-gate.app) ecosystem.

## 📖 About

The AT Gateway manages the full lifecycle of an AT-command modem connected over a serial port: it runs the boot init sequence, gates on SIM readiness, tracks connection state and signal quality, and exposes the modem's send and receive capabilities over an HTTP API — as a standalone gateway for private/local networks, with no cloud dependency.

## 📚 Table of Contents

- [📱 AT Gateway](#-at-gateway)
  - [📖 About](#-about)
  - [📚 Table of Contents](#-table-of-contents)
  - [⭐ Features](#-features)
  - [📦 Prerequisites](#-prerequisites)
  - [🚀 Getting Started](#-getting-started)
  - [⚙️ Configuration](#️-configuration)
  - [📦 Deployment](#-deployment)
  - [🔌 API Overview](#-api-overview)
  - [📚 Documentation](#-documentation)
  - [🤝 Contributing](#-contributing)
  - [📄 License](#-license)

## ⭐ Features

- AT-command modem bring-up: `AT`, `ATE0`, `+CMEE=1`, `+CMGF=0` (PDU), `+CNMI=2,1,0,1,0`, `+CPIN?` READY gate
- Modem state machine with reconnect handling, per-command timeouts, and a retained-entry store sweep (`AT+CMGL=4`)
- Outgoing SMS: persistent FIFO queue (`202 Accepted` + `Location`), automatic multi-part concatenation (GSM-7 / UCS-2), E.164 phone validation, `scheduleAt`/`validUntil` scheduling, cancellation
- Delivery reports (`+CDS`) mapped to `sms:delivered` / `sms:failed` webhook events
- Incoming SMS pipeline: `+CMTI` → `AT+CMGR` → inbox → `AT+CMGD`, multi-part reassembly, `GET /inbox` and `POST /inbox/refresh` webhook replay
- Webhooks: registry API, HMAC-SHA256 signing (`X-Signature` + `X-Timestamp`), persistent queue with exponential retry; events `sms:sent`, `sms:delivered`, `sms:failed`, `sms:cancelled`, `sms:received`, `sms:data-received`, `app:started`, `system:ping`
- Signal quality polling and Prometheus metrics (`/metrics`)
- Local device identity: a stable device ID minted on first boot and persisted in `data/storage.json`
- Unified HTTP API with HTTP Basic auth and Swagger/OpenAPI docs
- Health endpoints via go-core-fx (`/health`, `/health/live`, `/health/ready`, `/health/startup`)
- Settings API (`GET`/`PUT`/`PATCH /api/v1/settings`) for the ping interval and webhook retry/signing key

### Not supported in this release

- Cloud mode — the gateway is standalone; no registration with a central server
- JWT auth and permission scopes (HTTP Basic only)
- `GET /logs` diagnostics endpoint
- Batch webhook events (`*:batch:*`); individual events only
- Outgoing data SMS and MMS (incoming data SMS is supported)
- Settings sections beyond `ping.*` and `webhooks.*`
- End-to-end encryption
- `DELETE /api/v1/devices/{id}` (returns `501 Not Implemented`)

## 📦 Prerequisites

- Go 1.25+ for building from source
- A serial AT-command modem (e.g. SIM800L) exposed as a serial device
- Serial device permissions for the modem port user

## 🚀 Getting Started

```bash
make deps
make build
AUTH__BASIC__PASSWORD=change-me ./bin/at-gateway
```

The default command is `serve`; the HTTP API listens on `127.0.0.1:3000`.

## ⚙️ Configuration

Configuration is loaded from a `.env` file in the working directory, environment variables (double-underscore nesting), or an optional YAML file via `CONFIG_PATH`; later sources win.

| Variable                 | Default                                    | Description                                                          |
| ------------------------ | ------------------------------------------ | -------------------------------------------------------------------- |
| `HTTP__ADDRESS`          | `127.0.0.1:3000`                           | HTTP API listen address                                              |
| `HTTP__PROXY_HEADER`     | `X-Forwarded-For`                          | Trusted proxy header                                                 |
| `HTTP__PROXIES`          | *(empty)*                                  | Trusted proxies (JSON array, e.g. `["127.0.0.1"]`)                   |
| `HTTP__OPENAPI__ENABLED` | `true`                                     | Enable Swagger UI at `/api/v1/docs`                                  |
| `MODEM__PORT`            | `/dev/ttyUSB0`                             | Serial device of the modem                                           |
| `MODEM__BAUD_RATE`       | `115200`                                   | Serial baud rate                                                     |
| `MODEM__INIT_TIMEOUT`    | `30s`                                      | Modem init timeout                                                   |
| `MODEM__COMMAND_TIMEOUT` | `30s`                                      | Per-command timeout                                                  |
| `DATABASE__URL`          | `sqlite://data/gateway.db?_foreign_keys=1` | SQLite database (messages, inbox, webhooks, settings)                |
| `STORAGE__PATH`          | `data/storage.json`                        | Local JSON storage file                                              |
| `AUTH__BASIC__USERNAME`  | `sms`                                      | Basic auth username                                                  |
| `AUTH__BASIC__PASSWORD`  | *(auto-generated)*                         | Basic auth password (generated and persisted on first boot if unset) |
| `DEVICE__NAME`           | *(empty)*                                  | Device display name                                                  |
| `CONFIG_PATH`            | *(empty)*                                  | Optional YAML config file path                                       |

Full reference: [Custom Gateway Setup](https://docs.sms-gate.app/getting-started/custom-gateway/); every key with its default: [.env.example](.env.example).

## 📦 Deployment

```bash
docker run -d --name at-gateway \
  --device /dev/ttyUSB0 \
  -v /path/to/at-gateway-data:/app/data \
  -e MODEM__PORT=/dev/ttyUSB0 \
  -e HTTP__ADDRESS=0.0.0.0:3000 \
  -e AUTH__BASIC__PASSWORD=change-me \
  -p 3000:3000 \
  ghcr.io/android-sms-gateway/at-gateway:latest
```

`/app/data` must be persisted (it holds the SQLite database `gateway.db`, the device identity in `storage.json`, and the auth password) and writable by UID 1000 (`appuser`), e.g. `chown 1000:1000 /path/to/at-gateway-data`.

## 🔌 API Overview

| Method                        | Path                                                                      | Description                                       |
| ----------------------------- | ------------------------------------------------------------------------- | ------------------------------------------------- |
| GET                           | `/health`, `/health/live`, `/health/ready`, `/health/startup`, `/metrics` | Health probes and Prometheus metrics (no auth)    |
| GET                           | `/api/v1/devices`                                                         | Local device and live SIM info                    |
| GET, POST, DELETE             | `/api/v1/messages`, `/api/v1/messages/{id}`                               | Enqueue (`202` + `Location`), list, fetch, cancel |
| GET, POST                     | `/api/v1/inbox`, `/api/v1/inbox/refresh`                                  | Incoming messages and webhook replay              |
| GET, POST, DELETE, PUT, PATCH | `/api/v1/webhooks`, `/api/v1/webhooks/{id}`, `/api/v1/settings`           | Webhook registry and runtime settings             |

All `/api/v1` routes require HTTP Basic auth (user `sms` by default); interactive reference: Swagger UI at `/api/v1/docs`. Full reference: [OpenAPI docs](https://docs.sms-gate.app/).

## 📚 Documentation

- [Custom Gateway Setup](https://docs.sms-gate.app/getting-started/custom-gateway/)
- [Central docs](https://docs.sms-gate.app/)

## 🤝 Contributing

Contributions are welcome. Open an issue or pull request; follow the repo's `make fmt` / `make lint` / `make test` checks and the [README style guide](https://docs.sms-gate.app/).

## 📄 License

Apache-2.0. See [LICENSE](LICENSE).

<!-- Reference-style badge URLs: style=for-the-badge is mandatory -->
[contributors-shield]: https://img.shields.io/github/contributors/android-sms-gateway/at-gateway?style=for-the-badge
[contributors-url]: https://github.com/android-sms-gateway/at-gateway/graphs/contributors
[forks-shield]: https://img.shields.io/github/forks/android-sms-gateway/at-gateway?style=for-the-badge
[forks-url]: https://github.com/android-sms-gateway/at-gateway/network/members
[stars-shield]: https://img.shields.io/github/stars/android-sms-gateway/at-gateway?style=for-the-badge
[stars-url]: https://github.com/android-sms-gateway/at-gateway/stargazers
[issues-shield]: https://img.shields.io/github/issues/android-sms-gateway/at-gateway?style=for-the-badge
[issues-url]: https://github.com/android-sms-gateway/at-gateway/issues
[license-shield]: https://img.shields.io/github/license/android-sms-gateway/at-gateway?style=for-the-badge
[license-url]: https://github.com/android-sms-gateway/at-gateway/blob/master/LICENSE

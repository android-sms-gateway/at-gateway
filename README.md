# 📱 AT Gateway

[![Contributors][contributors-shield]][contributors-url]
[![Forks][forks-shield]][forks-url]
[![Stars][stars-shield]][stars-url]
[![Issues][issues-shield]][issues-url]
[![License][license-shield]][license-url]

A daemon that turns a serial AT-command modem (SIM800L class and similar) into an SMS gateway device with an HTTP API, health checks, and Prometheus telemetry. Part of the [SMSGate](https://sms-gate.app) ecosystem.

## 📖 About

The AT Gateway manages the full lifecycle of an AT-command modem connected over a serial port: it runs the boot init sequence, gates on SIM readiness, tracks connection state and signal quality, and exposes the device over a small HTTP API.

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

- AT-command modem bring-up: `AT`, `ATE0`, `+CMEE=1`, `+CMGF=1`, `+CNMI=2,1,0,0,0`, `+CPIN?` READY gate
- Modem state machine with reconnect handling and per-command timeouts
- Signal quality polling and Prometheus metrics (`/metrics`)
- Automatic device registration with persisted local storage
- Unified HTTP API with HTTP Basic auth and Swagger/OpenAPI docs
- Health endpoints via go-core-fx (`/health`, `/health/live`, `/health/ready`)

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

Configuration is loaded from environment variables (double-underscore nesting) or an optional YAML file via `CONFIG_PATH`.

| Variable                 | Default             | Description                         |
| ------------------------ | ------------------- | ----------------------------------- |
| `HTTP__ADDRESS`          | `127.0.0.1:3000`    | HTTP API listen address             |
| `HTTP__PROXY_HEADER`     | `X-Forwarded-For`   | Trusted proxy header                |
| `HTTP__PROXIES`          | *(empty)*           | Comma-separated trusted proxies     |
| `HTTP__OPENAPI__ENABLED` | `true`              | Enable Swagger UI at `/api/v1/docs` |
| `MODEM__PORT`            | `/dev/ttyUSB0`      | Serial device of the modem          |
| `MODEM__BAUD_RATE`       | `115200`            | Serial baud rate                    |
| `MODEM__INIT_TIMEOUT`    | `30s`               | Modem init timeout                  |
| `MODEM__COMMAND_TIMEOUT` | `10s`               | Per-command timeout                 |
| `STORAGE__PATH`          | `data/storage.json` | Local JSON storage file             |
| `AUTH__BASIC__USERNAME`  | `sms`               | Basic auth username                 |
| `AUTH__BASIC__PASSWORD`  | *(required)*        | Basic auth password                 |
| `DEVICE__NAME`           | *(empty)*           | Device display name                 |
| `CONFIG_PATH`            | *(empty)*           | Optional YAML config file path      |

Full reference: [Custom Gateway Setup](https://docs.sms-gate.app/getting-started/custom-gateway/).

## 📦 Deployment

```bash
docker run -d --name at-gateway \
  --device /dev/ttyUSB0 \
  -e MODEM__PORT=/dev/ttyUSB0 \
  -e AUTH__BASIC__PASSWORD=change-me \
  -p 3000:3000 \
  ghcr.io/android-sms-gateway/at-gateway:latest
```

## 🔌 API Overview

| Method | Path               | Description                     |
| ------ | ------------------ | ------------------------------- |
| GET    | `/health`          | Health probes (`live`, `ready`) |
| GET    | `/metrics`         | Prometheus metrics              |
| GET    | `/api/v1`          | Service status                  |
| GET    | `/api/v1/devices`  | Registered devices              |
| GET    | `/api/v1/messages` | Message endpoints (scaffolded)  |

All `/api/v1` routes require HTTP Basic auth. Full reference: [OpenAPI docs](https://docs.sms-gate.app/).

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
[license-url]: https://github.com/android-sms-gateway/at-gateway/blob/main/LICENSE

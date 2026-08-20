# at-gateway — AGENTS.md

## Commands
- `make deps` — go mod download
- `make gen` — go generate ./... (swag init)
- `make fmt` — golangci-lint fmt (goimports, golines 120, swaggo formatter)
- `make lint` — golangci-lint run --timeout=5m (very strict config)
- `make test` — go test -race -shuffle=on -count=1 -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./...
- `make coverage` — test + go tool cover text + HTML
- `make build` — binary to bin/$(BINARY_NAME)
- `make air` — live reload (requires air, sets TZ=UTC DEBUG=1)
- `make swagger` — standalone swag fmt + swag init

## Architecture
- **Entrypoint**: main.go — swag //go:generate directive; version injected via ldflags (appVersion, appBuildDate, appReleaseID)
- **CLI**: urfave/cli/v3 in internal/app.go — CLI shell with DefaultCommand "serve"; commands in internal/commands/serve/
- **DI**: Fx graphs live in each command (internal/commands/serve/serve.go)
- **Config**: go-core-fx/config — env vars + optional YAML via CONFIG_PATH env var
- **HTTP**: Fiber at 127.0.0.1:3000, routes under /api/v1, validation middleware at group level
- **Modem**: AT command serial modem (SIM800L and similar) via github.com/warthog618/modem v0.4.0 (pkg at) over internal/modem/port (go.bug.st/serial v1.8.0)
- **Modem internals**: internal/modem/ = Service state machine + Commands; init verbatim AT/ATE0/+CMEE=1/+CMGF=1/+CNMI=2,1,0,0,0/+CPIN? READY; lazy stale-response barrier; single-goroutine init abort on initCtx; port/ kept
- **Modem notes**: lib is ctx-free - per-command timeout via at.WithTimeout(effectiveCmdTimeout), ctx checked between commands; +CMT handler log-only (DEBUG redacted, body never logged; SMS receive = future gsm phase)
- **Modem telemetry (post-migration cleanup 2026-08-20)**: /metrics = 2 gauges (at_gateway_modem_state 0=disconnected,1=connecting,2=ready,3=error - StateBusy REMOVED, error remapped 4->3; at_gateway_modem_signal_quality_percent) + 3 counters (at_gateway_modem_commands_total labels command/status - command=initCommand tag, "" for bare AT/drain, status=ok|error; at_gateway_modem_sms_received_total - +CMT counter only, no PII; at_gateway_modem_reconnects_total - connect attempts) + 1 histogram (at_gateway_modem_command_duration_seconds, observed in Commands.exec() - the single choke point). SMSSentTotal REMOVED (no send path exists); re-add it WITH real send-path wiring in the SMS phase (registration copy in git history 017319a). NewCommands(at *at.AT, metrics *Metrics) - metrics REQUIRED non-nil. Signal telemetry refreshes via a Run ticker: unexported signalRefreshInterval, default 60s, zero = disabled, NO config/env key; Run LOOPS on the tick channel (ticks repeat; only ctx.Done()/at.Closed() exit Run); ticker.Stop on Run exit; SignalUpdate keeps its post-query staleness guard. StateBusy + ErrPortBusy REMOVED (never set / definition-only). CommandTimeout <= 0 maps to the 5s fallback at at.New (library WithTimeout(0) = IMMEDIATE timeout); InitTimeout <= 0 = immediate abort (no fallback) - both per the migration-plan pins, verified by debugger evidence at HEAD (edge tests failed 22/22 pre-fix).

## warthog618/modem Gotchas
- Library is ctx-free: per-command timeout = at.WithTimeout(effectiveCmdTimeout) at at.New (0=immediate,
  negative=unbounded, default 1s - always pass explicitly); ctx params on Commands methods are inert
  per-command, checked between commands only
- Never call library at.Init: it issues ATZ + ATE0 after Escape; for verbatim init parity use
  Command("") = bare AT, Command("E0") = ATE0, etc.
- processReq does NOT drain on deadline (ErrDeadlineExceeded leaves stale lines) - a LAZY drain barrier
  (drain Command("") on the NEXT command call after a timeout) is mandatory; inline drains double the
  abort bound.
- indLoop: registered-prefix handlers run on their OWN goroutine; WithTrailingLine consumes exactly 1
  trailing body line; the indication HEAD line is STILL forwarded after the handler (v0.4.0
  at/at.go:400-415) so a +CMT head can leak into an in-flight command's info[0] (info[0]-readers
  GMI/GMM/GSN; CutPrefix loop parsers immune). Body-never-leaks is the handler guarantee; head-leak is
  a non-forkable library limitation.
- cmdLoop is synchronous: Escape()/later commands queue behind an in-flight Command - Escape cannot
  abort it. Closed() fires on read EOF; AT is terminal - recreate per connect cycle; library has no
  Close() (port.Close is caller-owned).
- Command(cmd) strips the AT prefix itself and drops echo + empty lines; bare AT via Command("").
- Error taxonomy: ErrClosed, ErrDeadlineExceeded, ErrError, CMEError, CMSError - map to domain
  sentinels with preserved tag prefixes.
- go.mod delta: v0.4.0 is go 1.13 unpruned -> go get + go mod tidy adds ~18 go.sum lines (go.mod
  hashes: testify v1.4.0/go.mod, x/sys v0.0.0-20200413/go.mod, go-spew, objx, yaml.v2 x2, kr/pty,
  kr/text, niemeyer/pretty, check.v1 v0.0.0-20200227) and prunes 4 (kr/pretty v0.2.1, check.v1 2019);
  only pkg/errors becomes a new indirect require; assert the invariant (no new require lines beyond the
  direct dep + its imports; no version changes), not a fixed module list.
- Evidence hygiene: plain shasum = SHA-1 (40 hex); sha256 gates need shasum -a 256 (64 hex) - pin the
  algorithm in acceptance checks.
- go list -m -json MODULE@VERSION emits NO Require field - requirement sets come from go mod download
  -json / the cached .mod file.
- Scripted-modem harness: command-keyed/table-driven fake; drain Command("") collides with init row-1
  bare-AT key (serve OK for both); wedged tests need a dedicated silent fake (never responds).
- **Metrics**: Prometheus via fiberfx (auto), per-module counters via promauto

## Module Conventions
- Each package exposes Module(...) fx.Option (withRun bool for modules with background work)
- Handlers registered via group tags: Provide(..., fx.ResultTags(`group:"handlers"`))
- Internal-only deps use fx.Private
- Services with Run(ctx) use `fxutil.RegisterRunnable[*T]()` — conditionally via `withRun` bool
- Per-module named logger: logger.WithNamedLogger("name")
- Config module maps raw Config struct to sub-configs for fiberfx, openapi, modem

## Code Generation
- Swagger docs: go generate ./... (swag init --parseDependency --outputTypes go -g ./main.go -o ./internal/server/docs)
- Output: internal/server/docs/docs.go — DO NOT EDIT
- Live reload config: .air.toml

## Linting & CI
- golangci-lint v2, ~70 linters (exhaustruct, cyclop, gochecknoglobals, etc.)
- golines max length: 120
- Format + lint + test + coverage in that order
- CI: lint + test on push/PR to master in .github/workflows/go.yml
- CI: goreleaser snapshot on PR; release on v* tags
- Stale issues/PRs closed after 14d inactivity

## Testing
- No tests exist yet — add _test.go as needed
- Test flags: -race -shuffle=on -count=1 (disables cache, randomizes order)

## Build & Release
- GoReleaser v2 for linux/windows/darwin, CGO_ENABLED=0
- Go 1.25+
- Docker images pushed to ghcr.io on PR and release

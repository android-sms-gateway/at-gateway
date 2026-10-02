package modem

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/warthog618/modem/at"
	"go.uber.org/zap"
)

// effectiveCmdTimeoutFallback matches the legacy engine read-timeout fallback
// (5s) for CommandTimeout <= 0 configs.
const effectiveCmdTimeoutFallback = 5 * time.Second

// signalRefreshDefaultInterval is the default period of the signal-refresh
// ticker in Run. Zero disables the ticker entirely.
const signalRefreshDefaultInterval = 60 * time.Second

// deliveryReportChannelSize bounds the buffered +CDS delivery-report queue
// between the modem read loop and the messages worker. A full channel makes
// handleCDS drop the report (logged): consumers must drain promptly or a
// status report is lost and the recipient stays Sent.
const deliveryReportChannelSize = 64

// inboundChannelSize bounds the buffered queue of decoded inbound SMS parts
// between the +CMTI handler goroutines and the messages worker. A full channel
// makes the hand-off fail, but the store entry is KEPT so the modem can
// re-notify it (bounded by inboundChannelFullRetries).
const inboundChannelSize = 64

// inboundChannelFullRetries is the PER STORE ENTRY bound on failed hand-offs:
// after this many full-channel drops the entry is deleted anyway and the loss is
// counted as inboundDropChannelFullExhausted. The bound is what keeps the
// recoverable drop from turning into an unbounded re-notification loop.
const inboundChannelFullRetries = 3

// inboundChannelFullRefs bounds how many store entries the full-channel retry
// budget tracks at once. The bound is enforced by EVICTION, not by leaving a
// reference untracked: a new reference evicts the expired budgets first and
// then the oldest live one, so the per-entry attempt bound never depends on the
// order the references were seen in (see spendChannelFull).
const inboundChannelFullRefs = 64

// inboundChannelFullBudgetWindow is how long a store entry's full-channel
// hand-off budget survives between failures, i.e. the period within which a
// re-notification is taken for the SAME message. It is pinned at the default
// reassembly window (5m) because
//
//   - it is ~3x the worst-case time ONE entry can take (96s: 3 x 30s cmdTimeout
//     plus the 6s CMGD backoff), so a genuine re-notification burst of one
//     message never loses its budget mid-way, and
//   - a re-notified part that arrives later can no longer complete its
//     reassembly group anyway, so keeping a modem-level budget for it would be
//     bookkeeping with no effect on delivery.
//
// Past the window a (memory, index) reference is assumed to be a NEW message in
// a reused modem store slot and starts from a fresh budget.
const inboundChannelFullBudgetWindow = 5 * time.Minute

// inboundHandlerLimit caps the number of CONCURRENT +CMTI handlers. The
// library serialises every command over a single cmdLoop, so each handler
// blocks in cmdCh for up to the command timeout; the cap bounds how many
// handlers can queue behind it. Acquisition is non-blocking: saturation drops
// the indication instead of stalling the read loop.
const inboundHandlerLimit = 4

// Drop reasons for at_gateway_modem_inbound_parts_dropped_total.
const (
	inboundDropMalformedHead    = "malformed_head"
	inboundDropNotConnected     = "not_connected"
	inboundDropHandlerSaturated = "handler_saturated"
	inboundDropReadFailed       = "read_failed"
	inboundDropDecodeFailed     = "decode_failed"
	inboundDropChannelFull      = "channel_full"
	inboundDropChannelFullSpent = "channel_full_exhausted"
	inboundDropHandlerPanic     = "handler_panic"
)

// SIM identity of the single slot at-gateway drives, as the app:started payload
// requires it: the slot index is 0-BASED and the SIM number 1-BASED, exactly as
// the client-go SimCard documents them.
const (
	appStartedSlotIndex = 0
	appStartedSimNumber = 1
)

// storedRef identifies one entry of the modem message store. It is the unit of
// the full-channel retry budget: the budget must not leak into a LATER message
// that the modem stores at the same index.
type storedRef struct {
	memory int
	index  int
}

// channelFullBudget is the full-channel hand-off budget of ONE store entry.
type channelFullBudget struct {
	// attempts is how many failed hand-offs the entry has already spent; the
	// drop is retryable while it is below inboundChannelFullRetries.
	attempts int
	// seenAt is when the last failure was booked, i.e. when the budget window
	// (inboundChannelFullBudgetWindow) started. Without it a reference could
	// not be told apart from a later message in the same reused store slot.
	seenAt time.Time
}

// inboundHandoff is the outcome of offering one decoded part to the consumer
// channel, and therefore what happens to its store entry.
type inboundHandoff uint8

const (
	// inboundHandoffDelivered: the consumer owns the part, the store entry is
	// deleted.
	inboundHandoffDelivered inboundHandoff = iota
	// inboundHandoffRetry: the channel was full and the per-entry budget has
	// attempts left, the store entry is KEPT for a re-notification.
	inboundHandoffRetry
	// inboundHandoffDropped: the channel was full and the per-entry budget is
	// spent, so the entry is deleted and the loss is counted.
	inboundHandoffDropped
)

type Service struct {
	config Config

	state    State
	info     Info
	sim      SimInfo
	commands *Commands

	port port.Port
	at   *at.AT

	// signalRefreshInterval is the period of the Run ticker that refreshes
	// the signal telemetry via SignalUpdate. Zero disables the ticker; there
	// is NO config/env key - tests set it in-package.
	signalRefreshInterval time.Duration

	// deliveryReports carries decoded +CDS SMS-STATUS-REPORTs from the
	// modem read loop to consumers. The channel is created ONCE in
	// NewService and survives reconnects: connect() registers a fresh URC
	// handler per connection, but the handler always forwards to this
	// stable channel.
	deliveryReports chan DeliveryReport

	// inboundParts carries decoded SMS-DELIVER parts from the +CMTI handlers
	// to the messages worker, exactly like deliveryReports: created ONCE in
	// NewService and stable across reconnects.
	inboundParts chan InboundPart

	// inboundSlots is the pinned semaphore bounding concurrent +CMTI
	// handlers (capacity inboundHandlerLimit).
	inboundSlots chan struct{}

	// channelFullMu guards channelFullDrops, which the concurrent +CMTI
	// handlers touch on every failed hand-off.
	channelFullMu sync.Mutex

	// channelFullDrops is the per-store-entry budget of full-channel hand-off
	// failures (storedRef -> channelFullBudget), created ONCE in NewService.
	// An entry is removed as soon as its budget is exhausted, as soon as its
	// part reaches the consumer (forgetChannelFull), the moment its window
	// elapsed (sweepChannelFullBudgets, on a failure and on the Run tick) and -
	// to make room at the cap - by evicting the oldest live entry, so the live
	// count never exceeds inboundChannelFullRefs.
	channelFullDrops map[storedRef]channelFullBudget

	// portFactory opens the serial port; defaulted to port.Open. Tests
	// override it to inject scripted modems.
	portFactory func(port.Config) (port.Port, error)

	// webhooksSvc emits the app:started event at the first ready transition
	// (see emitAppStartedOnce).
	webhooksSvc *webhooks.Service

	// appStartedEmitted is the ONCE-PER-PROCESS guard of the app:started
	// emission. It is flipped before the emission runs, so a second ready
	// transition can never deliver a second event.
	appStartedEmitted atomic.Bool

	mu sync.RWMutex

	logger  *zap.Logger
	metrics *Metrics
}

// NewService builds the modem service in the Disconnected state; Run connects
// the serial port and drives the modem until its context is canceled. The
// webhooks service receives the app:started event once the modem is ready, so it
// is REQUIRED (fx fails startup when it is missing); a test that can never reach
// ready state - no modem behind it at all - may pass nil, the emission is
// unreachable there.
//
// Construction also warms the SMS character-set tables (see
// WarmGSMCharsets): the +CMTI handlers decode concurrently, and the upstream
// library builds those tables lazily without synchronization.
func NewService(config Config, loger *zap.Logger, metrics *Metrics, webhooksSvc *webhooks.Service) *Service {
	WarmGSMCharsets()

	return &Service{
		config: config,

		state: StateDisconnected,
		info: Info{
			Manufacturer: "",
			Model:        "",
			IMEI:         "",
		},
		sim: SimInfo{
			PhoneNumber:       "",
			ICCID:             "",
			Carrier:           "",
			NetworkRegistered: false,
			SignalQuality:     0,
			SignalPercent:     0,
		},
		commands: nil,

		port: nil,
		at:   nil,

		portFactory: port.Open,

		webhooksSvc:       webhooksSvc,
		appStartedEmitted: atomic.Bool{},

		signalRefreshInterval: signalRefreshDefaultInterval,
		deliveryReports:       make(chan DeliveryReport, deliveryReportChannelSize),
		inboundParts:          make(chan InboundPart, inboundChannelSize),
		inboundSlots:          make(chan struct{}, inboundHandlerLimit),
		channelFullMu:         sync.Mutex{},
		channelFullDrops:      make(map[storedRef]channelFullBudget),

		mu: sync.RWMutex{},

		logger:  loger,
		metrics: metrics,
	}
}

// Run connects to the serial port and keeps the modem initialized while ctx
// is live: it refreshes the SIM signal telemetry and reclaims the expired
// full-channel hand-off budgets on a ticker, and returns when the context is
// canceled or the modem read loop exits unexpectedly.
//
// A connect() failure means the modem never became ready, so no app:started
// webhook is emitted: the failure is logged at ERROR level (with the port, for
// the operator) before the error that stops the fx app is returned. The stage
// itself is counted in at_gateway_modem_init_failures_total, so a device that
// never comes up is observable even when the log is missed.
func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("connecting modem",
		zap.String("port", s.config.Port),
		zap.Int("baud", s.config.BaudRate),
	)

	if err := s.connect(ctx); err != nil {
		s.logger.Error("modem never reached ready state; the app:started webhook is not emitted",
			zap.Error(err),
			zap.String("port", s.config.Port),
		)
		return fmt.Errorf("connect: %w", err)
	}

	info := s.Info()
	sim := s.SIM()
	s.logger.Info("modem ready",
		zap.String("manufacturer", info.Manufacturer),
		zap.String("model", info.Model),
		zap.String("imei", info.IMEI),
		zap.String("sim_iccid", sim.ICCID),
		zap.String("carrier", sim.Carrier),
		zap.Int("signal_percent", sim.SignalPercent),
		zap.Bool("network_registered", sim.NetworkRegistered),
	)

	s.mu.RLock()
	at := s.at
	s.mu.RUnlock()

	if at == nil {
		s.disconnect()
		return fmt.Errorf("modem not started: %w", ErrModemNotStarted)
	}

	// Signal telemetry ticker: refreshes SIM signal fields + the
	// SignalQuality gauge every signalRefreshInterval. A nil channel never
	// fires, so zero interval disables the refresh (KISS; no config key).
	var tickCh <-chan time.Time
	if s.signalRefreshInterval > 0 {
		ticker := time.NewTicker(s.signalRefreshInterval)
		defer ticker.Stop()
		tickCh = ticker.C
	}

	// Loop until the ctx is canceled or the modem read loop exits: the
	// signal-refresh tick must NOT exit Run (ticks repeat periodically; only
	// the two exit channels terminate the loop).
	for {
		select {
		case <-ctx.Done():
		case <-at.Closed():
			s.logger.Warn("modem read loop exited unexpectedly")
		case <-tickCh:
			s.SignalUpdate(ctx)
			// Reclaim the full-channel budgets whose window elapsed on the SAME
			// tick: it is the existing periodic lifecycle, so a stale entry is
			// reclaimed even when no further inbound traffic arrives. No new
			// ticker is spent for it.
			s.sweepChannelFullBudgets(time.Now().UTC())
			continue
		}
		break
	}

	s.disconnect()
	return nil
}

func (s *Service) connect(ctx context.Context) error {
	s.setState(StateConnecting)
	s.metrics.ReconnectsTotal.Inc()

	port, err := s.portFactory(port.Config{
		Name:     s.config.Port,
		BaudRate: s.config.BaudRate,
	})
	if err != nil {
		s.metrics.IncInitFailure(initStagePortOpen)
		s.setState(StateError)
		return fmt.Errorf("open port %s: %w", s.config.Port, err)
	}

	// effectiveCmdTimeout: CommandTimeout <= 0 falls back to the legacy 5s
	// read timeout (at.WithTimeout(0) means IMMEDIATE timeout in the
	// library - never pass the raw config value).
	cmdTimeout := s.config.CommandTimeout
	if cmdTimeout <= 0 {
		cmdTimeout = effectiveCmdTimeoutFallback
	}

	at := at.New(port,
		at.WithTimeout(cmdTimeout),
		at.WithIndication("+CMTI:", s.handleCMTI),
		at.WithIndication("+CDS:", s.handleCDS, at.WithTrailingLine),
	)

	commands := NewCommands(at, s.metrics)

	s.mu.Lock()
	s.port = port
	s.at = at
	s.commands = commands
	s.mu.Unlock()

	// initCtx is derived from the Run ctx so shutdown cancellation propagates
	// through initCtx.Done(); Commands.Init checks it before the first row and
	// between rows.
	initCtx, cancel := context.WithTimeout(ctx, s.config.InitTimeout)
	defer cancel()

	initDone := make(chan error, 1)
	go func() {
		initDone <- commands.Init(initCtx)
	}()

	if initErr := <-initDone; initErr != nil {
		s.disconnect()
		s.setState(StateError)
		return fmt.Errorf("init: %w", initErr)
	}

	// Query-path ctx is checked between commands only (the library is
	// ctx-free); per-field failures keep the partial info with a warning.
	info, err := commands.GetModemInfo(ctx)
	if err != nil {
		s.logger.Warn("failed to get modem info", zap.Error(err))
	}

	sim, err := commands.GetSimInfo(ctx)
	if err != nil {
		s.logger.Warn("failed to get SIM info", zap.Error(err))
	}

	s.mu.Lock()
	s.info = info
	s.sim = sim
	s.mu.Unlock()

	s.metrics.SignalQuality.Set(float64(sim.SignalPercent))
	s.setState(StateReady)

	// The app:started event belongs HERE and nowhere else: s.info/s.sim were
	// just populated by GetModemInfo/GetSimInfo, so the SIM snapshot the
	// consumer receives is the one the modem actually reports. An fx OnStart
	// emission would carry an empty SIM instead.
	s.emitAppStartedOnce()

	return nil
}

// emitAppStartedOnce emits the app:started webhook EXACTLY ONCE per process,
// on the first ready transition. connect() is the only producer of StateReady
// and has no reconnect loop, so the once-guard is trivially satisfied today; it
// is kept explicit anyway, because a future reconnect loop must not turn the
// device-up signal into a repeating event.
//
// The emission is fire-and-forget (the webhook service swallows every error) and
// runs AFTER the ready state was set, so it can neither fail nor delay the modem
// startup. A modem that never reaches ready emits nothing at all: the failing
// stage is counted instead (see at_gateway_modem_init_failures_total).
func (s *Service) emitAppStartedOnce() {
	if !s.appStartedEmitted.CompareAndSwap(false, true) {
		return
	}

	sim := s.SIM()
	s.webhooksSvc.EmitAppStarted([]webhooks.SimCardInfo{{
		SlotIndex:   appStartedSlotIndex,
		SimNumber:   appStartedSimNumber,
		PhoneNumber: sim.PhoneNumber,
		CarrierName: sim.Carrier,
		ICCID:       sim.ICCID,
	}})
}

func (s *Service) disconnect() {
	s.mu.Lock()
	port := s.port
	s.port = nil
	s.at = nil
	s.commands = nil
	s.state = StateDisconnected
	s.metrics.ModemState.Set(float64(StateDisconnected))
	s.mu.Unlock()

	// The library AT is terminal after the port EOFs: closing the port closes
	// the library read pipeline, so no explicit at.Close exists. The debug log
	// AFTER Close is the manual-verification evidence for port-closed-once.
	if port != nil {
		_ = port.Close()
		s.logger.Debug("modem disconnected")
	}
}

func (s *Service) setState(st State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	s.metrics.ModemState.Set(float64(st))
}

// State returns the latest modem state under lock.
func (s *Service) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Info returns the last queried modem identity (manufacturer, model, IMEI)
// under lock.
func (s *Service) Info() Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

// SIM returns the last queried SIM details (phone number, ICCID, carrier,
// registration and signal quality) under lock.
func (s *Service) SIM() SimInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sim
}

// SignalUpdate refreshes the SIM signal fields and the SignalQuality gauge
// from +CSQ via Commands.GetSignal. Errors are swallowed (debug log, no-op).
// The POST-QUERY STALENESS GUARD re-checks the Commands identity under lock
// before writing sim/gauge, so a disconnect/reconnect during the query cannot
// leak a stale engine's values into the current one.
//
// SignalUpdate is exported and driven by the Run signal-refresh ticker
// (signalRefreshInterval, 60s default, zero = disabled).
func (s *Service) SignalUpdate(ctx context.Context) {
	s.mu.RLock()
	commands := s.commands
	s.mu.RUnlock()

	if commands == nil {
		return
	}

	quality, percent, err := commands.GetSignal(ctx)
	if err != nil {
		s.logger.Debug("signal update failed", zap.Error(err))
		return
	}

	s.mu.Lock()
	if s.commands != commands {
		s.mu.Unlock()
		return
	}
	s.sim.SignalQuality = quality
	s.sim.SignalPercent = percent
	s.metrics.SignalQuality.Set(float64(percent))
	s.mu.Unlock()
}

// SendSMS sends a PDU-mode SMS through the active Commands handle, mirroring
// SignalUpdate's identity-snapshot pattern: a nil Commands (disconnected or
// still connecting) maps to ErrModemNotStarted. Long texts are sent as
// concatenated multi-part messages; the returned slice carries one message
// reference per accepted part. withDeliveryReport requests a status report
// from the SC for the last part (see Commands.SendSMS). The ctx is INERT per
// command (see Commands.SendSMS). This wrapper is the public send entrypoint
// consumed by the messages worker.
func (s *Service) SendSMS(ctx context.Context, phoneNumber, text string, withDeliveryReport bool) ([]int, error) {
	s.mu.RLock()
	commands := s.commands
	s.mu.RUnlock()

	if commands == nil {
		return nil, fmt.Errorf("send SMS: %w", ErrModemNotStarted)
	}

	return commands.SendSMS(ctx, phoneNumber, text, withDeliveryReport)
}

// DeliveryReports returns the channel carrying decoded +CDS
// SMS-STATUS-REPORTs. The channel is stable across modem reconnects; a
// consumer that is too slow loses reports (handleCDS drops on a full
// channel). Subscribers should read from it until the context of their Run
// loop is canceled.
func (s *Service) DeliveryReports() <-chan DeliveryReport {
	return s.deliveryReports
}

// InboundParts returns the channel carrying decoded SMS-DELIVER parts read
// from the modem message store. The channel is stable across modem
// reconnects; a consumer that is too slow fails a hand-off on a full channel
// (see handleCMTI). The drop is recoverable while the per-entry retry budget
// lasts - the store entry stays readable for a re-notification - and final once
// it is spent. Subscribers should read from it until the context of their
// Run loop is canceled.
func (s *Service) InboundParts() <-chan InboundPart {
	return s.inboundParts
}

// handleCMTI consumes the "+CMTI:" unsolicited notification (head line only,
// no trailing body: the PDU lives in the modem store) and runs the inbound
// route for the referenced entry:
//
//	AT+CMGR -> decode -> hand off -> AT+CMGD
//
// CNMI stays at "+CNMI=2,1,0,1,0" (mt=1: store + emit +CMTI), so this handler
// is the ONLY inbound path; mt=2/+CMT stays rejected because WithTrailingLine
// consumes exactly one body line and multi-line URC bodies are not
// deterministic.
//
// The handler runs on its OWN goroutine (the library's indLoop spawns one
// goroutine per indication), so it must not assume serialization with other
// commands. Every step is therefore defensive:
//
//   - the concurrency cap (inboundHandlerLimit, acquired NON-blocking) keeps
//     the goroutine count bounded; saturation drops the indication and counts
//     reason=handler_saturated - the entry stays stored, so a later
//     re-notification can still deliver it;
//   - a nil Commands (post-disconnect indication) logs and returns;
//   - the decoded part is handed off with a NON-BLOCKING send, so a slow
//     consumer fails the hand-off instead of stalling the read loop. A full
//     channel does NOT delete the store entry: the entry stays readable so the
//     modem can re-notify it, and only the per-entry retry budget
//     (inboundChannelFullRetries, see handOffInboundPart) makes the drop final;
//   - the delete runs AFTER an accepted hand-off, so a persistent delete failure
//     never costs the consumer the message;
//   - recover() keeps a malformed PDU from taking the process down (the
//     handler goroutine is not supervised by Run).
//
// No PII is logged: neither the sender nor the message body ever reaches the
// logs, only the store reference, a drop reason and the decode error.
func (s *Service) handleCMTI(info []string) {
	defer s.recoverInbound("+CMTI")

	if len(info) == 0 {
		s.logger.Debug("inbound SMS dropped: empty +CMTI indication")
		s.metrics.IncInboundPartsDropped(inboundDropMalformedHead)

		return
	}

	memory, index, err := parseStoredReference(info[0])
	if err != nil {
		s.logger.Warn("inbound SMS dropped: unreadable store reference", zap.Error(err))
		s.metrics.IncInboundPartsDropped(inboundDropMalformedHead)

		return
	}

	// Non-blocking acquire: the library serialises commands over a single
	// cmdLoop, so a handler can block in cmdCh for up to CommandTimeout.
	// A saturated handler set must drop the indication, never the read loop.
	select {
	case s.inboundSlots <- struct{}{}:
	default:
		s.logger.Warn("inbound SMS dropped: handler limit reached",
			zap.Int("memory", memory),
			zap.Int("index", index),
		)
		s.metrics.IncInboundPartsDropped(inboundDropHandlerSaturated)

		return
	}
	s.metrics.IncInboundHandlersInflight()
	defer s.metrics.DecInboundHandlersInflight()
	defer func() { <-s.inboundSlots }()

	s.mu.RLock()
	commands := s.commands
	s.mu.RUnlock()
	if commands == nil {
		s.logger.Warn("inbound SMS dropped: modem commands unavailable",
			zap.Int("memory", memory),
			zap.Int("index", index),
		)
		s.metrics.IncInboundPartsDropped(inboundDropNotConnected)

		return
	}

	pduHex, err := commands.ReadStoredMessage(memory, index)
	if err != nil {
		s.logger.Warn("read stored message failed", zap.Error(err))
		s.metrics.IncInboundPartsDropped(inboundDropReadFailed)

		return
	}

	part, err := DecodeInboundSMS(pduHex)
	if err != nil {
		s.logger.Warn("inbound SMS dropped: undecodable stored PDU", zap.Error(err))
		s.metrics.IncInboundPartsDropped(inboundDropDecodeFailed)

		return
	}

	// A full consumer channel keeps the store entry: deleting it would turn a
	// recoverable drop into permanent loss, because a deleted entry can never
	// be re-notified.
	if s.handOffInboundPart(storedRef{memory: memory, index: index}, part, time.Now()) == inboundHandoffRetry {
		return
	}

	if deleteErr := s.deleteStoredMessage(commands, memory, index); deleteErr != nil {
		s.logger.Warn("delete stored message failed: the entry stays in the modem store "+
			"and may be re-notified",
			zap.Error(deleteErr),
			zap.Int("memory", memory),
			zap.Int("index", index),
		)
	}
}

// handOffInboundPart offers the decoded part to the consumer channel and reports
// what has to happen to its store entry. The send is NON-BLOCKING so a slow
// consumer never stalls the modem read loop; a full channel therefore fails the
// hand-off, and the store entry is kept so the modem re-notifies it:
//
//   - inboundHandoffDelivered - the consumer owns the part, the caller deletes
//     the store entry;
//   - inboundHandoffRetry - the channel was full and the per-entry budget
//     (inboundChannelFullRetries) still has attempts left: the caller KEEPS the
//     entry and the drop is counted as channel_full;
//   - inboundHandoffDropped - the budget is spent, so the caller deletes the
//     entry and the loss is counted as channel_full_exhausted. This is the only
//     bounded way the store sheds an entry a dead consumer can never take.
//
// now is the observation time of the drop, so the budget window can be decided
// without a second clock read.
func (s *Service) handOffInboundPart(ref storedRef, part InboundPart, now time.Time) inboundHandoff {
	select {
	case s.inboundParts <- part:
		s.forgetChannelFull(ref)

		return inboundHandoffDelivered
	default:
	}

	if s.spendChannelFull(ref, now) {
		s.logger.Warn(
			"inbound SMS not handed off: consumer channel full, the store entry is kept for a re-notification",
			zap.Int("memory", ref.memory),
			zap.Int("index", ref.index),
		)
		s.metrics.IncInboundPartsDropped(inboundDropChannelFull)

		return inboundHandoffRetry
	}

	s.logger.Warn("inbound SMS dropped: consumer channel full and the hand-off retry budget is spent, "+
		"the store entry is deleted",
		zap.Int("memory", ref.memory),
		zap.Int("index", ref.index),
		zap.Int("retries", inboundChannelFullRetries),
	)
	s.metrics.IncInboundPartsDropped(inboundDropChannelFullSpent)

	return inboundHandoffDropped
}

// spendChannelFull books one full-channel hand-off failure for a store entry and
// reports whether a retry is still allowed.
//
// The budget is bounded twice over: per entry (inboundChannelFullRetries
// attempts) and in tracked entries (inboundChannelFullRefs), so neither the
// re-notification retry loop nor the tracking map can grow without limit.
//
// A budget entry is the unit of a STORE SLOT, not of a message, and its
// lifetime is explicit:
//
//   - an entry is REMOVED as soon as its budget is spent - the loss is already
//     counted as inboundDropChannelFullExhausted, so a spent entry is pure
//     bookkeeping and keeping it is what used to make the map grow forever;
//   - an entry is REMOVED on an accepted hand-off (forgetChannelFull), so a
//     delivered message leaves nothing behind;
//   - an entry whose window elapsed (inboundChannelFullBudgetWindow) is REMOVED
//     the moment it is detected stale, so a slot the modem reused for a NEW
//     message starts from a fresh budget instead of inheriting a dead one. The
//     detection is physical, not a bypass: sweepChannelFullBudgetsLocked drops
//     every stale entry on the next failed hand-off and the Run ticker drops the
//     rest, so a stale entry is never merely ignored (it used to survive until
//     some UNRELATED reference spent a budget and evicted it as a side effect,
//     i.e. never when no further traffic arrived);
//   - at the cap, EVICTION makes room: the expired entries go first, then the
//     oldest live one. The cap therefore costs a reference its remaining
//     attempts instead of leaving the new one untracked, which is what made the
//     attempt bound order- and history-dependent.
func (s *Service) spendChannelFull(ref storedRef, now time.Time) bool {
	s.channelFullMu.Lock()
	defer s.channelFullMu.Unlock()

	// Reclaim FIRST: a stale entry is deleted here rather than being bypassed by
	// the lookup below, so every stale budget in the map leaves at this first
	// real opportunity - whether or not its own slot is ever referenced again.
	s.sweepChannelFullBudgetsLocked(now)

	budget, tracked := s.channelFullDrops[ref]
	switch {
	case tracked:
		budget.attempts++
		budget.seenAt = now
	default:
		// A never-seen reference, or a slot the sweep just reclaimed after the
		// window: a full budget, made room for by eviction if the cap is reached.
		budget = channelFullBudget{attempts: 1, seenAt: now}
		if len(s.channelFullDrops) >= inboundChannelFullRefs {
			s.evictOldestChannelFullBudgetLocked()
		}
	}

	s.channelFullDrops[ref] = budget
	if budget.attempts >= inboundChannelFullRetries {
		delete(s.channelFullDrops, ref)

		return false
	}

	return true
}

// sweepChannelFullBudgets removes every full-channel budget whose window
// (inboundChannelFullBudgetWindow) elapsed. It is the INDEPENDENT reclamation of
// the stale entries: it needs no new hand-off failure, so an entry whose slot is
// never re-referenced is reclaimed on the next Run tick instead of lingering.
//
// The map is hard-capped at inboundChannelFullRefs entries, so the worst case - no
// traffic AND no tick - is a bounded, fixed, few-hundred-byte cost, not an
// unbounded leak. That bound is a deliberate trade against adding a dedicated
// goroutine for a 64-entry map.
func (s *Service) sweepChannelFullBudgets(now time.Time) {
	s.channelFullMu.Lock()
	defer s.channelFullMu.Unlock()

	s.sweepChannelFullBudgetsLocked(now)
}

// sweepChannelFullBudgetsLocked deletes every window-elapsed budget. The caller
// holds channelFullMu.
func (s *Service) sweepChannelFullBudgetsLocked(now time.Time) {
	for ref, budget := range s.channelFullDrops {
		if now.Sub(budget.seenAt) >= inboundChannelFullBudgetWindow {
			delete(s.channelFullDrops, ref)
		}
	}
}

// evictOldestChannelFullBudgetLocked frees room in the tracking map for one more
// entry by dropping the OLDEST live budget, so the cap is always enforced by
// eviction and a new reference is never left without a budget. The expired
// entries are already gone: spendChannelFull sweeps them before it needs
// room, which is what makes the order EXPIRED FIRST, oldest live second. The
// caller holds channelFullMu.
func (s *Service) evictOldestChannelFullBudgetLocked() {
	var (
		oldest    storedRef
		oldestAt  time.Time
		oldestSet bool
	)
	for ref, budget := range s.channelFullDrops {
		if !oldestSet || budget.seenAt.Before(oldestAt) {
			oldest, oldestAt, oldestSet = ref, budget.seenAt, true
		}
	}
	if oldestSet {
		delete(s.channelFullDrops, oldest)
	}
}

// forgetChannelFull drops the retry budget of a store entry whose part reached
// the consumer, so the budget never leaks into a later message that the modem
// stores at the same index.
func (s *Service) forgetChannelFull(ref storedRef) {
	s.channelFullMu.Lock()
	defer s.channelFullMu.Unlock()

	delete(s.channelFullDrops, ref)
}

// deleteStoredMessage removes a read store entry with bounded exponential
// retries (see CmgdConfig). After the last failure the store-command failure
// counter is incremented and the error is returned; the caller only warns,
// because the entry remaining in the store is what makes a re-notification
// possible.
//
// RESIDUAL RISK: a modem that keeps rejecting +CMGD fills its own store. The
// retries and the counter make the condition observable and alertable, they
// do not solve it.
func (s *Service) deleteStoredMessage(commands *Commands, memory, index int) error {
	attempts := max(s.config.Cmgd.RetryAttempts, 1)

	var err error
	for attempt := range attempts {
		if attempt > 0 {
			time.Sleep(s.config.Cmgd.RetryBackoff << (attempt - 1))
		}
		if err = commands.DeleteStoredMessage(memory, index); err == nil {
			return nil
		}
	}

	s.metrics.IncStoreCommandFailure(storeCommandLabel)

	return fmt.Errorf("delete stored message: %w", err)
}

// recoverInbound turns a panic in an unsolicited-notification handler into a
// dropped part. The library spawns the handler goroutine itself, so without
// this guard a malformed PDU would take the whole process down instead of
// failing one message.
//
// It MUST be deferred directly (defer s.recoverInbound(...)): recover() only
// returns a value when the function calling it is itself the deferred one, so
// the recover call cannot live one frame deeper.
func (s *Service) recoverInbound(handler string) {
	if recovered := recover(); recovered != nil {
		s.reportInboundPanic(handler, recovered)
	}
}

// reportInboundPanic logs and counts a swallowed handler panic.
func (s *Service) reportInboundPanic(handler string, recovered any) {
	s.logger.Error("inbound SMS handler panic",
		zap.String("handler", handler),
		zap.Any("panic", recovered),
	)
	s.metrics.IncInboundPartsDropped(inboundDropHandlerPanic)
}

// handleCDS consumes "+CDS:" unsolicited notifications (SMS-STATUS-REPORTs,
// enabled by +CNMI <ds>=1) - the head line plus ONE trailing PDU body line
// via WithTrailingLine - so they cannot leak into in-flight command
// responses as info lines. The head is "+CDS: <length>" (PDU mode, no PII);
// the body is the hex PDU of the status report.
//
// Decoded reports are forwarded to the DeliveryReports channel with a
// NON-BLOCKING send: the handler runs on its own goroutine (indLoop spawns
// go ind.handler(n)) but must never stall the read loop. A full channel or a
// malformed/undecodable body is dropped with a log entry - a dropped report
// leaves the recipient in Sent state until the SC retries or the operator
// intervenes. The body (info[1]) is the recipient phone number in encoded
// form and is never logged.
func (s *Service) handleCDS(info []string) {
	defer s.recoverInbound("+CDS")

	if len(info) < 2 { //nolint:mnd // head + one body line
		s.logger.Debug("delivery report dropped: incomplete +CDS indication")
		return
	}

	report, err := DecodeDeliveryReport(info[1])
	if err != nil {
		s.logger.Debug("delivery report dropped: undecodable +CDS body", zap.Error(err))
		return
	}

	select {
	case s.deliveryReports <- report:
	default:
		s.logger.Warn("delivery report dropped: consumer channel full")
		if s.metrics != nil {
			s.metrics.DeliveryReportsDroppedTotal.Inc()
		}
	}
}

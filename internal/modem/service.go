package modem

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"github.com/warthog618/modem/at"
	"go.uber.org/zap"
)

// effectiveCmdTimeoutFallback matches the legacy engine read-timeout fallback
// (5s) for CommandTimeout <= 0 configs.
const effectiveCmdTimeoutFallback = 5 * time.Second

// signalRefreshDefaultInterval is the default period of the signal-refresh
// ticker in Run. Zero disables the ticker entirely.
const signalRefreshDefaultInterval = 60 * time.Second

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

	// portFactory opens the serial port; defaulted to port.Open. Tests
	// override it to inject scripted modems.
	portFactory func(port.Config) (port.Port, error)

	mu sync.RWMutex

	logger  *zap.Logger
	metrics *Metrics
}

func NewService(config Config, loger *zap.Logger, metrics *Metrics) *Service {
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

		signalRefreshInterval: signalRefreshDefaultInterval,

		mu: sync.RWMutex{},

		logger:  loger,
		metrics: metrics,
	}
}

func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("connecting modem",
		zap.String("port", s.config.Port),
		zap.Int("baud", s.config.BaudRate),
	)

	if err := s.connect(ctx); err != nil {
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
		at.WithIndication("+CMT:", s.handleCMT, at.WithTrailingLine),
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
	return nil
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

func (s *Service) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *Service) Info() Info {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

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

// cmtRedacted is the deterministic no-PII marker logged when a +CMT head line
// carries no parseable SCTS timestamp.
const cmtRedacted = "<redacted>"

// handleCMT consumes "+CMT:" unsolicited notifications (the head line plus ONE
// trailing body line via WithTrailingLine) so they cannot leak into in-flight
// command responses as info lines. It is LOG-ONLY: inbound SMS is discarded,
// same as the legacy drain(); the SMS phase replaces this handler.
//
// Only the SCTS timestamp is logged, at DEBUG level - the full head line
// carries the sender number and the body (info[1]) is PII. When the SCTS is
// absent or malformed, the fixed marker <redacted> is logged.
//
// The handler runs on its OWN goroutine (indLoop spawns go ind.handler(n)),
// so it must not be assumed to be serialized with commands.
//
// MULTI-LINE BODIES: WithTrailingLine collects exactly one body line;
// remaining lines may leak into an in-flight command response as info
// (parity with the pre-handler degradation). Do NOT claim deterministic
// multi-line URC handling. Note: the v0.4.0 indLoop forwards the indication
// HEAD line upstream even after consuming it (the range-loop `continue` does
// not skip the final out <- line), so a mid-command +CMT may still surface
// its head line in the in-flight response; the trailing BODY lines are the
// deterministic part this handler consumes.
func (s *Service) handleCMT(info []string) {
	if len(info) == 0 {
		return
	}

	// Counter-only inbound-SMS telemetry: messages remain discarded and the
	// log stays DEBUG-redacted; no content/PII is captured.
	s.metrics.SMSReceivedTotal.Inc()

	s.logger.Debug("modem SMS received (ignored)", zap.String("scts", redactCMTHead(info[0])))
}

// redactCMTHead reduces a "+CMT:" head line to its SCTS timestamp - the only
// field free of sender PII. A line without the prefix, or with an absent or
// malformed timestamp, yields the fixed marker <redacted>.
func redactCMTHead(head string) string {
	_, payload, found := strings.Cut(head, "+CMT:")
	if !found {
		return cmtRedacted
	}
	// Fields: <oa>,<alpha>,<scts>. The SCTS itself contains a comma
	// (yy/MM/dd,hh:mm:ss+zz), so it is everything after the second comma.
	fields := strings.SplitN(payload, ",", 3) //nolint:mnd // oa, alpha, scts
	if len(fields) < 3 {                      //nolint:mnd // oa, alpha, scts
		return cmtRedacted
	}
	scts := strings.Trim(strings.TrimSpace(fields[2]), `"`)
	if _, err := time.Parse("06/01/02,15:04:05-07", scts); err != nil {
		return cmtRedacted
	}

	return scts
}

//nolint:testpackage // in-package tests use the portFactory seam to inject scripted modems.
package modem

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"github.com/warthog618/modem/at"
	"go.uber.org/zap"
)

// TestService_InitTimeoutZero_ImmediateAbort pins the InitTimeout <= 0 edge:
// the abort select fires BEFORE the first row, so no command reaches the modem.
func TestService_InitTimeoutZero_ImmediateAbort(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireAT: {"OK"}})
	cfg := testConfig()
	cfg.InitTimeout = 0
	svc := newTestService(cfg, m, newTestMetrics())

	cancel, done := runService(t, svc)
	defer cancel()

	err := waitRun(t, done, 2*time.Second)
	if err == nil {
		t.Fatal("expected init abort error, got nil")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	if got := m.receivedCommands(); len(got) != 0 {
		t.Fatalf("commands = %v, want none (abort before first row)", got)
	}
}

// TestService_InitAbort_BetweenRows exercises the abort select BETWEEN rows:
// row 1 succeeds after the initCtx fired mid-row, and the sequence stops
// before row 2 is written, within the bound InitTimeout + effectiveCmdTimeout
// + 1s slack.
func TestService_InitAbort_BetweenRows(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireAT: {"OK"}})
	m.delay(wireAT, 400*time.Millisecond)
	cfg := testConfig()
	cfg.InitTimeout = 300 * time.Millisecond
	cfg.CommandTimeout = time.Second
	svc := newTestService(cfg, m, newTestMetrics())

	start := time.Now()
	cancel, done := runService(t, svc)
	defer cancel()

	err := waitRun(t, done, 3*time.Second)
	if err == nil {
		t.Fatal("expected init abort error, got nil")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	bound := cfg.InitTimeout + cfg.CommandTimeout
	if elapsed := time.Since(start); elapsed > bound+time.Second {
		t.Fatalf("elapsed = %v, want <= %v (bound + 1s slack)", elapsed, bound+time.Second)
	}
	if got := m.receivedCommands(); !slicesEqual(got, []string{wireAT}) {
		t.Fatalf("commands = %v, want [AT] only (abort before row 2)", got)
	}
	if got := svc.State(); got != StateError {
		t.Fatalf("state = %v, want %v", got, StateError)
	}
}

// TestService_InitAbort_RowDeadlineBound pins the abort bound on a silent
// modem: the in-flight row runs to its own per-command deadline, then the
// sequence aborts within InitTimeout + effectiveCmdTimeout + 1s slack.
func TestService_InitAbort_RowDeadlineBound(t *testing.T) {
	m := newScriptedModem(nil) // silent: never responds to row 1
	cfg := testConfig()
	cfg.InitTimeout = 600 * time.Millisecond
	cfg.CommandTimeout = 300 * time.Millisecond
	svc := newTestService(cfg, m, newTestMetrics())

	start := time.Now()
	cancel, done := runService(t, svc)
	defer cancel()

	err := waitRun(t, done, 5*time.Second)
	if err == nil {
		t.Fatal("expected init error, got nil")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	bound := cfg.InitTimeout + cfg.CommandTimeout
	if elapsed := time.Since(start); elapsed > bound+time.Second {
		t.Fatalf("elapsed = %v, want <= %v (bound + 1s slack)", elapsed, bound+time.Second)
	}
	if got := svc.State(); got != StateError {
		t.Fatalf("state = %v, want %v", got, StateError)
	}
}

// TestService_Init_FastFailOnRowError pins the fast-fail contract: an
// immediate row error returns in < min(2s, InitTimeout/3), NOT after the full
// InitTimeout (guards the silent long-timeout regression).
func TestService_Init_FastFailOnRowError(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireAT: {"ERROR"}})
	cfg := testConfig()
	cfg.InitTimeout = 6 * time.Second
	svc := newTestService(cfg, m, newTestMetrics())

	start := time.Now()
	cancel, done := runService(t, svc)
	defer cancel()

	err := waitRun(t, done, 2*time.Second)
	if err == nil {
		t.Fatal("expected init error, got nil")
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("elapsed = %v, want < min(2s, InitTimeout/3)", elapsed)
	}
}

// TestService_Run_CtxCancelDuringBlockedInit is the pinned RunCtx-cancel boot
// test: CommandTimeout=1s, InitTimeout=30s, silent fake, cancel ~0.1s after
// the first write; Run returns in < 2s and the port is closed exactly once.
// The in-flight row is inert to ctx (library has no context support), so the
// row runs to its own 1s deadline before the abort propagates.
func TestService_Run_CtxCancelDuringBlockedInit(t *testing.T) {
	m := newScriptedModem(nil) // silent: row 1 never responds
	cfg := testConfig()
	cfg.InitTimeout = 30 * time.Second
	cfg.CommandTimeout = time.Second
	metrics := newTestMetrics()
	svc := newTestService(cfg, m, metrics)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- svc.Run(ctx) }()

	<-m.firstWrite
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected connect error after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after RunCtx cancel")
	}
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("Run returned in %v, want < 2s", elapsed)
	}
	if got := m.rw.closedCount(); got != 1 {
		t.Fatalf("port closed %d times, want exactly 1", got)
	}
	if got := svc.State(); got != StateError {
		t.Fatalf("state = %v, want %v", got, StateError)
	}
}

// TestService_PortFactorySeam proves connect() routes through the injectable
// factory with the config mapping intact (no real serial device needed).
func TestService_PortFactorySeam(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	var gotName string
	var gotBaud int
	svc := NewService(testConfig(), zap.NewNop(), newTestMetrics())
	svc.portFactory = func(cfg port.Config) (port.Port, error) {
		gotName = cfg.Name
		gotBaud = cfg.BaudRate

		return m.rw, nil
	}

	cancel, done := runService(t, svc)
	defer cancel()

	waitForState(t, svc)

	if gotName != testPortName || gotBaud != testBaudRate {
		t.Fatalf("factory config = %q/%d, want %q/%d", gotName, gotBaud, testPortName, testBaudRate)
	}

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestService_Disconnect_ExactlyOnce locks the exactly-once close contract:
// Run is the sole lifecycle owner; after Run returns, disconnect has
// nil-outed at/port/commands under s.mu and closed the fake port exactly
// once; a second disconnect is idempotent (no second Close).
func TestService_Disconnect_ExactlyOnce(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	svc := newTestService(testConfig(), m, newTestMetrics())

	cancel, done := runService(t, svc)
	waitForState(t, svc)
	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := m.rw.closedCount(); got != 1 {
		t.Fatalf("port closed %d times, want exactly 1", got)
	}

	// Nil-out under lock: nothing escapes after disconnect.
	svc.mu.RLock()
	a, p, commands := svc.at, svc.port, svc.commands
	svc.mu.RUnlock()
	if a != nil || p != nil || commands != nil {
		t.Fatalf("at/port/commands = %v/%v/%v, want all nil after disconnect", a, p, commands)
	}
	if got := svc.State(); got != StateDisconnected {
		t.Fatalf("state = %v, want %v", got, StateDisconnected)
	}

	// Idempotent: a second disconnect must not close the port again.
	svc.disconnect()
	if got := m.rw.closedCount(); got != 1 {
		t.Fatalf("port closed %d times after second disconnect, want still 1", got)
	}
}

// TestService_Run_ClosedMidCommand_DisconnectOnce: a port EOF while a
// command is in flight closes the library AT (terminal): the in-flight
// Command returns ErrClosed, Run's select fires on at.Closed(), and
// disconnect() closes the port exactly once. The ErrClosed path is asserted
// via SignalUpdate's swallowed error (observed through the captured debug
// log: "signal update failed" carries the tag-wrapped at.ErrClosed).
func TestService_Run_ClosedMidCommand_DisconnectOnce(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	core := newCapturingCore()
	svc := NewService(testConfig(), zap.New(core), newTestMetrics())
	svc.portFactory = func(port.Config) (port.Port, error) { return m.rw, nil }

	cancel, done := runService(t, svc)
	defer cancel()
	waitForState(t, svc)

	// Drain the buffered boot commands (incl. the boot's +CSQ from
	// GetSimInfo) so waitForCommand below matches SignalUpdate's +CSQ only.
	m.receivedCommands()

	// Post-boot +CSQ goes SILENT so the command is provably in flight when
	// the port dies (a nil response table entry writes nothing).
	m.setResponse(wireCSQ, nil)

	sigDone := make(chan struct{})
	go func() {
		svc.SignalUpdate(context.Background())
		close(sigDone)
	}()

	waitForCommand(t, m, wireCSQ, 2*time.Second)

	// Unplug mid-command: engine read EOF -> library Closed (terminal). The
	// service's disconnect() is the ONLY Close caller.
	m.hangup()

	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case <-sigDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SignalUpdate did not return after close")
	}

	// The in-flight command's error IS at.ErrClosed: SignalUpdate swallows it
	// (debug log), and the captured entry proves the ErrClosed path end-to-end.
	l := waitForLog(t, core, "signal update failed")
	if err := logErrorField(l, "error"); err == nil || !errors.Is(err, at.ErrClosed) {
		t.Fatalf("swallowed signal error = %v, want at.ErrClosed", err)
	}

	if got := m.rw.closedCount(); got != 1 {
		t.Fatalf("port closed %d times, want exactly 1", got)
	}
	if got := svc.State(); got != StateDisconnected {
		t.Fatalf("state = %v, want %v", got, StateDisconnected)
	}
}

// TestService_Run_ReconnectTwoCycles drives connect() twice manually through
// two sequential Run cycles (the portFactory seam returns a FRESH modem per
// cycle). This locks the NO-AUTO-RECONNECT parity: Run returns after
// Closed() and fx keeps serving a dead modem; a reconnect is a NEW Run with
// a fresh at.New (the library AT is terminal after close, so reaching Ready
// twice proves the fresh handle). Both cycles must reach Ready and close
// their port exactly once.
func TestService_Run_ReconnectTwoCycles(t *testing.T) {
	svc := NewService(testConfig(), zap.NewNop(), newTestMetrics())
	var modems []*scriptedModem
	svc.portFactory = func(port.Config) (port.Port, error) {
		m := newScriptedModem(defaultBootResponses())
		modems = append(modems, m)

		return m.rw, nil
	}

	for cycle := 1; cycle <= 2; cycle++ {
		cancel, done := runService(t, svc)
		waitForState(t, svc)
		cancel()
		if err := waitRun(t, done, 2*time.Second); err != nil {
			t.Fatalf("Run cycle %d: %v", cycle, err)
		}
	}

	if got := len(modems); got != 2 {
		t.Fatalf("portFactory called %d times, want 2 (fresh at.New per connect)", got)
	}
	for i, m := range modems {
		if got := m.rw.closedCount(); got != 1 {
			t.Fatalf("cycle %d port closed %d times, want exactly 1", i+1, got)
		}
	}
	if got := svc.State(); got != StateDisconnected {
		t.Fatalf("state = %v, want %v", got, StateDisconnected)
	}
}

// TestService_Run_DiscriminatingShutdown is the DISCRIMINATING SHUTDOWN
// test: InitTimeout=30s > effectiveCmdTimeout=10s; row 1 (AT) responds OK
// after ~1.75s; rows 2+ are SILENT. The fake SIGNALS ROW-1-WRITTEN and the
// test cancels RunCtx ~100ms later, so cancellation provably lands after the
// first row was issued (no degenerate before-first-row abort). The abort
// select BETWEEN rows then fires as soon as row 1 completes (~1.8s) - Run
// returns in < 5s. WITHOUT the between-row select, row 2 (silent) would
// stall to its own 10s deadline and Run would return at ~11.5s (test fails).
// initCtx derives from RunCtx (context.WithTimeout(RunCtx, InitTimeout)), so
// RunCtx cancellation propagates through initCtx.Done() - a raw timer not
// derived from RunCtx fails this test.
func TestService_Run_DiscriminatingShutdown(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireAT: {"OK"}}) // rows 2+ silent (defaultOK=false)
	m.delay(wireAT, 1750*time.Millisecond)
	cfg := testConfig()
	cfg.InitTimeout = 30 * time.Second
	cfg.CommandTimeout = 10 * time.Second
	svc := newTestService(cfg, m, newTestMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	<-m.firstWrite // row 1 written: cancellation provably lands after it
	time.Sleep(100 * time.Millisecond)
	cancel()

	err := waitRun(t, done, 5*time.Second)
	if err == nil {
		t.Fatal("expected init abort error after RunCtx cancel")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	if got := m.receivedCommands(); !slicesEqual(got, []string{wireAT}) {
		t.Fatalf("commands = %v, want [AT] only (abort before row 2)", got)
	}
	if got := m.rw.closedCount(); got != 1 {
		t.Fatalf("port closed %d times, want exactly 1", got)
	}
	if got := svc.State(); got != StateError {
		t.Fatalf("state = %v, want %v", got, StateError)
	}
}

// TestService_Run_WedgedModemBound is the WEDGED-MODEM BOUND test. PINNED
// CONFIG: InitTimeout=5s < effectiveCmdTimeout=10s, so the init timer fires
// MID-command. The wedged fake is a DEDICATED SILENT fake that NEVER
// responds to row 1 - the shared command-keyed harness (which serves
// bare-AT->OK for the drain key) cannot double as it. Assertions: elapsed >=
// effectiveCmdTimeout (the in-flight row ran to its own deadline), elapsed <=
// bound + 1s (bound = InitTimeout + effectiveCmdTimeout = 15s), single Close,
// StateError, and no drain issued during the abort.
func TestService_Run_WedgedModemBound(t *testing.T) {
	m := newWedgedModem()
	cfg := testConfig()
	cfg.InitTimeout = 5 * time.Second
	cfg.CommandTimeout = 10 * time.Second
	svc := newTestService(cfg, m, newTestMetrics())

	start := time.Now()
	cancel, done := runService(t, svc)
	defer cancel()

	err := waitRun(t, done, 20*time.Second)
	if err == nil {
		t.Fatal("expected init error on wedged modem")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	elapsed := time.Since(start)
	if elapsed < cfg.CommandTimeout {
		t.Fatalf(
			"elapsed = %v, want >= effectiveCmdTimeout (%v): row ran to its own deadline",
			elapsed,
			cfg.CommandTimeout,
		)
	}
	bound := cfg.InitTimeout + cfg.CommandTimeout
	if elapsed > bound+time.Second {
		t.Fatalf("elapsed = %v, want <= %v (bound + 1s slack)", elapsed, bound+time.Second)
	}
	if got := m.rw.closedCount(); got != 1 {
		t.Fatalf("port closed %d times, want exactly 1", got)
	}
	if got := svc.State(); got != StateError {
		t.Fatalf("state = %v, want %v", got, StateError)
	}
	if got := m.receivedCommands(); !slicesEqual(got, []string{wireAT}) {
		t.Fatalf("commands = %v, want [AT] only (no drain during abort)", got)
	}
}

// TestService_SignalUpdate_ConcurrentDisconnect exercises the POST-QUERY
// STALENESS GUARD under -race: SignalUpdate is in flight on one Commands
// instance when disconnect() nil-outs s.commands under s.mu. The guard must
// prevent the stale call from writing sim/gauge, and the concurrent lock
// traffic (SignalUpdate reads, disconnect writes, SIM reads, gauge sets) must
// be race-free.
func TestService_SignalUpdate_ConcurrentDisconnect(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireCSQ: {"+CSQ: 12,0", "OK"}})
	m.delay(wireCSQ, 300*time.Millisecond)
	metrics := newTestMetrics()
	svc := newTestService(testConfig(), m, metrics)

	svc.mu.Lock()
	svc.commands = newTestCommands(t, m, 2*time.Second)
	svc.port = m.rw
	svc.mu.Unlock()

	done := make(chan struct{})
	go func() {
		svc.SignalUpdate(context.Background())
		close(done)
	}()

	select {
	case cmd := <-m.received:
		if cmd != wireCSQ {
			t.Fatalf("modem received %q, want %q", cmd, wireCSQ)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("modem did not receive AT+CSQ")
	}

	svc.disconnect() // concurrent with the in-flight query

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SignalUpdate did not return after disconnect")
	}

	if sim := svc.SIM(); sim.SignalQuality != 0 || sim.SignalPercent != 0 {
		t.Fatalf("signal = %d/%d, stale call must not write sim", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 0, "signal quality")
	if got := m.rw.closedCount(); got != 1 {
		t.Fatalf("port closed %d times, want exactly 1", got)
	}
	if got := svc.State(); got != StateDisconnected {
		t.Fatalf("state = %v, want %v", got, StateDisconnected)
	}
}

// TestService_ReconnectsTotal_CountsConnectCycles locks the connect-attempt
// counter: each connect() entry increments ReconnectsTotal (attempt
// telemetry; no auto-reconnect exists - two manual Run cycles must count 2).
func TestService_ReconnectsTotal_CountsConnectCycles(t *testing.T) {
	metrics := newTestMetrics()
	svc := NewService(testConfig(), zap.NewNop(), metrics)
	var modems []*scriptedModem
	svc.portFactory = func(port.Config) (port.Port, error) {
		m := newScriptedModem(defaultBootResponses())
		modems = append(modems, m)

		return m.rw, nil
	}

	for cycle := 1; cycle <= 2; cycle++ {
		cancel, done := runService(t, svc)
		waitForState(t, svc)
		cancel()
		if err := waitRun(t, done, 2*time.Second); err != nil {
			t.Fatalf("Run cycle %d: %v", cycle, err)
		}
	}

	if got := counterValue(t, metrics.ReconnectsTotal); got != 2 {
		t.Fatalf("ReconnectsTotal = %v, want 2 (one per connect cycle)", got)
	}
}

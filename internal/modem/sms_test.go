//nolint:testpackage // in-package tests exercise Commands internals (lazy drain barrier state).
package modem

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/warthog618/modem/at"
)

func newTestCommands(t *testing.T, m *scriptedModem, cmdTimeout time.Duration) *Commands {
	t.Helper()
	a := at.New(m.rw, at.WithTimeout(cmdTimeout))
	t.Cleanup(m.close)

	return NewCommands(a, newTestMetrics())
}

// TestCommands_Init_ByteOrder pins the verbatim init parity matrix on the
// library: Command("") -> AT, Command("E0") -> ATE0, +CMEE=1, +CMGF=1,
// +CNMI=2,1,0,0,0, +CPIN? READY gate - exact order, no ATZ.
func TestCommands_Init_ByteOrder(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireAT:        {"OK"},
		wireATE0:      {"OK"},
		wireCMEE:      {"OK"},
		wireCMGF:      {"OK"},
		wireCNMI:      {"OK"},
		wireCPINQuery: {"+CPIN: READY", "OK"},
	})
	commands := newTestCommands(t, m, time.Second)

	if err := commands.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	want := []string{wireAT, wireATE0, wireCMEE, wireCMGF, wireCNMI, wireCPINQuery}
	if got := m.receivedCommands(); !slicesEqual(got, want) {
		t.Fatalf("commands = %v, want %v", got, want)
	}
}

// TestCommands_Init_ErrorMapped pins the row error mapping: a plain ERROR row
// fails with ErrInitFailed and the preserved "tag (cmd):" prefix.
func TestCommands_Init_ErrorMapped(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireCMGF: {"ERROR"}})
	m.defaultOK = true
	commands := newTestCommands(t, m, time.Second)

	err := commands.Init(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInitFailed) {
		t.Fatalf("error = %v, want ErrInitFailed", err)
	}
	if !strings.HasPrefix(err.Error(), "text mode (AT+CMGF=1):") {
		t.Fatalf("error %q does not carry the tag prefix", err)
	}

	got := m.receivedCommands()
	if len(got) != 4 || !slicesEqual(got[:4], []string{wireAT, wireATE0, wireCMEE, wireCMGF}) {
		t.Fatalf("commands = %v, want first 4 rows only", got)
	}
}

// TestCommands_Init_CMEErrorMapped pins the +CME ERROR row mapping onto
// ErrInitFailed.
func TestCommands_Init_CMEErrorMapped(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireCNMI: {"+CME ERROR: 3"}})
	m.defaultOK = true
	commands := newTestCommands(t, m, time.Second)

	err := commands.Init(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInitFailed) {
		t.Fatalf("error = %v, want ErrInitFailed", err)
	}
	if !strings.HasPrefix(err.Error(), "SMS routing (AT+CNMI=2,1,0,0,0):") {
		t.Fatalf("error %q does not carry the tag prefix", err)
	}
}

// TestCommands_Init_DeadlineMapped pins the deadline mapping: a row that
// exceeds the per-command timeout fails with ErrModemTimeout.
func TestCommands_Init_DeadlineMapped(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireAT: {"OK"}, wireATE0: {"OK"}})
	m.delay(wireATE0, 500*time.Millisecond)
	commands := newTestCommands(t, m, 200*time.Millisecond)

	err := commands.Init(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	if !strings.HasPrefix(err.Error(), "echo off (ATE0):") {
		t.Fatalf("error %q does not carry the tag prefix", err)
	}
}

// TestCommands_Init_CtxCancelBeforeFirstRow pins the before-first-row abort
// select: an expired ctx fails with ErrModemTimeout and no command is issued.
func TestCommands_Init_CtxCancelBeforeFirstRow(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireAT: {"OK"}})
	commands := newTestCommands(t, m, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := commands.Init(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	if got := m.receivedCommands(); len(got) != 0 {
		t.Fatalf("commands = %v, want none", got)
	}
}

// TestCommands_Init_CtxInertPerCommand pins the between-rows abort select and
// the INERT-per-command contract: cancellation lands while row 1 is in
// flight, row 1 still completes, and the sequence stops at the next boundary.
func TestCommands_Init_CtxInertPerCommand(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireAT: {"OK"}})
	// Delay row 1 so the cancel provably lands while row 1 is in flight:
	// without the delay, the OK may complete before the test goroutine's
	// cancel(), letting row 2's non-blocking select win the race and issue
	// ATE0 (pre-existing flake; the between-row select then aborts later).
	m.delay(wireAT, 50*time.Millisecond)
	commands := newTestCommands(t, m, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- commands.Init(ctx) }()

	<-m.firstWrite
	cancel()

	err := <-done
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	if got := m.receivedCommands(); !slicesEqual(got, []string{wireAT}) {
		t.Fatalf("commands = %v, want [AT] only (abort before row 2)", got)
	}
}

// TestCommands_Barrier_DrainAfterTimeout is the lazy drain barrier contract:
// a timed-out command leaves stale lines; the next command call issues one
// drain (bare AT) before its own rows and completes cleanly.
func TestCommands_Barrier_DrainAfterTimeout(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireATE0:      {"OK"},
		wireCMEE:      {"OK"},
		wireCMGF:      {"OK"},
		wireCNMI:      {"OK"},
		wireCPINQuery: {"OK"},
	})
	m.defaultOK = false
	commands := newTestCommands(t, m, 200*time.Millisecond)

	// Row 1 (bare AT) never responds: first Init fails on its own deadline.
	err := commands.Init(context.Background())
	if err == nil || !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("first Init error = %v, want ErrModemTimeout", err)
	}

	// The next call drains (bare AT) before its own rows and completes cleanly.
	m.setResponse(wireAT, []string{"OK"})
	secondErr := commands.Init(context.Background())
	if secondErr != nil {
		t.Fatalf("second Init after drain: %v", secondErr)
	}

	// First Init wrote row 1 only; second Init drains (bare AT) then rows 1-6:
	// AT (row 1), AT (drain), AT (row 1 again), ATE0, ...
	want := []string{wireAT, wireAT, wireAT, wireATE0, wireCMEE, wireCMGF, wireCNMI, wireCPINQuery}
	if got := m.receivedCommands(); !slicesEqual(got, want) {
		t.Fatalf("commands = %v, want %v (drain = second bare AT)", got, want)
	}
}

// TestCommands_Barrier_DrainTimeoutPersists pins drain-failure persistence:
// a timed-out drain fails the whole call, SKIPS the real command, and keeps
// the pending-drain state; only a non-deadline drain outcome clears it.
func TestCommands_Barrier_DrainTimeoutPersists(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireATE0:      {"OK"},
		wireCMEE:      {"OK"},
		wireCMGF:      {"OK"},
		wireCNMI:      {"OK"},
		wireCPINQuery: {"OK"},
	})
	m.defaultOK = false
	commands := newTestCommands(t, m, 200*time.Millisecond)

	if err := commands.Init(context.Background()); err == nil || !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("first Init error = %v, want ErrModemTimeout", err)
	}

	// Drain still has no "AT" response: it times out, the whole call fails,
	// row 1 is skipped, and pendingDrain persists.
	initErr := commands.Init(context.Background())
	if initErr == nil || !errors.Is(initErr, ErrModemTimeout) {
		t.Fatalf("second Init error = %v, want ErrModemTimeout (drain timeout)", initErr)
	}
	if got := m.receivedCommands(); !slicesEqual(got, []string{wireAT, wireAT}) {
		t.Fatalf("commands = %v, want [AT, AT] (row skipped after failed drain)", got)
	}

	// Third call: drain succeeds (response now scripted), init completes.
	m.setResponse(wireAT, []string{"OK"})
	drainErr := commands.Init(context.Background())
	if drainErr != nil {
		t.Fatalf("third Init after drain: %v", drainErr)
	}
}

// TestCommands_GetSignal_ErrClosedMidCommand proves the Closed()-mid-command
// contract at the Commands layer (AC3): a read EOF while the +CSQ command is
// in flight makes the library Command return at.ErrClosed (the AT terminal
// closes), which the query path propagates as a tag-prefix wrap (no sentinel
// mapping).
func TestCommands_GetSignal_ErrClosedMidCommand(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireCSQ: {"+CSQ: 12,0", "OK"}})
	m.setResponse(wireCSQ, nil) // silent: the command stays in flight
	commands := newTestCommands(t, m, 2*time.Second)

	done := make(chan error, 1)
	go func() { _, _, err := commands.GetSignal(context.Background()); done <- err }()

	waitForCommand(t, m, wireCSQ, 2*time.Second)
	m.hangup() // read EOF mid-command: library AT closes (terminal)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, at.ErrClosed) {
			t.Fatalf("error = %v, want at.ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetSignal did not return after hangup")
	}
}

// TestCommands_Barrier_ConcurrentCalls exercises the lazy drain barrier mutex
// under -race: Commands serializes barrier-check + drain + command execution
// with c.mu (the library cmdCh serializes only the wire), so concurrent
// GetSimInfo + GetSignal calls must be race-free and both succeed (the
// harness is command-keyed, so wire interleaving is harmless).
func TestCommands_Barrier_ConcurrentCalls(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	m.setResponse(wireCSQ, []string{"+CSQ: 12,0", "OK"})
	commands := newTestCommands(t, m, 2*time.Second)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		sim, err := commands.GetSimInfo(context.Background())
		if err != nil {
			t.Errorf("GetSimInfo: %v", err)
		}
		if sim.PhoneNumber != fixturePhoneNumber || sim.SignalQuality != 12 || sim.SignalPercent != 38 {
			t.Errorf("GetSimInfo result = %+v, want populated sim", sim)
		}
	}()
	go func() {
		defer wg.Done()
		quality, percent, err := commands.GetSignal(context.Background())
		if err != nil {
			t.Errorf("GetSignal: %v", err)
		}
		if quality != 12 || percent != 38 {
			t.Errorf("signal = %d/%d, want 12/38", quality, percent)
		}
	}()
	wg.Wait()
}

// TestCommands_ExecMetrics_InitAndQuery locks the exec() telemetry on the
// init and query paths: every command increments CommandsTotal once with
// command=tag + status=ok|error and observes CommandDuration exactly once.
// GetModemInfo is called twice - the second call fails at GMM (ERROR row) to
// exercise the error label; GMI succeeds both times.
func TestCommands_ExecMetrics_InitAndQuery(t *testing.T) {
	metrics := newTestMetrics()
	m := newScriptedModem(map[string][]string{
		wireAT:        {"OK"},
		wireATE0:      {"OK"},
		wireCMEE:      {"OK"},
		wireCMGF:      {"OK"},
		wireCNMI:      {"OK"},
		wireCPINQuery: {"+CPIN: READY", "OK"},
		wireGMI:       {fixtureManufacturer, "OK"},
	})
	m.defaultOK = true
	a := at.New(m.rw, at.WithTimeout(2*time.Second))
	t.Cleanup(m.close)
	commands := NewCommands(a, metrics)

	if err := commands.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := commands.GetModemInfo(context.Background()); err != nil {
		t.Fatalf("GetModemInfo: %v", err)
	}

	// Error path: GMM fails on the second GetModemInfo call.
	m.setResponse(wireGMM, []string{fixtureErrorLine})
	if _, err := commands.GetModemInfo(context.Background()); err == nil {
		t.Fatal("expected GetModemInfo error on GMM ERROR row")
	}

	// Init rows: one ok per tag.
	for tag, want := range map[string]float64{
		"test":           1,
		"echo off":       1,
		"verbose errors": 1,
		"text mode":      1,
		"SMS routing":    1,
		"SIM PIN":        1,
	} {
		if got := counterVecValue(t, metrics.CommandsTotal, tag, "ok"); got != want {
			t.Fatalf("CommandsTotal{command=%q,status=ok} = %v, want %v", tag, got, want)
		}
	}
	// Query path: GMI ok twice; GMM ok once then error; GSN ok once.
	if got := counterVecValue(t, metrics.CommandsTotal, "+GMI", "ok"); got != 2 {
		t.Fatalf("CommandsTotal{+GMI,ok} = %v, want 2", got)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "+GMM", "ok"); got != 1 {
		t.Fatalf("CommandsTotal{+GMM,ok} = %v, want 1", got)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "+GMM", "error"); got != 1 {
		t.Fatalf("CommandsTotal{+GMM,error} = %v, want 1", got)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "+GSN", "ok"); got != 1 {
		t.Fatalf("CommandsTotal{+GSN,ok} = %v, want 1", got)
	}
	// The "error" status must never appear on an init row tag.
	if got := counterVecValue(t, metrics.CommandsTotal, "test", "error"); got != 0 {
		t.Fatalf("CommandsTotal{test,error} = %v, want 0", got)
	}

	// CommandDuration: one observation per command = total counter increments
	// (11 commands: 6 init rows + 3 + 2 query rows).
	if got := histogramCount(t, metrics.CommandDuration); got != 11 {
		t.Fatalf("CommandDuration observations = %d, want 11", got)
	}
}

// TestCommands_ExecMetrics_Drain locks the drain-path telemetry: a timed-out
// command is counted with status=error, and the next call's drain (bare AT)
// is counted with command="" before the recovered command's own ok.
func TestCommands_ExecMetrics_Drain(t *testing.T) {
	metrics := newTestMetrics()
	m := newScriptedModem(map[string][]string{wireCSQ: {"+CSQ: 12,0", "OK"}})
	m.setResponse(wireCSQ, nil) // silent: the +CSQ command times out
	a := at.New(m.rw, at.WithTimeout(200*time.Millisecond))
	t.Cleanup(m.close)
	commands := NewCommands(a, metrics)

	if _, _, err := commands.GetSignal(context.Background()); err == nil {
		t.Fatal("expected +CSQ timeout error")
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "+CSQ", "error"); got != 1 {
		t.Fatalf("CommandsTotal{+CSQ,error} = %v, want 1 (timed-out command)", got)
	}

	// Recovered modem: drain (bare AT) succeeds, then +CSQ returns clean.
	m.setResponse(wireCSQ, []string{"+CSQ: 12,0", "OK"})
	m.setResponse(wireAT, []string{"OK"})
	quality, percent, err := commands.GetSignal(context.Background())
	if err != nil {
		t.Fatalf("GetSignal after drain: %v", err)
	}
	if quality != 12 || percent != 38 {
		t.Fatalf("signal = %d/%d, want 12/38", quality, percent)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "", "ok"); got != 1 {
		t.Fatalf("CommandsTotal{command=\"\",ok} = %v, want 1 (drain)", got)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "+CSQ", "ok"); got != 1 {
		t.Fatalf("CommandsTotal{+CSQ,ok} = %v, want 1", got)
	}
	// Duration observed for the failed +CSQ, the drain and the retried +CSQ.
	if got := histogramCount(t, metrics.CommandDuration); got != 3 {
		t.Fatalf("CommandDuration observations = %d, want 3", got)
	}
}

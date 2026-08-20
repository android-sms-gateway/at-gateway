//nolint:testpackage // in-package tests use the portFactory seam to inject scripted modems.
package modem

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestService_Run_InitByteOrderAndState locks the boot path: exact init byte
// order (AT, ATE0, +CMEE=1, +CMGF=1, +CNMI=2,1,0,0,0, +CPIN? READY), Ready
// state + gauge (2), signal gauge 0, health provider ready, and the
// Disconnected state + gauge (0) after Run returns.
func TestService_Run_InitByteOrderAndState(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	metrics := newTestMetrics()
	svc := newTestService(testConfig(), m, metrics)

	cancel, done := runService(t, svc)
	defer cancel()

	waitForState(t, svc)

	if got := svc.State(); got != StateReady {
		t.Fatalf("state = %v, want %v", got, StateReady)
	}
	assertGauge(t, metrics.ModemState, float64(StateReady), "state")
	assertGauge(t, metrics.SignalQuality, 0, "signal quality")
	assertHealthReady(t, svc)

	want := []string{wireAT, wireATE0, wireCMEE, wireCMGF, wireCNMI, wireCPINQuery}
	got := m.receivedCommands()
	if len(got) < len(want) {
		t.Fatalf("received %d commands, want at least %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := svc.State(); got != StateDisconnected {
		t.Fatalf("state after disconnect = %v, want %v", got, StateDisconnected)
	}
	assertGauge(t, metrics.ModemState, float64(StateDisconnected), "state")
}

// TestService_Run_InitError locks the init failure path: a row ERROR fails the
// boot with a tag-prefixed error and leaves the service in StateError (gauge 3).
func TestService_Run_InitError(t *testing.T) {
	m := newScriptedModem(map[string][]string{wireAT: {"ERROR"}})
	metrics := newTestMetrics()
	svc := newTestService(testConfig(), m, metrics)

	cancel, done := runService(t, svc)
	defer cancel()

	err := waitRun(t, done, 5*time.Second)
	if err == nil {
		t.Fatal("expected init error, got nil")
	}
	if !strings.Contains(err.Error(), "test (AT):") {
		t.Fatalf("error %q does not carry the row tag prefix", err)
	}
	if got := svc.State(); got != StateError {
		t.Fatalf("state = %v, want %v", got, StateError)
	}
	assertGauge(t, metrics.ModemState, float64(StateError), "state")
	if got := m.receivedCommands(); !slicesEqual(got, []string{wireAT}) {
		t.Fatalf("commands = %v, want [AT]", got)
	}
}

// TestService_Run_CPINNotReady locks the +CPIN? READY gate: a non-READY status
// fails the boot with ErrSIMNotReady.
func TestService_Run_CPINNotReady(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireAT:        {"OK"},
		wireATE0:      {"OK"},
		wireCMEE:      {"OK"},
		wireCMGF:      {"OK"},
		wireCNMI:      {"OK"},
		wireCPINQuery: {"+CPIN: SIM PIN", "OK"},
	})
	svc := newTestService(testConfig(), m, newTestMetrics())

	cancel, done := runService(t, svc)
	defer cancel()

	err := waitRun(t, done, 5*time.Second)
	if err == nil {
		t.Fatal("expected SIM PIN error, got nil")
	}
	if !errors.Is(err, ErrSIMNotReady) {
		t.Fatalf("error = %v, want ErrSIMNotReady", err)
	}
	if got := svc.State(); got != StateError {
		t.Fatalf("state = %v, want %v", got, StateError)
	}
}

// TestService_Run_EdgeInitRowTimeout is the pinned edge test: CommandTimeout=0
// (5s fallback on both engines), InitTimeout=10s, init row 1 delayed ~4s -
// boot succeeds on both engines within the 5s binding bound.
func TestService_Run_EdgeInitRowTimeout(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	m.delay(wireAT, 4*time.Second)
	cfg := testConfig()
	cfg.CommandTimeout = 0
	svc := newTestService(cfg, m, newTestMetrics())

	start := time.Now()
	cancel, done := runService(t, svc)
	defer cancel()

	waitForState(t, svc)

	elapsed := time.Since(start)
	if elapsed < 3*time.Second || elapsed > 6*time.Second {
		t.Fatalf("elapsed = %v, want ~4s (5s binding bound on both engines)", elapsed)
	}

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

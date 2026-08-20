//nolint:testpackage // in-package tests use the portFactory seam and direct Commands access.
package modem

// PHASE-2 TELEMETRY SUITE: GetSignal semantics, the query-path deadline
// sentinel, the interleaved +CMT leak (set-membership; the consume handler
// closes it in Phase 3), the query-path CommandTimeout<=0 edge delta (the
// stay-green carve-out for the Phase-1 instant-fail lock), and observable-only
// SignalUpdate tests (SIM() fields + SignalQuality gauge, no wire sequence
// assertions).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fixtureCMTHead is a realistic +CMT head line. Under CNMI=2,1,0,0,0 a
// mid-command +CMT leaks into the in-flight response as an unknown info line
// (the Phase-3 consume handler closes this); info[0]-reading parsers
// (GMI/GMM/GSN) may yield it instead of the clean value.
const fixtureCMTHead = "+CMT: \"+1234567890\",\"\",\"26/08/16,12:00:00+14\""

// TestService_Run_CommandTimeoutZero_PopulatedWithin5s is the QUERY-PATH EDGE
// DELTA test (stay-green carve-out). With CommandTimeout <= 0 the OLD engine's
// per-command [context.WithTimeout] expired instantly -> instant fail +
// empty Info/SIM fields (Phase-1 lock assertion); the NEW engine maps the
// config to at.WithTimeout(5s) at at.New (effectiveCmdTimeout fallback), so
// connect() populates Info/SIM within the 5s binding bound. The Phase-1
// instant-fail lock is superseded by this delta.
func TestService_Run_CommandTimeoutZero_PopulatedWithin5s(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	metrics := newTestMetrics()
	cfg := testConfig()
	cfg.CommandTimeout = 0
	svc := newTestService(cfg, m, metrics)

	start := time.Now()
	cancel, done := runService(t, svc)
	defer cancel()

	waitForState(t, svc)

	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("elapsed = %v, want within the 5s fallback bound", elapsed)
	}

	info := svc.Info()
	sim := svc.SIM()
	if info.Manufacturer != fixtureManufacturer {
		t.Fatalf("Manufacturer = %q, want %q (populated within 5s delta)", info.Manufacturer, fixtureManufacturer)
	}
	if info.Model != fixtureModel {
		t.Fatalf("Model = %q, want %q", info.Model, fixtureModel)
	}
	if info.IMEI != fixtureIMEI {
		t.Fatalf("IMEI = %q, want %q", info.IMEI, fixtureIMEI)
	}
	if sim.PhoneNumber != fixturePhoneNumber {
		t.Fatalf("PhoneNumber = %q, want %q", sim.PhoneNumber, fixturePhoneNumber)
	}
	if sim.ICCID != fixtureICCID {
		t.Fatalf("ICCID = %q, want %q", sim.ICCID, fixtureICCID)
	}
	if sim.Carrier != fixtureCarrier {
		t.Fatalf("Carrier = %q, want %q", sim.Carrier, fixtureCarrier)
	}

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestCommands_GetSignal locks the +CSQ happy path and boundary values:
// rssi 0 -> 0%, rssi 12 -> 38%, rssi 31 -> 100%.
func TestCommands_GetSignal(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		quality int
		percent int
	}{
		{name: "typical", line: "+CSQ: 12,0", quality: 12, percent: 38},
		{name: "zero", line: "+CSQ: 0,0", quality: 0, percent: 0},
		{name: "max", line: "+CSQ: 31,99", quality: 31, percent: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newScriptedModem(map[string][]string{wireCSQ: {tt.line, "OK"}})
			commands := newTestCommands(t, m, 2*time.Second)

			quality, percent, err := commands.GetSignal(context.Background())
			if err != nil {
				t.Fatalf("GetSignal: %v", err)
			}
			if quality != tt.quality || percent != tt.percent {
				t.Fatalf("signal = %d/%d, want %d/%d", quality, percent, tt.quality, tt.percent)
			}
		})
	}
}

// TestCommands_GetSignal_Error locks the error paths: a malformed +CSQ line,
// an absent +CSQ line, and a command error all return (0, 0, error).
func TestCommands_GetSignal_Error(t *testing.T) {
	tests := []struct {
		name string
		key  string
		rows []string
	}{
		{name: "malformed rssi", key: wireCSQ, rows: []string{"+CSQ: abc,0", "OK"}},
		{name: "absent +CSQ line", key: wireCSQ, rows: []string{"OK"}},
		{name: "command error", key: wireCSQ, rows: []string{fixtureErrorLine}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newScriptedModem(map[string][]string{tt.key: tt.rows})
			commands := newTestCommands(t, m, 2*time.Second)

			quality, percent, err := commands.GetSignal(context.Background())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if quality != 0 || percent != 0 {
				t.Fatalf("signal = %d/%d, want 0/0 on error", quality, percent)
			}
		})
	}
}

// TestCommands_GetSimInfo_SwallowMalformedCSQ locks the NO-COUPLING rule: for
// the SAME malformed +CSQ condition that makes GetSignal return an error,
// GetSimInfo's swallow path returns (0, 0, nil).
func TestCommands_GetSimInfo_SwallowMalformedCSQ(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireCNUM: {fixtureCNUMLine, "OK"},
		wireCCID: {fixtureICCID, "OK"},
		wireCOPS: {fixtureCOPSLine, "OK"},
		wireCSQ:  {"+CSQ: abc,0", "OK"},
		wireCREG: {fixtureCREGLine, "OK"},
	})
	commands := newTestCommands(t, m, 2*time.Second)

	sim, err := commands.GetSimInfo(context.Background())
	if err != nil {
		t.Fatalf("GetSimInfo: %v", err)
	}
	if sim.SignalQuality != 0 || sim.SignalPercent != 0 {
		t.Fatalf(
			"signal = %d/%d, want 0/0 (swallow path, no coupling with GetSignal)",
			sim.SignalQuality,
			sim.SignalPercent,
		)
	}
	if sim.PhoneNumber != fixturePhoneNumber {
		t.Fatalf("PhoneNumber = %q, later fields unaffected", sim.PhoneNumber)
	}
}

// TestCommands_GetModemInfo_DeadlineSentinel locks the query-path deadline
// mapping: a per-command ErrDeadlineExceeded surfaces as an ErrModemTimeout
// wrapped in the field prefix (log parity with the legacy at.ErrTimeout
// text), with partial info.
func TestCommands_GetModemInfo_DeadlineSentinel(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireGMI: {fixtureManufacturer, "OK"},
		wireGMM: {fixtureModel, "OK"},
		wireGSN: {fixtureIMEI, "OK"},
	})
	m.delay(wireGMI, 500*time.Millisecond)
	commands := newTestCommands(t, m, 200*time.Millisecond)

	info, err := commands.GetModemInfo(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.HasPrefix(err.Error(), "manufacturer:") {
		t.Fatalf("error %q does not carry the field prefix", err)
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout (deadline sentinel)", err)
	}
	if info != (Info{}) {
		t.Fatalf("info = %+v, want zeroed (failed at the first field)", info)
	}
}

// TestCommands_GetModemInfo_InterleavedCMT documents the URC INTERIM leak
// (Phase-2 only; Phase 3 registers the consume handler): with CNMI=2,1,0,0,0
// active and no handler, a mid-command +CMT line leaks into the in-flight
// response as an unknown info line. GMI/GMM/GSN read info[0], so their
// results are asserted as SET MEMBERSHIP {clean value, +CMT head line}
// (corruption is timing-dependent). The CutPrefix loop parsers
// (CNUM/COPS/CSQ/CREG) are immune: exact assertions. This Commands-level
// fixture has NO handler registered (newTestCommands), so the leak class is
// exercised directly; the Phase-3 handler consumes the trailing BODY lines,
// while the v0.4.0 indLoop still forwards the head line upstream - the
// set-membership assertion remains valid with the handler too.
func TestCommands_GetModemInfo_InterleavedCMT(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireGMI:  {fixtureCMTHead, fixtureManufacturer, "OK"},
		wireGMM:  {fixtureCMTHead, fixtureModel, "OK"},
		wireGSN:  {fixtureCMTHead, fixtureIMEI, "OK"},
		wireCNUM: {fixtureCMTHead, fixtureCNUMLine, "OK"},
		wireCCID: {fixtureICCID, "OK"},
		wireCOPS: {fixtureCMTHead, fixtureCOPSLine, "OK"},
		wireCSQ:  {fixtureCMTHead, fixtureCSQLine, "OK"},
		wireCREG: {fixtureCMTHead, fixtureCREGLine, "OK"},
	})
	commands := newTestCommands(t, m, 2*time.Second)

	info, err := commands.GetModemInfo(context.Background())
	if err != nil {
		t.Fatalf("GetModemInfo: %v", err)
	}
	if info.Manufacturer != fixtureManufacturer && info.Manufacturer != fixtureCMTHead {
		t.Fatalf("Manufacturer = %q, want one of {%q, %q}", info.Manufacturer, fixtureManufacturer, fixtureCMTHead)
	}
	if info.Model != fixtureModel && info.Model != fixtureCMTHead {
		t.Fatalf("Model = %q, want one of {%q, %q}", info.Model, fixtureModel, fixtureCMTHead)
	}
	if info.IMEI != fixtureIMEI && info.IMEI != fixtureCMTHead {
		t.Fatalf("IMEI = %q, want one of {%q, %q}", info.IMEI, fixtureIMEI, fixtureCMTHead)
	}

	sim, err := commands.GetSimInfo(context.Background())
	if err != nil {
		t.Fatalf("GetSimInfo: %v", err)
	}
	if sim.PhoneNumber != fixturePhoneNumber {
		t.Fatalf("PhoneNumber = %q, loop parser corrupted", sim.PhoneNumber)
	}
	if sim.Carrier != fixtureCarrier {
		t.Fatalf("Carrier = %q, loop parser corrupted", sim.Carrier)
	}
	if sim.SignalQuality != 12 || sim.SignalPercent != 38 {
		t.Fatalf("signal = %d/%d, loop parser corrupted", sim.SignalQuality, sim.SignalPercent)
	}
	if !sim.NetworkRegistered {
		t.Fatal("NetworkRegistered = false, loop parser corrupted")
	}
}

// TestService_SignalUpdate is OBSERVABLE-ONLY: after SignalUpdate the SIM()
// signal fields and the SignalQuality gauge reflect +CSQ. No wire sequence
// assertions: the new engine issues one +CSQ command where the legacy engine
// ran the 5-command GetSimInfo path.
func TestService_SignalUpdate(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	metrics := newTestMetrics()
	svc := newTestService(testConfig(), m, metrics)

	cancel, done := runService(t, svc)
	defer cancel()

	waitForState(t, svc)

	m.setResponse(wireCSQ, []string{"+CSQ: 12,0", "OK"})
	svc.SignalUpdate(context.Background())

	sim := svc.SIM()
	if sim.SignalQuality != 12 || sim.SignalPercent != 38 {
		t.Fatalf("signal = %d/%d, want 12/38", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 38, "signal quality")

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestService_SignalUpdate_NoopWhenNotConnected locks the nil-engine no-op:
// SignalUpdate with no live Commands returns without touching sim/gauge.
func TestService_SignalUpdate_NoopWhenNotConnected(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	metrics := newTestMetrics()
	svc := newTestService(testConfig(), m, metrics)

	svc.SignalUpdate(context.Background())

	if sim := svc.SIM(); sim.SignalQuality != 0 || sim.SignalPercent != 0 {
		t.Fatalf("signal = %d/%d, want 0/0 (no-op)", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 0, "signal quality")
}

// TestService_SignalUpdate_StalenessGuardSwapsEngine exercises the POST-QUERY
// STALENESS GUARD directly: SignalUpdate queries via one Commands instance,
// the engine is swapped (disconnect/reconnect) while GetSignal is in flight,
// and the stale call must NOT write sim/gauge; a subsequent call on the
// current engine must.
//
// The swap lands exactly mid-call: modem A delays the +CSQ response, the test
// waits until A received AT+CSQ (GetSignal blocked), swaps s.commands to B
// under lock, then lets the delayed response complete.
func TestService_SignalUpdate_StalenessGuardSwapsEngine(t *testing.T) {
	mA := newScriptedModem(map[string][]string{wireCSQ: {"+CSQ: 12,0", "OK"}})
	mA.delay(wireCSQ, 400*time.Millisecond)
	mB := newScriptedModem(map[string][]string{wireCSQ: {"+CSQ: 20,0", "OK"}})
	metrics := newTestMetrics()
	svc := newTestService(testConfig(), mA, metrics)
	commandsA := newTestCommands(t, mA, 2*time.Second)
	commandsB := newTestCommands(t, mB, 2*time.Second)

	svc.mu.Lock()
	svc.commands = commandsA
	svc.mu.Unlock()

	done := make(chan struct{})
	go func() {
		svc.SignalUpdate(context.Background())
		close(done)
	}()

	select {
	case cmd := <-mA.received:
		if cmd != wireCSQ {
			t.Fatalf("modem A received %q, want %q", cmd, wireCSQ)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("modem A did not receive AT+CSQ")
	}

	svc.mu.Lock()
	svc.commands = commandsB
	svc.mu.Unlock()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SignalUpdate did not return after the engine swap")
	}

	if sim := svc.SIM(); sim.SignalQuality != 0 || sim.SignalPercent != 0 {
		t.Fatalf("signal = %d/%d, stale call must not write sim", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 0, "signal quality")

	svc.SignalUpdate(context.Background())

	if sim := svc.SIM(); sim.SignalQuality != 20 || sim.SignalPercent != 64 {
		t.Fatalf("signal = %d/%d, want 20/64 from the current engine", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 64, "signal quality")
}

// TestService_SignalRefreshTicker locks the Run ticker PERIODICITY: with a
// SHORT signalRefreshInterval, the SIM() signal fields and the SignalQuality
// gauge update on the FIRST and then on the SECOND tick - WITHOUT any
// explicit SignalUpdate call - and Run keeps running after the ticks
// (no-return-after-tick: the tick channel must not exit Run). The second
// refresh (12/38 -> 20/64 after the response table changes) can only come
// from a repeated ticker event.
func TestService_SignalRefreshTicker(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	metrics := newTestMetrics()
	svc := newTestService(testConfig(), m, metrics)
	svc.signalRefreshInterval = 50 * time.Millisecond

	cancel, done := runService(t, svc)
	defer cancel()
	waitForState(t, svc)

	// Post-boot +CSQ value: the FIRST tick must pick it up on its own.
	m.setResponse(wireCSQ, []string{"+CSQ: 12,0", "OK"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sim := svc.SIM(); sim.SignalQuality == 12 && sim.SignalPercent == 38 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sim := svc.SIM()
	if sim.SignalQuality != 12 || sim.SignalPercent != 38 {
		t.Fatalf("signal = %d/%d, want 12/38 (first tick refresh)", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 38, "signal quality")

	// Change the response: the 20/64 value can only arrive via a SECOND
	// ticker event (no explicit SignalUpdate call in this test).
	m.setResponse(wireCSQ, []string{"+CSQ: 20,0", "OK"})

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := svc.SIM(); got.SignalQuality == 20 && got.SignalPercent == 64 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	sim = svc.SIM()
	if sim.SignalQuality != 20 || sim.SignalPercent != 64 {
		t.Fatalf("signal = %d/%d, want 20/64 (second tick refresh)", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 64, "signal quality")

	// EXPLICIT no-return-after-tick: Run must still be running after two
	// refresh ticks (a single-case select would have disconnected after the
	// first tick and this select would fire).
	select {
	case err := <-done:
		t.Fatalf("Run returned after ticker ticks (err=%v), want still running", err)
	default:
	}

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestService_SignalRefreshTickerDisabled locks the zero-interval contract:
// signalRefreshInterval = 0 disables the ticker - post-boot +CSQ changes
// never reach sim/gauge (a 50ms-enabled ticker would have refreshed within
// the 200ms wait).
func TestService_SignalRefreshTickerDisabled(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	metrics := newTestMetrics()
	svc := newTestService(testConfig(), m, metrics)
	svc.signalRefreshInterval = 0

	cancel, done := runService(t, svc)
	defer cancel()
	waitForState(t, svc)

	m.setResponse(wireCSQ, []string{"+CSQ: 12,0", "OK"})

	time.Sleep(200 * time.Millisecond)

	sim := svc.SIM()
	if sim.SignalQuality != 0 || sim.SignalPercent != 0 {
		t.Fatalf("signal = %d/%d, want 0/0 (ticker disabled)", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 0, "signal quality")

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

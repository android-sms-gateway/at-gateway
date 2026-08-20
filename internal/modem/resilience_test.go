//nolint:testpackage // in-package tests wire svc.handleCMT directly and access Service state.
package modem

// PHASE-3 RESILIENCE SUITE (+CMT handler): the log-only consume handler
// registered at connect() (at.WithIndication("+CMT:", ...,
// at.WithTrailingLine)) must close the mid-command +CMT leak, log only a
// REDACTED head (SCTS timestamp; never sender or body), fall back to the
// fixed <redacted> marker on absent/malformed SCTS, and lock the HEAD-ONLY
// degradation (next modem line consumed as body - self-recovering one-line
// corruption). The handler runs on its own goroutine (indLoop spawns
// go ind.handler(n)), so assertions poll the log capture instead of assuming
// ordering.

import (
	"context"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"github.com/warthog618/modem/at"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	// fixtureCMTSender is a distinct sender number used by the fallback
	// fixtures; it must never appear in any log field or message.
	fixtureCMTSender = "+79995551234"
	// fixtureCMTBody is the +CMT message body (PII); it must never be logged.
	fixtureCMTBody = "top secret payload"
	// fixtureCMTSCTS is the parseable SCTS timestamp of fixtureCMTHead - the
	// only non-PII head field, and the only thing the handler may log.
	fixtureCMTSCTS = "26/08/16,12:00:00+14"
)

// newCMTTestService builds a Service over the given modem with a capturing
// logger (production wiring: portFactory seam + the +CMT indication is
// registered by connect()).
func newCMTTestService(t *testing.T, m *scriptedModem) (*Service, *capturingCore, *Metrics) {
	t.Helper()
	core := newCapturingCore()
	metrics := newTestMetrics()
	svc := NewService(testConfig(), zap.New(core), metrics)
	svc.portFactory = func(port.Config) (port.Port, error) { return m.rw, nil }

	return svc, core, metrics
}

// TestService_CMT_Handler_LogsRedactedTimestamp locks the happy path: a full
// +CMT notification (head + body) is consumed while idle, and the handler
// logs ONLY the SCTS timestamp at DEBUG level - never the sender number
// (present in the raw head line) nor the body (info[1], PII).
func TestService_CMT_Handler_LogsRedactedTimestamp(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	svc, core, _ := newCMTTestService(t, m)

	cancel, done := runService(t, svc)
	defer cancel()
	waitForState(t, svc)

	m.inject(fixtureCMTHead)
	m.inject(fixtureCMTBody)

	l := waitForLog(t, core, "SMS")
	if l.entry.Level != zapcore.DebugLevel {
		t.Fatalf("level = %v, want %v", l.entry.Level, zapcore.DebugLevel)
	}
	if got := logField(l, "scts"); got != fixtureCMTSCTS {
		t.Fatalf("scts = %q, want %q (timestamp only, quotes stripped)", got, fixtureCMTSCTS)
	}
	assertCMTNoPII(t, l)

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestRedactCMTHead locks the redaction function directly: a valid head line
// yields the SCTS timestamp only (quotes stripped); absent/malformed SCTS and
// a missing "+CMT:" prefix yield the fixed <redacted> marker. The no-prefix
// branch is defensive (indLoop only invokes the handler for matching lines).
func TestRedactCMTHead(t *testing.T) {
	tests := []struct {
		name string
		head string
		want string
	}{
		{name: "valid quoted scts", head: fixtureCMTHead, want: fixtureCMTSCTS},
		{name: "valid unquoted scts", head: `+CMT: "+123","",26/08/16,12:00:00+14`, want: fixtureCMTSCTS},
		{name: "absent scts", head: `+CMT: "+123",""`, want: cmtRedacted},
		{name: "malformed scts", head: `+CMT: "+123","","not-a-timestamp"`, want: cmtRedacted},
		{name: "truncated scts", head: `+CMT: "+123","","26/08/16"`, want: cmtRedacted},
		{name: "no prefix", head: "unrelated unsolicited line", want: cmtRedacted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactCMTHead(tt.head); got != tt.want {
				t.Fatalf("redactCMTHead(%q) = %q, want %q", tt.head, got, tt.want)
			}
		})
	}
}

// TestService_CMT_Handler_RedactionFallback locks the deterministic no-PII
// failure mode at the handler: when the SCTS is ABSENT or MALFORMED in the
// head line, the handler logs the fixed marker <redacted>.
func TestService_CMT_Handler_RedactionFallback(t *testing.T) {
	tests := []struct {
		name string
		head string
	}{
		{name: "absent scts", head: `+CMT: "` + fixtureCMTSender + `",""`},
		{name: "malformed scts", head: `+CMT: "` + fixtureCMTSender + `","","not-a-timestamp"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newScriptedModem(defaultBootResponses())
			svc, core, _ := newCMTTestService(t, m)

			cancel, done := runService(t, svc)
			defer cancel()
			waitForState(t, svc)

			m.inject(tt.head)
			m.inject(fixtureCMTBody)

			l := waitForLog(t, core, "SMS")
			if got := logField(l, "scts"); got != cmtRedacted {
				t.Fatalf("scts = %q, want %q (deterministic no-PII fallback)", got, cmtRedacted)
			}
			assertCMTNoPII(t, l)

			cancel()
			if err := waitRun(t, done, 2*time.Second); err != nil {
				t.Fatalf("Run: %v", err)
			}
		})
	}
}

// TestService_CMT_Handler_MidCommandLeakClosed locks the consume handler's
// deterministic effect: the trailing BODY line of a +CMT notification
// arriving while GetModemInfo is in flight is consumed by indLoop
// (WithTrailingLine), so it can NEVER leak into the in-flight response. The
// HEAD line, however, IS still forwarded upstream by the v0.4.0 indLoop (the
// range-loop `continue` does not skip the final `out <- line`), so an
// info[0]-reading parser may see the +CMT head instead of the clean value -
// the SAME set-membership class as the Phase-2 interleaved test. Assertions:
// body never leaks (deterministic), Manufacturer in {clean, +CMT head}
// (timing-dependent, library-verified).
func TestService_CMT_Handler_MidCommandLeakClosed(t *testing.T) {
	m := newScriptedModem(map[string][]string{
		wireGMI: {fixtureManufacturer, "OK"},
		wireGMM: {fixtureModel, "OK"},
		wireGSN: {fixtureIMEI, "OK"},
	})
	m.delay(wireGMI, 400*time.Millisecond)
	core := newCapturingCore()
	svc := NewService(testConfig(), zap.New(core), newTestMetrics())
	a := at.New(m.rw, at.WithTimeout(2*time.Second), at.WithIndication("+CMT:", svc.handleCMT, at.WithTrailingLine))
	svc.mu.Lock()
	svc.commands = NewCommands(a, newTestMetrics())
	svc.mu.Unlock()
	t.Cleanup(m.close)

	done := make(chan struct{})
	var info Info
	var err error
	go func() {
		info, err = svc.commands.GetModemInfo(context.Background())
		close(done)
	}()

	waitForCommand(t, m, wireGMI, 2*time.Second)

	// +CMT lands while GMI is in flight (the modem sleeps inside its delay).
	m.inject(fixtureCMTHead)
	m.inject(fixtureCMTBody)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("GetModemInfo did not return")
	}
	if err != nil {
		t.Fatalf("GetModemInfo: %v", err)
	}
	if info.Manufacturer != fixtureManufacturer && info.Manufacturer != fixtureCMTHead {
		t.Fatalf(
			"Manufacturer = %q, want one of {%q, %q} (body must never leak)",
			info.Manufacturer,
			fixtureManufacturer,
			fixtureCMTHead,
		)
	}
	for name, val := range map[string]string{"Model": info.Model, "IMEI": info.IMEI} {
		if val == fixtureCMTBody || val == fixtureCMTHead {
			t.Fatalf("%s = %q, +CMT lines leaked into a later field", name, val)
		}
	}

	// The body was consumed; the handler logged only the redacted head.
	l := waitForLog(t, core, "SMS")
	assertCMTNoPII(t, l)
}

// TestService_CMT_Handler_HeadOnlyFixture locks the documented HEAD-ONLY
// degradation: a +CMT head line WITHOUT a body makes indLoop block on the
// trailing-line read, so the NEXT modem line (the first line of the next
// command's response) is consumed as the body. The first SignalUpdate then
// degrades (no +CSQ line reaches its response - no-op), and the SECOND
// SignalUpdate is clean: self-recovering one-line corruption.
func TestService_CMT_Handler_HeadOnlyFixture(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	svc, core, metrics := newCMTTestService(t, m)

	cancel, done := runService(t, svc)
	defer cancel()
	waitForState(t, svc)

	// HEAD-ONLY +CMT while idle (no body line follows).
	m.inject(fixtureCMTHead)

	// The next modem line (+CSQ info line) is consumed as the CMT body.
	m.setResponse(wireCSQ, []string{"+CSQ: 12,0", "OK"})
	svc.SignalUpdate(context.Background())
	if sim := svc.SIM(); sim.SignalQuality != 0 || sim.SignalPercent != 0 {
		t.Fatalf(
			"signal = %d/%d after first SignalUpdate, want 0/0 (first line consumed as +CMT body)",
			sim.SignalQuality,
			sim.SignalPercent,
		)
	}
	assertGauge(t, metrics.SignalQuality, 0, "signal quality")

	// SELF-RECOVERY: the next command is clean (one-line corruption only).
	svc.SignalUpdate(context.Background())
	if sim := svc.SIM(); sim.SignalQuality != 12 || sim.SignalPercent != 38 {
		t.Fatalf("signal = %d/%d, want 12/38 (self-recovering)", sim.SignalQuality, sim.SignalPercent)
	}
	assertGauge(t, metrics.SignalQuality, 38, "signal quality")

	// The consumed body line was never logged (body = PII).
	l := waitForLog(t, core, "SMS")
	assertCMTNoPII(t, l)

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// TestService_CMT_Handler_SMSReceivedCounter locks the inbound-SMS counter:
// each +CMT notification increments SMSReceivedTotal exactly once (counter
// only - messages stay discarded, the log stays DEBUG-redacted). The handler
// runs on its own goroutine, so the counter is polled.
func TestService_CMT_Handler_SMSReceivedCounter(t *testing.T) {
	m := newScriptedModem(defaultBootResponses())
	svc, _, metrics := newCMTTestService(t, m)

	cancel, done := runService(t, svc)
	defer cancel()
	waitForState(t, svc)

	m.inject(fixtureCMTHead)
	m.inject(fixtureCMTBody)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counterValue(t, metrics.SMSReceivedTotal) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := counterValue(t, metrics.SMSReceivedTotal); got != 1 {
		t.Fatalf("SMSReceivedTotal = %v, want 1 (one +CMT)", got)
	}

	cancel()
	if err := waitRun(t, done, 2*time.Second); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

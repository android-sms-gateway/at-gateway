//nolint:testpackage // in-package tests exercise Commands internals (lazy drain barrier state).
package modem

import (
	"context"
	"errors"
	"io"
	"maps"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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

// smsModem is a scripted fake port for the two-step AT+CMGS flow. The engine
// writes "AT+CMGS=\"<phone>\"\r" (quoted destination per TS 27.005; carriage
// return, no line feed) and, after the ">" prompt, the payload terminated by
// Ctrl-Z (\x1a); the shared
// scriptedModem harness cannot see those tokens (its scanner splits on LF
// only), so this fake splits received bytes on CR, LF and Ctrl-Z. While a
// payload is expected (after a ">" prompt was served) ONLY the Ctrl-Z
// terminates the token, so multi-line texts stay byte-exact. The Ctrl-Z stays
// part of the payload token, making byte-exact payload assertions possible.
// Unknown keys stay silent (wedged fixtures).
type smsModem struct {
	mu        sync.Mutex
	responses map[string][]string
	received  []string

	expectingPayload bool

	rw    *pipePort
	respW *io.PipeWriter
	done  chan struct{}
}

func newSMSModem(responses map[string][]string) *smsModem {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()
	m := &smsModem{
		responses: responses,
		rw:        &pipePort{r: respR, w: cmdW},
		respW:     respW,
		done:      make(chan struct{}),
	}
	go m.run(cmdR)

	return m
}

func (m *smsModem) run(r *io.PipeReader) {
	defer close(m.done)
	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	lastCR := false
	for {
		n, err := r.Read(one)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		b := one[0]
		if m.expectingPayload {
			buf = append(buf, b)
			if b == '\x1a' {
				m.respond(string(buf))
				buf = buf[:0]
			}
			continue
		}
		switch b {
		case '\r':
			m.respond(string(buf))
			buf = buf[:0]
		case '\n':
			if !lastCR { // a CRLF pair is one token (the CR already fired)
				m.respond(string(buf))
				buf = buf[:0]
			}
		default:
			buf = append(buf, b)
		}
		lastCR = b == '\r'
	}
}

func (m *smsModem) respond(key string) {
	m.mu.Lock()
	m.received = append(m.received, key)
	lines := m.responses[key]
	m.mu.Unlock()

	if len(lines) == 0 {
		return // silent fixture (wedged command)
	}

	var b strings.Builder
	gotPrompt := false
	for _, line := range lines {
		if line == ">" {
			gotPrompt = true
		}
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	_, _ = m.respW.Write([]byte(b.String()))
	m.expectingPayload = gotPrompt
}

// receivedCommands returns all tokens received so far (commands and payloads).
func (m *smsModem) receivedCommands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.received...)
}

// close shuts the engine-facing port down; the modem goroutine exits on EOF.
func (m *smsModem) close() {
	_ = m.rw.Close()
}

// smsMetrics returns test metrics with the SMSSentTotal counter initialized
// (newTestMetrics predates the send path; the field is wired here instead of
// touching the shared helper).
func smsMetrics() *Metrics {
	m := newTestMetrics()
	m.SMSSentTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "at_gateway_modem_sms_sent_total",
		Help: "total SMS sent (test)",
	})

	return m
}

// newSMSCommands wires a Commands instance over the smsModem fake with the
// full boot response table plus the given extra keys.
func newSMSCommands(
	t *testing.T,
	extra map[string][]string,
	cmdTimeout time.Duration,
) (*Commands, *smsModem, *Metrics) {
	t.Helper()
	resp := defaultBootResponses()
	maps.Copy(resp, extra)
	m := newSMSModem(resp)
	metrics := smsMetrics()
	a := at.New(m.rw, at.WithTimeout(cmdTimeout))
	t.Cleanup(m.close)
	commands := NewCommands(a, metrics)
	if err := commands.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}

	return commands, m, metrics
}

// TestCommands_SendSMS_Success pins the AT+CMGS success flow end-to-end: the
// command line with the QUOTED phone number (TS 27.005: AT+CMGS="+7..."), the
// ">" prompt, the payload (byte-exact including the Ctrl-Z terminator), the
// +CMGS: <mr> parse and the SMSSentTotal increment. Wire keys are HARD-CODED
// quoted literals, never derived from the implementation expression, so a
// quoting regression fails the fixture. Phone and text pass through byte-exact
// across input variations.
func TestCommands_SendSMS_Success(t *testing.T) {
	tests := []struct {
		name   string
		cmdKey string
		phone  string
		text   string
		ref    int
	}{
		{name: "plain", cmdKey: `AT+CMGS="+79990001234"`, phone: "+79990001234", text: "Hello, world!", ref: 7},
		{
			name:   "multi-line",
			cmdKey: `AT+CMGS="+15551234567"`,
			phone:  "+15551234567",
			text:   "line one\nline two\r\nline three",
			ref:    1,
		},
		{
			name:   "punctuation",
			cmdKey: `AT+CMGS="88001234567"`,
			phone:  "88001234567",
			text:   "{}[]|~^\\\"@#$%&*",
			ref:    42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commands, m, metrics := newSMSCommands(t, map[string][]string{
				tt.cmdKey:        {">"},
				tt.text + "\x1a": {"+CMGS: " + strconv.Itoa(tt.ref), "OK"},
			}, time.Second)

			ref, err := commands.SendSMS(context.Background(), tt.phone, tt.text)
			if err != nil {
				t.Fatalf("SendSMS: %v", err)
			}
			if ref != tt.ref {
				t.Fatalf("ref = %d, want %d", ref, tt.ref)
			}

			want := []string{wireAT, wireATE0, wireCMEE, wireCMGF, wireCNMI, wireCPINQuery, tt.cmdKey, tt.text + "\x1a"}
			if got := m.receivedCommands(); !slicesEqual(got, want) {
				t.Fatalf("commands = %q, want %q (payload must include Ctrl-Z)", got, want)
			}
			if got := counterValue(t, metrics.SMSSentTotal); got != 1 {
				t.Fatalf("SMSSentTotal = %v, want 1", got)
			}
			if got := counterVecValue(t, metrics.CommandsTotal, "send SMS", "ok"); got != 1 {
				t.Fatalf("CommandsTotal{send SMS,ok} = %v, want 1", got)
			}
		})
	}
}

// TestCommands_SendSMS_WireFormatConformance pins the exact AT+CMGS wire
// format on the fake: 3GPP TS 27.005 requires the destination address QUOTED
// (AT+CMGS="+79990001234") - SIM800L rejects the unquoted form with +CMS
// ERROR. The expected wire line is a HARD-CODED quoted literal (not derived
// from the implementation expression), and the payload must arrive byte-identical.
func TestCommands_SendSMS_WireFormatConformance(t *testing.T) {
	const phone = "+79990001234"
	const text = "Hello, world!"
	commands, m, _ := newSMSCommands(t, map[string][]string{
		`AT+CMGS="+79990001234"`: {">"},
		text + "\x1a":            {"+CMGS: 7", "OK"},
	}, time.Second)

	ref, err := commands.SendSMS(context.Background(), phone, text)
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if ref != 7 {
		t.Fatalf("ref = %d, want 7", ref)
	}

	want := []string{
		wireAT,
		wireATE0,
		wireCMEE,
		wireCMGF,
		wireCNMI,
		wireCPINQuery,
		`AT+CMGS="+79990001234"`,
		text + "\x1a",
	}
	if got := m.receivedCommands(); !slicesEqual(got, want) {
		t.Fatalf("commands = %q, want %q (wire line must be AT+CMGS=\"+79990001234\", payload byte-exact)", got, want)
	}
}

// TestCommands_SendSMS_PhoneHardening pins the pre-wire phone validation: a
// phone containing a quote, CR or LF would corrupt the AT+CMGS command line,
// so SendSMS rejects it with ErrInvalidPhone BEFORE any modem traffic.
func TestCommands_SendSMS_PhoneHardening(t *testing.T) {
	tests := []struct {
		name  string
		phone string
	}{
		{name: "quote", phone: `+7999"0001234`},
		{name: "carriage return", phone: "+7999\r0001234"},
		{name: "newline", phone: "+7999\n0001234"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commands, m, _ := newSMSCommands(t, nil, time.Second)

			_, err := commands.SendSMS(context.Background(), tt.phone, "hi")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, ErrInvalidPhone) {
				t.Fatalf("error = %v, want ErrInvalidPhone", err)
			}

			want := []string{wireAT, wireATE0, wireCMEE, wireCMGF, wireCNMI, wireCPINQuery}
			if got := m.receivedCommands(); !slicesEqual(got, want) {
				t.Fatalf("commands = %q, want boot rows only (no send traffic for phone %q)", got, tt.phone)
			}
		})
	}
}

// TestCommands_SendSMS_CMGSHeadLeakImmune pins the +CMGS: reference parser
// against the v0.4.0 indication-head leak: a leaked head line carrying
// "+CMGS:" as a MID-LINE SUBSTRING arrives as an unknown info line before the
// legit +CMGS: response (no +CMT handler is registered in the SMS harness, so
// the head leaks upstream). The parser must match ONLY lines actually
// STARTING with "+CMGS:" (CutPrefix loop): the reference is parsed from the
// prefixed line, never from the leak - neither a parse error (unparseable
// substring) nor a false reference (numeric substring) may occur.
func TestCommands_SendSMS_CMGSHeadLeakImmune(t *testing.T) {
	const phone = "+79990001234"
	const text = "Hello, world!"
	const cmdKey = `AT+CMGS="+79990001234"`

	tests := []struct {
		name string
		leak string
	}{
		{
			name: "unparseable substring",
			leak: `+CMT: "+CMGS:","123","24/08/26,12:00:00+00"`,
		},
		{
			name: "numeric substring false-match bait",
			leak: `+CMT: "x",+CMGS: 42`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			commands, _, metrics := newSMSCommands(t, map[string][]string{
				cmdKey:        {">"},
				text + "\x1a": {tt.leak, "+CMGS: 7", "OK"},
			}, time.Second)

			ref, err := commands.SendSMS(context.Background(), phone, text)
			if err != nil {
				t.Fatalf("SendSMS: %v", err)
			}
			if ref != 7 {
				t.Fatalf("ref = %d, want 7 (parsed only from the +CMGS: prefixed line, not leak %q)", ref, tt.leak)
			}
			if got := counterValue(t, metrics.SMSSentTotal); got != 1 {
				t.Fatalf("SMSSentTotal = %v, want 1", got)
			}
		})
	}
}

// TestCommands_SendSMS_CMSErrorMapped pins the +CMS ERROR mapping onto the
// ErrSendFailed domain sentinel with the preserved tag prefix; no send counter
// is incremented.
func TestCommands_SendSMS_CMSErrorMapped(t *testing.T) {
	const phone = "+79990001234"
	const text = "Hello, world!"
	commands, m, metrics := newSMSCommands(t, map[string][]string{
		`AT+CMGS="+79990001234"`: {">"},
		text + "\x1a":            {"+CMS ERROR: 332"},
	}, time.Second)

	_, err := commands.SendSMS(context.Background(), phone, text)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrSendFailed) {
		t.Fatalf("error = %v, want ErrSendFailed", err)
	}
	if !strings.HasPrefix(err.Error(), "send SMS (AT+CMGS=\"+79990001234\"):") {
		t.Fatalf("error %q does not carry the tag prefix", err)
	}
	if got := counterValue(t, metrics.SMSSentTotal); got != 0 {
		t.Fatalf("SMSSentTotal = %v, want 0 (rejected send)", got)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "send SMS", "error"); got != 1 {
		t.Fatalf("CommandsTotal{send SMS,error} = %v, want 1", got)
	}

	got := m.receivedCommands()
	want := []string{
		wireAT,
		wireATE0,
		wireCMEE,
		wireCMGF,
		wireCNMI,
		wireCPINQuery,
		`AT+CMGS="+79990001234"`,
		text + "\x1a",
	}
	if !slicesEqual(got, want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

// TestCommands_SendSMS_CMSErrorCodePreserved pins the dual error wrap on the
// send path: the +CMS ERROR code must stay [errors.As]-able (at.CMSError with
// the numeric code) while ErrSendFailed stays [errors.Is]-able, and the message
// carries the library "CMS Error: <code>" text (mapSMSError previously
// swallowed the code).
func TestCommands_SendSMS_CMSErrorCodePreserved(t *testing.T) {
	const phone = "+79990001234"
	const text = "Hello, world!"
	commands, _, _ := newSMSCommands(t, map[string][]string{
		`AT+CMGS="+79990001234"`: {">"},
		text + "\x1a":            {"+CMS ERROR: 332"},
	}, time.Second)

	_, err := commands.SendSMS(context.Background(), phone, text)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrSendFailed) {
		t.Fatalf("error = %v, want ErrSendFailed", err)
	}
	var cms at.CMSError
	if !errors.As(err, &cms) {
		t.Fatalf("error = %v, want errors.As to find at.CMSError", err)
	}
	if string(cms) != "332" {
		t.Fatalf("CMS code = %q, want 332", string(cms))
	}
	if !strings.Contains(err.Error(), "CMS Error") {
		t.Fatalf("error %q does not carry the library CMS Error text", err)
	}
}

// TestCommands_SendSMS_DeadlineMappedAndDrain pins the timeout path: a silent
// modem fails SendSMS with ErrModemTimeout, sets the lazy drain barrier, and
// the next send drains (bare AT) before its own rows complete.
func TestCommands_SendSMS_DeadlineMappedAndDrain(t *testing.T) {
	const phone = "+79990001234"
	const text = "Hello, world!"

	// Wedged fake: no +CMGS keys, so the first send times out. The drain key
	// "AT" is served by the boot table (DRAIN-KEY COLLISION, same as the
	// shared harness).
	commands, m, metrics := newSMSCommands(t, nil, 200*time.Millisecond)

	_, err := commands.SendSMS(context.Background(), phone, text)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrModemTimeout) {
		t.Fatalf("error = %v, want ErrModemTimeout", err)
	}
	if !strings.HasPrefix(err.Error(), "send SMS (AT+CMGS=\"+79990001234\"):") {
		t.Fatalf("error %q does not carry the tag prefix", err)
	}

	// Recovered modem: the drain (bare AT) and the full +CMGS flow succeed.
	m.mu.Lock()
	m.responses[`AT+CMGS="+79990001234"`] = []string{">"}
	m.responses[text+"\x1a"] = []string{"+CMGS: 7", "OK"}
	m.mu.Unlock()

	ref, sendErr := commands.SendSMS(context.Background(), phone, text)
	if sendErr != nil {
		t.Fatalf("SendSMS after drain: %v", sendErr)
	}
	if ref != 7 {
		t.Fatalf("ref = %d, want 7", ref)
	}

	// First send: +CMGS command then the library's escape on timeout; second
	// send: drain (bare AT), then the +CMGS command and the payload.
	want := []string{
		wireAT, wireATE0, wireCMEE, wireCMGF, wireCNMI, wireCPINQuery,
		`AT+CMGS="+79990001234"`, "\x1b",
		wireAT, `AT+CMGS="+79990001234"`, text + "\x1a",
	}
	if got := m.receivedCommands(); !slicesEqual(got, want) {
		t.Fatalf("commands = %q, want %q (drain = bare AT after the escape)", got, want)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "send SMS", "error"); got != 1 {
		t.Fatalf("CommandsTotal{send SMS,error} = %v, want 1 (timed-out send)", got)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "send SMS", "ok"); got != 1 {
		t.Fatalf("CommandsTotal{send SMS,ok} = %v, want 1", got)
	}
	if got := counterVecValue(t, metrics.CommandsTotal, "", "ok"); got != 1 {
		t.Fatalf("CommandsTotal{command=\"\",ok} = %v, want 1 (drain)", got)
	}
	if got := counterValue(t, metrics.SMSSentTotal); got != 1 {
		t.Fatalf("SMSSentTotal = %v, want 1 (only the completed send)", got)
	}
}

// TestCommands_SendSMS_CtxCanceledBeforeSend pins the ctx pre-check: a
// canceled context fails the send before any modem traffic.
func TestCommands_SendSMS_CtxCanceledBeforeSend(t *testing.T) {
	commands, m, _ := newSMSCommands(t, nil, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := commands.SendSMS(ctx, "+79990001234", "hi")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	want := []string{wireAT, wireATE0, wireCMEE, wireCMGF, wireCNMI, wireCPINQuery}
	if got := m.receivedCommands(); !slicesEqual(got, want) {
		t.Fatalf("commands = %q, want %q (no send traffic)", got, want)
	}
}

// TestCommands_SendSMS_NotInitialized pins the initialization guard: sending
// before a successful Init fails with ErrModemNotStarted and no traffic.
func TestCommands_SendSMS_NotInitialized(t *testing.T) {
	m := newSMSModem(defaultBootResponses())
	commands := NewCommands(at.New(m.rw, at.WithTimeout(time.Second)), smsMetrics())
	t.Cleanup(m.close)

	_, err := commands.SendSMS(context.Background(), "+79990001234", "hi")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrModemNotStarted) {
		t.Fatalf("error = %v, want ErrModemNotStarted", err)
	}
	if got := m.receivedCommands(); len(got) != 0 {
		t.Fatalf("commands = %q, want none", got)
	}
}

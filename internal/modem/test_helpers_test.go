//nolint:testpackage // in-package tests need the portFactory seam and direct access to unexported Service/Commands state.
package modem

import (
	"bufio"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem/port"
	"github.com/go-core-fx/healthfx"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	testPortName = "ttyFake"
	testBaudRate = 115200
)

// Wire keys for the command-keyed fixture table. Consts keep goconst happy:
// the legacy sms.go table shares these literals and must stay untouched in
// the behavior-lock commit.
const (
	wireAT        = "AT"
	wireATE0      = "ATE0"
	wireCMEE      = "AT+CMEE=1"
	wireCMGF      = "AT+CMGF=1"
	wireCNMI      = "AT+CNMI=2,1,0,0,0"
	wireCPINQuery = "AT+CPIN?"
	wireGMI       = "AT+GMI"
	wireGMM       = "AT+GMM"
	wireGSN       = "AT+GSN"
	wireCNUM      = "AT+CNUM"
	wireCCID      = "AT+CCID"
	wireCOPS      = "AT+COPS?"
	wireCSQ       = "AT+CSQ"
	wireCREG      = "AT+CREG?"
)

// defaultBootResponses returns the command-keyed response table for a full
// successful boot. The DRAIN-KEY COLLISION is explicit: "AT" is both init row
// 1 and the lazy drain command - the table serves it with OK in both contexts.
func defaultBootResponses() map[string][]string {
	return map[string][]string{
		wireAT:        {"OK"},
		wireATE0:      {"OK"},
		wireCMEE:      {"OK"},
		wireCMGF:      {"OK"},
		wireCNMI:      {"OK"},
		wireCPINQuery: {"+CPIN: READY", "OK"},
		wireGMI:       {"SIMCOM_SIM800L", "OK"},
		wireGMM:       {"SIM800L", "OK"},
		wireGSN:       {"123456789012345", "OK"},
		wireCNUM:      {"+CNUM: \"\",\"+1234567890\",129", "OK"},
		wireCCID:      {"89860001020304050607", "OK"},
		wireCOPS:      {"+COPS: 0,0,\"MTS\",7", "OK"},
		wireCSQ:       {"+CSQ: 0,0", "OK"},
		wireCREG:      {"+CREG: 0,1", "OK"},
	}
}

// pipePort is an [io.ReadWriteCloser] backed by a pipe pair. Closing it closes
// both pipe ends, which EOFs any reader on either side.
type pipePort struct {
	r *io.PipeReader
	w *io.PipeWriter

	closeCount atomic.Int32
}

func (p *pipePort) Read(b []byte) (int, error) {
	return p.r.Read(b)
}

func (p *pipePort) Write(b []byte) (int, error) {
	return p.w.Write(b)
}

func (p *pipePort) Close() error {
	p.closeCount.Add(1)
	_ = p.r.Close()
	_ = p.w.Close()

	return nil
}

func (p *pipePort) closedCount() int {
	return int(p.closeCount.Load())
}

// scriptedModem answers each received command line with a scripted response
// keyed by the raw wire line (e.g. "AT", "ATE0", "AT+CMEE=1"), so one table
// serves the legacy engine (Exec("AT")) and the warthog618 library
// (Command("")). Unscripted commands are acknowledged with OK when defaultOK
// is set; otherwise they are left unanswered (silent/wedged fixtures).
type scriptedModem struct {
	mu        sync.RWMutex
	responses map[string][]string
	delays    map[string]time.Duration
	delayN    map[string]int
	defaultOK bool

	received   chan string
	firstWrite chan struct{}

	rw    *pipePort
	respW *io.PipeWriter // modem-side writer for unsolicited injections
	done  chan struct{}
}

func newScriptedModem(responses map[string][]string) *scriptedModem {
	cmdR, cmdW := io.Pipe()
	respR, respW := io.Pipe()
	m := &scriptedModem{
		responses:  responses,
		delays:     map[string]time.Duration{},
		delayN:     map[string]int{},
		received:   make(chan string, 64),
		firstWrite: make(chan struct{}),
		rw:         &pipePort{r: respR, w: cmdW},
		respW:      respW,
		done:       make(chan struct{}),
	}
	go m.run(bufio.NewScanner(cmdR), respW)

	return m
}

// setResponse installs or replaces the scripted response for a command key.
// Safe to call concurrently with the modem goroutine.
func (m *scriptedModem) setResponse(key string, lines []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[key] = lines
}

// delay makes the modem wait d before answering the first write of key.
func (m *scriptedModem) delay(key string, d time.Duration) {
	m.delays[key] = d
	m.delayN[key] = 1
}

// close shuts the engine-facing port down; the modem goroutine exits on EOF.
func (m *scriptedModem) close() {
	_ = m.rw.Close()
}

// hangup simulates a modem unplug: only the engine-facing READ side EOFs, so
// the library AT closes WITHOUT the service's port Close being pre-empted
// (the service's disconnect() remains the only Close caller).
func (m *scriptedModem) hangup() {
	_ = m.rw.r.Close()
}

// inject writes a raw line (CRLF-terminated) into the modem->engine stream,
// simulating an unsolicited notification (e.g. a +CMT URC). Only call it when
// the modem goroutine is not writing a response (idle or inside a delay) so
// the pipe write order stays deterministic.
func (m *scriptedModem) inject(line string) {
	_, _ = m.respW.Write([]byte(line + "\r\n"))
}

// newWedgedModem returns the DEDICATED SILENT fake for wedged-modem tests:
// it NEVER responds to row 1 (or any command). It deliberately is NOT the
// shared command-keyed harness, which serves bare-AT->OK for the drain key
// (DRAIN-KEY COLLISION) - that collision must not leak into wedged fixtures.
func newWedgedModem() *scriptedModem {
	return newScriptedModem(nil) // defaultOK=false: every command stays silent
}

// receivedCommands drains and returns all commands written by the engine.
func (m *scriptedModem) receivedCommands() []string {
	cmds := make([]string, 0, len(m.received))
	for {
		select {
		case cmd := <-m.received:
			cmds = append(cmds, cmd)
		default:
			return cmds
		}
	}
}

// waitForCommand consumes the received stream until the engine wrote want
// (boot sequences write many commands first, so plain channel reads are not
// enough).
func waitForCommand(t *testing.T, m *scriptedModem, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case cmd := <-m.received:
			if cmd == want {
				return
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("modem did not receive %q within %v", want, timeout)
}

func (m *scriptedModem) run(scanner *bufio.Scanner, w *io.PipeWriter) {
	defer close(m.done)
	var first sync.Once
	for scanner.Scan() {
		key := scanner.Text()
		m.received <- key
		first.Do(func() { close(m.firstWrite) })

		if n := m.delayN[key]; n > 0 {
			m.delayN[key] = n - 1
			time.Sleep(m.delays[key])
		}

		m.mu.RLock()
		lines, ok := m.responses[key]
		m.mu.RUnlock()
		if !ok {
			if !m.defaultOK {
				continue // silent fixture: never respond
			}
			lines = []string{"OK"}
		}

		var b strings.Builder
		for _, line := range lines {
			b.WriteString(line)
			b.WriteString("\r\n")
		}
		_, _ = w.Write([]byte(b.String()))
	}
}

func testConfig() Config {
	return Config{
		Port:           testPortName,
		BaudRate:       testBaudRate,
		InitTimeout:    10 * time.Second,
		CommandTimeout: 2 * time.Second,
	}
}

func newTestService(cfg Config, m *scriptedModem, metrics *Metrics) *Service {
	svc := NewService(cfg, zap.NewNop(), metrics)
	svc.portFactory = func(port.Config) (port.Port, error) { return m.rw, nil }

	return svc
}

// newTestMetrics constructs the FULL Metrics struct as a plain
// promauto-free literal: prometheus.NewCounterVec/NewHistogram/NewCounter/
// NewGauge directly, so tests never touch the global promauto registry
// (duplicate-registration panics). Only wired fields are asserted.
func newTestMetrics() *Metrics {
	return &Metrics{
		CommandsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "at_gateway_modem_commands_total",
				Help: "total AT commands (test)",
			},
			[]string{"command", "status"},
		),
		CommandDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "at_gateway_modem_command_duration_seconds",
				Help:    "AT command duration (test)",
				Buckets: []float64{0.1, 0.5, 1, 2, 5},
			},
		),
		SMSReceivedTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "at_gateway_modem_sms_received_total",
			Help: "total SMS received (test)",
		}),
		ModemState: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "at_gateway_modem_state_test",
			Help: "modem state (test)",
		}),
		SignalQuality: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "at_gateway_modem_signal_quality_test",
			Help: "signal quality (test)",
		}),
		ReconnectsTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "at_gateway_modem_reconnects_total",
			Help: "total reconnects (test)",
		}),
	}
}

func runService(t *testing.T, svc *Service) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	return cancel, done
}

func waitRun(t *testing.T, done chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatalf("Run did not return within %v", timeout)

		return nil
	}
}

func waitForState(t *testing.T, svc *Service) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if svc.State() == StateReady {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state %s not reached within 10s, current: %s", StateReady, svc.State())
}

func assertGauge(t *testing.T, g prometheus.Gauge, want float64, name string) {
	t.Helper()
	if got := gaugeValue(t, g); got != want {
		t.Fatalf("%s gauge = %v, want %v", name, got, want)
	}
}

// gaugeValue returns the current value of an unregistered gauge via a fresh
// registry (avoids duplicate registration of promauto collectors and keeps
// the go.mod graph untouched).
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(g); err != nil {
		t.Fatalf("register gauge: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather gauge: %v", err)
	}
	if len(mfs) != 1 || len(mfs[0].GetMetric()) != 1 || mfs[0].GetMetric()[0].GetGauge() == nil {
		t.Fatalf("unexpected gauge gather result")
	}

	return mfs[0].GetMetric()[0].GetGauge().GetValue()
}

// counterValue returns the current value of an unregistered counter via a
// fresh registry (same promauto-avoidance rationale as gaugeValue).
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register counter: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather counter: %v", err)
	}
	if len(mfs) != 1 || len(mfs[0].GetMetric()) != 1 || mfs[0].GetMetric()[0].GetCounter() == nil {
		t.Fatalf("unexpected counter gather result")
	}

	return mfs[0].GetMetric()[0].GetCounter().GetValue()
}

// counterVecValue returns the value of an unregistered counter-vec child for
// the given labels via a fresh registry (same rationale as gaugeValue).
func counterVecValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	child, err := cv.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("counter-vec label %v: %v", labels, err)
	}

	return counterValue(t, child)
}

// histogramCount returns the observation count of an unregistered histogram
// via a fresh registry.
func histogramCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	reg := prometheus.NewRegistry()
	if err := reg.Register(h); err != nil {
		t.Fatalf("register histogram: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather histogram: %v", err)
	}
	if len(mfs) != 1 || len(mfs[0].GetMetric()) != 1 || mfs[0].GetMetric()[0].GetHistogram() == nil {
		t.Fatalf("unexpected histogram gather result")
	}

	return mfs[0].GetMetric()[0].GetHistogram().GetSampleCount()
}

func assertHealthReady(t *testing.T, svc *Service) {
	t.Helper()
	checks, err := NewHealthProvider(svc).ReadyProbe(context.Background())
	if err != nil {
		t.Fatalf("ready probe: %v", err)
	}
	if checks["modem"].Status != healthfx.StatusPass {
		t.Fatalf("health status = %v, want pass", checks["modem"].Status)
	}
}

// capturedLog is one entry captured by capturingCore.
type capturedLog struct {
	entry  zapcore.Entry
	fields []zapcore.Field
}

// capturingCore collects zap entries so tests can assert on log content
// (level, fields) without a real sink. Stdlib-only test helper.
type capturingCore struct {
	mu      sync.Mutex
	entries []capturedLog
}

func newCapturingCore() *capturingCore {
	return &capturingCore{}
}

func (c *capturingCore) Enabled(zapcore.Level) bool { return true }

func (c *capturingCore) With([]zapcore.Field) zapcore.Core { return c }

func (c *capturingCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(e, c)
}

func (c *capturingCore) Write(e zapcore.Entry, f []zapcore.Field) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, capturedLog{entry: e, fields: append([]zapcore.Field(nil), f...)})

	return nil
}

func (c *capturingCore) Sync() error { return nil }

// waitForLog polls the capture until an entry whose message contains want
// appears (the +CMT handler and other asynchronous loggers run on their own
// goroutines - no ordering assumed).
func waitForLog(t *testing.T, c *capturingCore, want string) *capturedLog {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for i := range c.entries {
			if strings.Contains(c.entries[i].entry.Message, want) {
				l := c.entries[i]
				c.mu.Unlock()

				return &l
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no log entry containing %q within 2s", want)

	return nil
}

// logErrorField returns the error carried by a zap.Error field (stored in
// Field.Interface for zapcore.ErrorType), or nil if absent.
func logErrorField(l *capturedLog, key string) error {
	for _, f := range l.fields {
		if f.Key == key && f.Type == zapcore.ErrorType {
			if err, ok := f.Interface.(error); ok {
				return err
			}
		}
	}

	return nil
}

// logField returns the string value of a zap field by key, or "" if absent.
func logField(l *capturedLog, key string) string {
	for _, f := range l.fields {
		if f.Key == key {
			return f.String
		}
	}

	return ""
}

// assertCMTNoPII fails when the captured +CMT handler entry leaks the sender
// number (fixtureCMTSender or the digits inside fixtureCMTHead) or the
// message body (all PII; only the SCTS timestamp is allowed).
func assertCMTNoPII(t *testing.T, l *capturedLog) {
	t.Helper()
	if strings.Contains(l.entry.Message, fixtureCMTSender) {
		t.Fatalf("log message %q contains sender PII", l.entry.Message)
	}
	for _, f := range l.fields {
		if strings.Contains(f.String, fixtureCMTSender) || strings.Contains(f.String, fixtureCMTBody) {
			t.Fatalf("log field %q contains PII", f.String)
		}
	}
}

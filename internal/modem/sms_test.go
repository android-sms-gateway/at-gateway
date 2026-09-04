package modem_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/warthog618/modem/at"
)

// Wire constants shared by the scripted-modem assertions.
const (
	// smsCtrlZ terminates the SMS payload of a +CMGS exchange (the library
	// appends it to the payload write).
	smsCtrlZ = "\x1a"
	// testPhone is the recipient used in every send vector.
	testPhone = "+79990001234"
	// commandTimeout bounds every AT command in the harness; timeout tests
	// deliberately starve a command to cross it.
	commandTimeout = 150 * time.Millisecond
)

// scriptedModem is an in-memory serial port: every Write pushes response
// chunks produced by onWrite into the response queue, Read pops them. A write
// with no scripted response stays silent so the library command times out -
// exactly what a starved real modem would do. All responses are recorded for
// wire assertions.
type scriptedModem struct {
	responses chan []byte
	onWrite   func(string) [][]byte

	mu     sync.Mutex
	writes []string
	closed bool
}

func (m *scriptedModem) Read(p []byte) (int, error) {
	data, ok := <-m.responses
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, data)
	return n, nil
}

func (m *scriptedModem) Write(p []byte) (int, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return 0, at.ErrClosed
	}
	m.writes = append(m.writes, string(p))
	responses := m.onWrite(string(p))
	m.mu.Unlock()

	for _, r := range responses {
		m.responses <- r
	}

	return len(p), nil
}

func (m *scriptedModem) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.responses)
	}
	return nil
}

// getWrites returns a copy of the recorded wire writes.
func (m *scriptedModem) getWrites() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.writes...)
}

// okResponse wraps a modem response body into a wire chunk terminated with
// CRLF pairs as a real modem emits them.
func okResponse(body string) []byte {
	return []byte("\r\n" + body + "\r\n")
}

// initResponder answers the standard boot init sequence rows: plain commands
// with OK, +CPIN? with a READY status line followed by OK.
func initResponder(w string) [][]byte {
	switch w {
	case "AT\r\n", "ATE0\r\n", "AT+CMEE=1\r\n", "AT+CMGF=0\r\n", "AT+CNMI=2,1,0,0,0\r\n":
		return [][]byte{okResponse("OK")}
	case "AT+CPIN?\r\n":
		return [][]byte{okResponse("+CPIN: READY"), okResponse("OK")}
	}
	return nil
}

// smsResponder answers the PDU send flow: the +CMGS command line is answered
// with the ">" prompt (no CRLF, as a real modem emits it), the Ctrl-Z
// terminated payload with the next scripted +CMGS acknowledgement followed by
// OK. An unscripted payload is rejected with +CMS ERROR 500.
func smsResponder(acks ...string) func(string) [][]byte {
	payloads := 0

	return func(w string) [][]byte {
		if resps := initResponder(w); resps != nil {
			return resps
		}
		switch {
		case strings.HasPrefix(w, "AT+CMGS="):
			return [][]byte{[]byte("\r\n> ")}
		case strings.HasSuffix(w, smsCtrlZ):
			if payloads < len(acks) {
				resp := acks[payloads]
				payloads++

				return [][]byte{okResponse("+CMGS: " + resp), okResponse("OK")}
			}

			return [][]byte{okResponse("+CMS ERROR: 500")}
		}
		return nil
	}
}

// newCommands boots a scripted modem through the full init sequence and
// returns the ready Commands handle plus the modem for wire assertions.
func newCommands(t *testing.T, respond func(string) [][]byte) (*modem.Commands, *scriptedModem) {
	t.Helper()

	m := &scriptedModem{
		responses: make(chan []byte, 256),
		onWrite:   respond,
	}

	a := at.New(m, at.WithTimeout(commandTimeout))
	commands := modem.NewCommands(a, newTestMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := commands.Init(ctx); err != nil {
		m.Close()
		t.Fatalf("init scripted modem: %v", err)
	}

	t.Cleanup(func() { m.Close() })

	return commands, m
}

// newTestMetrics builds modem metrics with plain constructors (never
// promauto in tests) and the help strings promlinter demands.
func newTestMetrics() *modem.Metrics {
	commandsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "test_commands_total",
			Help: "Test counter",
		},
		[]string{"command", "status"},
	)
	commandDuration := prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "test_command_duration_seconds",
			Help:    "Test histogram",
			Buckets: []float64{0.1, 0.5, 1, 2, 5},
		},
	)

	return &modem.Metrics{
		CommandsTotal:   commandsTotal,
		CommandDuration: commandDuration,
	}
}

// sendWrites extracts the wire writes belonging to one SMS send: the +CMGS
// command line and the payload write that follows it.
func sendWrites(writes []string) [][]string {
	sends := make([][]string, 0)
	for _, w := range writes {
		switch {
		case strings.HasPrefix(w, "AT+CMGS="):
			sends = append(sends, []string{w})
		case len(sends) > 0:
			sends[len(sends)-1] = append(sends[len(sends)-1], w)
		}
	}

	return sends
}

// sendVector is one pinned PDU exchange: the expected command line and the
// exact hex payload (SMSC + TPDU) the modem must receive, terminated by
// Ctrl-Z.
type sendVector struct {
	cmd     string
	payload string
}

func (v sendVector) writeLines() []string {
	return []string{"AT" + v.cmd + "\r", v.payload + smsCtrlZ}
}

// TestSendSMS_SinglePart pins the single-part send: "hello" (GSM-7, 5
// septets) becomes ONE PDU exchange carrying the phone as a TP-DA inside the
// TPDU - the phone never appears on the command line in PDU mode.
func TestSendSMS_SinglePart(t *testing.T) {
	commands, m := newCommands(t, smsResponder("1"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	refs, err := commands.SendSMS(ctx, testPhone, "hello")
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if len(refs) != 1 || refs[0] != 1 {
		t.Fatalf("refs = %v, want [1]", refs)
	}

	sends := sendWrites(m.getWrites())
	if len(sends) != 1 {
		t.Fatalf("got %d sends, want 1", len(sends))
	}
	want := sendVector{
		cmd:     "+CMGS=18",
		payload: "0001010b919799001032f4000005e8329bfd06",
	}
	if !equalStrings(sends[0], want.writeLines()) {
		t.Errorf("wire = %q, want %q", sends[0], want.writeLines())
	}
}

// TestSendSMS_UCS2Fallback pins the automatic UCS-2 fallback: a Cyrillic
// text cannot be GSM-7 encoded, so the DCS flips to UCS-2 (0x08) and each
// character takes two octets.
func TestSendSMS_UCS2Fallback(t *testing.T) {
	commands, m := newCommands(t, smsResponder("1"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	refs, err := commands.SendSMS(ctx, testPhone, "привет")
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if len(refs) != 1 || refs[0] != 1 {
		t.Fatalf("refs = %v, want [1]", refs)
	}

	sends := sendWrites(m.getWrites())
	if len(sends) != 1 {
		t.Fatalf("got %d sends, want 1", len(sends))
	}
	want := sendVector{
		cmd:     "+CMGS=25",
		payload: "0001010b919799001032f400080c043f04400438043204350442",
	}
	if !equalStrings(sends[0], want.writeLines()) {
		t.Errorf("wire = %q, want %q", sends[0], want.writeLines())
	}
}

// TestSendSMS_MultiPart pins the concatenated send: a 161-character text
// splits into two PDUs sharing one 8-bit concatenation reference (UDH IEI
// 0x00, UDHL 05, total 02) with per-part sequence numbers 01 and 02. Both
// parts are sent back-to-back as separate +CMGS exchanges and both message
// references are returned.
func TestSendSMS_MultiPart(t *testing.T) {
	commands, m := newCommands(t, smsResponder("10", "11"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	longText := strings.Repeat("a", 161)

	refs, err := commands.SendSMS(ctx, testPhone, longText)
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if len(refs) != 2 || refs[0] != 10 || refs[1] != 11 {
		t.Fatalf("refs = %v, want [10 11]", refs)
	}

	sends := sendWrites(m.getWrites())
	if len(sends) != 2 {
		t.Fatalf("got %d sends, want 2", len(sends))
	}
	want := []sendVector{
		{
			cmd:     "+CMGS=153",
			payload: "0041010b919799001032f40000a0050003010201c2e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3e170381c0e87c3",
		},
		{
			cmd:     "+CMGS=27",
			payload: "0041020b919799001032f400000f050003010202c2e170381c0e8701",
		},
	}
	for i, w := range want {
		if !equalStrings(sends[i], w.writeLines()) {
			t.Errorf("part %d wire = %q, want %q", i, sends[i], w.writeLines())
		}
	}
}

// TestSendSMS_ConcatRefIncrements pins the shared-encoder behavior: two
// back-to-back multi-part sends must not reuse the 8-bit concatenation
// reference, or a receiving phone could interleave the parts. The reference
// byte (offset 11 in the UDH, right after 050003) advances 01 -> 02.
func TestSendSMS_ConcatRefIncrements(t *testing.T) {
	commands, m := newCommands(t, smsResponder("1", "2", "3", "4"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	longText := strings.Repeat("a", 161)
	for i := range 2 {
		if _, err := commands.SendSMS(ctx, testPhone, longText); err != nil {
			t.Fatalf("SendSMS %d: %v", i, err)
		}
	}

	sends := sendWrites(m.getWrites())
	if len(sends) != 4 {
		t.Fatalf("got %d sends, want 4", len(sends))
	}
	wantRefs := []string{"010201", "010202", "020201", "020202"}
	for i, s := range sends {
		payload := s[1]
		if !strings.Contains(payload, "050003"+wantRefs[i]) {
			t.Errorf("part %d payload %q does not carry concat UDH ref/seq %q", i, payload, wantRefs[i])
		}
	}
}

// TestSendSMS_NotInitialized pins the init gate: a send against a Commands
// handle that never ran Init fails with ErrModemNotStarted and writes
// nothing to the wire.
func TestSendSMS_NotInitialized(t *testing.T) {
	m := &scriptedModem{
		responses: make(chan []byte, 256),
		onWrite:   func(string) [][]byte { return nil },
	}
	defer m.Close()

	a := at.New(m, at.WithTimeout(commandTimeout))
	commands := modem.NewCommands(a, newTestMetrics())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := commands.SendSMS(ctx, testPhone, "hello")
	if !errors.Is(err, modem.ErrModemNotStarted) {
		t.Fatalf("SendSMS error = %v, want ErrModemNotStarted", err)
	}
	if writes := m.getWrites(); len(writes) != 0 {
		t.Fatalf("wire writes = %q, want none before Init", writes)
	}
}

// TestSendSMS_ContextCanceled pins the ctx gate: a canceled context rejects
// the send before any encode or modem traffic.
func TestSendSMS_ContextCanceled(t *testing.T) {
	commands, m := newCommands(t, smsResponder("1"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := commands.SendSMS(ctx, testPhone, "hello")
	if err == nil {
		t.Fatal("SendSMS = nil error, want context error")
	}
	if writes := strings.Join(m.getWrites(), ""); strings.Contains(writes, "AT+CMGS=") {
		t.Fatalf("canceled send reached the +CMGS wire: %q", writes)
	}
}

// TestSendSMS_EmptyText pins the encode gate: an empty text fails with
// ErrInvalidText BEFORE any +CMGS traffic.
func TestSendSMS_EmptyText(t *testing.T) {
	commands, m := newCommands(t, smsResponder())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := commands.SendSMS(ctx, testPhone, "")
	if !errors.Is(err, modem.ErrInvalidText) {
		t.Fatalf("SendSMS error = %v, want ErrInvalidText", err)
	}
	if strings.Contains(strings.Join(m.getWrites(), ""), "AT+CMGS=") {
		t.Fatal("empty text reached the +CMGS wire")
	}
}

// TestSendSMS_ProtocolCeiling pins the 255-part hard cap: a text needing 256
// parts (the 8-bit sequence number would wrap to 0) is rejected before any
// modem traffic.
func TestSendSMS_ProtocolCeiling(t *testing.T) {
	commands, m := newCommands(t, smsResponder())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	longText := strings.Repeat("a", 255*153+1)

	_, err := commands.SendSMS(ctx, testPhone, longText)
	if !errors.Is(err, modem.ErrInvalidText) {
		t.Fatalf("SendSMS error = %v, want ErrInvalidText", err)
	}
	if !strings.Contains(err.Error(), "maximum is 255") {
		t.Fatalf("error %q does not name the protocol ceiling", err)
	}
	if strings.Contains(strings.Join(m.getWrites(), ""), "AT+CMGS=") {
		t.Fatal("over-ceiling text reached the +CMGS wire")
	}
}

// TestSendSMS_RejectedByModem pins the rejection path for a single-part
// send: a +CMS ERROR maps to ErrSendFailed and stays [errors.As]-able as an
// at.CMSError; no message reference is returned.
func TestSendSMS_RejectedByModem(t *testing.T) {
	respond := func(w string) [][]byte {
		if resps := initResponder(w); resps != nil {
			return resps
		}
		switch {
		case strings.HasPrefix(w, "AT+CMGS="):
			return [][]byte{[]byte("\r\n> ")}
		case strings.HasSuffix(w, smsCtrlZ):
			return [][]byte{okResponse("+CMS ERROR: 500")}
		}
		return nil
	}
	commands, _ := newCommands(t, respond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	refs, err := commands.SendSMS(ctx, testPhone, "hello")
	if err == nil {
		t.Fatal("SendSMS = nil error, want rejection")
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want none for a rejected send", refs)
	}
	if !errors.Is(err, modem.ErrSendFailed) {
		t.Fatalf("error = %v, want ErrSendFailed", err)
	}
	var cms at.CMSError
	if !errors.As(err, &cms) {
		t.Fatalf("error = %v, want an errors.As-able +CMS error", err)
	}
}

// TestSendSMS_MidSequenceRejection pins the partial-failure contract: when
// part 2 of 2 is rejected after part 1 was accepted, the reference of the
// accepted part is returned together with an error naming the failing part.
func TestSendSMS_MidSequenceRejection(t *testing.T) {
	commands, _ := newCommands(t, smsResponder("7"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	longText := strings.Repeat("a", 161)

	refs, err := commands.SendSMS(ctx, testPhone, longText)
	if err == nil {
		t.Fatal("SendSMS = nil error, want rejection of part 2")
	}
	if len(refs) != 1 || refs[0] != 7 {
		t.Fatalf("refs = %v, want the accepted part-1 reference [7]", refs)
	}
	if !strings.Contains(err.Error(), "part 2 of 2") {
		t.Fatalf("error %q does not name the failing part", err)
	}
	if !errors.Is(err, modem.ErrSendFailed) {
		t.Fatalf("error = %v, want ErrSendFailed", err)
	}
	var cms at.CMSError
	if !errors.As(err, &cms) {
		t.Fatalf("error = %v, want an errors.As-able +CMS error", err)
	}
}

// TestSendSMS_TimeoutDrain pins the lazy drain barrier on the send path: a
// +CMGS exchange that never completes times out with ErrModemTimeout and
// arms the drain; the next send drains the stale response window with a bare
// AT command before its own +CMGS traffic.
func TestSendSMS_TimeoutDrain(t *testing.T) {
	silentSends := 1
	respond := func(w string) [][]byte {
		switch {
		case silentSends > 0 && strings.HasPrefix(w, "AT+CMGS="):
			silentSends--
			return nil
		default:
			return smsResponder("21", "22")(w)
		}
	}
	commands, m := newCommands(t, respond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := commands.SendSMS(ctx, testPhone, strings.Repeat("a", 161))
	if !errors.Is(err, modem.ErrModemTimeout) {
		t.Fatalf("SendSMS error = %v, want ErrModemTimeout", err)
	}

	// The modem is healthy again: the next send must work...
	if _, retryErr := commands.SendSMS(ctx, testPhone, strings.Repeat("a", 161)); retryErr != nil {
		t.Fatalf("SendSMS after drain: %v", retryErr)
	}

	// ...and the drain must have issued a bare AT command between the timed
	// out send and the retried one.
	writes := strings.Join(m.getWrites(), "\n")
	if !strings.Contains(writes, "AT\r\n") {
		t.Fatal("no bare AT drain command between the timed-out send and the retry")
	}
}

// TestSendSMS_NoCMGSLine pins the malformed-response path: a modem that
// acknowledges with OK but never emits a +CMGS reference line yields the
// errNoCMGSLine failure message.
func TestSendSMS_NoCMGSLine(t *testing.T) {
	respond := func(w string) [][]byte {
		if resps := initResponder(w); resps != nil {
			return resps
		}
		switch {
		case strings.HasPrefix(w, "AT+CMGS="):
			return [][]byte{[]byte("\r\n> ")}
		case strings.HasSuffix(w, smsCtrlZ):
			return [][]byte{okResponse("OK")}
		}
		return nil
	}
	commands, _ := newCommands(t, respond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := commands.SendSMS(ctx, testPhone, "hello")
	if err == nil {
		t.Fatal("SendSMS = nil error, want missing-reference failure")
	}
	if !strings.Contains(err.Error(), "no +CMGS line in response") {
		t.Fatalf("error %q does not describe the missing reference line", err)
	}
}

// equalStrings compares two string slices element-wise.
func equalStrings(a, b []string) bool {
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

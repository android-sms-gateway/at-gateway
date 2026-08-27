package modem

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/warthog618/modem/at"
)

// initCommand describes one row of the modem init sequence.
type initCommand struct {
	cmd     string // library command form (no AT prefix, no CRLF)
	display string // wire form used in error messages and tags
	tag     string
}

// Commands wraps a warthog618/modem AT handle with domain behavior.
type Commands struct {
	at *at.AT

	// metrics is REQUIRED (non-nil): exec() records CommandsTotal +
	// CommandDuration for every library command (incl. drains).
	metrics *Metrics

	// mu serializes barrier-check + drain + command execution. The library
	// cmdCh serializes only the wire, not the barrier state.
	mu sync.Mutex
	// pendingDrain is set when a command failed with ErrDeadlineExceeded and
	// stale response lines may still be queued; the next command call drains
	// them first (lazy drain barrier).
	pendingDrain bool
	// initialized is set by a successful Init and gates the send path:
	// SendSMS refuses to run against a modem that was never initialized.
	initialized bool
}

// NewCommands creates a new Commands instance bound to the library AT handle.
// metrics is required (non-nil) for command telemetry.
func NewCommands(at *at.AT, metrics *Metrics) *Commands {
	return &Commands{
		at:           at,
		metrics:      metrics,
		mu:           sync.Mutex{},
		pendingDrain: false,
		initialized:  false,
	}
}

// Init runs the boot init sequence in exact order: AT, ATE0, +CMEE=1,
// +CMGF=1, +CNMI=2,1,0,0,0 and the +CPIN? READY gate.
//
// The ctx parameter is INERT per command: the library has no context support,
// so an in-flight command always runs to its own per-command timeout. ctx is
// checked BEFORE the first row and BETWEEN rows, so cancellation aborts the
// sequence at the next row boundary.
func (c *Commands) Init(ctx context.Context) error {
	commands := []initCommand{
		{cmd: "", display: "AT", tag: "test"},
		{cmd: "E0", display: "ATE0", tag: "echo off"},
		{cmd: "+CMEE=1", display: "AT+CMEE=1", tag: "verbose errors"},
		{cmd: "+CMGF=1", display: "AT+CMGF=1", tag: "text mode"},
		{cmd: "+CNMI=2,1,0,0,0", display: "AT+CNMI=2,1,0,0,0", tag: "SMS routing"},
		{cmd: "+CPIN?", display: "AT+CPIN?", tag: "SIM PIN"},
	}

	for _, cmd := range commands {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, ErrModemTimeout)
		default:
		}

		lines, err := c.exec(cmd)
		if err != nil {
			return err
		}
		if cmd.tag == "SIM PIN" {
			for _, line := range lines {
				if suffix, ok := strings.CutPrefix(line, "+CPIN:"); ok {
					status := strings.TrimSpace(suffix)
					if status != "READY" {
						return fmt.Errorf("%s: %s: %w", cmd.tag, status, ErrSIMNotReady)
					}
				}
			}
		}
	}

	// The modem is ready for the send path only after the full sequence
	// (incl. the +CPIN READY gate) completed.
	c.initialized = true

	return nil
}

// GetModemInfo queries the modem manufacturer, model and IMEI via GMI/GMM/GSN
// (first response line each). A failing field returns the partial info
// collected so far plus a wrapped error ("manufacturer:" / "model:" /
// "IMEI:"); later fields are not queried.
//
// The ctx parameter is INERT per command (the library has no context support):
// it is checked between commands only, so cancellation aborts the sequence at
// the next field boundary with the field-wrapped error.
func (c *Commands) GetModemInfo(ctx context.Context) (Info, error) {
	info := Info{
		Manufacturer: "",
		Model:        "",
		IMEI:         "",
	}

	manufacturer, err := c.atGetString(ctx, "+GMI")
	if err != nil {
		return info, fmt.Errorf("manufacturer: %w", err)
	}
	info.Manufacturer = manufacturer

	model, err := c.atGetString(ctx, "+GMM")
	if err != nil {
		return info, fmt.Errorf("model: %w", err)
	}
	info.Model = model

	imei, err := c.atGetString(ctx, "+GSN")
	if err != nil {
		return info, fmt.Errorf("IMEI: %w", err)
	}
	info.IMEI = imei

	return info, nil
}

// GetSimInfo queries CNUM/CCID/COPS/CSQ/CREG with swallow-error semantics:
// failed or malformed queries yield empty/zero fields and never an error.
//
// The ctx parameter is INERT per command (the library has no context support):
// it is checked between commands only; once canceled, the remaining fields
// stay zeroed.
func (c *Commands) GetSimInfo(ctx context.Context) (SimInfo, error) {
	info := SimInfo{
		PhoneNumber:       "",
		ICCID:             "",
		Carrier:           "",
		NetworkRegistered: false,
		SignalQuality:     0,
		SignalPercent:     0,
	}

	info.PhoneNumber = c.atGetCNUM(ctx)
	info.ICCID = c.atGetFirstLine(ctx, "+CCID")
	info.Carrier = c.atGetCOPS(ctx)
	info.SignalQuality, info.SignalPercent = c.atGetCSQ(ctx)
	info.NetworkRegistered = c.atGetCREG(ctx)

	return info, nil
}

// errNoCSQLine is returned by GetSignal when the +CSQ response carries no
// +CSQ line (malformed/absent telemetry).
var errNoCSQLine = errors.New("no +CSQ line in response")

// GetSignal queries the signal quality via +CSQ and returns (signalQuality,
// signalPercent, err). A malformed or absent +CSQ line, or a command error,
// returns (0, 0, error); GetSimInfo's swallow path returns (0, 0, nil) for
// the same condition - the two paths share parseCSQ but do not couple
// behavior. The ctx parameter is INERT per command (the library has no
// context support): it is checked before the query only.
func (c *Commands) GetSignal(ctx context.Context) (int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, fmt.Errorf("signal quality: %w", err)
	}

	lines, err := c.exec(queryCommand("+CSQ"))
	if err != nil {
		return 0, 0, err
	}
	for _, line := range lines {
		if _, after, found := strings.Cut(line, "+CSQ:"); found {
			quality, percent, perr := c.parseCSQ(after)
			if perr != nil {
				return 0, 0, fmt.Errorf("signal quality: %w", perr)
			}

			return quality, percent, nil
		}
	}

	return 0, 0, errNoCSQLine
}

// queryCommand builds the init-command descriptor used by query paths: the
// library command form (no AT prefix), the wire display form for error text
// and the command tag.
func queryCommand(cmd string) initCommand {
	return initCommand{cmd: cmd, display: "AT" + cmd, tag: cmd}
}

// atGetString returns the trimmed first response line of a query command, or
// an error when the command failed.
func (c *Commands) atGetString(ctx context.Context, cmd string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", cmd, err)
	}

	lines, err := c.exec(queryCommand(cmd))
	if err != nil {
		return "", err
	}
	if len(lines) > 0 {
		return strings.TrimSpace(lines[0]), nil
	}

	return "", nil
}

// atGetFirstLine returns the trimmed first response line of a query command,
// or "" on error or an empty response.
func (c *Commands) atGetFirstLine(ctx context.Context, cmd string) string {
	if ctx.Err() != nil {
		return ""
	}

	lines, err := c.exec(queryCommand(cmd))
	if err != nil || len(lines) == 0 {
		return ""
	}

	return strings.TrimSpace(lines[0])
}

// atGetCNUM extracts the phone number (field 1) from the first +CNUM line.
func (c *Commands) atGetCNUM(ctx context.Context) string {
	if ctx.Err() != nil {
		return ""
	}

	lines, err := c.exec(queryCommand("+CNUM"))
	if err != nil {
		return ""
	}
	for _, line := range lines {
		if _, after, found := strings.Cut(line, "+CNUM:"); found {
			parts := strings.Split(after, ",")
			if len(parts) >= 2 { //nolint:mnd // CNUM format: name,number,type
				return strings.Trim(parts[1], "\"")
			}
		}
	}

	return ""
}

// atGetCOPS extracts the carrier name (field 2) from the first +COPS line.
func (c *Commands) atGetCOPS(ctx context.Context) string {
	if ctx.Err() != nil {
		return ""
	}

	lines, err := c.exec(queryCommand("+COPS?"))
	if err != nil {
		return ""
	}
	for _, line := range lines {
		if _, after, found := strings.Cut(line, "+COPS:"); found {
			parts := strings.Split(after, ",")
			if len(parts) >= 3 { //nolint:mnd // COPS format: mode,format,name,act
				return strings.Trim(parts[2], "\"")
			}
		}
	}

	return ""
}

// atGetCSQ returns the signal quality and percent from the first +CSQ line,
// or (0, 0) on any failure or malformed value (swallow path).
func (c *Commands) atGetCSQ(ctx context.Context) (int, int) {
	if ctx.Err() != nil {
		return 0, 0
	}

	lines, err := c.exec(queryCommand("+CSQ"))
	if err != nil || len(lines) == 0 {
		return 0, 0
	}
	for _, line := range lines {
		if _, after, found := strings.Cut(line, "+CSQ:"); found {
			quality, percent, _ := c.parseCSQ(after)

			return quality, percent
		}
	}

	return 0, 0
}

// atGetCREG reports whether the first +CREG line shows network registration
// (stat 1 or 5), or false on any failure.
func (c *Commands) atGetCREG(ctx context.Context) bool {
	if ctx.Err() != nil {
		return false
	}

	lines, err := c.exec(queryCommand("+CREG?"))
	if err != nil {
		return false
	}
	for _, line := range lines {
		if _, after, found := strings.Cut(line, "+CREG:"); found {
			parts := strings.Split(after, ",")
			if len(parts) >= 2 { //nolint:mnd // CREG format: stat,...
				status := strings.TrimSpace(parts[1])

				return status == "1" || status == "5"
			}
		}
	}

	return false
}

// parseCSQ parses the raw +CSQ payload (rssi[,ber]) into a signal quality and
// a 0-100 percent. A malformed rssi returns an error; out-of-range values
// keep the legacy behavior (rssi, 0).
func (c *Commands) parseCSQ(raw string) (int, int, error) {
	val := strings.TrimSpace(strings.SplitN(raw, ",", 2)[0]) //nolint:mnd // always 2 parts
	rssi, err := strconv.Atoi(val)
	if err != nil {
		return 0, 0, fmt.Errorf("rssi: %w", err)
	}
	if rssi < 0 || rssi > 31 {
		return rssi, 0, nil
	}

	return rssi, rssi * 100 / 31, nil //nolint:mnd // 100% scale
}

// exec runs one command through the lazy drain barrier.
//
// If a previous command failed with ErrDeadlineExceeded, one drain
// Command("") is issued first (result discarded) so stale response lines
// cannot corrupt the new response. A timed-out drain fails the WHOLE call
// (abort-on-first-drain-timeout: GetSimInfo returns an error and the
// remaining commands are SKIPPED) and KEEPS the pending-drain state; a
// non-deadline drain outcome clears it (the ERROR status terminates the
// response; residual stale lines fall into the documented multi-line
// degradation class).
//
// WEDGED GETSIMINFO ESTIMATES (pinned): persistent wedge worst case ~10s
// (1 command timeout + 1 failed drain); recovering-modem case ~30s
// (5 commands x 5s + 1 x 5s successful drain).
// drainLocked runs one bare-AT drain command when the lazy drain barrier is
// set; the caller must hold c.mu. It returns true when the drain timed out
// (stale lines persist: the whole call must fail and the barrier stays set).
// A non-deadline drain outcome clears the barrier (the ERROR status
// terminates the response; residual stale lines fall into the documented
// multi-line degradation class).
func (c *Commands) drainLocked() bool {
	if !c.pendingDrain {
		return false
	}

	start := time.Now()
	_, err := c.at.Command("")
	c.metrics.CommandDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		// The drain is a bare AT command: label command "".
		c.metrics.CommandsTotal.WithLabelValues("", "error").Inc()
		if errors.Is(err, at.ErrDeadlineExceeded) {
			// Stale lines persist: fail the whole call, keep the barrier.
			return true
		}
		c.pendingDrain = false

		return false
	}

	c.metrics.CommandsTotal.WithLabelValues("", "ok").Inc()
	c.pendingDrain = false

	return false
}

func (c *Commands) exec(cmd initCommand) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.drainLocked() {
		return nil, fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, ErrModemTimeout)
	}

	start := time.Now()
	lines, err := c.at.Command(cmd.cmd)
	c.metrics.CommandDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		c.metrics.CommandsTotal.WithLabelValues(cmd.tag, "error").Inc()

		return nil, c.mapCommandError(cmd, err)
	}

	c.metrics.CommandsTotal.WithLabelValues(cmd.tag, "ok").Inc()

	return lines, nil
}

// mapCommandError maps library errors onto domain errors, preserving the
// "tag (cmd):" prefix so pre-swap lock tests (prefix + non-nil) stay green.
// The generic-ERROR/+CME/+CMS branch keeps BOTH sentinels: ErrInitFailed stays
// [errors.Is]-able AND the library error (at.CMEError/at.CMSError with its code)
// stays [errors.As]-able via the trailing %w.
func (c *Commands) mapCommandError(cmd initCommand, err error) error {
	var cme at.CMEError
	var cms at.CMSError
	switch {
	case errors.Is(err, at.ErrDeadlineExceeded):
		c.pendingDrain = true
		return fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, ErrModemTimeout)
	case errors.Is(err, at.ErrError), errors.As(err, &cme), errors.As(err, &cms):
		return fmt.Errorf("%s (%s): %w: %w", cmd.tag, cmd.display, ErrInitFailed, err)
	default:
		// ErrClosed and any other library error: tag-prefix wrap only, no
		// sentinel mapping (ErrModemNotStarted untouched).
		return fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, err)
	}
}

// SendSMS sends one text-mode SMS via AT+CMGS using the library's two-step
// SMS flow: the command line with the QUOTED phone number (3GPP TS 27.005
// requires <da> in quotes: AT+CMGS="+79990001234"; SIM800L rejects the
// unquoted form with +CMS ERROR; the library passes the command through
// verbatim), then - after the ">" prompt - the payload terminated by Ctrl-Z
// (the library appends the 0x1A terminator itself; no raw port fallback is
// needed). It returns the message reference <mr> parsed from the +CMGS
// response.
//
// A phone containing a quote, CR or LF would corrupt the command line and is
// rejected with ErrInvalidPhone before any modem traffic.
//
// Errors are mapped like the query path: a +CMS/+CME ERROR or generic ERROR
// wraps ErrSendFailed (the +CMS/+CME code stays [errors.As]-able), a per-command
// deadline wraps ErrModemTimeout and sets the lazy drain barrier (execSMS runs
// through the same drain semantics as exec). The ctx is INERT per command (the
// library has no context support): it is checked before the send only.
//
// SMSSentTotal is incremented ONLY when the +CMGS response yields a message
// reference - a completed send, never a validation or queued message.
func (c *Commands) SendSMS(ctx context.Context, phoneNumber, text string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("send SMS: %w", err)
	}
	if !c.initialized {
		return 0, fmt.Errorf("send SMS: %w", ErrModemNotStarted)
	}
	if strings.ContainsAny(phoneNumber, "\"\r\n") {
		return 0, fmt.Errorf("send SMS: %w: phone %q must not contain quotes, CR or LF", ErrInvalidPhone, phoneNumber)
	}

	cmd := initCommand{
		cmd:     "+CMGS=\"" + phoneNumber + "\"",
		display: "AT+CMGS=\"" + phoneNumber + "\"",
		tag:     "send SMS",
	}
	lines, err := c.execSMS(cmd, text)
	if err != nil {
		return 0, err
	}
	// The +CMGS: indication head can leak into this response as an unknown
	// info line (v0.4.0 indLoop forwards heads after handlers; with CNMI
	// active a +CMT head may precede our +CMGS line). Match ONLY lines that
	// actually START with "+CMGS:" via CutPrefix, so a mid-line "+CMGS:"
	// substring in a leaked head can never be parsed as the reference.
	for _, line := range lines {
		if suffix, ok := strings.CutPrefix(line, "+CMGS:"); ok {
			mr, perr := strconv.Atoi(strings.TrimSpace(suffix))
			if perr != nil {
				return 0, fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, perr)
			}
			c.metrics.SMSSentTotal.Inc()

			return mr, nil
		}
	}

	return 0, fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, errNoCMGSLine)
}

// execSMS runs one SMS send command through the lazy drain barrier, mirroring
// exec() but using the library's two-step SMS flow (SMSCommand: command line,
// ">" prompt, payload + Ctrl-Z). Metrics follow exec() exactly: the send is
// counted with its tag, the drain with the "" command label.
func (c *Commands) execSMS(cmd initCommand, payload string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.drainLocked() {
		return nil, fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, ErrModemTimeout)
	}

	start := time.Now()
	lines, err := c.at.SMSCommand(cmd.cmd, payload)
	c.metrics.CommandDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		c.metrics.CommandsTotal.WithLabelValues(cmd.tag, "error").Inc()

		return nil, c.mapSMSError(cmd, err)
	}

	c.metrics.CommandsTotal.WithLabelValues(cmd.tag, "ok").Inc()

	return lines, nil
}

// mapSMSError maps library SMS-command errors onto domain errors, preserving
// the "tag (cmd):" prefix like mapCommandError. A deadline sets the lazy
// drain barrier; a generic ERROR or +CMS/+CME ERROR wraps ErrSendFailed and
// KEEPS the library error in the chain (trailing %w) so the +CMS/+CME code
// stays [errors.As]-able.
func (c *Commands) mapSMSError(cmd initCommand, err error) error {
	var cme at.CMEError
	var cms at.CMSError
	switch {
	case errors.Is(err, at.ErrDeadlineExceeded):
		c.pendingDrain = true
		return fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, ErrModemTimeout)
	case errors.Is(err, at.ErrError), errors.As(err, &cme), errors.As(err, &cms):
		return fmt.Errorf("%s (%s): %w: %w", cmd.tag, cmd.display, ErrSendFailed, err)
	default:
		// ErrClosed and any other library error: tag-prefix wrap only.
		return fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, err)
	}
}

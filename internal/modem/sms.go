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
	"github.com/warthog618/sms"
	"github.com/warthog618/sms/encoding/pdumode"
	"github.com/warthog618/sms/encoding/tpdu"
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

	// encoder converts texts to SMS-SUBMIT TPDUs. The encoder is SHARED across
	// SendSMS calls so its internal concatenation-reference and MR counters
	// stay monotonic: back-to-back multi-part messages to the same phone must
	// not reuse the same 8-bit concatenation reference.
	encoder *sms.Encoder

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
		encoder:      sms.NewEncoder(sms.AsSubmit),
		mu:           sync.Mutex{},
		pendingDrain: false,
		initialized:  false,
	}
}

// Init runs the boot init sequence in exact order: AT, ATE0, +CMEE=1,
// +CMGF=0, +CNMI=2,1,0,1,0 and the +CPIN? READY gate. The modem is left in
// PDU mode (+CMGF=0): text mode cannot carry a user data header, so every
// send - single- and multi-part alike - is a PDU exchange. The +CNMI <ds>=1
// routes SMS-STATUS-REPORT URCs (+CDS) to the TE; reports are only ever
// generated when a send requests one (TP-SRR), so the routing change is
// inert for sends without a delivery report.
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
		{cmd: "+CMGF=0", display: "AT+CMGF=0", tag: "PDU mode"},
		{cmd: "+CNMI=2,1,0,1,0", display: "AT+CNMI=2,1,0,1,0", tag: "SMS routing"},
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

// SendSMS sends a PDU-mode SMS via AT+CMGS using the library's two-step SMS
// flow: the command line carrying the TPDU LENGTH in octets
// (AT+CMGS=<length>; 3GPP TS 27.005 PDU mode), then - after the ">" prompt -
// the hex-encoded PDU (SMSC + TPDU) terminated by Ctrl-Z (the library appends
// the 0x1A terminator itself; no raw port fallback is needed).
//
// The text is encoded into SMS-SUBMIT TPDUs with the shared encoder (GSM-7
// default alphabet; text outside it falls back to UCS-2). Text longer than a
// single part is split into CONCATENATED parts carrying the 8-bit
// concatenation UDH (IEI 0x00) so the receiving phone reassembles them; each
// part is one AT+CMGS exchange. All parts are encoded BEFORE the first modem
// traffic, so encoding failures (e.g. an empty text or a text exceeding the
// 255-part protocol ceiling) reject the whole send without side effects. The
// phone number never appears on the command line in PDU mode, so the
// text-mode quote/CR/LF injection guard does not apply.
//
// When withDeliveryReport is true, the TP-SRR bit of the LAST part is set
// (the modem requests a status report from the SC for that part; the SC
// never generates reports for parts that do not request one). The last part
// is the single part whose reference the +CMGS responses expose to callers:
// the whole concatenated message counts as delivered when the last part
// reached the phone, and the report's TP-MR matches the last returned
// reference. Earlier parts never carry TP-SRR, so a recipient produces at
// most one report and the report cannot be mis-attributed to a sibling part.
//
// It returns one message reference <mr> per part ACCEPTED by the modem. On a
// mid-sequence failure (a part rejected after earlier parts were accepted)
// the references of the accepted parts are returned together with an error
// naming the failing part; a failure of the only part returns the bare mapped
// error. Callers must treat a recipient as failed on any error - earlier
// parts may already be in the network.
//
// Errors are mapped like the query path: a +CMS/+CME ERROR or generic ERROR
// wraps ErrSendFailed (the +CMS/+CME code stays [errors.As]-able), a per-command
// deadline wraps ErrModemTimeout and sets the lazy drain barrier (execSMS runs
// through the same drain semantics as exec). The ctx is INERT per command (the
// library has no context support): it is checked before the encode and between
// parts only.
func (c *Commands) SendSMS(ctx context.Context, phoneNumber, text string, withDeliveryReport bool) ([]int, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("send SMS: %w", err)
	}
	if !c.initialized {
		return nil, fmt.Errorf("send SMS: %w", ErrModemNotStarted)
	}

	pdus, err := c.encoder.Encode([]byte(text), sms.To(phoneNumber))
	if err != nil {
		return nil, fmt.Errorf("send SMS: encode: %w", err)
	}
	if len(pdus) == 0 {
		return nil, fmt.Errorf("send SMS: %w: text is empty", ErrInvalidText)
	}
	if len(pdus) > smsMaxParts {
		return nil, fmt.Errorf(
			"send SMS: %w: text is %d parts long, maximum is %d",
			ErrInvalidText,
			len(pdus),
			smsMaxParts,
		)
	}

	// Request the status report on the LAST part only: the SC answers one
	// report per TP-SRR request and only the last part's reference is exposed
	// to callers (see the method docs), keeping the report-to-recipient
	// mapping 1:1. The bit is OR-ed after the shared encoder ran, so the
	// encoder's internal state stays untouched.
	if withDeliveryReport {
		pdus[len(pdus)-1].FirstOctet |= tpdu.FoSRR
	}

	refs := make([]int, 0, len(pdus))
	for i, p := range pdus {
		select {
		case <-ctx.Done():
			return c.partialRefs(refs, i, len(pdus), ctx.Err())
		default:
		}

		ref, sendErr := c.sendPDU(p)
		if sendErr != nil {
			return c.partialRefs(refs, i, len(pdus), sendErr)
		}
		refs = append(refs, ref)
	}

	return refs, nil
}

// sendPDU sends one already-encoded TPDU part and returns its message
// reference parsed from the +CMGS response. Failures are returned bare; the
// caller (partialRefs) adds the part context when the message has several
// parts.
func (c *Commands) sendPDU(p tpdu.TPDU) (int, error) {
	tp, err := p.MarshalBinary()
	if err != nil {
		return 0, fmt.Errorf("marshal TPDU: %w", err)
	}

	// The PDU carries an empty SMSC address (the zero value marshals to a
	// single 0x00 length octet): the modem fills in the SIM's service centre.
	var pdu pdumode.PDU
	pdu.TPDU = tp
	hexPayload, err := pdu.MarshalHexString()
	if err != nil {
		return 0, fmt.Errorf("marshal PDU: %w", err)
	}

	cmd := initCommand{
		cmd:     "+CMGS=" + strconv.Itoa(len(tp)),
		display: "AT+CMGS=" + strconv.Itoa(len(tp)),
		tag:     "send SMS",
	}
	lines, err := c.execSMS(cmd, hexPayload)
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

			return mr, nil
		}
	}

	return 0, fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, errNoCMGSLine)
}

// partialRefs shapes a part failure: for a multi-part message the cause is
// wrapped with the failing part index, and the references of the parts
// accepted before the failure are returned alongside the error. A single-part
// message keeps the bare cause and nil refs.
func (c *Commands) partialRefs(refs []int, failing, total int, cause error) ([]int, error) {
	if total == 1 {
		return nil, cause
	}

	return refs, fmt.Errorf("part %d of %d: %w", failing+1, total, cause)
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

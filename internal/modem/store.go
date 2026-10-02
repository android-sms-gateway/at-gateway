package modem

import (
	"fmt"
	"strconv"
	"strings"
)

// storeCommandLabel is the metrics <op> label of every command that mutates
// the modem message store.
const storeCommandLabel = "cmgd"

// storeRefFieldCount is the number of comma-separated fields a "+CMTI:
// <mem>,<index>" head must carry to name a store entry.
const storeRefFieldCount = 2

// storePDUFieldIndex is the 0-based position of the inline PDU inside a
// "+CMGR: <stat>,<length>[,<pdu>]" header (3GPP TS 27.005 3.4.2): the body is
// the THIRD field, so it exists only from that index on.
const storePDUFieldIndex = 2

// parseStoredReference parses the "+CMTI: <mem>,<index>" indication head into
// the numeric memory and index arguments of +CMGR/+CMGD. <mem> is a quoted or
// unquoted name; an unknown name or a malformed index is an error.
func parseStoredReference(head string) (int, int, error) {
	_, payload, found := strings.Cut(head, "+CMTI:")
	if !found {
		return 0, 0, fmt.Errorf("%w: %q", ErrNotStoredReference, head)
	}

	fields := strings.Split(payload, ",")
	if len(fields) < storeRefFieldCount {
		return 0, 0, fmt.Errorf("%w: %q", ErrNotStoredReference, head)
	}

	memory, err := storedMemoryID(strings.Trim(strings.TrimSpace(fields[0]), `"`))
	if err != nil {
		return 0, 0, err
	}

	index, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: index: %s", ErrNotStoredReference, err.Error())
	}
	if index < 0 {
		return 0, 0, fmt.Errorf("%w: negative index %d", ErrNotStoredReference, index)
	}

	return memory, index, nil
}

// storedMemoryID maps a modem memory name to the numeric <mem> argument.
func storedMemoryID(name string) (int, error) {
	switch strings.ToUpper(name) {
	case "ME", "SM":
		return 0, nil
	case "MT":
		return 1, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrNotStoredReference, name)
	}
}

// dropURCHeads returns the response lines that belong to the command whose
// expected info prefix is want: every '+'-prefixed line that does NOT start
// with want is dropped.
//
// WHY (warthog618/modem v0.4.0): the library leaks unsolicited result codes
// into command responses. Its indication loop forwards a MATCHED indication
// head line into the response channel anyway - the `continue` after
// `go ind.handler(n)` binds to the inner prefix loop, so `out <- line` is
// still reached - so registering +CMTI/+CMT/+CDS does NOT suppress the leak.
// parseRxLine maps every '+'-prefixed line that is not OK/ERROR/CONNECT to an
// info line, so URCs this gateway never registers (+CREG, +CBM, +CCINFO,
// +CEREG, +CLIP, +COLP) leak as well. A positional parse (a status header
// line paired with its body line) would shift and silently corrupt, so the
// heads must be DROPPED before pairing - scanning past them is not enough.
//
// RESIDUAL: lines that do not start with '+' (unsolicited junk) stay outside
// this predicate; the +CMGS send path has always carried the same exposure.
func dropURCHeads(lines []string, want string) []string {
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, want) {
			continue
		}
		filtered = append(filtered, line)
	}

	return filtered
}

// storedPDU extracts the hex PDU body from the response of a store read. After
// the unsolicited heads are dropped (see dropURCHeads) the parse is
// POSITIONAL: the first surviving line is the response header
// "<prefix><stat>,<length>[,<pdu>]" (3GPP TS 27.005 3.4.2), and a modem that
// omits the inline PDU puts the body on the following line.
func storedPDU(lines []string, want string) (string, bool) {
	filtered := dropURCHeads(lines, want)
	if len(filtered) == 0 {
		return "", false
	}

	header, found := strings.CutPrefix(filtered[0], want)
	if !found {
		return "", false
	}

	fields := strings.Split(header, ",")
	if len(fields) > storePDUFieldIndex {
		if pdu := strings.TrimSpace(fields[storePDUFieldIndex]); pdu != "" {
			return pdu, true
		}
	}
	if len(filtered) > 1 {
		if body := strings.TrimSpace(filtered[1]); body != "" {
			return body, true
		}
	}

	return "", false
}

// ReadStoredMessage reads the hex PDU of the message stored at (memory, index)
// through AT+CMGR. The response head is parsed POSITIONALLY, so unsolicited
// result codes are dropped first (see dropURCHeads). A response without a
// parseable PDU body fails with errNoStoredMessageLine.
func (c *Commands) ReadStoredMessage(memory, index int) (string, error) {
	cmd := initCommand{
		cmd:     "+CMGR=" + strconv.Itoa(memory) + "," + strconv.Itoa(index),
		display: "AT+CMGR=" + strconv.Itoa(memory) + "," + strconv.Itoa(index),
		tag:     "read stored message",
	}

	lines, err := c.exec(cmd)
	if err != nil {
		return "", err
	}

	pdu, found := storedPDU(lines, "+CMGR:")
	if !found {
		return "", fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, errNoStoredMessageLine)
	}

	return pdu, nil
}

// DeleteStoredMessage removes the message stored at (memory, index) through
// AT+CMGD. The caller owns the retry policy (see Service); this is one attempt.
func (c *Commands) DeleteStoredMessage(memory, index int) error {
	cmd := initCommand{
		cmd:     "+CMGD=" + strconv.Itoa(memory) + "," + strconv.Itoa(index),
		display: "AT+CMGD=" + strconv.Itoa(memory) + "," + strconv.Itoa(index),
		tag:     "delete stored message",
	}

	if _, err := c.exec(cmd); err != nil {
		return fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, err)
	}

	return nil
}

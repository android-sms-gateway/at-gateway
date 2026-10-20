package modem

import (
	"encoding/hex"
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

// storePDUFieldIndex is the 0-based position of a PDU carried INLINE in a
// "+CMGR: <stat>,<alpha>,<length>,<pdu>" header (3GPP TS 27.005 3.4.2 makes
// <alpha> optional and the SIM800 emits it - captured head `+CMGR: 1,"",23`).
// The ALPHA field shifts every later field one slot right: index 2 holds
// <length>, not the PDU. Reading index 2 anyway returned the LENGTH as the
// PDU ("23" -> one octet 0x23 -> SMSC length 35 -> "decode stored PDU: tpdu:
// error decoding toa at octet 1: underflow"), dropping every inbound message.
// The PDU normally follows the header on its own line (see storedPDU); this
// index is the header-only fallback.
const storePDUFieldIndex = 3

// parseStoredReference parses the "+CMTI: <mem>,<index>" indication head into
// the numeric memory and index arguments of +CMGR/+CMGD. <mem> is a quoted or
// unquoted name; an unknown name or a malformed index is an error.
//
// <mem> is NOT part of the +CMGR or +CMGD command line (see ReadStoredMessage
// and DeleteStoredMessage) - it still names the store for the storedRef retry
// budget and for the drop logs.
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
// "<prefix><stat>,<alpha>,<length>" (3GPP TS 27.005 3.4.2) and the PDU body
// FOLLOWS it on the next line - per the standard that body line is
// authoritative whenever it is present and non-empty. Only when the response
// carries NO body line does the parse fall back to a PDU carried inline in
// the header at storePDUFieldIndex, and that candidate must actually look
// like a PDU (non-empty even-length hex) so the <length> field can never be
// returned as if it were one.
func storedPDU(lines []string, want string) (string, bool) {
	filtered := dropURCHeads(lines, want)
	if len(filtered) == 0 {
		return "", false
	}

	header, found := strings.CutPrefix(filtered[0], want)
	if !found {
		return "", false
	}

	if len(filtered) > 1 {
		if body := strings.TrimSpace(filtered[1]); body != "" {
			return body, true
		}
	}

	fields := strings.Split(header, ",")
	if len(fields) <= storePDUFieldIndex {
		return "", false
	}
	pdu := strings.TrimSpace(fields[storePDUFieldIndex])
	if pdu == "" {
		return "", false
	}
	if _, err := hex.DecodeString(pdu); err != nil {
		return "", false
	}

	return pdu, true
}

// ReadStoredMessage reads the hex PDU of the message stored at index through
// AT+CMGR. The response head is parsed POSITIONALLY, so unsolicited result
// codes are dropped first (see dropURCHeads). A response without a parseable
// PDU body fails with errNoStoredMessageLine.
//
// +CMGR takes a SINGLE <index> argument (3GPP TS 27.005 3.4.2; the SIM800
// Series AT Command Manual spells it "AT+CMGR=<index>[,<mode>]"), because the
// memory is the PREFERRED storage selected with +CPMS - the "<mem>,<index>"
// pair belongs to +CMGL/+CMGD in the standard. Sending the memory prefix anyway
// is not merely ignored by a SIMCOM module: it is rejected outright as an
// invalid memory index, so every inbound read failed with
//
//	read stored message failed {"error": "read stored message (AT+CMGR=0,3):
//	modem initialization failed: CMS Error: 321"}
func (c *Commands) ReadStoredMessage(index int) (string, error) {
	cmd := initCommand{
		cmd:     "+CMGR=" + strconv.Itoa(index),
		display: "AT+CMGR=" + strconv.Itoa(index),
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

// DeleteStoredMessage removes the message stored at index through AT+CMGD.
// The caller owns the retry policy (see Service); this is one attempt.
//
// +CMGD takes a SINGLE <index> argument from the preferred message storage
// (SIM800 Series AT Command Manual V1.10 sec 4.2.1: "AT+CMGD=<index>
// [,<delflag>]"; the manual's own examples are AT+CMGD=1, AT+CMGD=3). There is
// NO "<mem>,<index>" form. The memory prefix is not merely ignored by a
// SIMCOM module: the modem parses the MESSAGE INDEX as <delflag>, so index 1-3
// trigger bulk category deletes and index 4 deletes ALL stored messages -
// a data-destruction path on the inbound flow. Do not "restore" the memory
// prefix. The optional <delflag> is omitted on purpose: delflag defaults to 0,
// which deletes ONLY the specified index - exactly the semantic this path
// needs (read one, delete that one).
func (c *Commands) DeleteStoredMessage(index int) error {
	cmd := initCommand{
		cmd:     "+CMGD=" + strconv.Itoa(index),
		display: "AT+CMGD=" + strconv.Itoa(index),
		tag:     "delete stored message",
	}

	if _, err := c.exec(cmd); err != nil {
		return fmt.Errorf("%s (%s): %w", cmd.tag, cmd.display, err)
	}

	return nil
}

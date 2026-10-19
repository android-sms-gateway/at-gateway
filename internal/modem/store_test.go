package modem_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
)

// storeSelectModem records every +CPMS selection and answers the store
// commands, so a test can assert WHICH preferred store each read/delete/list
// selected and that the selection is CACHED across operations on the same
// store.
type storeSelectModem struct {
	selections []string
	cmglLines  []string
}

func (m *storeSelectModem) respond(w string) [][]byte {
	if mem, found := strings.CutPrefix(w, "AT+CPMS="); found {
		m.selections = append(m.selections, strings.TrimSpace(mem))

		return [][]byte{okResponse("OK")}
	}
	if resps := initResponder(w); resps != nil {
		return resps
	}

	switch {
	case strings.HasPrefix(w, "AT+CMGR="):
		return [][]byte{okResponse(cmgrHeader), okResponse(inboundTextPDU), okResponse("OK")}
	case strings.HasPrefix(w, "AT+CMGD="):
		return [][]byte{okResponse("OK")}
	case strings.HasPrefix(w, "AT+CMGL="):
		responses := make([][]byte, 0, len(m.cmglLines)+1)
		for _, line := range m.cmglLines {
			responses = append(responses, okResponse(line))
		}

		return append(responses, okResponse("OK"))
	}

	return nil
}

// TestReadStoredMessage_SelectsNotifiedStoreAndCaches pins the store-selection
// contract: +CMGR is issued against the PREFERRED store, so the notified store
// is selected first via +CPMS - and selected only when it CHANGES (a second
// read of the same store must not re-issue +CPMS).
func TestReadStoredMessage_SelectsNotifiedStoreAndCaches(t *testing.T) {
	t.Parallel()

	modem := &storeSelectModem{selections: []string{}, cmglLines: nil}
	commands, scratch := newCommands(t, modem.respond)

	for _, index := range []int{1, 2} {
		if _, err := commands.ReadStoredMessage("ME", index); err != nil {
			t.Fatalf("ReadStoredMessage(ME, %d): %v", index, err)
		}
	}
	if _, err := commands.ReadStoredMessage("SM", 3); err != nil {
		t.Fatalf("ReadStoredMessage(SM, 3): %v", err)
	}

	want := []string{`"ME"`, `"SM"`}
	if !slices.Equal(modem.selections, want) {
		t.Errorf("+CPMS selections = %v, want %v (cached across reads of the same store)", modem.selections, want)
	}

	writes := strings.Join(scratch.getWrites(), "")
	if !strings.Contains(writes, "AT+CPMS=\"ME\"\r\nAT+CMGR=1\r\n") {
		t.Errorf("wire does not select ME before the first read: %q", writes)
	}
}

// TestDeleteStoredMessage_SelectsNotifiedStore pins that the delete also primes
// the preferred store, so a retried delete after a re-selection targets the
// same store the read used.
func TestDeleteStoredMessage_SelectsNotifiedStore(t *testing.T) {
	t.Parallel()

	modem := &storeSelectModem{selections: []string{}, cmglLines: nil}
	commands, scratch := newCommands(t, modem.respond)

	if err := commands.DeleteStoredMessage("MT", 7); err != nil {
		t.Fatalf("DeleteStoredMessage: %v", err)
	}

	if !slices.Equal(modem.selections, []string{`"MT"`}) {
		t.Errorf("+CPMS selections = %v, want [\"MT\"]", modem.selections)
	}
	writes := strings.Join(scratch.getWrites(), "")
	if !strings.Contains(writes, "AT+CPMS=\"MT\"\r\nAT+CMGD=7\r\n") {
		t.Errorf("wire does not select MT before the delete: %q", writes)
	}
}

// TestListStoredMessages pins the recovery listing: AT+CMGL=4 lists ALL
// messages of the preferred store and the <index> of every "+CMGL:" header is
// returned, while a malformed header is skipped rather than aborting.
func TestListStoredMessages(t *testing.T) {
	t.Parallel()

	modem := &storeSelectModem{selections: []string{}, cmglLines: []string{
		`+CMGL: 2,1,"",42`,
		inboundTextPDU,
		`+CMGL: 5,1,"",42`,
		inboundTextPDU,
		`+CMGL: not-a-number,1,"",42`,
		inboundTextPDU,
	}}
	commands, scratch := newCommands(t, modem.respond)

	indices, err := commands.ListStoredMessages("SM")
	if err != nil {
		t.Fatalf("ListStoredMessages: %v", err)
	}
	if want := []int{2, 5}; !slices.Equal(indices, want) {
		t.Errorf("indices = %v, want %v", indices, want)
	}

	if !slices.Equal(modem.selections, []string{`"SM"`}) {
		t.Errorf("+CPMS selections = %v, want [\"SM\"]", modem.selections)
	}
	writes := strings.Join(scratch.getWrites(), "")
	if !strings.Contains(writes, "AT+CMGL=4\r\n") {
		t.Errorf("wire = %q, want AT+CMGL=4", writes)
	}
}

// cmgrHeader is the SIM800's real +CMGR response head (3GPP TS 27.005 3.4.2
// PDU mode with the optional <alpha> field emitted): "<stat>,<alpha>,
// <length>". The PDU body follows on the NEXT line, so index 2 of the header
// is <length> and never a PDU.
const cmgrHeader = `+CMGR: 1,"",42`

// Owner-captured AT+CMGR exchange from the real SIM800: the alpha-bearing
// header line followed by the PDU body line. The PDU decodes to sender
// +79504241345 with content "Hello".
const (
	cmgrCapturedHeader = `+CMGR: 1,"",23`
	cmgrCapturedPDU    = "07919740430900F0040B919705241443F500006201214001812105C8329BFD06"
)

// storeResponder answers AT+CMGR with the given info lines (already including
// any unsolicited head lines the test wants to leak) and AT+CMGD with OK.
// Every attempt is counted so the retry policy can be asserted on the wire.
type storeResponder struct {
	readLines []string
	reads     int
	deletes   int
}

func (r *storeResponder) respond(w string) [][]byte {
	if resps := initResponder(w); resps != nil {
		return resps
	}

	switch {
	case strings.HasPrefix(w, "AT+CMGR="):
		r.reads++
		responses := make([][]byte, 0, len(r.readLines)+1)
		for _, line := range r.readLines {
			responses = append(responses, okResponse(line))
		}

		return append(responses, okResponse("OK"))
	case strings.HasPrefix(w, "AT+CMGD="):
		r.deletes++

		return [][]byte{okResponse("OK")}
	}

	return nil
}

// TestReadStoredMessage_DropsLeakedURCHeads pins the LEAK MITIGATION: the
// library routes every '+'-prefixed line that is not OK/ERROR/CONNECT into the
// response of the in-flight command - registered indication heads (+CMTI,
// +CMT, +CDS) AND unregistered URCs (+CREG, +CBM, ...) alike. Because +CMGR
// is parsed POSITIONALLY (status header line paired with its PDU body), the
// heads must be DROPPED before pairing: each leak shape below would otherwise
// shift the parse and yield the wrong body (or none).
func TestReadStoredMessage_DropsLeakedURCHeads(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
	}{
		{
			name:  "clean response",
			lines: []string{cmgrHeader, inboundTextPDU},
		},
		{
			name:  "registered head mid-command",
			lines: []string{`+CMTI: "ME",3`, cmgrHeader, inboundTextPDU},
		},
		{
			name:  "head before the response head",
			lines: []string{`+CMTI: "ME",3`, cmgrHeader, inboundTextPDU},
		},
		{
			name:  "two registered heads before the response",
			lines: []string{`+CMT: "ME",2`, `+CMTI: "ME",3`, cmgrHeader, inboundTextPDU},
		},
		{
			name:  "unregistered +CREG head",
			lines: []string{"+CREG: 1", cmgrHeader, inboundTextPDU},
		},
		{
			name:  "unregistered +CBM head",
			lines: []string{"+CBM: 9", "+CREG: 2", cmgrHeader, inboundTextPDU},
		},
		{
			name:  "registered +CDS head",
			lines: []string{`+CDS: 24`, cmgrHeader, inboundTextPDU},
		},
		{
			name:  "unregistered head AFTER the response head",
			lines: []string{cmgrHeader, inboundTextPDU, "+CCINFO: 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			responder := &storeResponder{readLines: tt.lines, reads: 0, deletes: 0}
			commands, scratch := newCommands(t, responder.respond)

			got, err := commands.ReadStoredMessage("SM", 3)
			if err != nil {
				t.Fatalf("ReadStoredMessage: %v", err)
			}
			if got != inboundTextPDU {
				t.Errorf("PDU = %q, want %q", got, inboundTextPDU)
			}
			if responder.reads != 1 {
				t.Errorf("+CMGR attempts = %d, want 1", responder.reads)
			}

			if writes := scratch.getWrites(); len(writes) == 0 ||
				!strings.HasPrefix(writes[len(writes)-1], "AT+CMGR=3\r\n") {
				t.Errorf("wire tail = %q, want the single-argument AT+CMGR=3 command", writes)
			}
		})
	}
}

// TestReadStoredMessage_PositionalBodyLine pins the real SIM800 parse shape:
// the modem answers the "+CMGR: <stat>,<alpha>,<length>" header and puts the
// PDU on its own following line. The body line is paired with the head
// positionally - after the unsolicited heads were dropped - so the PDU is
// recovered and the header's <length> field is never mistaken for it.
func TestReadStoredMessage_PositionalBodyLine(t *testing.T) {
	t.Parallel()

	responder := &storeResponder{readLines: []string{
		"+CREG: 1",
		cmgrHeader,
		inboundTextPDU,
	}, reads: 0, deletes: 0}
	commands, _ := newCommands(t, responder.respond)

	got, err := commands.ReadStoredMessage("SM", 1)
	if err != nil {
		t.Fatalf("ReadStoredMessage: %v", err)
	}
	if got != inboundTextPDU {
		t.Errorf("PDU = %q, want %q", got, inboundTextPDU)
	}
}

// TestReadStoredMessage_MissingPDULine pins the failure surface: an OK-only
// response (a modem that answered but returned no PDU) is rejected instead of
// decoding an empty body into a webhook event.
func TestReadStoredMessage_MissingPDULine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		lines []string
	}{
		{name: "no info lines at all", lines: nil},
		{name: "only leaked heads", lines: []string{"+CREG: 1"}},
		{name: "head without a body line", lines: []string{cmgrHeader}},
		{name: "empty inline pdu field", lines: []string{`+CMGR: 1,"",42,`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			responder := &storeResponder{readLines: tt.lines, reads: 0, deletes: 0}
			commands, _ := newCommands(t, responder.respond)

			got, err := commands.ReadStoredMessage("SM", 1)
			if err == nil {
				t.Fatalf("ReadStoredMessage = %q, want error", got)
			}
			if got != "" {
				t.Errorf("PDU = %q, want empty on failure", got)
			}
			if !strings.Contains(err.Error(), "read stored message") {
				t.Errorf("error = %q, want the command tag", err.Error())
			}
		})
	}
}

// TestReadStoredMessage_RealSIM800Capture pins the owner-reported live
// failure: the real SIM800 answers "+CMGR: 1,\"\",23" and puts the PDU on the
// NEXT line. The header's index-2 field is the LENGTH ("23"), never a PDU -
// returning it pushed one octet 0x23 into the decoder as an SMSC length of 35
// and every inbound message died with
// "decode stored PDU: tpdu: error decoding toa at octet 1: underflow".
func TestReadStoredMessage_RealSIM800Capture(t *testing.T) {
	t.Parallel()

	responder := &storeResponder{readLines: []string{cmgrCapturedHeader, cmgrCapturedPDU}, reads: 0, deletes: 0}
	commands, _ := newCommands(t, responder.respond)

	got, err := commands.ReadStoredMessage("SM", 1)
	if err != nil {
		t.Fatalf("ReadStoredMessage: %v", err)
	}
	if got != cmgrCapturedPDU {
		t.Fatalf("PDU = %q, want the captured body line %q", got, cmgrCapturedPDU)
	}

	part, err := modem.DecodeInboundSMS(got)
	if err != nil {
		t.Fatalf("DecodeInboundSMS: %v", err)
	}
	if part.Sender != "+79504241345" {
		t.Errorf("sender = %q, want +79504241345", part.Sender)
	}
	if part.Content != "Hello" {
		t.Errorf("content = %q, want %q", part.Content, "Hello")
	}
}

// TestReadStoredMessage_LengthFieldIsNeverAPDU pins the negative half of the
// capture: with NO body line, the header's <length> field ("23") must be
// rejected outright - never returned as the PDU for the decoder to choke on.
func TestReadStoredMessage_LengthFieldIsNeverAPDU(t *testing.T) {
	t.Parallel()

	responder := &storeResponder{readLines: []string{cmgrCapturedHeader}, reads: 0, deletes: 0}
	commands, _ := newCommands(t, responder.respond)

	got, err := commands.ReadStoredMessage("SM", 1)
	if err == nil {
		t.Fatalf("ReadStoredMessage = %q, want error - the length field is not a PDU", got)
	}
	if got != "" {
		t.Errorf("PDU = %q, want empty", got)
	}
	if !strings.Contains(err.Error(), "no +CMGR line in response") {
		t.Errorf("error = %q, want errNoStoredMessageLine", err.Error())
	}
}

// TestReadStoredMessage_InlinePDUWithoutBodyLine pins the guarded fallback:
// when a header carries NO body line but enough fields for an inline PDU
// (alpha shifts it to index 3), the candidate is returned only if it looks
// like a PDU - non-empty even-length hex.
func TestReadStoredMessage_InlinePDUWithoutBodyLine(t *testing.T) {
	t.Parallel()

	responder := &storeResponder{readLines: []string{cmgrHeader + "," + inboundTextPDU}, reads: 0, deletes: 0}
	commands, _ := newCommands(t, responder.respond)

	got, err := commands.ReadStoredMessage("SM", 1)
	if err != nil {
		t.Fatalf("ReadStoredMessage: %v", err)
	}
	if got != inboundTextPDU {
		t.Errorf("PDU = %q, want %q", got, inboundTextPDU)
	}
}

// cms321Modem is a SIM800-shaped store: it REJECTS the 3GPP "<mem>,<index>"
// read form with "+CMS ERROR: 321" - exactly what the real SIM800L answered -
// and only answers the single-argument "<index>" form. A read therefore proves
// which signature was actually put on the wire.
type cms321Modem struct {
	twoArgReads int
	oneArgReads int
}

func (m *cms321Modem) respond(w string) [][]byte {
	if resps := initResponder(w); resps != nil {
		return resps
	}

	switch {
	case strings.HasPrefix(w, "AT+CMGR=0,"):
		m.twoArgReads++

		return [][]byte{okResponse("+CMS ERROR: 321")}
	case strings.HasPrefix(w, "AT+CMGR="):
		m.oneArgReads++

		return [][]byte{okResponse(cmgrHeader), okResponse(inboundTextPDU), okResponse("OK")}
	}

	return nil
}

// TestReadStoredMessage_SingleArgumentForm pins the COMMAND SIGNATURE against
// the only modem this gateway has been proven on. 3GPP TS 27.005 3.4.2 gives
// +CMGR a SINGLE <index>; the "<mem>,<index>" pair belongs to +CMGL in the
// standard, and the SIM800 Series AT Command Manual V1.10 (sec 4.2.1) spells
// +CMGD as "AT+CMGD=<index>[,<delflag>]" too, so neither store command carries
// the memory. On the SIM800L the memory prefix is not merely ignored: it turns
// every inbound read into
//
//	read stored message failed {"error": "read stored message (AT+CMGR=0,3):
//	modem initialization failed: CMS Error: 321"}
//
// The scripted modem below answers 321 to the two-argument form, so this test
// is RED whenever the memory prefix comes back.
func TestReadStoredMessage_SingleArgumentForm(t *testing.T) {
	t.Parallel()

	modem321 := &cms321Modem{twoArgReads: 0, oneArgReads: 0}
	commands, scratch := newCommands(t, modem321.respond)

	got, err := commands.ReadStoredMessage("SM", 3)
	if err != nil {
		t.Fatalf("ReadStoredMessage: %v", err)
	}
	if got != inboundTextPDU {
		t.Errorf("PDU = %q, want %q", got, inboundTextPDU)
	}
	if modem321.twoArgReads != 0 {
		t.Errorf("AT+CMGR=<mem>,<index> reads = %d, want 0 - the memory prefix is CMS 321", modem321.twoArgReads)
	}
	if modem321.oneArgReads != 1 {
		t.Errorf("AT+CMGR=<index> reads = %d, want 1", modem321.oneArgReads)
	}

	writes := scratch.getWrites()
	if len(writes) == 0 || !strings.HasPrefix(writes[len(writes)-1], "AT+CMGR=3\r\n") {
		t.Errorf("wire tail = %q, want the single-argument AT+CMGR=3 command", writes)
	}
}

// TestReadStoredMessage_ConcurrentWithSend pins the COMMAND-SAFETY contract:
// the library serialises commands over one cmdLoop, so a store read issued
// from an indication goroutine BLOCKS behind an in-flight send instead of
// corrupting it. Both commands must complete with their own parse intact.
func TestReadStoredMessage_ConcurrentWithSend(t *testing.T) {
	t.Parallel()

	responder := &storeResponder{readLines: []string{`+CMTI: "ME",1`, cmgrHeader, inboundTextPDU}, reads: 0, deletes: 0}
	respond := func(w string) [][]byte {
		if resps := smsResponder("7")(w); resps != nil {
			return resps
		}

		return responder.respond(w)
	}
	commands, _ := newCommands(t, respond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	type readResult struct {
		pdu string
		err error
	}
	reads := make(chan readResult, 1)
	go func() {
		pdu, err := commands.ReadStoredMessage("SM", 1)
		reads <- readResult{pdu: pdu, err: err}
	}()

	refs, err := commands.SendSMS(ctx, testPhone, "hello", false)
	if err != nil {
		t.Fatalf("SendSMS: %v", err)
	}
	if len(refs) != 1 || refs[0] != 7 {
		t.Fatalf("refs = %v, want [7]", refs)
	}

	select {
	case got := <-reads:
		if got.err != nil {
			t.Fatalf("ReadStoredMessage: %v", got.err)
		}
		if got.pdu != inboundTextPDU {
			t.Errorf("PDU = %q, want %q", got.pdu, inboundTextPDU)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadStoredMessage never completed")
	}
}

// TestDeleteStoredMessage_UsesTheStoreReference pins the delete command form
// (one attempt; the retry policy lives in the service) and that a rejected
// delete surfaces the modem error. The wire pin is EXACT equality, so the
// single-argument form carries no delflag argument either.
func TestDeleteStoredMessage_UsesTheStoreReference(t *testing.T) {
	t.Parallel()

	responder := &storeResponder{readLines: nil, reads: 0, deletes: 0}
	commands, scratch := newCommands(t, responder.respond)

	if err := commands.DeleteStoredMessage("SM", 4); err != nil {
		t.Fatalf("DeleteStoredMessage: %v", err)
	}
	if responder.deletes != 1 {
		t.Errorf("+CMGD attempts = %d, want 1", responder.deletes)
	}
	writes := scratch.getWrites()
	if len(writes) == 0 || writes[len(writes)-1] != "AT+CMGD=4\r\n" {
		t.Errorf("wire tail = %q, want exactly AT+CMGD=4 - a single index, "+
			"no memory prefix and no delflag argument", writes)
	}

	respond := func(w string) [][]byte {
		if strings.HasPrefix(w, "AT+CMGD=") {
			return [][]byte{okResponse("+CMS ERROR: 321")}
		}

		return (&storeResponder{readLines: nil, reads: 0, deletes: 0}).respond(w)
	}
	commands, _ = newCommands(t, respond)

	err := commands.DeleteStoredMessage("SM", 4)
	if !errors.Is(err, modem.ErrInitFailed) {
		t.Fatalf("error = %v, want ErrInitFailed", err)
	}
	if !strings.Contains(err.Error(), "delete stored message") {
		t.Errorf("error = %q, want the command tag", err.Error())
	}
}

// cmgdSigModem is a SIM800-shaped store for the DELETE signature: it rejects
// every two-argument +CMGD write - both the "<mem>,<index>" memory-prefix form
// and the "<index>,<delflag>" form - with "+CMS ERROR: 321", and answers only
// the single-argument "<index>" form. Any comma in the payload is a defect:
// on the SIM800 the first extra argument is parsed as <delflag>, so index 1-3
// trigger bulk category deletes and index 4 deletes ALL stored messages.
type cmgdSigModem struct {
	twoArgDeletes int
	oneArgDeletes int
}

func (m *cmgdSigModem) respond(w string) [][]byte {
	if resps := initResponder(w); resps != nil {
		return resps
	}

	if strings.HasPrefix(w, "AT+CMGD=") {
		if strings.Contains(w, ",") {
			m.twoArgDeletes++

			return [][]byte{okResponse("+CMS ERROR: 321")}
		}
		m.oneArgDeletes++

		return [][]byte{okResponse("OK")}
	}

	return nil
}

// TestDeleteStoredMessage_SingleArgumentForm pins the DELETE command signature
// the same way the +CMGR read signature is pinned: the SIM800 Series AT
// Command Manual V1.10 (sec 4.2.1) gives +CMGD a SINGLE <index> from the
// preferred message storage - there is NO "<mem>,<index>" form - and this test
// is RED whenever the memory prefix or a delflag argument comes back. The wire
// pin is EXACT equality, so "AT+CMGD=3,1" fails it just like "AT+CMGD=0,3".
func TestDeleteStoredMessage_SingleArgumentForm(t *testing.T) {
	t.Parallel()

	modem321 := &cmgdSigModem{twoArgDeletes: 0, oneArgDeletes: 0}
	commands, scratch := newCommands(t, modem321.respond)

	if err := commands.DeleteStoredMessage("SM", 3); err != nil {
		t.Fatalf("DeleteStoredMessage: %v", err)
	}
	if modem321.twoArgDeletes != 0 {
		t.Errorf("two-argument +CMGD writes = %d, want 0 - the SIM800 parses the "+
			"message index as <delflag> (bulk delete / delete-all)", modem321.twoArgDeletes)
	}
	if modem321.oneArgDeletes != 1 {
		t.Errorf("AT+CMGD=<index> deletes = %d, want 1", modem321.oneArgDeletes)
	}

	writes := scratch.getWrites()
	if len(writes) == 0 || writes[len(writes)-1] != "AT+CMGD=3\r\n" {
		t.Errorf("wire tail = %q, want exactly AT+CMGD=3 - no memory prefix and "+
			"no delflag argument", writes)
	}
}

// TestParseStoredReference pins the +CMTI head parser: the memory NAME is
// returned uppercased (quoted or unquoted; the exact form +CPMS accepts) and a
// malformed head is rejected with the sentinel instead of indexing the store at
// a wrong position.
func TestParseStoredReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		head      string
		wantMem   string
		wantIndex int
		wantErr   bool
	}{
		{name: "quoted ME", head: `+CMTI: "ME",1`, wantMem: "ME", wantIndex: 1, wantErr: false},
		{name: "unquoted ME", head: "+CMTI: ME,12", wantMem: "ME", wantIndex: 12, wantErr: false},
		{name: "SM alias", head: `+CMTI: "SM",0`, wantMem: "SM", wantIndex: 0, wantErr: false},
		{name: "MT alias", head: `+CMTI: "MT",7`, wantMem: "MT", wantIndex: 7, wantErr: false},
		{name: "lowercase normalized", head: `+CMTI: "me",3`, wantMem: "ME", wantIndex: 3, wantErr: false},
		{name: "padded fields", head: `+CMTI: "ME" , 5`, wantMem: "ME", wantIndex: 5, wantErr: false},
		{name: "missing index", head: `+CMTI: "ME"`, wantMem: "", wantIndex: 0, wantErr: true},
		{name: "unknown memory", head: `+CMTI: "XX",1`, wantMem: "", wantIndex: 0, wantErr: true},
		{name: "non-numeric index", head: `+CMTI: "ME",abc`, wantMem: "", wantIndex: 0, wantErr: true},
		{name: "negative index", head: `+CMTI: "ME",-1`, wantMem: "", wantIndex: 0, wantErr: true},
		{name: "not a +CMTI head", head: cmgrCapturedHeader, wantMem: "", wantIndex: 0, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			memory, index, err := modem.ParseStoredReference(tt.head)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseStoredReference(%q) = (%q, %d), want error", tt.head, memory, index)
				}
				if !errors.Is(err, modem.ErrNotStoredReference) {
					t.Errorf("error = %v, want ErrNotStoredReference", err)
				}

				return
			}
			if err != nil {
				t.Fatalf("ParseStoredReference(%q): %v", tt.head, err)
			}
			if memory != tt.wantMem || index != tt.wantIndex {
				t.Errorf("reference = (%q, %d), want (%q, %d)", memory, index, tt.wantMem, tt.wantIndex)
			}
		})
	}
}

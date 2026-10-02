package modem_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
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
	tests := []struct {
		name  string
		lines []string
	}{
		{
			name:  "clean response",
			lines: []string{"+CMGR: 1,42," + inboundTextPDU},
		},
		{
			name:  "registered head mid-command",
			lines: []string{`+CMTI: "ME",3`, "+CMGR: 1,42," + inboundTextPDU},
		},
		{
			name:  "head before the response head",
			lines: []string{`+CMTI: "ME",3`, "+CMGR: 1,42," + inboundTextPDU},
		},
		{
			name:  "two registered heads before the response",
			lines: []string{`+CMT: "ME",2`, `+CMTI: "ME",3`, "+CMGR: 1,42," + inboundTextPDU},
		},
		{
			name:  "unregistered +CREG head",
			lines: []string{"+CREG: 1", "+CMGR: 1,42," + inboundTextPDU},
		},
		{
			name:  "unregistered +CBM head",
			lines: []string{"+CBM: 9", "+CREG: 2", "+CMGR: 1,42," + inboundTextPDU},
		},
		{
			name:  "registered +CDS head",
			lines: []string{`+CDS: 24`, "+CMGR: 1,42," + inboundTextPDU},
		},
		{
			name:  "unregistered head AFTER the response head",
			lines: []string{"+CMGR: 1,42," + inboundTextPDU, "+CCINFO: 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responder := &storeResponder{readLines: tt.lines}
			commands, scratch := newCommands(t, responder.respond)

			got, err := commands.ReadStoredMessage(0, 3)
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
				!strings.HasPrefix(writes[len(writes)-1], "AT+CMGR=0,3") {
				t.Errorf("wire tail = %q, want the AT+CMGR=0,3 command", writes)
			}
		})
	}
}

// TestReadStoredMessage_PositionalBodyLine pins the second parse shape: a
// modem that answers "+CMGR: 1,42" and puts the PDU on its own following
// line. The body line is paired with the head positionally - after the
// unsolicited heads were dropped - so the PDU is still recovered.
func TestReadStoredMessage_PositionalBodyLine(t *testing.T) {
	responder := &storeResponder{readLines: []string{
		"+CREG: 1",
		"+CMGR: 1,42",
		inboundTextPDU,
	}}
	commands, _ := newCommands(t, responder.respond)

	got, err := commands.ReadStoredMessage(0, 1)
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
	tests := []struct {
		name  string
		lines []string
	}{
		{name: "no info lines at all", lines: nil},
		{name: "only leaked heads", lines: []string{"+CREG: 1"}},
		{name: "head without a body line", lines: []string{"+CMGR: 1,42"}},
		{name: "empty pdu field", lines: []string{"+CMGR: 1,42,"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responder := &storeResponder{readLines: tt.lines}
			commands, _ := newCommands(t, responder.respond)

			got, err := commands.ReadStoredMessage(0, 1)
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

// TestReadStoredMessage_ConcurrentWithSend pins the COMMAND-SAFETY contract:
// the library serialises commands over one cmdLoop, so a store read issued
// from an indication goroutine BLOCKS behind an in-flight send instead of
// corrupting it. Both commands must complete with their own parse intact.
func TestReadStoredMessage_ConcurrentWithSend(t *testing.T) {
	responder := &storeResponder{readLines: []string{`+CMTI: "ME",1`, "+CMGR: 1,42," + inboundTextPDU}}
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
		pdu, err := commands.ReadStoredMessage(0, 1)
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
// delete surfaces the modem error.
func TestDeleteStoredMessage_UsesTheStoreReference(t *testing.T) {
	responder := &storeResponder{}
	commands, scratch := newCommands(t, responder.respond)

	if err := commands.DeleteStoredMessage(0, 4); err != nil {
		t.Fatalf("DeleteStoredMessage: %v", err)
	}
	if responder.deletes != 1 {
		t.Errorf("+CMGD attempts = %d, want 1", responder.deletes)
	}
	writes := scratch.getWrites()
	if len(writes) == 0 || !strings.HasPrefix(writes[len(writes)-1], "AT+CMGD=0,4") {
		t.Errorf("wire tail = %q, want the AT+CMGD=0,4 command", writes)
	}

	respond := func(w string) [][]byte {
		if strings.HasPrefix(w, "AT+CMGD=") {
			return [][]byte{okResponse("+CMS ERROR: 321")}
		}

		return (&storeResponder{}).respond(w)
	}
	commands, _ = newCommands(t, respond)

	err := commands.DeleteStoredMessage(0, 4)
	if !errors.Is(err, modem.ErrInitFailed) {
		t.Fatalf("error = %v, want ErrInitFailed", err)
	}
	if !strings.Contains(err.Error(), "delete stored message") {
		t.Errorf("error = %q, want the command tag", err.Error())
	}
}

// TestParseStoredReference pins the +CMTI head parser: the memory name maps to
// its numeric form (quoted or unquoted) and a malformed head is rejected with
// the sentinel instead of indexing the store at a wrong position.
func TestParseStoredReference(t *testing.T) {
	tests := []struct {
		name      string
		head      string
		wantMem   int
		wantIndex int
		wantErr   bool
	}{
		{name: "quoted ME", head: `+CMTI: "ME",1`, wantMem: 0, wantIndex: 1},
		{name: "unquoted ME", head: "+CMTI: ME,12", wantMem: 0, wantIndex: 12},
		{name: "SM alias", head: `+CMTI: "SM",0`, wantMem: 0, wantIndex: 0},
		{name: "MT alias", head: `+CMTI: "MT",7`, wantMem: 1, wantIndex: 7},
		{name: "padded fields", head: `+CMTI: "ME" , 5`, wantMem: 0, wantIndex: 5},
		{name: "missing index", head: `+CMTI: "ME"`, wantErr: true},
		{name: "unknown memory", head: `+CMTI: "XX",1`, wantErr: true},
		{name: "non-numeric index", head: `+CMTI: "ME",abc`, wantErr: true},
		{name: "negative index", head: `+CMTI: "ME",-1`, wantErr: true},
		{name: "not a +CMTI head", head: "+CMGR: 1,42,00", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory, index, err := modem.ParseStoredReference(tt.head)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseStoredReference(%q) = (%d, %d), want error", tt.head, memory, index)
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
				t.Errorf("reference = (%d, %d), want (%d, %d)", memory, index, tt.wantMem, tt.wantIndex)
			}
		})
	}
}

package modem_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
)

// Byte-exact SMS-DELIVER fixtures (PDU-mode, empty SMSC address "00"), all
// carrying sender +79990001234 and TP-SCTS 2026-10-02 12:00:00 +00 unless the
// name says otherwise. They are literal hex, NOT encoder output, so the
// decoder is pinned against the wire bytes a modem writes.
const (
	// 04 | OA | PID | DCS 00 | SCTS | UDL 05 | GSM-7 "hello".
	inboundTextPDU = "00040b919799001032F400006201202100000005e8329bfd06"
	// Concat part 1/2 with the 8-bit IEI 0x00: ref 0x42, 2 segments,
	// sequence 1/2, GSM-7 "abcdefgh" / "ijklmnop".
	inboundConcat8Part1PDU = "00440b919799001032F40000620120210000000f050003420201c2e231b96c3ea301"
	inboundConcat8Part2PDU = "00440b919799001032F40000620120210000000f050003420202d2ea35bbed7ec301"
	// Concat part 1/2 with the 16-bit IEI 0x08: ref 0x0042, 2 segments,
	// sequence 1/2 - the SAME numeric reference as the 8-bit pair above.
	inboundConcat16Part1PDU = "00440b919799001032F4000062012021000000100608040042020161f1985c369fd1"
	inboundConcat16Part2PDU = "00440b919799001032F4000062012021000000100608040042020269f59add76bfe1"
	// Data SMS with the 8-bit port IEI 0x04: destination port 0x16 (22) FIRST,
	// originator port 0x00 second (3GPP TS 23.040 9.2.3.24.4), DCS 0x04
	// (8-bit alphabet), payload cafe.
	inboundPort8PDU = "00440b919799001032F4000462012021000000070404021600cafe"
	// Data SMS with the 16-bit port IEI 0x05: a 4-octet payload of destination
	// port 0x0050 (80) FIRST then originator port 0x0000 (3GPP TS 23.040
	// 9.2.3.24.5), payload cafebabe.
	inboundPort16PDU = "00440b919799001032F40004620120210000000b06050400500000cafebabe"
	// 8-bit port IEI 0x04 with a non-conformant 4-octet payload: the decoder
	// must ignore it rather than read an originator octet as the port.
	inboundPort8BadLenPDU = "00440b919799001032F40004620120210000000906040400160000cafe"
	// UCS-2 text "привет" (DCS 0x08).
	inboundUCS2PDU = "00040b919799001032F40008620120210000000c043f04400438043204350442"
	// Same text, but the TP-SCTS year is 99: an implausible stamp that the
	// decoder must report as absent.
	inboundBadSCTSPDU = "00040b919799001032F400009901202100000005e8329bfd06"
)

// TestDecodeInboundSMS_TextFixtures pins the decoded values of every text
// fixture: sender from TP-OA (E.164 with '+'), the TP-SCTS stamp in UTC, the
// decoded content and the concatenation descriptor - read with the
// width-EXPLICIT accessors, so the 8-bit and 16-bit references of the SAME
// numeric value (0x42) stay distinguishable.
func TestDecodeInboundSMS_TextFixtures(t *testing.T) {
	wantAt := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		pdu         string
		wantContent string
		wantConcat  *modem.ConcatRef
	}{
		{
			name:        "single part gsm7",
			pdu:         inboundTextPDU,
			wantContent: "hello",
			wantConcat:  nil,
		},
		{
			name:        "ucs2 fallback",
			pdu:         inboundUCS2PDU,
			wantContent: "привет",
			wantConcat:  nil,
		},
		{
			name:        "concat 8-bit part 1",
			pdu:         inboundConcat8Part1PDU,
			wantContent: "abcdefgh",
			wantConcat:  &modem.ConcatRef{Segments: 2, Sequence: 1, Ref: 0x42, RefWidth: 8},
		},
		{
			name:        "concat 8-bit part 2",
			pdu:         inboundConcat8Part2PDU,
			wantContent: "ijklmnop",
			wantConcat:  &modem.ConcatRef{Segments: 2, Sequence: 2, Ref: 0x42, RefWidth: 8},
		},
		{
			name:        "concat 16-bit part 1",
			pdu:         inboundConcat16Part1PDU,
			wantContent: "abcdefgh",
			wantConcat:  &modem.ConcatRef{Segments: 2, Sequence: 1, Ref: 0x42, RefWidth: 16},
		},
		{
			name:        "concat 16-bit part 2",
			pdu:         inboundConcat16Part2PDU,
			wantContent: "ijklmnop",
			wantConcat:  &modem.ConcatRef{Segments: 2, Sequence: 2, Ref: 0x42, RefWidth: 16},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			part, err := modem.DecodeInboundSMS(tt.pdu)
			if err != nil {
				t.Fatalf("DecodeInboundSMS: %v", err)
			}

			if part.Sender != "+79990001234" {
				t.Errorf("sender = %q, want +79990001234", part.Sender)
			}
			if !part.TimestampValid {
				t.Error("TimestampValid = false, want true for a 2026 stamp")
			}
			if !part.ReceivedAt.Equal(wantAt) {
				t.Errorf("receivedAt = %s, want %s", part.ReceivedAt, wantAt)
			}
			if part.Content != tt.wantContent {
				t.Errorf("content = %q, want %q", part.Content, tt.wantContent)
			}
			if part.Ported {
				t.Error("ported = true, want false for a text SMS")
			}
			if part.Port != 0 {
				t.Errorf("port = %d, want 0 without a port IE", part.Port)
			}
			assertConcatRef(t, part.Concat, tt.wantConcat)
		})
	}
}

// TestDecodeInboundSMS_PortFixtures pins the DESTINATION-port extraction for
// BOTH port IEs: the 8-bit IEI 0x04 (destination port FIRST, originator
// second) and the 16-bit IEI 0x05 (destination FIRST, originator second). It
// also pins that a port IE with a non-conformant payload length is IGNORED, so
// a malformed header falls back to treating the part as non-ported instead of
// reading an arbitrary octet. The 8-bit alphabet leaves the payload as raw
// octets, which is what sms:data-received base64-encodes.
func TestDecodeInboundSMS_PortFixtures(t *testing.T) {
	tests := []struct {
		name       string
		pdu        string
		wantPorted bool
		wantPort   uint16
		wantData   string
	}{
		{name: "8-bit port IE", pdu: inboundPort8PDU, wantPorted: true, wantPort: 22, wantData: "\xca\xfe"},
		{name: "16-bit port IE", pdu: inboundPort16PDU, wantPorted: true, wantPort: 80, wantData: "\xca\xfe\xba\xbe"},
		{
			name:       "8-bit IE wrong length ignored",
			pdu:        inboundPort8BadLenPDU,
			wantPorted: false,
			wantPort:   0,
			wantData:   "\xca\xfe",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			part, err := modem.DecodeInboundSMS(tt.pdu)
			if err != nil {
				t.Fatalf("DecodeInboundSMS: %v", err)
			}

			if part.Ported != tt.wantPorted {
				t.Errorf("ported = %v, want %v", part.Ported, tt.wantPorted)
			}
			if part.Port != tt.wantPort {
				t.Errorf("port = %d, want %d", part.Port, tt.wantPort)
			}
			if part.Content != tt.wantData {
				t.Errorf("content = %q, want %q", part.Content, tt.wantData)
			}
			if part.Concat != nil {
				t.Errorf("concat = %+v, want nil for a single-part data SMS", part.Concat)
			}
		})
	}
}

// TestDecodeInboundSMS_MalformedSCTSReportsTimestampInvalid pins the stable
// fallback identity precondition: an implausible TP-SCTS is reported as
// ABSENT (zero time, TimestampValid false) while the rest of the message
// still decodes, so the caller omits the timestamp component from the
// message id instead of pinning a wrong one.
func TestDecodeInboundSMS_MalformedSCTSReportsTimestampInvalid(t *testing.T) {
	part, err := modem.DecodeInboundSMS(inboundBadSCTSPDU)
	if err != nil {
		t.Fatalf("DecodeInboundSMS: %v", err)
	}

	if part.TimestampValid {
		t.Error("TimestampValid = true, want false for a 1999 stamp")
	}
	if !part.ReceivedAt.IsZero() {
		t.Errorf("receivedAt = %s, want the zero time", part.ReceivedAt)
	}
	if part.Content != "hello" {
		t.Errorf("content = %q, want %q", part.Content, "hello")
	}
	if part.Sender != "+79990001234" {
		t.Errorf("sender = %q, want +79990001234", part.Sender)
	}
}

// TestDecodeInboundSMS_MalformedInputs pins the error surface: a truncated
// PDU, a TPDU that is not an SMS-DELIVER and a non-hex body all fail instead
// of yielding a half-decoded part the webhook layer would emit.
func TestDecodeInboundSMS_MalformedInputs(t *testing.T) {
	tests := []struct {
		name    string
		pdu     string
		wantErr string
	}{
		{
			name:    "truncated before TP-SCTS",
			pdu:     inboundTextPDU[:30],
			wantErr: "decode SMS-DELIVER",
		},
		{
			name:    "truncated inside the payload",
			pdu:     inboundTextPDU[:40],
			wantErr: "decode SMS-DELIVER",
		},
		{
			name:    "status report body is not an SMS-DELIVER",
			pdu:     statusReportPDU,
			wantErr: "decode SMS-DELIVER",
		},
		{
			name:    "non-hex body",
			pdu:     "zzzz",
			wantErr: "decode stored PDU",
		},
		{
			name:    "empty body",
			pdu:     "",
			wantErr: "decode stored PDU",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			part, err := modem.DecodeInboundSMS(tt.pdu)
			if err == nil {
				t.Fatalf("DecodeInboundSMS(%q) = %+v, want error", tt.pdu, part)
			}
			if got := err.Error(); !strings.Contains(got, tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", got, tt.wantErr)
			}
		})
	}
}

// TestDecodeInboundSMS_NoDeliverSentinel pins that a store entry which
// decodes cleanly but is NOT an SMS-DELIVER (here the SMS-STATUS-REPORT
// fixture of delivery_test.go, the realistic wrong-type body a modem could
// return) fails with the sentinel, matchable with [errors.Is] per the
// sentinel-error convention.
func TestDecodeInboundSMS_NoDeliverSentinel(t *testing.T) {
	_, err := modem.DecodeInboundSMS(statusReportPDU)
	if !errors.Is(err, modem.ErrNotSmsDeliver) {
		t.Fatalf("error = %v, want ErrNotSmsDeliver", err)
	}
}

func assertConcatRef(t *testing.T, got *modem.ConcatRef, want *modem.ConcatRef) {
	t.Helper()

	switch {
	case got == nil && want == nil:
		return
	case got == nil || want == nil:
		t.Fatalf("concat = %+v, want %+v", got, want)
	case *got != *want:
		t.Fatalf("concat = %+v, want %+v", got, want)
	}
}

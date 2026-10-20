package modem

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/warthog618/sms"
	"github.com/warthog618/sms/encoding/gsm7/charset"
	"github.com/warthog618/sms/encoding/pdumode"
	"github.com/warthog618/sms/encoding/tpdu"
)

const (
	// ieiPort8 is the 8-bit destination port UDH IEI (data octet, port).
	ieiPort8 byte = 0x04
	// ieiPort16 is the 16-bit destination port UDH IEI.
	ieiPort16 byte = 0x05

	// concatRefWidth8 and concatRefWidth16 tag the reference width so two
	// messages whose references collide across widths (0x42 as 8-bit and
	// 0x0042 as 16-bit) never share one reassembly group.
	concatRefWidth8  = 8
	concatRefWidth16 = 16

	// portDataLen8 is the payload length of the 8-bit destination port IE
	// (IEI 0x04): one octet of DESTINATION port followed by one octet of
	// originator port (3GPP TS 23.040 9.2.3.24.4).
	portDataLen8 = 2
	// portDataLen16 is the payload length of the 16-bit destination port IE
	// (IEI 0x05): a two-octet DESTINATION port followed by a two-octet
	// originator port (3GPP TS 23.040 9.2.3.24.5).
	portDataLen16 = 4

	// minInboundSCTSYear is the earliest plausible year of a TP-SCTS stamp.
	// A stamp before it is treated as absent (see sctsUTC).
	minInboundSCTSYear = 2000
)

// WarmGSMCharsets forces the warthog618/sms character-set tables to be built
// while a single goroutine owns them.
//
// WHY: the library builds its default AND per-language encoders/decoders
// LAZILY and WITHOUT synchronization (gsm7/charset: `if defaultDecoder == nil
// { defaultDecoder = ... }`, `if *di.e == nil { *di.e = di.g() }`). Several
// +CMTI handlers decode concurrently (up to inboundHandlerLimit), so the first
// concurrent GSM-7 decode would race on those package-level tables - a real
// data race the race detector flags and, in production, a corrupted map.
// After this warm-up every accessor only READS the tables.
//
// It is called from NewService, i.e. once at construction, strictly before Run
// registers the +CMTI handler. Repeat calls are cheap: the accessors short-
// circuit on the now-initialized values.
func WarmGSMCharsets() {
	for nli := charset.Start; nli < charset.End; nli++ {
		charset.NewDecoder(nli)
		charset.NewExtDecoder(nli)
		charset.NewEncoder(nli)
		charset.NewExtEncoder(nli)
	}

	charset.DefaultDecoder()
	charset.DefaultExtDecoder()
	charset.DefaultEncoder()
	charset.DefaultExtEncoder()
}

// ConcatRef is the concatenation descriptor of one SMS-DELIVER part: which
// message it belongs to (reference plus its width), how many parts that
// message has and which part this is.
type ConcatRef struct {
	// Segments is the total number of parts of the concatenated message.
	Segments int
	// Sequence is the 1-based part number.
	Sequence int
	// Ref is the concatenation reference.
	Ref int
	// RefWidth is the reference width in bits (8 or 16); it is part of the
	// reassembly key so the two widths never merge.
	RefWidth int
}

// InboundPart is one decoded SMS-DELIVER read from the modem message store.
// Sender and ReceivedAt come from the TPDU itself (TP-OA / TP-SCTS) - never
// from the indication head, which in PDU mode carries only "<mem>,<index>".
type InboundPart struct {
	// Sender is the TP-OA originating address, prefixed with '+' when the
	// modem reports an international number.
	Sender string
	// ReceivedAt is the TP-SCTS stamp in UTC; the zero value when
	// TimestampValid is false.
	ReceivedAt time.Time
	// TimestampValid reports whether ReceivedAt is usable. When false the
	// message identity must omit the timestamp component so it stays
	// stable across retries.
	TimestampValid bool
	// Content is the decoded TP-UD: UTF-8 text for the GSM-7/UCS-2
	// alphabets, the raw octets as a string for the 8-bit alphabet.
	Content string
	// Port is the destination port from UDH IEI 0x05 (or 0x04), which makes
	// the part a port-addressed DATA SMS rather than text.
	Port uint16
	// Ported reports whether Content is a port-addressed data payload.
	Ported bool
	// Concat is the concatenation descriptor; nil for a single-part message.
	Concat *ConcatRef
}

// DecodeInboundSMS parses the hex PDU body returned by AT+CMGR (PDU-mode
// SMS-DELIVER, 3GPP TS 23.040 9.2.2.1). The concatenation reference is read
// with the width-EXPLICIT accessors: the width-ambiguous ConcatInfo() tries
// 8-bit first and falls through to 16-bit, which would collapse the two
// widths into one key. A body that is not an SMS-DELIVER, or that cannot be
// decoded at all, is an error; the caller drops it.
func DecodeInboundSMS(bodyHex string) (InboundPart, error) {
	pdu, err := pdumode.UnmarshalHexString(bodyHex)
	if err != nil {
		return InboundPart{}, fmt.Errorf("decode stored PDU: %w", err)
	}

	decoded, err := sms.Unmarshal(pdu.TPDU)
	if err != nil {
		return InboundPart{}, fmt.Errorf("decode SMS-DELIVER: %w", err)
	}
	if decoded.SmsType() != tpdu.SmsDeliver {
		return InboundPart{}, fmt.Errorf("decode SMS-DELIVER: %w: %s", ErrNotSmsDeliver, decoded.SmsType())
	}

	alphabet, err := decoded.Alphabet()
	if err != nil {
		return InboundPart{}, fmt.Errorf("decode alphabet: %w", err)
	}
	content, err := tpdu.DecodeUserData(decoded.UD, decoded.UDH, alphabet, tpdu.WithAllCharsets)
	if err != nil {
		return InboundPart{}, fmt.Errorf("decode user data: %w", err)
	}

	receivedAt, timestampValid := sctsUTC(decoded.SCTS)
	port, ported := destinationPort(decoded.UDH)

	return InboundPart{
		Sender:         decoded.OA.Number(),
		ReceivedAt:     receivedAt,
		TimestampValid: timestampValid,
		Content:        string(content),
		Port:           port,
		Ported:         ported,
		Concat:         concatRef(decoded.UDH),
	}, nil
}

// sctsUTC converts a decoded TP-SCTS stamp into UTC. The stamp is the
// originator's local time (the timezone quarter-hours are encoded in its last
// octet), so the conversion normalises it to UTC. A stamp that is absent
// (zero) or implausible - earlier than minInboundSCTSYear, which no live
// network produces - is reported as INVALID so the caller omits the
// timestamp from the message identity instead of pinning a wrong one.
func sctsUTC(scts tpdu.Timestamp) (time.Time, bool) {
	if scts.Time.IsZero() || scts.Year() < minInboundSCTSYear {
		return time.Time{}, false
	}

	return scts.UTC(), true
}

// destinationPort extracts the DESTINATION port from the user data header: the
// 16-bit IEI 0x05 wins over the 8-bit IEI 0x04 when a header carries both.
// In both IEs the destination port comes FIRST and the originator port second
// (3GPP TS 23.040 9.2.3.24.4/24.5), so reading the second octet of the 8-bit
// form yields the ORIGINATOR port. A port IE with a non-conformant payload
// length (anything but 2 octets for 0x04, 4 octets for 0x05) is ignored.
func destinationPort(udh tpdu.UserDataHeader) (uint16, bool) {
	if ie, ok := udh.IE(ieiPort16); ok && len(ie.Data) == portDataLen16 {
		return binary.BigEndian.Uint16(ie.Data[:2]), true
	}
	if ie, ok := udh.IE(ieiPort8); ok && len(ie.Data) == portDataLen8 {
		return uint16(ie.Data[0]), true
	}

	return 0, false
}

// concatRef extracts the concatenation descriptor with the width-EXPLICIT
// accessors (ConcatInfo8, then ConcatInfo16); nil when the header carries no
// concatenation IE. Both widths are reported with their own RefWidth so the
// reassembler can key on (sender, refWidth, ref).
func concatRef(udh tpdu.UserDataHeader) *ConcatRef {
	if segments, sequence, ref, ok := udh.ConcatInfo8(); ok {
		return &ConcatRef{
			Segments: segments,
			Sequence: sequence,
			Ref:      ref,
			RefWidth: concatRefWidth8,
		}
	}
	if segments, sequence, ref, ok := udh.ConcatInfo16(); ok {
		return &ConcatRef{
			Segments: segments,
			Sequence: sequence,
			Ref:      ref,
			RefWidth: concatRefWidth16,
		}
	}

	return nil
}

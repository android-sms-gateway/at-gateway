package modem

import (
	"fmt"

	"github.com/warthog618/sms"
)

// SMS part sizes in PDU mode: a single-segment SMS carries 140 octets of user
// data - 160 GSM-7 septets, or 70 UCS-2 characters - and a concatenated part
// loses 6 octets (7 in 7-bit terms) to the concatenation UDH: 153 GSM-7
// septets or 67 UCS-2 characters per part. Text longer than one part is split
// into concatenated parts carrying a UDH with the 8-bit concatenation
// information element (IEI 0x00) so the receiving phone reassembles it.
const (
	// smsMaxParts is the protocol ceiling for concatenated messages: the
	// sequence number and reference fields of the concatenation IE are 8-bit,
	// so at most 255 parts can exist.
	smsMaxParts = 255
)

// SegmentCount encodes the text exactly like the send path (GSM-7 default
// alphabet with the automatic UCS-2 fallback for non-GSM-7 runes) and returns
// the number of SMS parts it would occupy. An empty text yields (0, nil).
func SegmentCount(text string) (int, error) {
	if text == "" {
		return 0, nil
	}

	pdus, err := sms.Encode([]byte(text))
	if err != nil {
		return 0, fmt.Errorf("encode text: %w", err)
	}

	return len(pdus), nil
}

// ValidateText validates an SMS text for PDU-mode sending: the text must not
// be empty and must not occupy more than maxParts parts. A maxParts of zero
// disables the limit; the protocol ceiling of 255 parts always applies.
func ValidateText(text string, maxParts int) error {
	if text == "" {
		return fmt.Errorf("%w: text is empty", ErrInvalidText)
	}

	parts, err := SegmentCount(text)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidText, err)
	}
	if parts > smsMaxParts {
		return fmt.Errorf("%w: text is %d SMS parts long, maximum is %d", ErrInvalidText, parts, smsMaxParts)
	}
	if maxParts > 0 && parts > maxParts {
		return fmt.Errorf("%w: text is %d SMS parts long, maximum is %d", ErrInvalidText, parts, maxParts)
	}

	return nil
}

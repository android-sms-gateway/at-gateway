package modem

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// SMS text-mode limits: printable 7-bit ASCII runes 0x20-0x7E plus newline
// and carriage return, up to 160 characters (a single-segment SMS). The full
// GSM-7 alphabet (including extended runes) is deferred to a later milestone.
const (
	smsTextMaxChars = 160

	asciiPrintableMin = 0x20 // first printable 7-bit ASCII rune (space)
	asciiPrintableMax = 0x7E // last printable 7-bit ASCII rune (tilde)
	smsNewline        = '\n'
	smsCarriageReturn = '\r'
)

// errInvalidText is the wrapped sentinel for every ValidateASCII rejection;
// the wrapped message describes the specific problem.
var errInvalidText = errors.New("invalid text")

// ValidateASCII validates an SMS text for text-mode sending: printable 7-bit
// ASCII runes (0x20-0x7E) plus newline and carriage return, at most 160
// characters. It rejects an empty text, a text longer than 160 characters,
// and any rune outside the allowed set; the returned error describes the
// first offending character.
func ValidateASCII(text string) error {
	if text == "" {
		return fmt.Errorf("%w: text is empty", errInvalidText)
	}
	if n := utf8.RuneCountInString(text); n > smsTextMaxChars {
		return fmt.Errorf("%w: text is %d characters long, maximum is %d", errInvalidText, n, smsTextMaxChars)
	}
	for pos, r := range text {
		if (r >= asciiPrintableMin && r <= asciiPrintableMax) || r == smsNewline || r == smsCarriageReturn {
			continue
		}

		return fmt.Errorf(
			"%w: text contains unsupported character %q (U+%04X) at position %d: only printable 7-bit ASCII (0x20-0x7E), newline and carriage return are allowed",
			errInvalidText,
			r,
			r,
			pos,
		)
	}

	return nil
}

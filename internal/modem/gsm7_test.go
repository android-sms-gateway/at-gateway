package modem_test

import (
	"strings"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
)

// TestValidateASCII_Accept pins the validator's happy path: printable 7-bit
// ASCII runes 0x20-0x7E plus newline and carriage return are accepted, up to
// the 160-character single-segment limit.
func TestValidateASCII_Accept(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "space only", text: " "},
		{name: "typical", text: "Hello, world! 123"},
		{name: "newline", text: "line one\nline two"},
		{name: "carriage return", text: "line one\rline two"},
		{name: "CRLF pair", text: "line one\r\nline two"},
		{name: "159 chars", text: strings.Repeat("a", 159)},
		{name: "160 chars", text: strings.Repeat("a", 160)},
		{name: "160 chars including newline", text: strings.Repeat("a", 159) + "\n"},
		{name: "all digits and symbols", text: "0123456789!@#$%^&*()_+-=[]{};:'\",./<>?\\|`~"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := modem.ValidateASCII(tt.text); err != nil {
				t.Fatalf("ValidateASCII(%q) = %v, want nil", tt.text, err)
			}
		})
	}
}

// TestValidateASCII_Reject pins the validator's rejection paths: empty text,
// over-length text and any rune outside the printable-ASCII plus newline/
// carriage-return set.
func TestValidateASCII_Reject(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "empty", text: ""},
		{name: "161 chars", text: strings.Repeat("a", 161)},
		{name: "cyrillic", text: "привет"},
		{name: "umlaut", text: "café"},
		{name: "em dash", text: "hello — world"},
		{name: "emoji", text: "hello 🙂"},
		{name: "tab", text: "a\tb"},
		{name: "nul byte", text: "a\x00b"},
		{name: "delete rune", text: "a\x7fb"},
		{name: "non-breaking space", text: "a\u00a0b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := modem.ValidateASCII(tt.text); err == nil {
				t.Fatalf("ValidateASCII(%q) = nil, want error", tt.text)
			}
		})
	}
}

// TestValidateASCII_ErrorDescribesOffendingRune pins the actionable error
// contract: the message names the first offending rune and its code point.
func TestValidateASCII_ErrorDescribesOffendingRune(t *testing.T) {
	err := modem.ValidateASCII("hello ü")
	if err == nil {
		t.Fatal("ValidateASCII = nil, want error")
	}
	if !strings.Contains(err.Error(), "'ü'") {
		t.Fatalf("error %q does not name the offending rune", err)
	}
	if !strings.Contains(err.Error(), "U+00FC") {
		t.Fatalf("error %q does not include the rune code point", err)
	}
}

// TestValidateASCII_ErrorReportsPosition pins the error position: the first
// offending rune is reported with its index.
func TestValidateASCII_ErrorReportsPosition(t *testing.T) {
	err := modem.ValidateASCII("ab£c")
	if err == nil {
		t.Fatal("ValidateASCII = nil, want error")
	}
	if !strings.Contains(err.Error(), "position 2") {
		t.Fatalf("error %q does not report the offending position", err)
	}
}

// TestValidateASCII_TooLongError pins the over-length error message.
func TestValidateASCII_TooLongError(t *testing.T) {
	err := modem.ValidateASCII(strings.Repeat("x", 161))
	if err == nil {
		t.Fatal("ValidateASCII = nil, want error")
	}
	if !strings.Contains(err.Error(), "161") {
		t.Fatalf("error %q does not report the actual length", err)
	}
}

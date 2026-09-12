package modem_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
)

// TestSegmentCount pins the part-count table used by ValidateText and the
// send path: GSM-7 default alphabet with the automatic UCS-2 fallback for
// non-GSM-7 runes. A part holds 153 GSM-7 septets (extended runes like the
// Euro sign cost two septets) or 67 UCS-2 characters once the concatenation
// UDH is present; a single-part SMS fits 160 septets / 70 UCS-2 characters.
func TestSegmentCount(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{"ascii single part", strings.Repeat("a", 160), 1},
		{"ascii just over one part", strings.Repeat("a", 161), 2},
		{"ascii part boundary", strings.Repeat("a", 153), 1},
		{"ascii two full parts", strings.Repeat("a", 306), 2},
		{"ascii just over two parts", strings.Repeat("a", 307), 3},
		{"extended rune two septets", strings.Repeat("€", 80), 1},
		{"extended rune over one part", strings.Repeat("€", 81), 2},
		{"ucs2 single part", strings.Repeat("п", 70), 1},
		{"ucs2 just over one part", strings.Repeat("п", 71), 2},
		{"ucs2 two full parts", strings.Repeat("п", 134), 2},
		{"ucs2 just over two parts", strings.Repeat("п", 135), 3},
		{"emoji surrogate pair", "👍", 1},
		{"protocol ceiling", strings.Repeat("a", 255*153), 255},
		{"empty", "", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := modem.SegmentCount(tt.text)
			if err != nil {
				t.Fatalf("SegmentCount(%q) = error %v, want %d parts", tt.text, err, tt.want)
			}
			if got != tt.want {
				t.Errorf("SegmentCount(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

// TestValidateText_Accept pins the validator happy path: ASCII, extended
// GSM-7 runes and UCS-2 text all pass within the part cap.
func TestValidateText_Accept(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxParts int
	}{
		{"ascii", strings.Repeat("a", 160), 10},
		{"extended runes", strings.Repeat("€", 160), 10},
		{"cyrillic ucs2", strings.Repeat("п", 140), 10},
		{"multi part within cap", strings.Repeat("a", 306), 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := modem.ValidateText(tt.text, tt.maxParts); err != nil {
				t.Fatalf("ValidateText() = %v, want nil", err)
			}
		})
	}
}

// TestValidateText_Reject pins the rejection paths: empty text, text beyond
// the configured cap, and text beyond the protocol ceiling (the cap only
// relaxes within the protocol; a zero maxParts keeps the ceiling).
func TestValidateText_Reject(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxParts int
		wantErr  string
	}{
		{"empty", "", 10, "text is empty"},
		{"over configured cap", strings.Repeat("a", 1531), 10, "text is 11 SMS parts long, maximum is 10"},
		{"protocol ceiling with cap", strings.Repeat("a", 255*153+1), 10, "maximum is 255"},
		{"protocol ceiling without cap", strings.Repeat("a", 255*153+1), 0, "maximum is 255"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := modem.ValidateText(tt.text, tt.maxParts)
			if err == nil {
				t.Fatal("ValidateText() = nil, want error")
			}
			if !errors.Is(err, modem.ErrInvalidText) {
				t.Fatalf("error = %v, want modem.ErrInvalidText", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("message = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// TestValidateText_CapBoundary pins the exact cap boundary: 1530 ASCII
// characters are exactly 10 parts and pass; the configurable cap is
// inclusive.
func TestValidateText_CapBoundary(t *testing.T) {
	if err := modem.ValidateText(strings.Repeat("a", 1530), 10); err != nil {
		t.Fatalf("ValidateText(10 parts) = %v, want nil", err)
	}
}

package modem_test

import (
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
)

// Status report fixture (PDU-mode, SMSC field absent):
//
//	00                          SMSC address length (absent)
//	02                          first octet: TP-MTI 10 (SMS-STATUS-REPORT)
//	2a                          TP-MR 42
//	0b 91 9799001032f4          TP-RA +79990001234 (E.164, TON international)
//	62 90 01 61 03 85 08        TP-SCTS 26/09/01 16:30:58 +02
//	62 90 01 61 03 85 08        TP-DT  same
//	00                          TP-ST 0x00 (short message received by the SME)
const (
	statusReportPhone  = "79990001234"
	statusReportPDU    = "00022a0b919799001032f4629001610385086290016103850800"
	deliveryPDU        = "0000006290016103850800"
	truncatedStatusPDU = "00022a0b919799001032f46290"
)

func TestDecodeDeliveryReport_Success(t *testing.T) {
	report, err := modem.DecodeDeliveryReport(statusReportPDU)
	if err != nil {
		t.Fatalf("DecodeDeliveryReport: %v", err)
	}
	if report.Reference != 42 {
		t.Errorf("Reference = %d, want 42", report.Reference)
	}
	if report.Phone != statusReportPhone {
		t.Errorf("Phone = %q, want %q", report.Phone, statusReportPhone)
	}
	if report.Status != 0x00 {
		t.Errorf("Status = 0x%02X, want 0x00", report.Status)
	}
}

func TestDecodeDeliveryReport_WrongType(t *testing.T) {
	_, err := modem.DecodeDeliveryReport(deliveryPDU)
	if err == nil {
		t.Fatal("DecodeDeliveryReport = nil error, want rejection of a non-status-report PDU")
	}
}

func TestDecodeDeliveryReport_Malformed(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"non hex", "zz"},
		{"truncated", truncatedStatusPDU},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := modem.DecodeDeliveryReport(tt.body); err == nil {
				t.Fatalf("DecodeDeliveryReport(%q) = nil error, want error", tt.body)
			}
		})
	}
}

package modem

import (
	"fmt"

	"github.com/warthog618/sms"
	"github.com/warthog618/sms/encoding/pdumode"
	"github.com/warthog618/sms/encoding/tpdu"
)

// DeliveryReport is one decoded +CDS SMS-STATUS-REPORT: the reference of the
// submitted message part the SC reports about, the recipient address the
// report was issued for (digits only, no leading '+') and the raw TP-ST
// status octet (3GPP TS 23.040 9.2.3.15).
type DeliveryReport struct {
	// Reference is the TP-MR of the submitted part, matching the reference a
	// +CMGS response returned for that part.
	Reference int
	// Phone is the TP-RA recipient address of the original submit. It may be
	// empty when the report carries no usable address.
	Phone string
	// Status is the raw TP-ST octet; classification (delivered, temporary,
	// permanent) is a caller concern.
	Status byte
}

// DecodeDeliveryReport parses the hex PDU body of a "+CDS: <length>" URC
// (PDU-mode SMS-STATUS-REPORT). A body that is not a status report - or that
// cannot be decoded - yields an error; the caller decides how to treat the
// failure (the +CDS stream is log-only when the gateway does not recognize a
// report).
func DecodeDeliveryReport(bodyHex string) (DeliveryReport, error) {
	pdu, err := pdumode.UnmarshalHexString(bodyHex)
	if err != nil {
		return DeliveryReport{}, fmt.Errorf("decode +CDS PDU: %w", err)
	}

	decoded, err := sms.Unmarshal(pdu.TPDU)
	if err != nil {
		return DeliveryReport{}, fmt.Errorf("decode status report: %w", err)
	}
	if decoded.SmsType() != tpdu.SmsStatusReport {
		return DeliveryReport{}, fmt.Errorf("decode status report: %w: %s", errNotStatusReport, decoded.SmsType())
	}

	return DeliveryReport{
		Reference: int(decoded.MR),
		Phone:     decoded.RA.Addr,
		Status:    decoded.ST,
	}, nil
}

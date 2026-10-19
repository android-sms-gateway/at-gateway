package webhooks_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/webhooks"
	"github.com/android-sms-gateway/client-go/smsgateway"
)

// simPtr is the pointer form the wire DTO needs for an attribute the SIM
// actually reported; an unreported attribute is a nil pointer, never a pointer
// to an empty string.
func simPtr(value string) *string {
	return &value
}

// reportedSim is the SIM snapshot of a modem whose SIM answered every optional
// query: the +CNUM (MSISDN), +COPS (carrier) and +CCID (identifier) reads
// at-gateway performs at ready state.
func reportedSim() webhooks.SimCardInfo {
	return webhooks.SimCardInfo{
		SlotIndex:   0,
		SimNumber:   1,
		PhoneNumber: "+79991234567",
		CarrierName: "MegaFon",
		ICCID:       "25001123456789012345",
	}
}

// TestEmitAppStarted_RawEnvelopeIsByteIdenticalToThePayload pins the app:started
// wire contract on the RAW STORED ENVELOPE BYTES: the payload sub-document is
// byte-identical to the marshalled client-go AppStartedPayload (simCards with
// the 0-based slotIndex, the 1-based simNumber and every reported SIM
// attribute), inside an envelope carrying the event, the webhook and the local
// device.
func TestEmitAppStarted_RawEnvelopeIsByteIdenticalToThePayload(t *testing.T) {
	t.Parallel()

	svc, repo, _, devicesSvc := newInboundFixture(t, inboundConfig())
	ctx := context.Background()

	if err := svc.Replace(ctx, newWebhookDTO(
		"app-started",
		nil,
		"https://example.com/app-started",
		smsgateway.WebhookEventAppStarted,
	)); err != nil {
		t.Fatalf("replace webhook: %v", err)
	}

	sim := reportedSim()
	svc.EmitAppStarted([]webhooks.SimCardInfo{sim})

	payloads := storedPayloads(t, repo)
	raw, ok := payloads["app-started"]
	if !ok {
		t.Fatalf("stored payloads = %v, want one for the webhook", payloads)
	}

	var envelope struct {
		ID        string                  `json:"id"`
		WebhookID string                  `json:"webhookId"`
		Event     smsgateway.WebhookEvent `json:"event"`
		DeviceID  string                  `json:"deviceId"`
		Payload   json.RawMessage         `json:"payload"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode stored envelope: %v", err)
	}
	if envelope.Event != smsgateway.WebhookEventAppStarted {
		t.Errorf("envelope event = %q, want %q", envelope.Event, smsgateway.WebhookEventAppStarted)
	}
	if envelope.WebhookID != "app-started" {
		t.Errorf("envelope webhookId = %q, want app-started", envelope.WebhookID)
	}
	if envelope.DeviceID != devicesSvc.Get().ID {
		t.Errorf("envelope deviceId = %q, want the local device", envelope.DeviceID)
	}
	if envelope.ID == "" {
		t.Error("envelope id is empty")
	}

	want, err := json.Marshal(smsgateway.AppStartedPayload{
		SimCards: []smsgateway.SimCard{
			{
				SlotIndex:   sim.SlotIndex,
				SimNumber:   sim.SimNumber,
				PhoneNumber: simPtr(sim.PhoneNumber),
				CarrierName: simPtr(sim.CarrierName),
				ICCID:       simPtr(sim.ICCID),
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal expected payload: %v", err)
	}
	if string(envelope.Payload) != string(want) {
		t.Errorf("stored payload = %s, want byte-identical %s", envelope.Payload, want)
	}
}

// TestEmitAppStarted_SimCardMapping pins the simCards mapping across the SIM
// snapshots a modem can report: an attribute the SIM did not report is
// OMITTED (client-go omitempty pointers), never sent as an empty string, and an
// empty card list drops the simCards key altogether (the payload contract
// allows a device that reports no SIM at all).
func TestEmitAppStarted_SimCardMapping(t *testing.T) {
	t.Parallel()

	secondSlot := webhooks.SimCardInfo{
		SlotIndex:   1,
		SimNumber:   2,
		PhoneNumber: "",
		CarrierName: "Tele2",
		ICCID:       "",
	}

	tests := []struct {
		name     string
		simCards []webhooks.SimCardInfo
		want     []smsgateway.SimCard
		omitted  []string
	}{
		{
			name:     "every attribute reported",
			simCards: []webhooks.SimCardInfo{reportedSim()},
			want: []smsgateway.SimCard{
				{
					SlotIndex:   0,
					SimNumber:   1,
					PhoneNumber: simPtr("+79991234567"),
					CarrierName: simPtr("MegaFon"),
					ICCID:       simPtr("25001123456789012345"),
				},
			},
			omitted: nil,
		},
		{
			name:     "only the mandatory attributes",
			simCards: []webhooks.SimCardInfo{{SlotIndex: 0, SimNumber: 1, PhoneNumber: "", CarrierName: "", ICCID: ""}},
			want: []smsgateway.SimCard{
				{SlotIndex: 0, SimNumber: 1, PhoneNumber: nil, CarrierName: nil, ICCID: nil},
			},
			omitted: []string{"phoneNumber", "carrierName", "iccid"},
		},
		{
			name: "carrier reported, MSISDN and ICCID not",
			simCards: []webhooks.SimCardInfo{
				{SlotIndex: 0, SimNumber: 1, PhoneNumber: "", CarrierName: "MegaFon", ICCID: ""},
			},
			want: []smsgateway.SimCard{
				{SlotIndex: 0, SimNumber: 1, PhoneNumber: nil, CarrierName: simPtr("MegaFon"), ICCID: nil},
			},
			omitted: []string{"phoneNumber", "iccid"},
		},
		{
			name:     "two cards keep their own slot and sim numbers",
			simCards: []webhooks.SimCardInfo{reportedSim(), secondSlot},
			want: []smsgateway.SimCard{
				{
					SlotIndex:   0,
					SimNumber:   1,
					PhoneNumber: simPtr("+79991234567"),
					CarrierName: simPtr("MegaFon"),
					ICCID:       simPtr("25001123456789012345"),
				},
				{
					SlotIndex:   1,
					SimNumber:   2,
					PhoneNumber: nil,
					CarrierName: simPtr("Tele2"),
					ICCID:       nil,
				},
			},
			omitted: nil,
		},
		{
			name:     "no SIM reported at all",
			simCards: nil,
			want:     nil,
			omitted:  []string{"simCards"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, repo, _, _ := newInboundFixture(t, inboundConfig())
			ctx := context.Background()

			if err := svc.Replace(ctx, newWebhookDTO(
				"app-started-mapping",
				nil,
				"https://example.com/app-started",
				smsgateway.WebhookEventAppStarted,
			)); err != nil {
				t.Fatalf("replace webhook: %v", err)
			}

			svc.EmitAppStarted(tt.simCards)

			items := dueItems(t, repo)
			if len(items) != 1 {
				t.Fatalf("queued items = %d, want 1", len(items))
			}

			var envelope struct {
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal([]byte(items[0].Payload), &envelope); err != nil {
				t.Fatalf("decode stored envelope: %v", err)
			}

			want, err := json.Marshal(smsgateway.AppStartedPayload{SimCards: tt.want})
			if err != nil {
				t.Fatalf("marshal expected payload: %v", err)
			}
			if string(envelope.Payload) != string(want) {
				t.Errorf("stored payload = %s, want byte-identical %s", envelope.Payload, want)
			}

			for _, field := range tt.omitted {
				if strings.Contains(string(envelope.Payload), `"`+field+`"`) {
					t.Errorf("stored payload %s carries the unreported field %q", envelope.Payload, field)
				}
			}
			if strings.Contains(string(envelope.Payload), `:""`) {
				t.Errorf("stored payload %s carries an empty-string attribute", envelope.Payload)
			}
		})
	}
}

// TestEmitAppStarted_NoRegisteredWebhookStoresNothing pins the fire-and-forget
// boundary: with no webhook registered for app:started the emission performs no
// queue write at all (emit() no-ops on a non-matching registry), and a webhook
// registered for a DIFFERENT event is never served.
func TestEmitAppStarted_NoRegisteredWebhookStoresNothing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		events []smsgateway.WebhookEvent
	}{
		{name: "empty registry", events: nil},
		{
			name:   "only other events registered",
			events: []smsgateway.WebhookEvent{smsgateway.WebhookEventSmsSent, smsgateway.WebhookEventSystemPing},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, repo, _, _ := newInboundFixture(t, inboundConfig())
			ctx := context.Background()

			for index, event := range tt.events {
				if err := svc.Replace(ctx, newWebhookDTO(
					"other-"+event,
					nil,
					"https://example.com/other",
					event,
				)); err != nil {
					t.Fatalf("replace webhook %d: %v", index, err)
				}
			}

			svc.EmitAppStarted([]webhooks.SimCardInfo{reportedSim()})

			if got := len(dueItems(t, repo)); got != 0 {
				t.Errorf("queued items = %d, want 0", got)
			}
		})
	}
}

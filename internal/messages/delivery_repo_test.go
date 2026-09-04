package messages_test

import (
	"context"
	"errors"
	"testing"

	"github.com/android-sms-gateway/at-gateway/internal/messages"
	"github.com/android-sms-gateway/client-go/smsgateway"
	"github.com/samber/lo"
)

// sendRecipient moves one recipient of a created message through the send
// path states (Processed -> Sent) and records its message reference, the
// state a delivery report later matches on.
func sendRecipient(t *testing.T, repo *messages.Repository, extID, phone string, ref int) {
	t.Helper()
	ctx := context.Background()

	if err := repo.SetRecipientProcessed(ctx, extID, phone); err != nil {
		t.Fatalf("set recipient processed: %v", err)
	}
	if err := repo.SetRecipientSent(ctx, extID, phone, ref); err != nil {
		t.Fatalf("set recipient sent: %v", err)
	}
}

// TestSetRecipientDeliveredByRef_Flip pins the delivery-report transition: a
// Sent recipient whose stored reference matches the report is flipped to
// Delivered, the state history is appended and the owning message ext_id is
// returned.
func TestSetRecipientDeliveredByRef_Flip(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	const phone = "+79990001234"

	input := newInput("m-delivered", phone)
	input.WithDeliveryReport = lo.ToPtr(true)
	if err := repo.Create(ctx, input); err != nil {
		t.Fatalf("create message: %v", err)
	}
	sendRecipient(t, repo, "m-delivered", phone, 42)

	extID, err := repo.SetRecipientDeliveredByRef(ctx, 42, "79990001234")
	if err != nil {
		t.Fatalf("SetRecipientDeliveredByRef: %v", err)
	}
	if extID != "m-delivered" {
		t.Errorf("ext_id = %q, want %q", extID, "m-delivered")
	}

	message, err := repo.GetByID(ctx, "m-delivered")
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	recipient := message.Recipients[0]
	if recipient.State != smsgateway.ProcessingStateDelivered {
		t.Errorf("recipient state = %q, want Delivered", recipient.State)
	}
	if len(recipient.States) != 4 { // Pending, Processed, Sent, Delivered
		t.Errorf("states history has %d entries, want 4", len(recipient.States))
	}
	if latest := recipient.States[len(recipient.States)-1]; latest.State != smsgateway.ProcessingStateDelivered {
		t.Errorf("latest state = %q, want Delivered", latest.State)
	}
}

// TestSetRecipientDeliveredByRef_Defaults pins the default-true semantics: a
// recipient whose message stores no with_delivery_report option (legacy
// rows, or rows enqueued before the feature existed) still accepts the
// report - the option defaults to true when absent.
func TestSetRecipientDeliveredByRef_Defaults(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	const phone = "+79990001234"

	if err := repo.Create(ctx, newInput("m-legacy", phone)); err != nil {
		t.Fatalf("create message: %v", err)
	}
	sendRecipient(t, repo, "m-legacy", phone, 42)

	if _, err := repo.SetRecipientDeliveredByRef(ctx, 42, "79990001234"); err != nil {
		t.Fatalf("SetRecipientDeliveredByRef: %v", err)
	}

	message, err := repo.GetByID(ctx, "m-legacy")
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if state := message.Recipients[0].State; state != smsgateway.ProcessingStateDelivered {
		t.Errorf("recipient state = %q, want Delivered", state)
	}
}

// TestSetRecipientDeliveredByRef_Guards pins every no-match condition: the
// report must reference a Sent recipient of a delivery-report-enabled
// message with a matching phone, and a flip is a one-shot event.
func TestSetRecipientDeliveredByRef_Guards(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, repo *messages.Repository)
		ref   int
		phone string
	}{
		{
			name: "wrong reference",
			setup: func(t *testing.T, repo *messages.Repository) {
				createSentRecipient(t, repo, "m-guard-ref", "+79990001234", 42)
			},
			ref:   43,
			phone: "+79990001234",
		},
		{
			name: "wrong phone",
			setup: func(t *testing.T, repo *messages.Repository) {
				createSentRecipient(t, repo, "m-guard-phone", "+79990001234", 42)
			},
			ref:   42,
			phone: "+79990004321",
		},
		{
			name: "delivery report disabled",
			setup: func(t *testing.T, repo *messages.Repository) {
				input := newInput("m-guard-opt-out", "+79990001234")
				input.WithDeliveryReport = lo.ToPtr(false)
				if err := repo.Create(context.Background(), input); err != nil {
					t.Fatalf("create message: %v", err)
				}
				sendRecipient(t, repo, "m-guard-opt-out", "+79990001234", 42)
			},
			ref:   42,
			phone: "+79990001234",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := newRepository(t)
			ctx := context.Background()

			tt.setup(t, repo)

			if _, err := repo.SetRecipientDeliveredByRef(ctx, tt.ref, tt.phone); !errors.Is(err, messages.ErrNotFound) {
				t.Fatalf("SetRecipientDeliveredByRef = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestSetRecipientDeliveredByRef_Duplicate pins the idempotency of reports:
// a second report for the same reference finds no Sent recipient (it already
// flipped) and yields ErrNotFound.
func TestSetRecipientDeliveredByRef_Duplicate(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	const phone = "+79990001234"

	createSentRecipient(t, repo, "m-dup", phone, 42)

	if _, err := repo.SetRecipientDeliveredByRef(ctx, 42, "79990001234"); err != nil {
		t.Fatalf("first SetRecipientDeliveredByRef: %v", err)
	}
	if _, err := repo.SetRecipientDeliveredByRef(ctx, 42, "79990001234"); !errors.Is(err, messages.ErrNotFound) {
		t.Fatalf("duplicate SetRecipientDeliveredByRef = %v, want ErrNotFound", err)
	}
}

// TestSetRecipientDeliveredByRef_OldestMatch pins the MR-wrap tie-break: two
// open recipients sharing one reference (the 8-bit TP-MR wrapped) are
// resolved by phone, and a phone-less lookup flips the OLDEST one only.
func TestSetRecipientDeliveredByRef_OldestMatch(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	const phoneA = "+79990001234"
	const phoneB = "+79990004321"

	createSentRecipient(t, repo, "m-wrap-a", phoneA, 7)
	createSentRecipient(t, repo, "m-wrap-b", phoneB, 7)

	extID, err := repo.SetRecipientDeliveredByRef(ctx, 7, "79990004321")
	if err != nil {
		t.Fatalf("SetRecipientDeliveredByRef: %v", err)
	}
	if extID != "m-wrap-b" {
		t.Errorf("ext_id = %q, want the matching message %q", extID, "m-wrap-b")
	}

	messageB, err := repo.GetByID(ctx, "m-wrap-b")
	if err != nil {
		t.Fatalf("get message b: %v", err)
	}
	if state := messageB.Recipients[0].State; state != smsgateway.ProcessingStateDelivered {
		t.Errorf("recipient b state = %q, want Delivered", state)
	}

	messageA, err := repo.GetByID(ctx, "m-wrap-a")
	if err != nil {
		t.Fatalf("get message a: %v", err)
	}
	if state := messageA.Recipients[0].State; state != smsgateway.ProcessingStateSent {
		t.Errorf("recipient a state = %q, want Sent (untouched)", state)
	}
}

// TestSetRecipientFailedByRef pins the permanent-failure transition: the
// flipped recipient carries the report reason in its error field.
func TestSetRecipientFailedByRef(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	const phone = "+79990001234"
	const reason = "delivery report: SC status 0x41"

	createSentRecipient(t, repo, "m-failed-dr", phone, 42)

	extID, err := repo.SetRecipientFailedByRef(ctx, 42, "79990001234", reason)
	if err != nil {
		t.Fatalf("SetRecipientFailedByRef: %v", err)
	}
	if extID != "m-failed-dr" {
		t.Errorf("ext_id = %q, want %q", extID, "m-failed-dr")
	}

	message, err := repo.GetByID(ctx, "m-failed-dr")
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	recipient := message.Recipients[0]
	if recipient.State != smsgateway.ProcessingStateFailed {
		t.Errorf("recipient state = %q, want Failed", recipient.State)
	}
	if recipient.Error == nil || *recipient.Error != reason {
		t.Errorf("recipient error = %v, want %q", recipient.Error, reason)
	}
}

// createSentRecipient creates one message with a single Sent recipient
// carrying the given reference.
func createSentRecipient(t *testing.T, repo *messages.Repository, extID, phone string, ref int) {
	t.Helper()

	input := newInput(extID, phone)
	input.WithDeliveryReport = lo.ToPtr(true)
	if err := repo.Create(context.Background(), input); err != nil {
		t.Fatalf("create message: %v", err)
	}
	sendRecipient(t, repo, extID, phone, ref)
}

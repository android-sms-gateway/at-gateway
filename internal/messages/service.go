package messages

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/client-go/smsgateway"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/nyaruka/phonenumbers"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

type Service struct {
	config   Config
	messages *Repository

	devicesSvc *devices.Service
	modemSvc   *modem.Service

	metrics *Metrics

	logger *zap.Logger
}

func NewService(
	config Config,
	messages *Repository,
	devicesSvc *devices.Service,
	modemSvc *modem.Service,
	metrics *Metrics,
	logger *zap.Logger,
) *Service {
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.DefaultRegion == "" {
		config.DefaultRegion = defaultRegion
	}

	return &Service{
		config:   config,
		messages: messages,

		devicesSvc: devicesSvc,
		modemSvc:   modemSvc,

		metrics: metrics,

		logger: logger,
	}
}

// EnqueueOptions controls how an enqueued message is prepared.
type EnqueueOptions struct {
	// SkipPhoneValidation disables E.164 validation and normalization of
	// phone numbers; numbers are stored verbatim.
	SkipPhoneValidation bool
}

// Enqueue validates the input, generates the ext_id when absent (the service
// is the SOLE ext_id generator), resolves the device ID and persists the
// message as Pending. The returned message carries the resolved device ID.
func (s *Service) Enqueue(ctx context.Context, input MessageInput, opts EnqueueOptions) (*Message, error) {
	if input.IsEncrypted {
		return nil, fmt.Errorf("%w: encrypted messages are not supported", ErrNotSupported)
	}
	if input.SimNumber != nil && *input.SimNumber != 1 {
		return nil, fmt.Errorf("%w: only SIM 1 is supported", ErrNotSupported)
	}

	// At least one non-empty phone number.
	if len(input.PhoneNumbers) == 0 || slices.Contains(input.PhoneNumbers, "") {
		return nil, ErrInvalidPhoneNumbers
	}

	// Normalize phone numbers unless explicitly skipped.
	if !opts.SkipPhoneValidation {
		for i, v := range input.PhoneNumbers {
			phone, err := s.cleanPhoneNumber(v)
			if err != nil {
				return nil, fmt.Errorf("failed to use phone in row %d: %w", i+1, err)
			}
			input.PhoneNumbers[i] = phone
		}
	}

	if input.ExtID == "" {
		input.ExtID = gonanoid.Must()
	}
	if input.DeviceID == nil {
		input.DeviceID = lo.ToPtr(s.devicesSvc.Get().ID)
	}
	if *input.DeviceID != s.devicesSvc.Get().ID {
		return nil, fmt.Errorf("%w: %q", ErrDeviceNotFound, *input.DeviceID)
	}

	if err := s.messages.Create(ctx, &input); err != nil {
		return nil, fmt.Errorf("create message: %w", err)
	}
	s.metrics.EnqueuedTotal.Inc()

	message, err := s.messages.GetByID(ctx, input.ExtID)
	if err != nil {
		return nil, fmt.Errorf("load enqueued message: %w", err)
	}

	return message, nil
}

// Get returns the message with the given ext_id.
func (s *Service) Get(ctx context.Context, extID string) (*Message, error) {
	message, err := s.messages.GetByID(ctx, extID)
	if err != nil {
		return nil, err
	}

	return message, nil
}

// List returns one page of messages plus the total count matching the filter.
func (s *Service) List(ctx context.Context, options ListOptions) ([]Message, int, error) {
	result, total, err := s.messages.List(ctx, options)
	if err != nil {
		return nil, 0, err
	}

	return result, total, nil
}

// Cancel moves a Pending message to Cancelled and returns the updated message.
func (s *Service) Cancel(ctx context.Context, extID string) (*Message, error) {
	message, err := s.messages.Cancel(ctx, extID)
	if err != nil {
		return nil, err
	}
	s.metrics.CancelledTotal.Inc()

	return message, nil
}

func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case report := <-s.modemSvc.DeliveryReports():
			s.handleDeliveryReport(ctx, report)
		case <-ticker.C:
			for s.processPending(ctx) {
				select {
				case <-ctx.Done():
					return nil
				default:
				}
			}
		}
	}
}

// handleDeliveryReport applies one +CDS status report to the recipient that
// requested it and re-derives the message state. It runs on the Run loop
// goroutine, so database work is serialized with message processing. Errors
// are logged and swallowed - a status report must never take the worker
// down.
func (s *Service) handleDeliveryReport(ctx context.Context, report modem.DeliveryReport) {
	state, reason, handled := deliveryReportState(report.Status)
	if !handled {
		// The SC reports a temporary error (0x20..0x3F) and will retry on
		// its own; the recipient stays Sent.
		s.logger.Debug("delivery report ignored: temporary error",
			zap.Int("ref", report.Reference),
			zap.Int("status", int(report.Status)),
		)
		return
	}

	var extID string
	var err error
	if state == smsgateway.ProcessingStateDelivered {
		extID, err = s.messages.SetRecipientDeliveredByRef(ctx, report.Reference, report.Phone)
	} else {
		extID, err = s.messages.SetRecipientFailedByRef(ctx, report.Reference, report.Phone, reason)
	}
	if errors.Is(err, ErrNotFound) {
		// No Sent recipient owns this reference (duplicate report, unknown
		// message, MR reused after a wrap, or the report was not requested).
		s.logger.Debug("delivery report ignored: no matching recipient",
			zap.Int("ref", report.Reference),
			zap.Int("status", int(report.Status)),
		)
		return
	}
	if err != nil {
		s.logger.Error("apply delivery report", zap.Error(err))
		return
	}

	s.logger.Debug("delivery report applied",
		zap.String("ext_id", extID),
		zap.Int("ref", report.Reference),
		zap.String("state", string(state)),
	)
	if state == smsgateway.ProcessingStateDelivered {
		s.metrics.DeliveredTotal.Inc()
	} else {
		s.metrics.FailedTotal.Inc()
	}

	// Re-derive the message state from its recipients: all Delivered flips
	// the message to Delivered, all-terminal-with-at-least-one-Failed flips
	// it to Failed. resolveFinalState mirrors the send-batch resolution, so
	// the same mixed-outcome rules apply; SetState guards keep terminal
	// states terminal.
	message, err := s.messages.GetByID(ctx, extID)
	if err != nil {
		s.logger.Error("load delivery-reported message", zap.Error(err))
		return
	}

	states := make([]smsgateway.ProcessingState, 0, len(message.Recipients))
	for _, recipient := range message.Recipients {
		states = append(states, recipient.State)
	}
	if finalState := s.resolveFinalState(states); finalState != message.State {
		if stateErr := s.messages.SetState(ctx, extID, finalState); stateErr != nil {
			s.logger.Error("set message state after delivery report", zap.Error(stateErr))
		}
	}
}

// deliveryReportState maps a raw TP-ST octet onto the recipient state the
// report demands, following the Android gateway's classification (3GPP TS
// 23.040 9.2.3.15):
//
//   - 0x00..0x1F: the message reached the recipient - Delivered.
//   - 0x20..0x3F: temporary error - the SC is (or was) retrying; the
//     recipient stays Sent and the report is ignored (handled=false).
//   - 0x40..0x7F: permanent error - Failed with the status as the reason.
func deliveryReportState(status byte) (smsgateway.ProcessingState, string, bool) {
	switch {
	case status < 0x20: //nolint:mnd // TP-ST class boundaries
		return smsgateway.ProcessingStateDelivered, "", true
	case status < 0x40: //nolint:mnd // TP-ST class boundaries
		return "", "", false
	default:
		return smsgateway.ProcessingStateFailed,
			fmt.Sprintf("delivery report: SC status 0x%02X", status), true
	}
}

func (s *Service) processPending(ctx context.Context) bool {
	message, err := s.messages.DequeueNextPending(ctx)
	if errors.Is(err, ErrNotFound) {
		return false
	}

	if err != nil {
		s.logger.Error("dequeue next pending message", zap.Error(err))
		return false
	}

	s.logger.Debug("processing pending message", zap.String("ext_id", message.ID))

	states := make([]smsgateway.ProcessingState, 0, len(message.Recipients))
	for _, recipient := range message.Recipients {
		state, recipientErr := s.processRecipient(ctx, message, recipient)
		if recipientErr != nil {
			s.logger.Error("process recipient", zap.Error(recipientErr))
			return false
		}
		states = append(states, state)
	}

	finalState := s.resolveFinalState(states)
	s.logger.Debug("final state", zap.String("ext_id", message.ID), zap.String("state", string(finalState)))

	if stateErr := s.messages.SetState(ctx, message.ID, finalState); stateErr != nil {
		s.logger.Error("set state", zap.Error(stateErr))
		return false
	}

	return true
}

func (s *Service) processRecipient(
	ctx context.Context,
	message *Message,
	recipient Recipient,
) (smsgateway.ProcessingState, error) {
	if !recipient.processable() {
		return recipient.State, nil
	}

	if err := s.messages.SetRecipientProcessed(ctx, message.ID, recipient.PhoneNumber); err != nil {
		return smsgateway.ProcessingStateFailed, err
	}

	if message.TextContent == nil {
		s.metrics.FailedTotal.Inc()

		return smsgateway.ProcessingStateFailed, s.messages.SetRecipientFailed(
			ctx,
			message.ID,
			recipient.PhoneNumber,
			"only text messages are supported",
		)
	}

	// Preflight the part count BEFORE any modem traffic: the encode is
	// deterministic and a text that cannot be sent within the configured cap
	// must never burn modem commands. The check mirrors the send-path
	// encoding (GSM-7 default alphabet, UCS-2 fallback), so a text passing
	// here always fits the cap.
	if err := modem.ValidateText(message.TextContent.Text, s.config.MaxSegments); err != nil {
		s.metrics.FailedTotal.Inc()

		return smsgateway.ProcessingStateFailed, s.messages.SetRecipientFailed(
			ctx,
			message.ID,
			recipient.PhoneNumber,
			err.Error(),
		)
	}

	// A missing withDeliveryReport option defaults to TRUE (ecosystem
	// parity): the SC is asked for a status report unless the client
	// explicitly opted out. The reference of the LAST accepted part is
	// stored below and matches the TP-MR of the report requested on that
	// part.
	refs, err := s.modemSvc.SendSMS(
		ctx,
		recipient.PhoneNumber,
		message.TextContent.Text,
		lo.FromPtrOr(message.WithDeliveryReport, true),
	)
	if err != nil {
		s.metrics.FailedTotal.Inc()

		return smsgateway.ProcessingStateFailed, s.messages.SetRecipientFailed(
			ctx,
			message.ID,
			recipient.PhoneNumber,
			err.Error(),
		)
	}

	// ref_id stores the reference of the LAST accepted part: a recipient is
	// Sent only when the whole multi-part sequence reached the modem.
	s.metrics.SentTotal.Inc()

	return smsgateway.ProcessingStateSent, s.messages.SetRecipientSent(
		ctx,
		message.ID,
		recipient.PhoneNumber,
		refs[len(refs)-1],
	)
}

func (s *Service) resolveFinalState(states []smsgateway.ProcessingState) smsgateway.ProcessingState {
	finalState := smsgateway.ProcessingStateSent

	switch {
	case slices.Contains(states, smsgateway.ProcessingStatePending):
		finalState = smsgateway.ProcessingStatePending
	case slices.Contains(states, smsgateway.ProcessingStateCancelled):
		finalState = smsgateway.ProcessingStateCancelled
	case slices.Contains(states, smsgateway.ProcessingStateProcessed):
		finalState = smsgateway.ProcessingStateProcessed

	case lo.Count(states, smsgateway.ProcessingStateFailed) == len(states):
		finalState = smsgateway.ProcessingStateFailed
	case lo.Count(states, smsgateway.ProcessingStateDelivered) == len(states):
		finalState = smsgateway.ProcessingStateDelivered
	}

	return finalState
}

// cleanPhoneNumber parses the input as a phone number of the configured
// default region, requires a valid mobile number and returns its canonical
// E.164 form. The region is only used for numbers without an international
// prefix.
func (s *Service) cleanPhoneNumber(input string) (string, error) {
	phone, err := phonenumbers.Parse(input, s.config.DefaultRegion)
	if err != nil {
		return input, fmt.Errorf("%w: %s", ErrInvalidPhoneNumbers, err.Error())
	}

	if !phonenumbers.IsValidNumber(phone) {
		return input, fmt.Errorf("%w: invalid phone number", ErrInvalidPhoneNumbers)
	}

	phoneNumberType := phonenumbers.GetNumberType(phone)
	if phoneNumberType != phonenumbers.MOBILE && phoneNumberType != phonenumbers.FIXED_LINE_OR_MOBILE {
		return input, fmt.Errorf("%w: not mobile phone number", ErrInvalidPhoneNumbers)
	}

	return phonenumbers.Format(phone, phonenumbers.E164), nil
}

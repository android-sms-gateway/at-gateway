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
	"github.com/samber/lo"
	"go.uber.org/zap"
)

// pollIntervalFallback is the worker sleep after an empty queue when the
// configured PollInterval is zero or negative.
const pollIntervalFallback = time.Second

// Service owns the message business rules: all validation, the ext_id
// generation and the background FIFO send worker. It is the only layer that
// talks to the modem; the repository trusts its inputs.
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
	return &Service{
		config:   config,
		messages: messages,

		devicesSvc: devicesSvc,
		modemSvc:   modemSvc,

		metrics: metrics,

		logger: logger,
	}
}

// Enqueue validates the input, generates the ext_id when absent (the service
// is the SOLE ext_id generator), resolves the device ID and persists the
// message as Pending. The returned message carries the resolved device ID.
func (s *Service) Enqueue(ctx context.Context, input MessageInput) (*Message, error) {
	// Content shape first: both text+data or neither -> ErrInvalidContent
	// (MessageInput.Content wraps the sentinel; marshal errors pass through).
	if _, _, err := input.Content(); err != nil {
		return nil, err
	}
	// Data messages are not supported (MVP).
	if input.DataContent != nil {
		return nil, ErrNotSupported
	}
	// Text bodies must pass the modem ASCII rules.
	if input.TextContent != nil {
		if err := modem.ValidateASCII(input.TextContent.Text); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidText, err.Error())
		}
	}
	// At least one non-empty phone number.
	if len(input.PhoneNumbers) == 0 || slices.Contains(input.PhoneNumbers, "") {
		return nil, ErrInvalidPhoneNumbers
	}

	if input.ExtID == "" {
		input.ExtID = gonanoid.Must()
	}
	if input.DeviceID == nil {
		input.DeviceID = lo.ToPtr(s.devicesSvc.Get().ID)
	}

	if err := s.messages.Create(ctx, &input); err != nil {
		return nil, fmt.Errorf("create message: %w", err)
	}
	if s.metrics != nil {
		s.metrics.EnqueuedTotal.Inc()
	}

	message, err := s.messages.GetByID(ctx, input.ExtID)
	if err != nil {
		return nil, fmt.Errorf("load enqueued message: %w", err)
	}
	message.DeviceID = *input.DeviceID

	return message, nil
}

// Get returns the message with the given ext_id.
func (s *Service) Get(ctx context.Context, extID string) (*Message, error) {
	message, err := s.messages.GetByID(ctx, extID)
	if err != nil {
		return nil, err
	}
	message.DeviceID = s.devicesSvc.Get().ID

	return message, nil
}

// List returns one page of messages plus the total count matching the filter.
func (s *Service) List(ctx context.Context, filter ListFilter) ([]Message, int, error) {
	result, total, err := s.messages.List(ctx, filter)
	if err != nil {
		return nil, 0, err
	}

	deviceID := s.devicesSvc.Get().ID
	for i := range result {
		result[i].DeviceID = deviceID
	}

	return result, total, nil
}

// Cancel moves a Pending message to Cancelled and returns the updated message.
func (s *Service) Cancel(ctx context.Context, extID string) (*Message, error) {
	message, err := s.messages.Cancel(ctx, extID)
	if err != nil {
		return nil, err
	}
	if s.metrics != nil {
		s.metrics.CancelledTotal.Inc()
	}
	message.DeviceID = s.devicesSvc.Get().ID

	return message, nil
}

// Run is the background FIFO worker: it polls the oldest Pending message,
// sends every recipient through the modem and moves the message along the
// android state ladder. It returns only when ctx is done.
func (s *Service) Run(ctx context.Context) error {
	poll := s.config.PollInterval
	if poll <= 0 {
		poll = pollIntervalFallback
	}

	for ctx.Err() == nil {
		message, err := s.messages.GetNextPending(ctx)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				s.logger.Error("get next pending message", zap.Error(err))
			}
			if !sleep(ctx, poll) {
				return nil
			}
			continue
		}

		s.processMessage(ctx, message)
	}

	return nil
}

// processMessage sends one message recipient-first and moves the message
// state via the ladder. It is safe against concurrent cancellation: a message
// that leaves Pending mid-send (Cancelled) or reaches the terminal Failed
// state is never moved further.
func (s *Service) processMessage(ctx context.Context, message *Message) {
	// Pre-send Pending re-check: the message may have left Pending (e.g.
	// cancelled) between GetNextPending and here.
	if current, err := s.messages.GetByID(ctx, message.ID); err != nil ||
		current.State != smsgateway.ProcessingStatePending {
		return
	}

	// Only Enqueue-created messages reach the worker; a textless message
	// (created directly through the repository) is failed so it leaves the
	// queue instead of panicking or spinning.
	if message.TextContent == nil {
		s.failTextless(ctx, message)
		return
	}

	currentState := message.State
	states := make([]smsgateway.ProcessingState, len(message.Recipients))
	for i := range states {
		// Unprocessed recipients are still Pending: the ladder sees the
		// current state of every recipient, so the message stays Pending
		// until the last recipient is handled.
		states[i] = smsgateway.ProcessingStatePending
	}
	for i, recipient := range message.Recipients {
		states[i] = s.sendRecipient(ctx, message.ID, recipient.PhoneNumber, message.TextContent.Text)

		derived := deriveMessageState(states)
		if derived == currentState {
			continue
		}
		if !s.advanceState(ctx, message.ID, derived) {
			return
		}
		currentState = derived
	}
}

// sendRecipient transmits one SMS through the modem and records the outcome
// on the recipient (ref on success, error on failure). It returns the state
// the recipient reached.
func (s *Service) sendRecipient(ctx context.Context, messageID, phone, text string) smsgateway.ProcessingState {
	refID, sendErr := s.modemSvc.SendSMS(ctx, phone, text)
	if sendErr != nil {
		errStr := sendErr.Error()
		if updateErr := s.messages.UpdateRecipientState(
			ctx, messageID, phone, smsgateway.ProcessingStateFailed, nil, &errStr,
		); updateErr != nil {
			s.logger.Error("update recipient state", zap.Error(updateErr))
		}
		if s.metrics != nil {
			s.metrics.FailedTotal.Inc()
		}

		return smsgateway.ProcessingStateFailed
	}

	if setErr := s.messages.SetRecipientRef(ctx, messageID, phone, refID); setErr != nil {
		s.logger.Error("set recipient ref", zap.Error(setErr))
	}
	if updateErr := s.messages.UpdateRecipientState(
		ctx, messageID, phone, smsgateway.ProcessingStateSent, &refID, nil,
	); updateErr != nil {
		s.logger.Error("update recipient state", zap.Error(updateErr))
	}
	if s.metrics != nil {
		s.metrics.SentTotal.Inc()
	}

	return smsgateway.ProcessingStateSent
}

// advanceState appends derived to the message state history. It reports
// whether the append happened: a message that left Pending mid-send (cancel
// race) or reached the terminal Failed state is never moved further.
func (s *Service) advanceState(ctx context.Context, messageID string, derived smsgateway.ProcessingState) bool {
	if current, err := s.messages.GetByID(ctx, messageID); err != nil ||
		current.State != smsgateway.ProcessingStatePending {
		return false
	}
	if appendErr := s.messages.AppendMessageState(ctx, messageID, derived); appendErr != nil {
		s.logger.Error("append message state", zap.Error(appendErr))
		return false
	}

	return true
}

// failTextless marks every recipient Failed and moves the message to the
// terminal Failed state (defensive path for messages created outside Enqueue).
func (s *Service) failTextless(ctx context.Context, message *Message) {
	errStr := "message has no text content"
	for _, recipient := range message.Recipients {
		if updateErr := s.messages.UpdateRecipientState(
			ctx, message.ID, recipient.PhoneNumber, smsgateway.ProcessingStateFailed, nil, &errStr,
		); updateErr != nil {
			s.logger.Error("update recipient state", zap.Error(updateErr))
		}
	}
	if appendErr := s.messages.AppendMessageState(ctx, message.ID, smsgateway.ProcessingStateFailed); appendErr != nil {
		s.logger.Error("append message state", zap.Error(appendErr))
	}
}

// deriveMessageState aggregates the current recipient states onto the message
// state following the android ladder: any Pending -> Pending, any Cancelled
// -> Cancelled, all Failed -> Failed, else -> Sent.
func deriveMessageState(recipientStates []smsgateway.ProcessingState) smsgateway.ProcessingState {
	if slices.Contains(recipientStates, smsgateway.ProcessingStatePending) {
		return smsgateway.ProcessingStatePending
	}
	if slices.Contains(recipientStates, smsgateway.ProcessingStateCancelled) {
		return smsgateway.ProcessingStateCancelled
	}
	for _, state := range recipientStates {
		if state != smsgateway.ProcessingStateFailed {
			return smsgateway.ProcessingStateSent
		}
	}

	return smsgateway.ProcessingStateFailed
}

// sleep waits for the full duration or until ctx is done; it reports whether
// the duration elapsed (false means the context was canceled).
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

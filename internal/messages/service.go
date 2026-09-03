package messages

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/devices"
	"github.com/android-sms-gateway/at-gateway/internal/modem"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/samber/lo"
	"go.uber.org/zap"
)

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
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
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

// Enqueue validates the input, generates the ext_id when absent (the service
// is the SOLE ext_id generator), resolves the device ID and persists the
// message as Pending. The returned message carries the resolved device ID.
func (s *Service) Enqueue(ctx context.Context, input MessageInput) (*Message, error) {
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

func (s *Service) processPending(ctx context.Context) bool {
	message, err := s.messages.DequeueNextPending(ctx)
	if errors.Is(err, ErrNotFound) {
		return false
	}

	if err != nil {
		s.logger.Error("dequeue next pending message", zap.Error(err))
		return true
	}

	s.logger.Debug("processing pending message", zap.String("ext_id", message.ID))

	return true
}

// poll := s.config.PollInterval
// if poll <= 0 {
// 	poll = time.Second
// }

// for ctx.Err() == nil {
// 	message, err := s.messages.GetNextPending(ctx)
// 	if err != nil {
// 		if !errors.Is(err, ErrNotFound) {
// 			s.logger.Error("get next pending message", zap.Error(err))
// 		}
// 		if !s.sleepOrDone(ctx, poll) {
// 			return nil
// 		}
// 		continue
// 	}

// 	if message.TextContent == nil {
// 		// Data messages are not supported (MVP).
// 		continue
// 	}

// 	// Pre-send Pending re-check: the message may have left Pending (e.g.
// 	// cancelled) between GetNextPending and here.
// 	current, err := s.messages.GetByID(ctx, message.ID)
// 	if err != nil || current.State != smsgateway.ProcessingStatePending {
// 		continue
// 	}

// 	currentState := message.State
// 	states := make([]smsgateway.ProcessingState, len(message.Recipients))
// 	for i := range states {
// 		states[i] = smsgateway.ProcessingStatePending
// 	}
// 	for i, recipient := range message.Recipients {
// 		states[i] = s.sendRecipient(ctx, message, recipient)

// 		var shouldBreak bool
// 		currentState, shouldBreak = s.advanceMessageState(ctx, message, deriveMessageState(states), currentState)
// 		if shouldBreak {
// 			break
// 		}
// 	}
// }

// return nil

// // sendRecipient dispatches a single SMS through the modem and persists the
// // per-recipient outcome (Sent or Failed). It returns the resulting recipient
// // processing state.
// func (s *Service) sendRecipient(ctx context.Context, message *Message, recipient Recipient) smsgateway.ProcessingState {
// 	refID, sendErr := s.modemSvc.SendSMS(ctx, recipient.PhoneNumber, message.TextContent.Text)
// 	if sendErr != nil {
// 		errStr := sendErr.Error()
// 		if updateErr := s.messages.UpdateRecipientState(
// 			ctx, message.ID, recipient.PhoneNumber, smsgateway.ProcessingStateFailed, nil, &errStr,
// 		); updateErr != nil {
// 			s.logger.Error("update recipient state", zap.Error(updateErr))
// 		}
// 		if s.metrics != nil {
// 			s.metrics.FailedTotal.Inc()
// 		}

// 		return smsgateway.ProcessingStateFailed
// 	}

// 	if setErr := s.messages.SetRecipientRef(ctx, message.ID, recipient.PhoneNumber, refID); setErr != nil {
// 		s.logger.Error("set recipient ref", zap.Error(setErr))
// 	}
// 	if updateErr := s.messages.UpdateRecipientState(
// 		ctx, message.ID, recipient.PhoneNumber, smsgateway.ProcessingStateSent, &refID, nil,
// 	); updateErr != nil {
// 		s.logger.Error("update recipient state", zap.Error(updateErr))
// 	}
// 	if s.metrics != nil {
// 		s.metrics.SentTotal.Inc()
// 	}

// 	return smsgateway.ProcessingStateSent
// }

// // advanceMessageState derives the next message state from the recipient states
// // array and, when it differs from currentState, persists it via an atomic
// // append. It returns the (possibly unchanged) currentState and a shouldBreak
// // flag: true when the recipient loop must stop (message left Pending or append
// // failed).
// func (s *Service) advanceMessageState(
// 	ctx context.Context,
// 	message *Message,
// 	derived smsgateway.ProcessingState,
// 	currentState smsgateway.ProcessingState,
// ) (smsgateway.ProcessingState, bool) {
// 	if derived == currentState {
// 		return currentState, false
// 	}

// 	// Cancel-race guard: only a message that is still Pending may move
// 	// forward; a mid-send Cancel or terminal Failed state stops the run
// 	// for the remaining recipients.
// 	latest, stateErr := s.messages.GetByID(ctx, message.ID)
// 	if stateErr != nil || latest.State != smsgateway.ProcessingStatePending {
// 		return currentState, true
// 	}

// 	if appendErr := s.messages.AppendMessageState(ctx, message.ID, derived); appendErr != nil {
// 		s.logger.Error("append message state", zap.Error(appendErr))

// 		return currentState, true
// 	}

// 	return derived, false
// }

// // sleepOrDone blocks for d or until ctx is done. It returns true when the
// // timer fired (caller should continue), false when ctx was done (caller
// // should return).
// func (s *Service) sleepOrDone(ctx context.Context, d time.Duration) bool {
// 	timer := time.NewTimer(d)
// 	defer timer.Stop()

// 	select {
// 	case <-ctx.Done():
// 		return false
// 	case <-timer.C:
// 		return true
// 	}
// }

// // deriveMessageState aggregates the current recipient states onto the message
// // state following the android ladder: any Pending -> Pending, any Cancelled
// // -> Cancelled, all Failed -> Failed, else -> Sent.
// func deriveMessageState(recipientStates []smsgateway.ProcessingState) smsgateway.ProcessingState {
// 	if slices.Contains(recipientStates, smsgateway.ProcessingStatePending) {
// 		return smsgateway.ProcessingStatePending
// 	}
// 	if slices.Contains(recipientStates, smsgateway.ProcessingStateCancelled) {
// 		return smsgateway.ProcessingStateCancelled
// 	}
// 	for _, state := range recipientStates {
// 		if state != smsgateway.ProcessingStateFailed {
// 			return smsgateway.ProcessingStateSent
// 		}
// 	}

// 	return smsgateway.ProcessingStateFailed
// }

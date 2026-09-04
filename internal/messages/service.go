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
		return recipient.State, s.messages.SetRecipientFailed(
			ctx,
			message.ID,
			recipient.PhoneNumber,
			"only text messages are supported",
		)
	}

	refID, err := s.modemSvc.SendSMS(ctx, recipient.PhoneNumber, message.TextContent.Text)
	if err != nil {
		return smsgateway.ProcessingStateFailed, s.messages.SetRecipientFailed(
			ctx,
			message.ID,
			recipient.PhoneNumber,
			err.Error(),
		)
	}

	return smsgateway.ProcessingStateSent, s.messages.SetRecipientSent(ctx, message.ID, recipient.PhoneNumber, refID)
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

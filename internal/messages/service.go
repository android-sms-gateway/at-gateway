package messages

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/android-sms-gateway/at-gateway/internal/modem"
	"github.com/android-sms-gateway/client-go/smsgateway"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"go.uber.org/zap"
)

// Service is the concrete messages business service: it validates and
// persists enqueued messages, serves message-state queries, cancels pending
// messages and runs the FIFO background send worker. No interfaces: the send
// dependency is bound to the concrete *modem.Service in New.
type Service struct {
	config Config

	repo *Repository

	// sendSMS sends one text SMS and returns the modem message reference
	// (+CMGS <mr>). It is bound to *modem.Service.SendSMS in New; tests
	// replace it with a concrete fake sender stub.
	sendSMS func(ctx context.Context, phoneNumber, text string) (int, error)

	metrics *Metrics
	logger  *zap.Logger
}

// New returns a Service bound to the given concrete modem Service.
func New(config Config, repo *Repository, sender *modem.Service, metrics *Metrics, logger *zap.Logger) *Service {
	return &Service{
		config:  config,
		repo:    repo,
		sendSMS: sender.SendSMS,
		metrics: metrics,
		logger:  logger,
	}
}

// Enqueue validates and persists a new text message in Pending state and
// returns its state DTO; the DTO ID is the resource ID for Location headers.
// The input is the wire DTO the HTTP handler already parsed: DataMessage is
// rejected with ErrNotSupported, deviceId is ignored (single-device MVP), the
// text comes from TextMessage (or the deprecated Message field via
// GetTextMessage) and must pass modem.ValidateASCII, and PhoneNumbers must
// contain at least one non-empty entry. TTL/ValidUntil/ScheduleAt and
// isEncrypted are ignored (scheduling and E2E are out of MVP scope; a
// client-supplied ID is replaced by a generated one).
func (s *Service) Enqueue(ctx context.Context, request smsgateway.Message) (*smsgateway.MessageState, error) {
	if request.GetDataMessage() != nil {
		return nil, ErrNotSupported
	}

	textMessage := request.GetTextMessage()
	if textMessage == nil {
		return nil, fmt.Errorf("%w: textMessage is required", ErrInvalidText)
	}
	if err := modem.ValidateASCII(textMessage.Text); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidText, err)
	}

	if len(request.PhoneNumbers) == 0 {
		return nil, ErrInvalidPhoneNumbers
	}
	if slices.Contains(request.PhoneNumbers, "") {
		return nil, fmt.Errorf("%w: phone number must not be empty", ErrInvalidPhoneNumbers)
	}

	now := time.Now().UTC()
	message := &Message{
		ID:                 gonanoid.Must(),
		DeviceID:           s.config.DeviceID,
		State:              StatePending,
		IsHashed:           false,
		IsEncrypted:        false,
		TextMessage:        textMessage.Text,
		SimNumber:          simNumber(request.SimNumber),
		WithDeliveryReport: request.WithDeliveryReport != nil && *request.WithDeliveryReport,
		Priority:           int(request.Priority),
		Recipients:         request.PhoneNumbers,
		States:             []StateChange{{State: StatePending, At: now}},
		ErrorMessage:       nil,
		CreatedAt:          now,
		UpdatedAt:          now,
		ProcessedAt:        nil,
		SentAt:             nil,
		FailedAt:           nil,
	}

	if _, err := s.repo.Create(ctx, message); err != nil {
		return nil, fmt.Errorf("create message: %w", err)
	}
	s.metrics.EnqueuedTotal.Inc()

	return toMessageState(message), nil
}

// Get returns the state DTO of the message with the given ID, or the
// repository ErrNotFound when it does not exist.
func (s *Service) Get(ctx context.Context, id string) (*smsgateway.MessageState, error) {
	message, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get message: %w", err)
	}

	return toMessageState(message), nil
}

// List returns one page of message-state DTOs plus the total count matching
// the filter (see ListFilter for pagination and ordering semantics).
func (s *Service) List(ctx context.Context, filter ListFilter) (smsgateway.GetMessagesResponse, int, error) {
	rows, total, err := s.repo.List(ctx, filter)
	if err != nil {
		return nil, 0, fmt.Errorf("list messages: %w", err)
	}

	result := make(smsgateway.GetMessagesResponse, 0, len(rows))
	for i := range rows {
		result = append(result, *toMessageState(&rows[i]))
	}

	return result, total, nil
}

// Cancel atomically moves a Pending message to Cancelled and returns its
// state DTO. A message that already left Pending yields ErrNotPending; an
// unknown ID yields ErrNotFound (both from the repository).
func (s *Service) Cancel(ctx context.Context, id string) (*smsgateway.MessageState, error) {
	message, err := s.repo.Cancel(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("cancel message: %w", err)
	}
	s.metrics.CancelledTotal.Inc()

	return toMessageState(message), nil
}

// Run is the background FIFO worker. It repeatedly picks the oldest Pending
// message, sends it to every recipient via the modem and records Sent (with
// sent_at) or Failed (with error_message and failed_at), then continues with
// the next Pending message - strictly single-concurrent, matching the
// SIM800L send path. When the queue is empty it sleeps PollInterval (zero
// falls back to one second) and polls again. Run returns nil when ctx is
// canceled; a repository failure returns a wrapped error (fx shuts down).
//
// Cancel-safety: a message is re-checked to still be Pending before sending
// and again before recording the terminal state, so a Cancel racing the send
// wins and the message stays Cancelled. A crash mid-send leaves the message
// Pending and it is re-sent on the next boot (accepted MVP limitation).
func (s *Service) Run(ctx context.Context) error {
	for {
		message, err := s.repo.GetNextPending(ctx)
		switch {
		case ctx.Err() != nil:
			// Shutdown raced the queue poll; exit gracefully.
			//nolint:nilerr // graceful shutdown: the poll error is irrelevant once ctx is canceled
			return nil
		case errors.Is(err, ErrNotFound):
			if !s.wait(ctx) {
				return nil
			}
			continue
		case err != nil:
			return fmt.Errorf("get next pending message: %w", err)
		}

		s.process(ctx, message)
	}
}

// process sends message to every recipient and records the terminal state.
// A partial multi-recipient send is recorded as Failed (per-recipient
// tracking is out of MVP scope).
func (s *Service) process(ctx context.Context, message *Message) {
	// Re-check Pending: a Cancel that arrived after GetNextPending must win
	// before any modem traffic.
	if !s.isPending(ctx, message.ID) {
		return
	}

	for _, phone := range message.Recipients {
		ref, sendErr := s.sendSMS(ctx, phone, message.TextMessage)
		if sendErr != nil {
			errorMessage := sendErr.Error()
			s.recordTerminalState(ctx, message, StateFailed, &errorMessage)

			return
		}
		// The +CMGS reference is transient: it proves a completed send but
		// has no column to persist to (see domain.go).
		s.logger.Debug("SMS sent", zap.String("id", message.ID), zap.Int("ref", ref))
	}

	s.recordTerminalState(ctx, message, StateSent, nil)
}

// recordTerminalState moves message to the given terminal state, but only
// while the message is still Pending: a concurrent Cancel during the send
// wins and the message stays Cancelled (the SMS may already be on the wire -
// accepted MVP limitation; a tiny check-to-update race remains without a
// repository-level guard).
func (s *Service) recordTerminalState(ctx context.Context, message *Message, state State, errorMessage *string) {
	if !s.isPending(ctx, message.ID) {
		return
	}

	if err := s.repo.UpdateState(ctx, message.ID, state, errorMessage); err != nil {
		s.logger.Error("failed to record terminal state", zap.String("id", message.ID), zap.Error(err))

		return
	}

	switch state {
	case StateSent:
		s.metrics.SentTotal.Inc()
	case StateFailed:
		s.metrics.FailedTotal.Inc()
	case StatePending, StateCancelled:
		// Non-terminal states are never recorded by this helper.
	}
}

// isPending reports whether the message is still Pending, re-reading the
// persisted state so a Cancel that raced the worker wins.
func (s *Service) isPending(ctx context.Context, id string) bool {
	current, err := s.repo.GetByID(ctx, id)
	if err != nil {
		s.logger.Error("failed to inspect pending message", zap.String("id", id), zap.Error(err))

		return false
	}
	if current.State != StatePending {
		s.logger.Debug(
			"skipping message that left pending state",
			zap.String("id", id),
			zap.String("state", string(current.State)),
		)

		return false
	}

	return true
}

// wait sleeps for the poll interval and returns false when ctx is canceled.
// A zero or negative PollInterval falls back to one second so the worker can
// never busy-loop.
func (s *Service) wait(ctx context.Context) bool {
	interval := s.config.PollInterval
	if interval <= 0 {
		interval = time.Second
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// simNumber converts the DTO's optional SIM selector to the domain pointer.
// The MVP modem has a single SIM, so the value is persisted for parity but
// never used for routing.
func simNumber(sim *uint8) *int {
	if sim == nil {
		return nil
	}
	value := int(*sim)

	return &value
}

// toMessageState maps a domain message onto the client-go wire DTO with
// server-parity shapes: every recipient shares the message state (per-
// recipient error populated for Failed), the state history collapses onto
// the map form (last entry wins on repeated states) and content is withheld
// (includeContent is not part of the MVP).
func toMessageState(message *Message) *smsgateway.MessageState {
	recipients := make([]smsgateway.RecipientState, 0, len(message.Recipients))
	for _, phone := range message.Recipients {
		recipient := smsgateway.RecipientState{
			PhoneNumber: phone,
			State:       smsgateway.ProcessingState(message.State),
			Error:       nil,
		}
		if message.State == StateFailed {
			recipient.Error = message.ErrorMessage
		}
		recipients = append(recipients, recipient)
	}

	states := make(map[string]time.Time, len(message.States))
	for _, change := range message.States {
		states[string(change.State)] = change.At
	}

	return &smsgateway.MessageState{
		ID:            message.ID,
		DeviceID:      message.DeviceID,
		State:         smsgateway.ProcessingState(message.State),
		IsHashed:      message.IsHashed,
		IsEncrypted:   message.IsEncrypted,
		Recipients:    recipients,
		States:        states,
		TextMessage:   nil,
		DataMessage:   nil,
		HashedMessage: nil,
	}
}
